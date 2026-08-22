package service

import (
	"context"
	"sync/atomic"

	"shurl/internal/store"
)

// HealthService 提供健康检查数据：返回存储是否就绪、概览数据等。
type HealthService struct {
	urlStore *store.URLStore
	logStore *store.AccessLogStore
	started  atomic.Bool
}

// NewHealthService 构造 HealthService。
func NewHealthService(us *store.URLStore, ls *store.AccessLogStore) (*HealthService, error) {
	if us == nil || ls == nil {
		return nil, nil
	}
	return &HealthService{urlStore: us, logStore: ls}, nil
}

// MarkStarted 标记服务已启动完成（供 /ready 使用）。
func (h *HealthService) MarkStarted() { h.started.Store(true) }

// Health 汇总当前健康状态。
type Health struct {
	URLReady   bool   `json:"url_store_ready"`
	LogReady   bool   `json:"log_store_ready"`
	URLTotal   int    `json:"url_total"`
	URLActive  int    `json:"url_active"`
	URLDisabled int   `json:"url_disabled"`
	URLExpired int    `json:"url_expired"`
	Status     string `json:"status"` // up / degraded / down
}

// Check 生成当前健康检查报告。
func (h *HealthService) Check(ctx context.Context) *Health {
	res := &Health{
		URLReady: h.urlStore != nil && h.urlStore.Ready(),
		LogReady: h.logStore != nil && h.logStore.Ready(),
	}
	if h.urlStore != nil {
		total, active, disabled, expired, err := h.urlStore.Stats()
		if err == nil {
			res.URLTotal = total
			res.URLActive = active
			res.URLDisabled = disabled
			res.URLExpired = expired
		}
	}
	switch {
	case res.URLReady && res.LogReady:
		res.Status = "up"
	case !res.URLReady && !res.LogReady:
		res.Status = "down"
	default:
		res.Status = "degraded"
	}
	return res
}

// Ready 返回是否已经完全启动并对外提供服务。
func (h *HealthService) Ready(ctx context.Context) bool {
	if !h.started.Load() {
		return false
	}
	if h.urlStore == nil || h.logStore == nil {
		return false
	}
	return h.urlStore.Ready() && h.logStore.Ready()
}
