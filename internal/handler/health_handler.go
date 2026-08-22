package handler

import (
	"net/http"

	"shurl/internal/service"
	"shurl/pkg/response"
)

// HealthHandler 提供 /health 和 /ready 端点。
type HealthHandler struct {
	svc *service.HealthService
}

// NewHealthHandler 构造 HealthHandler。
func NewHealthHandler(svc *service.HealthService) (*HealthHandler, error) {
	return &HealthHandler{svc: svc}, nil
}

// Register 注册 /health 和 /ready。
func (h *HealthHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			response.Fail(w, http.StatusMethodNotAllowed, response.CodeBadReq, "method not allowed")
			return
		}
		h.Health(w, r)
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			response.Fail(w, http.StatusMethodNotAllowed, response.CodeBadReq, "method not allowed")
			return
		}
		h.Ready(w, r)
	})
}

// Health 返回 /health：用于 liveness 探针，总是返回存储层的状态汇总。
func (h *HealthHandler) Health(w http.ResponseWriter, r *http.Request) {
	res := h.svc.Check(r.Context())
	switch res.Status {
	case "down":
		response.FailWithData(w, http.StatusServiceUnavailable, response.CodeServer, "service unavailable", res)
	case "degraded":
		response.FailWithData(w, http.StatusOK, response.CodeSuccess, "degraded", res)
	default:
		response.OK(w, res)
	}
}

// Ready 返回 /ready：用于 readiness 探针，仅当所有组件都完全就绪才返回 200。
func (h *HealthHandler) Ready(w http.ResponseWriter, r *http.Request) {
	if h.svc.Ready(r.Context()) {
		response.OK(w, map[string]string{"status": "ready"})
		return
	}
	response.Fail(w, http.StatusServiceUnavailable, response.CodeServer, "not ready")
}
