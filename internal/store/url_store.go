package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/pkg/logger"
)

type URLStore struct {
	cfg      *config.StorageCfg
	mu       sync.RWMutex
	urls     map[string]*model.ShortURL
	ready    atomic.Bool
	dirty    atomic.Bool
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
	path     string
	flushOn  bool
	syncInt  time.Duration
}

func NewURLStore(cfg *config.Config) (*URLStore, error) {
	if cfg == nil {
		return nil, model.ErrStoreNotReady
	}
	return &URLStore{
		cfg:     &cfg.Storage,
		urls:    make(map[string]*model.ShortURL),
		path:    cfg.Storage.URLFilePath,
		flushOn: cfg.Storage.FlushOnWrite,
		syncInt: cfg.Storage.SyncInterval,
	}, nil
}

func (s *URLStore) Load(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.path == "" {
		return model.NewStoreError("LoadURL", "", errors.New("empty path"))
	}
	if err := EnsureDir(s.path); err != nil {
		return model.NewStoreError("EnsureDir", s.path, err)
	}
	select {
	case <-ctx.Done():
		return model.ErrCanceled
	default:
	}
	f, err := osReadOpen(s.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return model.NewStoreError("OpenURLFile", s.path, err)
		}
		s.ready.Store(true)
		s.startSyncerLocked()
		return nil
	}
	defer func() { _ = f.Close() }()

	data, err := readAll(f)
	if err != nil {
		return model.NewStoreError("ReadURLFile", s.path, err)
	}
	m := make(map[string]*model.ShortURL)
	if len(data) > 0 {
		if err := json.Unmarshal(data, &m); err != nil {
			return model.NewStoreError("ParseURLFile", s.path, err)
		}
	}
	s.urls = m
	s.ready.Store(true)
	s.startSyncerLocked()
	return nil
}

func (s *URLStore) startSyncerLocked() {
	if s.syncInt <= 0 {
		return
	}
	var ctx context.Context
	ctx, s.cancelFn = context.WithCancel(context.Background())
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.syncInt)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				if err := s.sync(); err != nil {
					logger.Error("url store final sync error", logger.Fields{"err": err.Error()})
				}
				return
			case <-ticker.C:
				if !s.dirty.Load() {
					continue
				}
				if err := s.sync(); err != nil {
					logger.Warn("url store periodic sync error", logger.Fields{"err": err.Error()})
				}
			}
		}
	}()
}

// cloneShortURL 返回 u 的深拷贝，避免外泄 store 内部活动的 *ShortURL 指针。
// 所有对外返回的 *ShortURL 都必须经此克隆，保证字段读写只发生在 store 的锁内。
func cloneShortURL(u *model.ShortURL) *model.ShortURL {
	if u == nil {
		return nil
	}
	cp := *u
	return &cp
}

// VisitOutcome 是 RecordVisit 的结果：单次重定向访问在写锁内完成判定与自增后，
// 返回足够的信息让 RedirectService 决定 302/410 状态，而无需触碰 *ShortURL 字段。
type VisitOutcome struct {
	Found      bool
	Disabled   bool   // 进入本次访问前该短码已是禁用状态
	Expired    bool   // 以 now 判定该短码已过期
	MaxVisited bool   // 本次访问触发 max-visits 自动禁用
	Visits     int64  // 自增后的访问次数（未自增则为 0）
	RawURL     string // 重定向目标（不重定向时为空）
	Code       string
}

func (s *URLStore) sync() error {
	s.mu.RLock()
	out := make(map[string]*model.ShortURL, len(s.urls))
	for k, v := range s.urls {
		cp := *v // 克隆值字段必须在 RLock 内读取
		out[k] = &cp
	}
	s.mu.RUnlock()
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return model.NewStoreError("MarshalURLs", "", err)
	}
	if err := WriteAtomic(s.path, data); err != nil {
		return model.NewStoreError("WriteAtomicURLFile", s.path, err)
	}
	s.dirty.Store(false)
	return nil
}

