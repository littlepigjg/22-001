package service

import (
	"context"
	"sync"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/store"
	"shurl/pkg/logger"
)

// JanitorService 定期检查过期/超限短码，并把它们置为 Disabled=true，
// 这样后续重定向请求就能直接返回 410，不必每次判断时间。
//
// 注意：为了避免在高并发下写入过多数据，本服务仅做「软失效」标记，
// 真正的数据删除交给用户手动触发。
type JanitorService struct {
	cfg      *config.JanitorCfg
	urlStore *store.URLStore

	// invalidate 在条目被巡检置为禁用后调用，用于通知解析层缓存失效。
	// 可选；默认 nil 表示无缓存需要失效。由 main 注入 resolver.ObserveInvalidated。
	invalidate func(code string)

	wg       sync.WaitGroup
	cancelFn context.CancelFunc
	started  bool
	mu       sync.Mutex
}

// NewJanitorService 构造 JanitorService。
func NewJanitorService(cfg *config.Config, us *store.URLStore) (*JanitorService, error) {
	if cfg == nil || us == nil {
		return nil, model.ErrStoreNotReady
	}
	return &JanitorService{
		cfg:      &cfg.Janitor,
		urlStore: us,
	}, nil
}

// SetInvalidator 注入缓存失效回调（用于在巡检禁用条目后失效 resolver 的 LRU 缓存）。
func (j *JanitorService) SetInvalidator(fn func(code string)) {
	j.invalidate = fn
}

// Start 启动后台巡检任务。若配置中关闭了 Janitor，会直接返回。
// 本方法是幂等的。
func (j *JanitorService) Start(ctx context.Context) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.started {
		return nil
	}
	if j.cfg == nil || !j.cfg.Enabled {
		logger.Info("janitor service disabled by config")
		j.started = true
		return nil
	}
	interval := j.cfg.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	batch := j.cfg.Batch
	if batch <= 0 {
		batch = 1000
	}

	var inner context.Context
	inner, j.cancelFn = context.WithCancel(context.Background())
	j.started = true
	j.wg.Add(1)
	go func() {
		defer j.wg.Done()
		// 启动时先立即执行一次。
		j.runOnce(batch)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-inner.Done():
				return
			case <-ticker.C:
				j.runOnce(batch)
			}
		}
	}()
	logger.Info("janitor service started", logger.Fields{"interval": interval.String(), "batch": batch})
	return nil
}

// Shutdown 停止后台巡检任务。
func (j *JanitorService) Shutdown(ctx context.Context) error {
	j.mu.Lock()
	if !j.started {
		j.mu.Unlock()
		return nil
	}
	if j.cancelFn != nil {
		j.cancelFn()
	}
	j.mu.Unlock()

	done := make(chan struct{})
	go func() {
		j.wg.Wait()
		close(done)
	}()
	if ctx == nil {
		<-done
		return nil
	}
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// RunOnce 手动触发一次巡检（供测试/调试使用）。
func (j *JanitorService) RunOnce(batch int) int {
	return j.runOnce(batch)
}

// runOnce 执行一次巡检，返回被标记的条目数。
func (j *JanitorService) runOnce(batch int) int {
	now := time.Now()
	var processed, marked int
	candidates := make([]string, 0, batch)

	err := j.urlStore.ForEach(func(u *model.ShortURL) bool {
		if processed >= batch {
			return false
		}
		if u == nil {
			return true
		}
		if u.Disabled {
			return true
		}
		if u.IsExpired(now) || u.ExceedsMaxVisits() {
			candidates = append(candidates, u.Code)
			marked++
		}
		processed++
		return true
	})
	if err != nil {
		logger.Warn("janitor forEach error", logger.Fields{"err": err.Error()})
		return 0
	}

	// 在 store 写锁内原子置 Disabled，消除「锁外改字段 + 与 IncrementVisits 竞争」。
	updated := 0
	for _, code := range candidates {
		if err := j.urlStore.Update(code, func(u *model.ShortURL) { u.Disabled = true }); err != nil {
			logger.Warn("janitor disable url error", logger.Fields{"err": err.Error(), "code": code})
			continue
		}
		// 失效解析层缓存，使后续重定向回源拿到 Disabled=true 的最新快照返回 410。
		if j.invalidate != nil {
			j.invalidate(code)
		}
		updated++
	}
	if updated > 0 {
		logger.Info("janitor sweep completed", logger.Fields{"processed": processed, "marked": marked, "updated": updated})
	}
	return updated
}
