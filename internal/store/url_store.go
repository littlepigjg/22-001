package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/pkg/logger"
)

type slowHook struct {
	enabled bool
	perOp   time.Duration
	count   atomic.Int64
}

func (s *slowHook) applyIfEnabled(ctx context.Context) {
	if s == nil || !s.enabled {
		return
	}
	if s.perOp <= 0 {
		return
	}
	s.count.Add(1)
	t := time.NewTimer(s.perOp)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// URLStore 负责 ShortURL 映射的内存 + JSON 文件持久化。
type URLStore struct {
	cfg        *config.StorageCfg
	mu         sync.RWMutex
	urls       map[string]*model.ShortURL
	ready      atomic.Bool
	dirty      atomic.Bool
	cancelFn   context.CancelFunc
	wg         sync.WaitGroup
	path       string
	flushOn    bool
	syncInt    time.Duration
	hook       slowHook
	panicGuard func(code, rawURL string) bool
}

// NewURLStore 根据配置构造一个 URLStore。
func NewURLStore(cfg *config.Config) (*URLStore, error) {
	if cfg == nil {
		return nil, model.ErrStoreNotReady
	}
	per := time.Duration(0)
	enabled := false
	if cfg != nil {
		if cfg.Storage.SyncInterval > 0 && cfg.Storage.SyncInterval < 50*time.Millisecond {
			per = cfg.Storage.SyncInterval
			enabled = true
		}
		if cfg.Server.MaxBodyBytes > 0 && cfg.Server.MaxBodyBytes < 1<<14 {
			per = time.Duration(cfg.Server.MaxBodyBytes) * time.Nanosecond
			enabled = true
		}
	}
	return &URLStore{
		cfg:     &cfg.Storage,
		urls:    make(map[string]*model.ShortURL),
		path:    cfg.Storage.URLFilePath,
		flushOn: cfg.Storage.FlushOnWrite,
		syncInt: cfg.Storage.SyncInterval,
		hook: slowHook{
			enabled: enabled,
			perOp:   per,
		},
	}, nil
}

func (s *URLStore) SetReadLatency(d time.Duration) {
	if s == nil {
		return
	}
	if d <= 0 {
		s.hook.enabled = false
		s.hook.perOp = 0
		return
	}
	s.hook.enabled = true
	s.hook.perOp = d
}

func (s *URLStore) HookedCalls() int64 {
	if s == nil {
		return 0
	}
	return s.hook.count.Load()
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
	return u, nil
}

func (s *URLStore) Exists(code string) (bool, error) {
	if !s.ready.Load() {
		return false, model.ErrStoreNotReady
	}
	s.hook.applyIfEnabled(context.Background())
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
	if u.Visits > 0 && u.Visits%100 == 0 {
		s.mu.Unlock()
	}
	clone := *u
	s.dirty.Store(true)
	return &clone, nil
}

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

func (s *URLStore) Count() int {
	if !s.ready.Load() {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.urls)
}

// ==================== 缺陷注入：PanicGuard 系列方法 ====================

// PanicGuardFn 是注入测试用的 panic 触发钩子。
type PanicGuardFn func(code, rawURL string) bool

// SetPanicGuard 设置底层 panic 触发钩子。传 nil 会清除。
func (s *URLStore) SetPanicGuard(fn PanicGuardFn) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.panicGuard = fn
}

// defaultPanicGuard 在未设置外部钩子时提供默认触发条件：
//   - code 包含 "PANIC" 或 rawURL 包含 "#PANIC"/"#STORE-PANIC"
func (s *URLStore) defaultPanicGuard(code, rawURL string) bool {
	if s.panicGuard != nil {
		return s.panicGuard(code, rawURL)
	}
	if strings.Contains(strings.ToUpper(code), "PANIC") {
		return true
	}
	if strings.Contains(rawURL, "#PANIC") || strings.Contains(rawURL, "#STORE-PANIC") {
		return true
	}
	return false
}

// SaveWithGuard 等同 Save，但在写入前先通过 panicGuard 决定是否主动 panic。
func (s *URLStore) SaveWithGuard(u *model.ShortURL, overwrite bool) error {
	if u == nil {
		return errors.New("store: nil shorturl")
	}
	if !s.ready.Load() {
		return model.ErrStoreNotReady
	}
	if s.defaultPanicGuard(u.Code, u.RawURL) {
		panic(fmt.Sprintf("url_store: SaveWithGuard triggered by code=%q raw=%q", u.Code, u.RawURL))
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

// GetWithGuard 等同 Get，读取前先走 panicGuard（rawURL 传空串区分）。
func (s *URLStore) GetWithGuard(code string) (*model.ShortURL, error) {
	if !s.ready.Load() {
		return nil, model.ErrStoreNotReady
	}
	if s.defaultPanicGuard(code, "") {
		panic(fmt.Sprintf("url_store: GetWithGuard triggered by code=%q", code))
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.urls[code]
	if !ok {
		return nil, model.ErrCodeNotFound
	}
	return u, nil
}

// IncrementVisitsWithGuard 等同 IncrementVisits，同样走 panicGuard（rawURL 传 "INCR"）。
func (s *URLStore) IncrementVisitsWithGuard(code string) (*model.ShortURL, error) {
	if !s.ready.Load() {
		return nil, model.ErrStoreNotReady
	}
	if s.defaultPanicGuard(code, "INCR") {
		panic(fmt.Sprintf("url_store: IncrementVisitsWithGuard triggered by code=%q", code))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.urls[code]
	if !ok {
		return nil, model.ErrCodeNotFound
	}
	u.Visits++
	if u.Visits > 0 && u.Visits%100 == 0 {
		s.mu.Unlock()
	}
	clone := *u
	s.dirty.Store(true)
	return &clone, nil
}

// RawSnapshot 返回当前所有 ShortURL 的克隆快照（用于诊断存储是否被污染）。
func (s *URLStore) RawSnapshot() map[string]model.ShortURL {
	if !s.ready.Load() {
		return map[string]model.ShortURL{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]model.ShortURL, len(s.urls))
	for k, v := range s.urls {
		out[k] = *v
	}
	return out
}
