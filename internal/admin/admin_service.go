package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"

	"shurl/internal/model"
	"shurl/pkg/logger"
	"shurl/pkg/workerpool"
)

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

// Service 管理服务实例。
type Service struct {
	mu        sync.Mutex
	flushers  []namedF
	syncers   []namedS
	closers   []namedC
	startedAt time.Time
	extra     map[string]any // 运行时额外元信息（可读写）
}

type namedF struct{ Name string; F Flusher }
type namedS struct{ Name string; S Syncer }
type namedC struct{ Name string; C Closer }

// New 创建管理服务。
func New(_log *logger.Logger) *Service {
	return &Service{
		startedAt: time.Now(),
		extra:     make(map[string]any),
	}
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
	// 尝试一次 Flush 同步：仅在有 flushers 时尝试（不写数据，只看有没有报错）。
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

	// 粗略测试：extra 的 json 兼容性（失败就把 map[key] 改成字符串）。
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

type DiagnosticCheck struct {
	Name     string
	Severity string
	Fn       func(context.Context) error
}

type TaskDiagnosticReport struct {
	GeneratedAt time.Time
	Duration    time.Duration
	Passed      int
	Failed      int
	Skipped     int
	Summary     model.ErrorReport
	FirstMsg    string
	LastMsg     string
}

func (s *Service) buildDiagnosticTasks() []DiagnosticCheck {
	s.mu.Lock()
	flushN := len(s.flushers)
	syncN := len(s.syncers)
	closeN := len(s.closers)
	s.mu.Unlock()
	checks := []DiagnosticCheck{
		{
			Name:     "admin.flushers_registered",
			Severity: "warn",
			Fn: func(_ context.Context) error {
				if flushN == 0 {
					return fmt.Errorf("no flushers registered")
				}
				return nil
			},
		},
		{
			Name:     "admin.syncers_registered",
			Severity: "warn",
			Fn: func(_ context.Context) error {
				if syncN == 0 {
					return fmt.Errorf("no syncers registered")
				}
				return nil
			},
		},
		{
			Name:     "admin.closers_registered",
			Severity: "info",
			Fn: func(_ context.Context) error {
				if closeN == 0 {
					return fmt.Errorf("no closers registered")
				}
				return nil
			},
		},
		{
			Name:     "admin.disk_hostname",
			Severity: "info",
			Fn: func(_ context.Context) error {
				hn, err := os.Hostname()
				if err != nil {
					return fmt.Errorf("hostname unavailable: %w", err)
				}
				if hn == "" {
					return fmt.Errorf("hostname is empty")
				}
				return nil
			},
		},
		{
			Name:     "admin.mem_pressure",
			Severity: "error",
			Fn: func(_ context.Context) error {
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				if ms.HeapInuse > ms.HeapAlloc && ms.HeapIdle < ms.HeapInuse/4 {
					return fmt.Errorf("heap pressure: inuse=%d idle=%d", ms.HeapInuse, ms.HeapIdle)
				}
				return nil
			},
		},
		{
			Name:     "admin.flush_all_silent",
			Severity: "error",
			Fn: func(_ context.Context) error {
				return s.FlushAllSilent()
			},
		},
	}
	return checks
}

func (s *Service) RunTaskDiagnostics(ctx context.Context) (*TaskDiagnosticReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	checks := s.buildDiagnosticTasks()
	pool, err := workerpool.New(2)
	if err != nil {
		return nil, fmt.Errorf("diagnostics: create pool: %w", err)
	}
	if err := pool.Start(ctx); err != nil {
		return nil, fmt.Errorf("diagnostics: start pool: %w", err)
	}
	started := time.Now()
	var extraErrs []error
	for _, c := range checks {
		task := c
		subErr := pool.Submit(workerpool.Task{
			Name: task.Name,
			Fn: func(inner context.Context) error {
				select {
				case <-inner.Done():
					return inner.Err()
				default:
				}
				if task.Fn == nil {
					return fmt.Errorf("task %s: nil function", task.Name)
				}
				if err := task.Fn(inner); err != nil {
					if task.Severity == "error" {
						return err
					}
					return fmt.Errorf("[%s] %s: %w", task.Severity, task.Name, err)
				}
				return nil
			},
		})
		if subErr != nil {
			extraErrs = append(extraErrs,
				fmt.Errorf("submit task[%s] failed: %w", task.Name, subErr))
		}
	}
	stopErr := pool.Stop()
	if stopErr != nil {
		extraErrs = append(extraErrs, stopErr)
	}
	poolErrs := pool.Errors()
	report := model.ReportErrors(poolErrs, extraErrs...)
	first := workerpool.FirstErr(poolErrs)
	last := workerpool.LastErr(poolErrs)
	result := &TaskDiagnosticReport{
		GeneratedAt: time.Now(),
		Duration:    time.Since(started),
		Passed:      len(checks) - report.Total,
		Failed:      report.Total,
		Skipped:     0,
		Summary:     report,
	}
	if first != nil {
		result.FirstMsg = first.Error()
	}
	if last != nil {
		result.LastMsg = last.Error()
	}
	if result.Passed < 0 {
		result.Passed = 0
	}
	return result, nil
}