func (s *URLStore) Flush() error {
	if s == nil {
		return nil
	}
	if !s.ready.Load() {
		return model.ErrStoreNotReady
	}
	return s.sync()
}

func (s *URLStore) Close() error {
	if s.cancelFn != nil {
		s.cancelFn()
	}
	s.wg.Wait()
	return s.sync()
}

func (s *URLStore) Ready() bool { return s.ready.Load() }

func (s *URLStore) Path() string { return s.path }

func (s *URLStore) Get(code string) (*model.ShortURL, error) {
	if !s.ready.Load() {
		return nil, model.ErrStoreNotReady
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.urls[code]
	if !ok {
		return nil, model.ErrCodeNotFound
	}
	// 返回克隆，调用方对字段的所有读写都不会触达 store 内部活动指针。
	return cloneShortURL(u), nil
}

func (s *URLStore) GetCached(code string) *model.ShortURL {
	if !s.ready.Load() {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.urls[code]
	if !ok {
		return nil
	}
	// 返回克隆：保证缓存层持有的也是快照，避免与并发变更竞争。
	return cloneShortURL(u)
}

func (s *URLStore) Exists(code string) (bool, error) {
	if !s.ready.Load() {
		return false, model.ErrStoreNotReady
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.urls[code]
	return ok, nil
}

func (s *URLStore) Save(u *model.ShortURL, overwrite bool) error {
	if u == nil {
		return errors.New("store: nil shorturl")
	}
	if !s.ready.Load() {
		return model.ErrStoreNotReady
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.urls[u.Code]
	if ok && !overwrite {
		return model.ErrCodeConflict
	}
	if ok && overwrite && existing != nil {
		existing.Visits = u.Visits
		existing.RawURL = u.RawURL
		existing.CreatedAt = u.CreatedAt
		existing.ExpireAt = u.ExpireAt
		existing.MaxVisits = u.MaxVisits
		existing.Custom = u.Custom
		existing.Disabled = u.Disabled
		existing.Remark = u.Remark
		s.dirty.Store(true)
		if s.flushOn {
			if err := s.flushLocked(); err != nil {
				return err
			}
		}
		return nil
	}
	clone := *u
	s.urls[u.Code] = &clone
	s.dirty.Store(true)
	if s.flushOn {
		if err := s.flushLocked(); err != nil {
			return err
		}
	}
	return nil
}

func (s *URLStore) flushLocked() error {
	cloneMap := make(map[string]*model.ShortURL, len(s.urls))
	for k, v := range s.urls {
		cp := *v
		cloneMap[k] = &cp
	}
	data, err := json.MarshalIndent(cloneMap, "", "  ")
	if err != nil {
		return model.NewStoreError("MarshalOnFlush", "", err)
	}
	if err := WriteAtomic(s.path, data); err != nil {
		return model.NewStoreError("WriteOnFlush", s.path, err)
	}
	s.dirty.Store(false)
	return nil
}

func (s *URLStore) Delete(code string) error {
	if !s.ready.Load() {
		return model.ErrStoreNotReady
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.urls[code]
	if !ok {
		return model.ErrCodeNotFound
	}
	if existing != nil {
		existing.Disabled = true
	}
	delete(s.urls, code)
	s.dirty.Store(true)
	if s.flushOn {
		if err := s.flushLocked(); err != nil {
			return err
		}
	}
	return nil
}

func (s *URLStore) IncrementVisits(code string) (*model.ShortURL, error) {
	if !s.ready.Load() {
		return nil, model.ErrStoreNotReady
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.urls[code]
	if !ok {
		return nil, model.ErrCodeNotFound
	}
	u.Visits++
	clone := *u
	s.dirty.Store(true)
	return &clone, nil
}

// RecordVisit 在写锁内完成一次重定向访问的全部副作用：
//  1. 判定 Disabled / Expired（命中则不自增，直接返回对应状态）；
//  2. u.Visits++；
//  3. 若达到 MaxVisits 则置 u.Disabled=true，并标记 MaxVisited。
//
// 它是重定向路径上对 Visits / Disabled 的唯一变更点，替代了过去
// HandleRedirect 直接改指针 + BulkIncrementVisits 异步再改一次的重复自增。
// now 由调用方传入（便于测试与重定向时间口径一致）。
func (s *URLStore) RecordVisit(code string, now time.Time) (VisitOutcome, error) {
	if !s.ready.Load() {
		return VisitOutcome{}, model.ErrStoreNotReady
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.urls[code]
	if !ok {
		return VisitOutcome{Code: code}, nil // Found=false
	}
	out := VisitOutcome{Found: true, Code: u.Code, RawURL: u.RawURL}
	switch {
	case u.Disabled:
		out.Disabled = true
		return out, nil
	case u.IsExpired(now):
		out.Expired = true
		return out, nil
	}
	u.Visits++
	out.Visits = u.Visits
	if u.MaxVisits > 0 && u.Visits >= u.MaxVisits {
		u.Disabled = true
		out.MaxVisited = true
	}
	s.dirty.Store(true)
	return out, nil
}

// SetDisabled 在写锁内原子地更新 Disabled 字段，替代 Get→改指针→Save 的往返
// （后者会用陈旧的 Visits 覆盖活动值）。供禁用 / 巡检使用。
func (s *URLStore) SetDisabled(code string, disabled bool) error {
	if !s.ready.Load() {
		return model.ErrStoreNotReady
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.urls[code]
	if !ok {
		return model.ErrCodeNotFound
	}
	u.Disabled = disabled
	s.dirty.Store(true)
	return nil
}

// SetRemark 在写锁内原子地更新 Remark 字段，替代 Get→改指针→Save 的往返。
func (s *URLStore) SetRemark(code, remark string) error {
	if !s.ready.Load() {
		return model.ErrStoreNotReady
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.urls[code]
	if !ok {
		return model.ErrCodeNotFound
	}
	u.Remark = remark
	s.dirty.Store(true)
	return nil
}

func (s *URLStore) ForEach(fn func(u *model.ShortURL) bool) error {
	if !s.ready.Load() {
		return model.ErrStoreNotReady
	}
	s.mu.RLock()
	// 在锁内克隆，保证 fn 拿到的是快照而非活动指针，避免与并发变更竞争。
	snapshot := make([]*model.ShortURL, 0, len(s.urls))
	for _, u := range s.urls {
		snapshot = append(snapshot, cloneShortURL(u))
	}
	s.mu.RUnlock()
	for _, u := range snapshot {
		if !fn(u) {
			return nil
		}
	}
	return nil
}

func (s *URLStore) ListCodes(limit int) ([]*model.ShortURL, error) {
	if !s.ready.Load() {
		return nil, model.ErrStoreNotReady
	}
	s.mu.RLock()
	out := make([]*model.ShortURL, 0, len(s.urls))
	for _, u := range s.urls {
		clone := *u
		out = append(out, &clone)
	}
	s.mu.RUnlock()
	sortShortURLByCreated(out)
	if limit > 0 && limit < len(out) {
		out = out[:limit]
	}
	return out, nil
}

func (s *URLStore) Stats() (total, active, disabled, expired int, err error) {
	if !s.ready.Load() {
		return 0, 0, 0, 0, model.ErrStoreNotReady
	}
	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()
	// 字段读取必须在锁内：否则与并发的 Disabled / Visits 变更竞争。
	for _, u := range s.urls {
		total++
		switch {
		case u.Disabled:
			disabled++
		case u.IsExpired(now):
			expired++
		default:
			active++
		}
	}
	return
}

func (s *URLStore) Count() int {
	if !s.ready.Load() {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.urls)
}

func osReadOpen(path string) (*os.File, error) { return os.Open(path) }
