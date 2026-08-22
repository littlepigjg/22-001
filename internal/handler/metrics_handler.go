package handler

import (
	"net/http"

	"shurl/internal/metrics"
)

// MetricsHandler 提供 /api/metrics 导出。
type MetricsHandler struct {
	svc *metrics.Service
}

// NewMetricsHandler 构造。
func NewMetricsHandler(svc *metrics.Service) *MetricsHandler {
	return &MetricsHandler{svc: svc}
}

// RegisterRoutes 注册路由。
func (h *MetricsHandler) RegisterRoutes(mux *http.ServeMux, prefix string) {
	if prefix == "" {
		prefix = "/api"
	}
	// 同时挂两个路径：
	//   GET /api/metrics          默认 JSON
	//   GET /metrics              Prometheus 风格（支持 ?format=json）
	mux.Handle(prefix+"/metrics", h.svc)
	mux.Handle("/metrics", h.svc)
	// 手动 reset 指标（仅 POST）
	mux.HandleFunc(prefix+"/metrics/reset", h.Reset)
}

// Reset POST /api/metrics/reset 清空所有指标（压测/调试用）。
func (h *MetricsHandler) Reset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	h.svc.Reset()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte(`{"ok":true}`))
}
