package handler

import (
	"net/http"
	"runtime/debug"
	"time"

	"shurl/internal/admin"
	"shurl/pkg/response"
)

// AdminHandler 处理 /api/admin/* 接口。
type AdminHandler struct {
	svc *admin.Service
}

// NewAdminHandler 构造。
func NewAdminHandler(svc *admin.Service) *AdminHandler {
	return &AdminHandler{svc: svc}
}

// RegisterRoutes 注册路由到给定 mux。
func (h *AdminHandler) RegisterRoutes(mux *http.ServeMux, prefix string) {
	if prefix == "" {
		prefix = "/api/admin"
	}
	mux.HandleFunc(prefix+"/health", h.Health)
	mux.HandleFunc(prefix+"/flush", h.Flush)
	mux.HandleFunc(prefix+"/runtime", h.Runtime)
	mux.HandleFunc(prefix+"/gc", h.GC)
}

// Health GET /api/admin/health 返回健康状态。
func (h *AdminHandler) Health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		response.Fail(w, http.StatusMethodNotAllowed, response.CodeBadReq, "method not allowed")
		return
	}
	status := http.StatusOK
	hc := h.svc.Health()
	if hc.Status != "ok" {
		status = http.StatusServiceUnavailable
	}
	response.FailWithData(w, status, response.CodeSuccess, hc.Status, hc)
}

// Flush POST /api/admin/flush 强制 flush & sync 所有持久组件。
func (h *AdminHandler) Flush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		response.Fail(w, http.StatusMethodNotAllowed, response.CodeBadReq, "method not allowed")
		return
	}
	started := time.Now()
	if err := h.svc.FlushAll(); err != nil {
		response.Fail(w, http.StatusInternalServerError, response.CodeServer, err.Error())
		return
	}
	response.OK(w, map[string]any{
		"ok":         true,
		"elapsed":    time.Since(started).String(),
		"elapsed_ns": time.Since(started).Nanoseconds(),
	})
}

// Runtime GET /api/admin/runtime 返回运行时元信息。
func (h *AdminHandler) Runtime(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		response.Fail(w, http.StatusMethodNotAllowed, response.CodeBadReq, "method not allowed")
		return
	}
	cfg := h.svc.RuntimeConfig()
	response.OK(w, cfg)
}

// GC POST /api/admin/gc 触发一次 runtime.GC（仅用于压测场景观察内存变化）。
func (h *AdminHandler) GC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		response.Fail(w, http.StatusMethodNotAllowed, response.CodeBadReq, "method not allowed")
		return
	}
	before := debug.GCStats{}
	debug.ReadGCStats(&before)
	beforeT := time.Now()
	debug.FreeOSMemory()
	after := debug.GCStats{}
	debug.ReadGCStats(&after)
	response.OK(w, map[string]any{
		"before_num_gc": before.NumGC,
		"after_num_gc":  after.NumGC,
		"last_gc":       after.LastGC.Format(time.RFC3339Nano),
		"elapsed_ns":    time.Since(beforeT).Nanoseconds(),
	})
}
