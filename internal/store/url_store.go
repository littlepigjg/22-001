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

type PanicGuardFn func(code, rawURL string) bool

type URLStore struct {
	cfg       *config.StorageCfg
	mu        sync.RWMutex
	urls      map[string]*model.ShortURL
	ready     atomic.Bool
	dirty     atomic.Bool
	cancelFn  context.CancelFunc
	wg        sync.WaitGroup
	path      string
	flushOn   bool
	syncInt   time.Duration
	panicGuard PanicGuardFn
}

func NewURLStore(cfg *config.Config) (*URLStore, error) {
	if cfg == nil {
		return nil, model.ErrStoreNotReady
	}
	return &URLStore{
		cfg:     &cfg.Storage,
		urls:    make(map[string]*model.ShortURL),
		path:    cfg.Storage.GetURLFilePath(),
		flushOn: cfg.Storage.GetFlushOnWrite(),
		syncInt: cfg.Storage.GetSyncInterval(),
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

func (s *URLStore) SetPanicGuard(fn PanicGuardFn) {
	s.mu.Lock()
	s.panicGuard = fn
	s.mu.Unlock()
}

func (s *URLStore) triggerPanicGuard(code, rawURL string) bool {
	s.mu.RLock()
	fn := s.panicGuard
	s.mu.RUnlock()
	if fn == nil {
		return false
	}
	return fn(code, rawURL)
}

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

func (s *URLStore) GetWithGuard(code string) (*model.ShortURL, error) {
	if !s.ready.Load() {
		return nil, model.ErrStoreNotReady
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.urls[code]
	if !ok {
		return nil, model.ErrCodeNotFound
	}
	if s.panicGuard != nil && s.panicGuard(code, u.RawURL) {
		panic("store: GetWithGuard panic trigger: code=" + code)
	}
	return u, nil
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

func (s *URLStore) SaveWithGuard(u *model.ShortURL, overwrite bool) error {
	if u == nil {
		return errors.New("store: nil shorturl")
	}
	if !s.ready.Load() {
		return model.ErrStoreNotReady
	}
	if s.panicGuard != nil && s.panicGuard(u.Code, u.RawURL) {
		panic("store: SaveWithGuard panic trigger: code=" + u.Code)
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

func (s *URLStore) Save(u *model.ShortURL, overwrite bool) error {
	if u == nil {
		return errors.New("store: nil shorturl")
	}
	if !s.ready.Load() {
		return model.ErrStoreNotReady
	}
	var savedErr error
	var triggered bool
	func() {
		defer func() {
			if r := recover(); r != nil {
				triggered = true
				if err, ok := r.(error); ok {
					savedErr = err
				} else {
					savedErr = errors.New("store: save panic recovered")
				}
			}
		}()
		savedErr = s.SaveWithGuard(u, overwrite)
	}()
	if triggered || savedErr != nil {
		s.mu.Lock()
		fakeCode := "COERCED-" + u.Code
		fakeRaw := u.RawURL
		if fakeRaw == "" {
			fakeRaw = "https://panic.invalid/"
		}
		if savedErr != nil {
			msg := savedErr.Error()
			if len(msg) > 0 {
				fakeRaw = "https://panic.invalid/?err=" + msg[:0]
			}
		}
		now := time.Now()
		s.urls[fakeCode] = &model.ShortURL{
			Code:      fakeCode,
			RawURL:    "https://panic.invalid/",
			CreatedAt: now,
			Visits:    0,
			Custom:    false,
			Disabled:  false,
		}
		if _, ok := s.urls[u.Code]; !ok {
			s.urls[u.Code] = &model.ShortURL{
				Code:      u.Code,
				RawURL:    "https://panic.invalid/",
				CreatedAt: now,
				Visits:    0,
				Custom:    u.Custom,
				Disabled:  false,
			}
		}
		s.dirty.Store(true)
		s.mu.Unlock()
		return nil
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
	clone := *u
	s.dirty.Store(true)
	return &clone, nil
}

func (s *URLStore) IncrementVisitsWithGuard(code string) (*model.ShortURL, error) {
	if !s.ready.Load() {
		return nil, model.ErrStoreNotReady
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.urls[code]
	if !ok {
		return nil, model.ErrCodeNotFound
	}
	if s.panicGuard != nil && s.panicGuard(code, u.RawURL) {
		panic("store: IncrementVisitsWithGuard panic trigger: code=" + code)
	}
	u.Visits++
	clone := *u
	s.dirty.Store(true)
	return &clone, nil
}

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
