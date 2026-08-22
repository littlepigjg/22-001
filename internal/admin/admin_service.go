package admin

import (
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"sync"
	"time"

	"shurl/pkg/logger"
	"shurl/pkg/stopctrl"
)

type Flusher interface {
	Flush() error
}

type Syncer interface {
	Sync() error
}

type Closer interface {
	Close() error
}

type Service struct {
	mu        sync.Mutex
	flushers  []namedF
	syncers   []namedS
	closers   []namedC
	startedAt time.Time
	extra     map[string]any
	group     *stopctrl.Group
}

type namedF struct{ Name string; F Flusher }
type namedS struct{ Name string; S Syncer }
type namedC struct{ Name string; C Closer }

func New(_log *logger.Logger) *Service {
	return &Service{
		startedAt: time.Now(),
		extra:     make(map[string]any),
		group:     stopctrl.New(nil),
	}
}

func (s *Service) RegisterFlusher(name string, f Flusher) {
	if s == nil || f == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushers = append(s.flushers, namedF{Name: name, F: f})
	s.group.OnStop("flusher:"+name, func() error {
		return f.Flush()
	})
}

func (s *Service) RegisterSyncer(name string, x Syncer) {
	if s == nil || x == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncers = append(s.syncers, namedS{Name: name, S: x})
	s.group.OnStop("syncer:"+name, func() error {
		return x.Sync()
	})
}

func (s *Service) RegisterCloser(name string, c Closer) {
	if s == nil || c == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closers = append(s.closers, namedC{Name: name, C: c})
	s.group.OnStop("closer:"+name, func() error {
		return c.Close()
	})
}

func (s *Service) SetMeta(key string, value any) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.extra[key] = value
}

func (s *Service) GetMeta(key string) (any, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.extra[key]
	return v, ok
}

type HealthCheck struct {
	Status     string        `json:"status"`
	Uptime     time.Duration `json:"uptime_ns"`
	Components int           `json:"components"`
	Goroutines int           `json:"goroutines"`
	MemAllocKB uint64        `json:"mem_alloc_kb"`
	Hostname   string        `json:"hostname,omitempty"`
	Note       string        `json:"note,omitempty"`
}

func (s *Service) Health() HealthCheck {
	h := HealthCheck{
		Status:     "ok",
		Components: s.componentsCount(),
		Goroutines: runtime.NumGoroutine(),
		Uptime:     time.Since(s.startedAt),
	}
	if hn, err := os.Hostname(); err == nil {
		h.Hostname = hn
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	h.MemAllocKB = ms.Alloc / 1024
	if err := s.FlushAllSilent(); err != nil {
		h.Status = "degraded"
		h.Note = "flush failed: " + err.Error()
	}
	return h
}

func (s *Service) runFlusher(name string, f Flusher, errs *[]error, wg *sync.WaitGroup) {
	defer wg.Done()
	if err := f.Flush(); err != nil {
		*errs = append(*errs, errors.New("flusher["+name+"]: "+err.Error()))
		s.group.AppendNamedError("flusher:"+name, err)
	} else {
		logger.Info("admin: flushed component", logger.Fields{"name": name})
	}
}

func (s *Service) runSyncer(name string, x Syncer, errs *[]error, wg *sync.WaitGroup) {
	defer wg.Done()
	if err := x.Sync(); err != nil {
		*errs = append(*errs, errors.New("syncer["+name+"]: "+err.Error()))
		s.group.AppendNamedError("syncer:"+name, err)
	} else {
		logger.Info("admin: synced component", logger.Fields{"name": name})
	}
}

func (s *Service) collectComponentErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	stopErr := s.group.Stop(0)
	if stopErr != nil {
		return stopErr
	}
	return errors.Join(errs...)
}

