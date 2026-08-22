package store

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/pkg/logger"
)

type AccessLogStore struct {
	cfg     *config.StorageCfg
	mu      sync.Mutex
	file    *os.File
	path    string
	ready   atomic.Bool
	wg      sync.WaitGroup
	cancel  context.CancelFunc
	syncInt time.Duration
}

func NewAccessLogStore(cfg *config.Config) (*AccessLogStore, error) {
	if cfg == nil {
		return nil, model.ErrStoreNotReady
	}
	return &AccessLogStore{
		cfg:     &cfg.Storage,
		path:    cfg.Storage.GetLogFilePath(),
		syncInt: cfg.Storage.GetSyncInterval(),
	}, nil
}

func (a *AccessLogStore) Open(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.path == "" {
		return model.NewStoreError("OpenLog", "", errors.New("empty path"))
	}
	if err := EnsureDir(a.path); err != nil {
		return model.NewStoreError("EnsureDir", a.path, err)
	}
	select {
	case <-ctx.Done():
		return model.ErrCanceled
	default:
	}
	f, err := OpenAppend(a.path)
	if err != nil {
		return model.NewStoreError("OpenLogFile", a.path, err)
	}
	a.file = f
	a.ready.Store(true)

	if a.syncInt > 0 {
		var inner context.Context
		inner, a.cancel = context.WithCancel(context.Background())
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			t := time.NewTicker(a.syncInt)
			defer t.Stop()
			for {
				select {
				case <-inner.Done():
					a.mu.Lock()
					if a.file != nil {
						_ = a.file.Sync()
					}
					a.mu.Unlock()
					return
				case <-t.C:
					a.mu.Lock()
					if a.file != nil {
						_ = a.file.Sync()
					}
					a.mu.Unlock()
				}
			}
		}()
	}
	return nil
}

func (a *AccessLogStore) Close() error {
	if a.cancel != nil {
		a.cancel()
	}
	a.wg.Wait()
	a.mu.Lock()
	defer a.mu.Unlock()
	var err error
	if a.file != nil {
		if err = a.file.Sync(); err != nil {
			logger.Warn("access log sync on close error", logger.Fields{"err": err.Error()})
		}
		err = a.file.Close()
		a.file = nil
	}
	a.ready.Store(false)
	return err
}

func (a *AccessLogStore) Ready() bool { return a.ready.Load() }

func (a *AccessLogStore) Path() string { return a.path }

func (a *AccessLogStore) Sync() error {
	if a == nil {
		return nil
	}
	if !a.ready.Load() {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file == nil {
		return nil
	}
	return a.file.Sync()
}

func (a *AccessLogStore) Append(log *model.AccessLog) error {
	if log == nil {
		return errors.New("store: nil access log")
	}
	if !a.ready.Load() {
		return model.ErrStoreNotReady
	}
	data, err := json.Marshal(log)
	if err != nil {
		return model.NewStoreError("MarshalLog", log.Code, err)
	}
	line := append(data, '\n')
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file == nil {
		return model.ErrStoreNotReady
	}
	if _, err := a.file.Write(line); err != nil {
		return model.NewStoreError("WriteLog", log.Code, err)
	}
	if a.cfg != nil && a.cfg.GetFlushOnWrite() {
		_ = a.file.Sync()
	}
	return nil
}

func (a *AccessLogStore) AppendMany(logs []*model.AccessLog) error {
	if len(logs) == 0 {
		return nil
	}
	if !a.ready.Load() {
		return model.ErrStoreNotReady
	}
	buf := make([]byte, 0, 512*len(logs))
	for _, l := range logs {
		if l == nil {
			continue
		}
		data, err := json.Marshal(l)
		if err != nil {
			return model.NewStoreError("MarshalLog", l.Code, err)
		}
		buf = append(buf, data...)
		buf = append(buf, '\n')
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file == nil {
		return model.ErrStoreNotReady
	}
	if _, err := a.file.Write(buf); err != nil {
		return model.NewStoreError("WriteLogs", "", err)
	}
	if a.cfg != nil && a.cfg.GetFlushOnWrite() {
		_ = a.file.Sync()
	}
	return nil
}

func (a *AccessLogStore) Scan(fn func(l *model.AccessLog) bool, maxRecords int) (int, error) {
	if !a.ready.Load() {
		return 0, model.ErrStoreNotReady
	}
	a.mu.Lock()
	path := a.path
	a.mu.Unlock()
	f, err := osReadOpen(path)
	if err != nil {
		return 0, model.NewStoreError("ScanLogOpen", path, err)
	}
	defer func() { _ = f.Close() }()

	dec := json.NewDecoder(f)
	count := 0
	for dec.More() {
		if maxRecords > 0 && count >= maxRecords {
			return count, nil
		}
		var l model.AccessLog
		if err := dec.Decode(&l); err != nil {
			logger.Warn("access log decode error, skip line", logger.Fields{"err": err.Error()})
			continue
		}
		count++
		if !fn(&l) {
			return count, nil
		}
	}
	return count, nil
}

func (a *AccessLogStore) CountLines() (int64, error) {
	if !a.ready.Load() {
		return 0, model.ErrStoreNotReady
	}
	a.mu.Lock()
	path := a.path
	a.mu.Unlock()
	f, err := osReadOpen(path)
	if err != nil {
		return 0, model.NewStoreError("CountLogLines", path, err)
	}
	defer func() { _ = f.Close() }()
	var n int64
	dec := json.NewDecoder(f)
	for dec.More() {
		var l model.AccessLog
		if err := dec.Decode(&l); err != nil {
			continue
		}
		n++
	}
	return n, nil
}

func (a *AccessLogStore) SizeBytes() (int64, error) {
	a.mu.Lock()
	path := a.path
	a.mu.Unlock()
	return FileSize(path)
}

func osReadOpen(path string) (*os.File, error) { return os.Open(path) }

func readAll(r io.Reader) ([]byte, error) { return io.ReadAll(r) }
