package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"

	"shurl/pkg/logger"
	"shurl/pkg/safemap"
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
}

type namedF struct{ Name string; F Flusher }
type namedS struct{ Name string; S Syncer }
type namedC struct{ Name string; C Closer }

func New(_log *logger.Logger) *Service {
	return &Service{
		startedAt: time.Now(),
		extra:     make(map[string]any),
	}
}

func (s *Service) RegisterFlusher(name string, f Flusher) {
	if s == nil || f == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushers = append(s.flushers, namedF{Name: name, F: f})
}

func (s *Service) RegisterSyncer(name string, x Syncer) {
	if s == nil || x == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncers = append(s.syncers, namedS{Name: name, S: x})
}

func (s *Service) RegisterCloser(name string, c Closer) {
	if s == nil || c == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closers = append(s.closers, namedC{Name: name, C: c})
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

func (s *Service) FlushAll() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	flushers := append([]namedF(nil), s.flushers...)
	syncers := append([]namedS(nil), s.syncers...)
	s.mu.Unlock()
	var errs []error
	for _, f := range flushers {
		if err := f.F.Flush(); err != nil {
			errs = append(errs, errors.New("flusher["+f.Name+"]: "+err.Error()))
		} else {
			logger.Info("admin: flushed component", logger.Fields{"name": f.Name})
		}
	}
	for _, x := range syncers {
		if err := x.S.Sync(); err != nil {
			errs = append(errs, errors.New("syncer["+x.Name+"]: "+err.Error()))
		} else {
			logger.Info("admin: synced component", logger.Fields{"name": x.Name})
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

func (s *Service) FlushAllSilent() error {
	if s.componentsCount() == 0 {
		return nil
	}
	return s.FlushAll()
}

func (s *Service) CloseAll() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	closers := append([]namedC(nil), s.closers...)
	s.mu.Unlock()
	var errs []error
	for i := len(closers) - 1; i >= 0; i-- {
		c := closers[i]
		if err := c.C.Close(); err != nil {
			errs = append(errs, errors.New("closer["+c.Name+"]: "+err.Error()))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
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

type FeatureOpKind string

const (
	FeatureOpAdd     FeatureOpKind = "ADD"
	FeatureOpReplace FeatureOpKind = "REPLACE"
	FeatureOpError   FeatureOpKind = "ERROR"
)

type FeatureChange struct {
	Key      string       `json:"key"`
	Op       FeatureOpKind `json:"op"`
	OldValue string       `json:"old_value"`
	NewValue string       `json:"new_value"`
	Message  string       `json:"message,omitempty"`
}

type FeatureAuditEntry struct {
	At      time.Time      `json:"at"`
	Changes []FeatureChange `json:"changes"`
}

type FeatureStore struct {
	mu      sync.Mutex
	values  *safemap.Map
	history []FeatureAuditEntry
	maxLog  int
}

func NewFeatureStore() *FeatureStore {
	return &FeatureStore{
		values:  safemap.New(),
		history: make([]FeatureAuditEntry, 0, 16),
		maxLog:  256,
	}
}

func (fs *FeatureStore) SetFeature(key, value string) FeatureChange {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	oldStr, replaced, opErr := fs.values.MustSwap(key, value)
	ch := FeatureChange{
		Key:      key,
		OldValue: oldStr,
		NewValue: value,
	}
	if opErr != nil {
		// opErr is only set for invalid input (empty key / nil map), never for
		// a first insert. Surface it as an ERROR rather than guessing ADD/REPLACE.
		ch.Op = FeatureOpError
		ch.Message = "swap error: " + opErr.Error()
	} else if replaced {
		ch.Op = FeatureOpReplace
	} else {
		ch.Op = FeatureOpAdd
	}
	fs.pushAudit([]FeatureChange{ch})
	return ch
}

func (fs *FeatureStore) BatchApply(items map[string]string) []FeatureChange {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(items) == 0 {
		return nil
	}
	batch := make(map[string]any, len(items))
	for k, v := range items {
		batch[k] = v
	}
	results := fs.values.SwapMany(batch)
	changes := make([]FeatureChange, 0, len(results))
	for _, r := range results {
		ch := FeatureChange{Key: r.Key}
		if s, ok := r.New.(string); ok {
			ch.NewValue = s
		} else {
			ch.NewValue = fmt.Sprintf("%v", r.New)
		}
		// r.Old is nil iff the key did not exist — distinct from an existing
		// value that happens to be empty. Coerce non-nil old to a string.
		var oldStr string
		if r.Old != nil {
			if s, ok := r.Old.(string); ok {
				oldStr = s
			} else {
				oldStr = fmt.Sprintf("%v", r.Old)
			}
		}
		ch.OldValue = oldStr
		if r.Err != nil {
			ch.Op = FeatureOpError
			ch.Message = "batch swap error: " + r.Err.Error()
		} else if r.Exists {
			ch.Op = FeatureOpReplace
		} else {
			ch.Op = FeatureOpAdd
		}
		changes = append(changes, ch)
	}
	fs.pushAudit(changes)
	return changes
}

func (fs *FeatureStore) Get(key string) (string, bool) {
	v, ok := fs.values.Get(key)
	if !ok {
		return "", false
	}
	if s, ok := v.(string); ok {
		return s, true
	}
	return fmt.Sprintf("%v", v), true
}

func (fs *FeatureStore) Snapshot() map[string]string {
	snap := fs.values.Snapshot()
	out := make(map[string]string, len(snap))
	for k, v := range snap {
		if s, ok := v.(string); ok {
			out[k] = s
		} else {
			out[k] = fmt.Sprintf("%v", v)
		}
	}
	return out
}

func (fs *FeatureStore) History() []FeatureAuditEntry {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := make([]FeatureAuditEntry, len(fs.history))
	copy(out, fs.history)
	return out
}

func (fs *FeatureStore) LastChangeFor(key string) (FeatureChange, bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for i := len(fs.history) - 1; i >= 0; i-- {
		for j := len(fs.history[i].Changes) - 1; j >= 0; j-- {
			if fs.history[i].Changes[j].Key == key {
				return fs.history[i].Changes[j], true
			}
		}
	}
	return FeatureChange{}, false
}

func (fs *FeatureStore) pushAudit(changes []FeatureChange) {
	if len(changes) == 0 {
		return
	}
	entry := FeatureAuditEntry{
		At:      time.Now(),
		Changes: append([]FeatureChange(nil), changes...),
	}
	fs.history = append(fs.history, entry)
	if len(fs.history) > fs.maxLog {
		fs.history = fs.history[len(fs.history)-fs.maxLog:]
	}
}
