package service

import (
	"context"
	"sync/atomic"
	"time"

	"shurl/internal/model"
	"shurl/internal/store"
)

type HealthService struct {
	urlStore *store.URLStore
	logStore *store.AccessLogStore
	started  atomic.Bool
}

func NewHealthService(us *store.URLStore, ls *store.AccessLogStore) (*HealthService, error) {
	if us == nil || ls == nil {
		return nil, nil
	}
	return &HealthService{urlStore: us, logStore: ls}, nil
}

func (h *HealthService) MarkStarted() { h.started.Store(true) }

type Health struct {
	URLReady    bool   `json:"url_store_ready"`
	LogReady    bool   `json:"log_store_ready"`
	URLTotal    int    `json:"url_total"`
	URLActive   int    `json:"url_active"`
	URLDisabled int    `json:"url_disabled"`
	URLExpired  int    `json:"url_expired"`
	Status      string `json:"status"`
}

// Check 汇总存储层状态用于 /health 探针。
//
// 通过 urlStore.ForEach 获取所有记录的快照（ForEach 在读锁下逐条返回深拷贝，
// 快照独立于存储内部对象），因此即便与后台过期巡检并发执行，也不会读取到
// 「写了一半」的字段——既无数据竞争，统计口径也与 URLStore.Stats 一致。
func (h *HealthService) Check(ctx context.Context) *Health {
	res := &Health{
		URLReady: h.urlStore != nil && h.urlStore.Ready(),
		LogReady: h.logStore != nil && h.logStore.Ready(),
	}
	if h.urlStore != nil {
		snapshots := make([]*model.ShortURL, 0, 64)
		err := h.urlStore.ForEach(func(u *model.ShortURL) bool {
			if u == nil {
				return true
			}
			snapshots = append(snapshots, u)
			return true
		})
		if err == nil {
			// 分桶口径与 URLStore.Stats 完全一致：Disabled 优先计入 disabled，
			// 否则按 IsExpired 计入 expired，剩余计入 active。这样 active +
			// disabled + expired 恒等于 total，且与存储层统计对得上。
			now := time.Now()
			total := len(snapshots)
			active, disabled, expired := 0, 0, 0
			for _, u := range snapshots {
				switch {
				case u.Disabled:
					disabled++
				case u.IsExpired(now):
					expired++
				default:
					active++
				}
			}
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

func (h *HealthService) Ready(ctx context.Context) bool {
	if !h.started.Load() {
		return false
	}
	if h.urlStore == nil || h.logStore == nil {
		return false
	}
	return h.urlStore.Ready() && h.logStore.Ready()
}