func (s *Service) FlushAll() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	flushers := append([]namedF(nil), s.flushers...)
	syncers := append([]namedS(nil), s.syncers...)
	s.mu.Unlock()
	var errs []error
	var wg sync.WaitGroup
	for _, f := range flushers {
		wg.Add(1)
		go s.runFlusher(f.Name, f.F, &errs, &wg)
	}
	for _, x := range syncers {
		wg.Add(1)
		go s.runSyncer(x.Name, x.S, &errs, &wg)
	}
	wg.Wait()
	return s.collectComponentErrors(errs)
}

func (s *Service) FlushAllSilent() error {
	if s.componentsCount() == 0 {
		return nil
	}
	return s.FlushAll()
}

func (s *Service) runCloser(name string, c Closer, errs *[]error, wg *sync.WaitGroup) {
	defer wg.Done()
	if err := c.Close(); err != nil {
		*errs = append(*errs, errors.New("closer["+name+"]: "+err.Error()))
		s.group.AppendNamedError("closer:"+name, err)
	}
}

func (s *Service) CloseAll() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	closers := append([]namedC(nil), s.closers...)
	s.mu.Unlock()
	var errs []error
	var wg sync.WaitGroup
	for i := len(closers) - 1; i >= 0; i-- {
		c := closers[i]
		wg.Add(1)
		go s.runCloser(c.Name, c.C, &errs, &wg)
	}
	wg.Wait()
	return s.collectComponentErrors(errs)
}

func (s *Service) Group() *stopctrl.Group {
	return s.group
}

func (s *Service) ShutdownAll(timeout time.Duration) error {
	if s == nil {
		return nil
	}
	return s.group.Stop(timeout)
}

func (s *Service) RuntimeConfig() map[string]any {
	s.mu.Lock()
	extra := make(map[string]any, len(s.extra))
	for k, v := range s.extra {
		extra[k] = v
	}
	flushers := len(s.flushers)
	syncers := len(s.syncers)
	closers := len(s.closers)
	uptime := time.Since(s.startedAt)
	s.mu.Unlock()

	if _, err := json.Marshal(extra); err != nil {
		for k, v := range extra {
			if _, err2 := json.Marshal(v); err2 != nil {
				extra[k] = "<unserializable: " + safeType(v) + ">"
			}
		}
	}
	cfg := runtimeConfigLocked()
	cfg["started_at"] = s.startedAt.Format(time.RFC3339)
	cfg["uptime_sec"] = uptime.Seconds()
	cfg["flushers"] = flushers
	cfg["syncers"] = syncers
	cfg["closers"] = closers
	cfg["app_meta"] = extra
	cfg["stopctrl_errors"] = s.group.ErrorCount()
	return cfg
}

func (s *Service) componentsCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.flushers) + len(s.syncers) + len(s.closers)
}

func runtimeConfigLocked() map[string]any {
	out := make(map[string]any, 16)
	out["go_version"] = runtime.Version()
	out["os"] = runtime.GOOS
	out["arch"] = runtime.GOARCH
	out["num_cpu"] = runtime.NumCPU()
	out["gomaxprocs"] = runtime.GOMAXPROCS(0)
	out["goroutines"] = runtime.NumGoroutine()
	out["pid"] = os.Getpid()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	out["mem_alloc"] = ms.Alloc
	out["mem_sys"] = ms.Sys
	out["mem_mallocs"] = ms.Mallocs
	out["mem_frees"] = ms.Frees
	out["heap_alloc"] = ms.HeapAlloc
	out["heap_idle"] = ms.HeapIdle
	out["heap_inuse"] = ms.HeapInuse
	out["num_gc"] = ms.NumGC
	out["gc_pause_total_ms"] = float64(ms.PauseTotalNs) / 1e6
	return out
}

func safeType(v any) string {
	switch v.(type) {
	case nil:
		return "nil"
	case bool:
		return "bool"
	case int, int8, int16, int32, int64:
		return "int"
	case uint, uint8, uint16, uint32, uint64:
		return "uint"
	case float32, float64:
		return "float"
	case string:
		return "string"
	case []byte:
		return "[]byte"
	case error:
		return "error"
	default:
		return "unsupported"
	}
}
