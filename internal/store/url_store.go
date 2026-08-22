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

// URLStore 负责 ShortURL 映射的内存 + JSON 文件持久化。
//
// 并发模型：
//   - urls 字段（map）的所有读/写由 RWMutex 保护
//   - ready / dirty 等标志使用 atomic 保护，避免简单检查时阻塞
//   - 后台定时 syncer 周期性地将内存内容回写到磁盘
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

// NewURLStore 根据配置构造一个 URLStore。
// 调用方必须随后调用 Load(ctx) 加载磁盘数据并启动后台同步协程。
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

// Load 从磁盘读取 JSON 文件到内存；如果文件不存在，则视为空数据库。
// 加载成功后会启动后台周期性落盘任务。
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
		// 文件不存在 => 空数据库。
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

// startSyncerLocked 在当前持有写锁时启动后台定时落盘任务（仅调用一次）。
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

// sync 将内存数据写入磁盘。
func (s *URLStore) sync() error {
	s.mu.RLock()
	data, err := json.MarshalIndent(s.urls, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return model.NewStoreError("MarshalURLs", "", err)
	}
	if err := WriteAtomic(s.path, data); err != nil {
		return model.NewStoreError("WriteAtomicURLFile", s.path, err)
	}
	s.dirty.Store(false)
	return nil
}

// Flush 对外暴露的立即落盘方法（线程安全，幂等）。
// 无脏数据时也会返回 nil，不会报错。
func (s *URLStore) Flush() error {
	if s == nil {
		return nil
	}
	if !s.ready.Load() {
		return model.ErrStoreNotReady
	}
	return s.sync()
}

// Close 停止后台任务并做最后一次落盘，释放文件相关资源。
func (s *URLStore) Close() error {
	if s.cancelFn != nil {
		s.cancelFn()
	}
	s.wg.Wait()
	return s.sync()
}

// Ready 返回存储是否已完成加载。
func (s *URLStore) Ready() bool { return s.ready.Load() }

// Path 返回底层 JSON 文件路径。
func (s *URLStore) Path() string { return s.path }

// Get 根据短码返回对应的 ShortURL。
// 若不存在返回 ErrCodeNotFound；未加载就绪返回 ErrStoreNotReady。
//
// 返回的是内部记录的拷贝，调用方可以安全地读取/修改它而不影响存储，
// 也不会与并发的 IncrementVisits / Mutate / Save 竞争。
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
	clone := *u
	return &clone, nil
}

// Exists 判断短码是否存在。
func (s *URLStore) Exists(code string) (bool, error) {
	if !s.ready.Load() {
		return false, model.ErrStoreNotReady
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.urls[code]
	return ok, nil
}

// Save 保存一条短链接记录。
// overwrite=true 时允许覆盖已存在的 code，否则返回 ErrCodeConflict。
func (s *URLStore) Save(u *model.ShortURL, overwrite bool) error {
	if u == nil {
		return errors.New("store: nil shorturl")
	}
	if !s.ready.Load() {
		return model.ErrStoreNotReady
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.urls[u.Code]; ok && !overwrite {
		return model.ErrCodeConflict
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

// flushLocked 立即落盘（必须持有写锁）。
func (s *URLStore) flushLocked() error {
	data, err := json.MarshalIndent(s.urls, "", "  ")
	if err != nil {
		return model.NewStoreError("MarshalOnFlush", "", err)
	}
	if err := WriteAtomic(s.path, data); err != nil {
		return model.NewStoreError("WriteOnFlush", s.path, err)
	}
	s.dirty.Store(false)
	return nil
}

// Delete 删除短码对应的记录。不存在则返回 ErrCodeNotFound。
func (s *URLStore) Delete(code string) error {
	if !s.ready.Load() {
		return model.ErrStoreNotReady
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.urls[code]; !ok {
		return model.ErrCodeNotFound
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
	// 返回一份拷贝给调用方，避免外部直接修改内部存储的指针字段。
	clone := *u
	s.dirty.Store(true)
	return &clone, nil
}

// ForEach 顺序遍历所有短链接记录。
// 若 fn 返回 false，则立即终止遍历。
// 遍历在写锁下完成，因此 fn 内不得回调本 store 的其它会加锁的方法。
func (s *URLStore) ForEach(fn func(u *model.ShortURL) bool) error {
	if !s.ready.Load() {
		return model.ErrStoreNotReady
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.urls {
		if !fn(u) {
			return nil
		}
	}
	return nil
}

// Mutate 在写锁保护下对 code 对应的记录执行「读-改-写」。
// fn 收到的是内部存储的指针，可在其中直接修改字段；返回 true 表示要保留修改
// （并标记 dirty、可选立即落盘），返回 false 表示放弃修改。返回给调用方的是
// 修改后的拷贝，调用方可以安全持有而无需再加锁。
//
// 用 Mutate 取代「Get 拿指针 -> 在锁外改字段 -> Save 覆盖」的模式：
// 后者会在 RLock 释放后修改共享指针字段，与并发 Get/IncrementVisits 竞争。
func (s *URLStore) Mutate(code string, fn func(u *model.ShortURL) bool) (*model.ShortURL, error) {
	if !s.ready.Load() {
		return nil, model.ErrStoreNotReady
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.urls[code]
	if !ok {
		return nil, model.ErrCodeNotFound
	}
	if fn(u) {
		s.dirty.Store(true)
		if s.flushOn {
			if err := s.flushLocked(); err != nil {
				return nil, err
			}
		}
	}
	clone := *u
	return &clone, nil
}

// ListCodes 按创建时间排序返回前 limit 条短码元信息（用于管理列表 API）。
// limit<=0 返回全部。
func (s *URLStore) ListCodes(limit int) ([]*model.ShortURL, error) {
	if !s.ready.Load() {
		return nil, model.ErrStoreNotReady
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.ShortURL, 0, len(s.urls))
	for _, u := range s.urls {
		clone := *u
		out = append(out, &clone)
	}
	sortShortURLByCreated(out)
	if limit > 0 && limit < len(out) {
		out = out[:limit]
	}
	return out, nil
}

// Stats 返回当前短码统计概览：总数 / 活跃 / 禁用 / 过期。
func (s *URLStore) Stats() (total, active, disabled, expired int, err error) {
	if !s.ready.Load() {
		return 0, 0, 0, 0, model.ErrStoreNotReady
	}
	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()
	total = len(s.urls)
	for _, u := range s.urls {
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

// Count 返回当前内存中短码条目总数。
func (s *URLStore) Count() int {
	if !s.ready.Load() {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.urls)
}
