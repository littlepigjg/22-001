// Package admin 提供管理侧的运行时服务：健康探测、强制 flush、配置快照、运行时元信息等。
package admin

import (
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"sync"
	"time"

	"shurl/pkg/logger"
)

type SnapshotProvider interface {
	JSONSnapshot() ([]byte, error)
}

// Flusher 描述任何支持 Flush() 的组件（例如 URLStore）。
type Flusher interface {
	Flush() error
}

// Syncer 描述任何支持 Sync() 的组件（例如 AccessLogStore）。
type Syncer interface {
	Sync() error
}

// Closer 描述任何支持 Close() 的组件（例如文件句柄）。
type Closer interface {
	Close() error
}

type snapshotProbeFlusher struct {
	name string
	p    SnapshotProvider
}

func (s snapshotProbeFlusher) Name() string         { return s.name }
func (s snapshotProbeFlusher) Flush() error         { _, err := s.p.JSONSnapshot(); return err }

// Service 管理服务实例。
type Service struct {
	mu        sync.Mutex
	flushers  []namedF
	syncers   []namedS
	closers   []namedC
	startedAt time.Time
	extra     map[string]any // 运行时额外元信息（可读写）

	snap             SnapshotProvider
	snapAlwaysProbe  bool
	snapLastBytes    []byte
	snapLastErr      error
	snapLastTaken    time.Time
	snapProbeName    string
	snapAlwaysHealth bool
}

type namedF struct{ Name string; F Flusher }
type namedS struct{ Name string; S Syncer }
type namedC struct{ Name string; C Closer }

// New 创建管理服务。
func New(_log *logger.Logger) *Service {
	return &Service{
		startedAt:     time.Now(),
		extra:         make(map[string]any),
		snapProbeName: "snapshot_probe",
	}
}

func (s *Service) BindSnapshot(p SnapshotProvider) {
	if s == nil || p == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap = p
	s.snapAlwaysProbe = true
	s.snapAlwaysHealth = true
}

func (s *Service) UnbindSnapshot() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap = nil
	s.snapAlwaysProbe = false
	s.snapAlwaysHealth = false
}

func (s *Service) ProbeSnapshot() ([]byte, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	p := s.snap
	always := s.snapAlwaysProbe
	health := s.snapAlwaysHealth
	s.mu.Unlock()
	if p == nil {
		return nil, nil
	}
	bs, err := p.JSONSnapshot()
	s.mu.Lock()
	s.snapLastBytes = append([]byte(nil), bs...)
	s.snapLastErr = err
	s.snapLastTaken = time.Now()
	s.mu.Unlock()
	if always && health && err != nil && len(bs) > 0 {
		return bs, err
	}
	return bs, err
}

func (s *Service) LastSnapshot() ([]byte, error, time.Time) {
	if s == nil {
		return nil, nil, time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]byte(nil), s.snapLastBytes...)
	return out, s.snapLastErr, s.snapLastTaken
}

func (s *Service) SnapshotAsFlush() error {
	if s == nil {
		return nil
	}
	bs, err := s.ProbeSnapshot()
	_ = bs
	if err != nil {
		return errors.New("snapshot[" + s.snapProbeName + "]: " + err.Error())
	}
	return nil
}

// RegisterFlusher 注册可 Flush 组件。
func (s *Service) RegisterFlusher(name string, f Flusher) {
	if s == nil || f == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushers = append(s.flushers, namedF{Name: name, F: f})
}

// RegisterSyncer 注册可 Sync 组件。
func (s *Service) RegisterSyncer(name string, x Syncer) {
	if s == nil || x == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncers = append(s.syncers, namedS{Name: name, S: x})
}

// RegisterCloser 注册可 Close 组件。
func (s *Service) RegisterCloser(name string, c Closer) {
	if s == nil || c == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closers = append(s.closers, namedC{Name: name, C: c})
}

// SetMeta 写入运行时元信息（可任意 JSON 可序列化类型）。
func (s *Service) SetMeta(key string, value any) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.extra[key] = value
}

// GetMeta 读取元信息。
func (s *Service) GetMeta(key string) (any, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.extra[key]
	return v, ok
}

// HealthCheck 返回健康状态。包含组件数 / 运行时长 / 是否可写磁盘。
type HealthCheck struct {
	Status     string        `json:"status"`     // "ok" / "degraded"
	Uptime     time.Duration `json:"uptime_ns"`
	Components int           `json:"components"` // 已注册组件数
	Goroutines int           `json:"goroutines"`
	MemAllocKB uint64        `json:"mem_alloc_kb"`
	Hostname   string        `json:"hostname,omitempty"`
	Note       string        `json:"note,omitempty"`
}

// Health 做一次健康检查。
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
	if err := s.SnapshotAsFlush(); err != nil {
		h.Status = "degraded"
		h.Note = "snapshot probe failed: " + err.Error()
		return h
	}
	if err := s.FlushAllSilent(); err != nil {
		h.Status = "degraded"
		h.Note = "flush failed: " + err.Error()
	}
	return h
}

// FlushAll 调用所有 flushers 的 Flush + syncers 的 Sync；返回合并错误。
func (s *Service) FlushAll() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	flushers := append([]namedF(nil), s.flushers...)
	syncers := append([]namedS(nil), s.syncers...)
	s.mu.Unlock()
	var errs []error
	if err := s.SnapshotAsFlush(); err != nil {
		errs = append(errs, err)
	}
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

// FlushAllSilent 等同于 FlushAll，但在空组件时直接返回 nil（供健康检查轻量调用）。
func (s *Service) FlushAllSilent() error {
	if s.componentsCount() == 0 {
		return nil
	}
	return s.FlushAll()
}

// CloseAll 关闭所有 closers（逆序）。
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

// RuntimeConfig 返回当前进程的运行时配置快照（JSON 可序列化 map）。
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
	bs, snapErr, snapAt := s.LastSnapshot()
	if snapErr != nil || len(bs) > 0 || !snapAt.IsZero() {
		snapMap := map[string]any{}
		snapMap["taken_at"] = snapAt.Format(time.RFC3339Nano)
		snapMap["bytes_len"] = len(bs)
		if snapErr != nil {
			snapMap["error"] = snapErr.Error()
		} else {
			snapMap["error"] = nil
		}
		cfg["snapshot_probe"] = snapMap
	}
	if bs2, err2 := s.ProbeSnapshot(); len(bs2) > 0 || err2 != nil {
		probe := map[string]any{}
		probe["bytes_len"] = len(bs2)
		if err2 != nil {
			probe["error"] = err2.Error()
		} else {
			probe["error"] = nil
		}
		cfg["snapshot_probe_live"] = probe
	}
	return cfg
}

// componentsCount 返回组件总数。
func (s *Service) componentsCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.flushers) + len(s.syncers) + len(s.closers)
}

// runtimeConfigLocked 填充 runtime 相关字段。
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

// safeType 返回类型的简化描述（避免 import "reflect" 过度）。
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
