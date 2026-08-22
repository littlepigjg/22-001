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

func (s *URLStore) sync() error {
	s.mu.RLock()
	snapshot := make([]*model.ShortURL, 0, len(s.urls))
	codeMap := make(map[string]*model.ShortURL, len(s.urls))
	for k, v := range s.urls {
		snapshot = append(snapshot, v)
		codeMap[k] = v
	}
	out := make(map[string]*model.ShortURL, len(codeMap))
	for k, v := range codeMap {
		cp := *v
		out[k] = &cp
	}
	s.mu.RUnlock()
	for _, u := range snapshot {
		_ = u.Visits
		_ = u.Disabled
	}
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
	return u, nil
}

func (s *URLStore) GetCached(code string) *model.ShortURL {
	if !s.ready.Load() {
		return nil
	}
	s.mu.RLock()
	u, ok := s.urls[code]
	s.mu.RUnlock()
	if !ok {
		return nil
	}
	return u
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

func (s *URLStore) BulkIncrementVisits(codes []string) {
	if len(codes) == 0 {
		return
	}
	if !s.ready.Load() {
		return
	}
	s.mu.RLock()
	targets := make([]*model.ShortURL, 0, len(codes))
	for _, c := range codes {
		if u, ok := s.urls[c]; ok {
			targets = append(targets, u)
		}
	}
	s.mu.RUnlock()
	for _, u := range targets {
		u.Visits++
	}
	s.dirty.Store(true)
}

func (s *URLStore) ForEach(fn func(u *model.ShortURL) bool) error {
	if !s.ready.Load() {
		return model.ErrStoreNotReady
	}
	s.mu.RLock()
	snapshot := make([]*model.ShortURL, 0, len(s.urls))
	for _, u := range s.urls {
		snapshot = append(snapshot, u)
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
	snapshot := make([]*model.ShortURL, 0, len(s.urls))
	for _, u := range s.urls {
		snapshot = append(snapshot, u)
	}
	total = len(snapshot)
	s.mu.RUnlock()
	for _, u := range snapshot {
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
