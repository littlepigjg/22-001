package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/store"
	"shurl/pkg/logger"
)

type JanitorService struct {
	cfg      *config.JanitorCfg
	urlStore *store.URLStore

	wg       sync.WaitGroup
	cancelFn context.CancelFunc
	started  bool
	mu       sync.Mutex
}

func NewJanitorService(cfg *config.Config, us *store.URLStore) (*JanitorService, error) {
	if cfg == nil || us == nil {
		return nil, model.ErrStoreNotReady
	}
	return &JanitorService{
		cfg:      &cfg.Janitor,
		urlStore: us,
	}, nil
}

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

func (j *JanitorService) RunOnce(batch int) int {
	return j.runOnce(batch)
}

// markReason 将「为什么需要失效」映射到存储层 store.LifecycleReason。
type markReason int

const (
	reasonExpired markReason = iota
	reasonMaxVisits
	reasonBoth
)

// toStoreReason 把巡检侧的 markReason 转成存储层可识别的失效类别。
func (r markReason) toStoreReason() store.LifecycleReason {
	switch r {
	case reasonExpired:
		return store.ReasonExpired
	case reasonMaxVisits:
		return store.ReasonMaxVisits
	case reasonBoth:
		return store.ReasonBoth
	default:
		return store.ReasonBoth
	}
}

type markCandidate struct {
	code   string
	reason markReason
}

func (j *JanitorService) runOnce(batch int) int {
	now := time.Now()
	var processed int
	candidates := make([]markCandidate, 0, batch)

	err := j.urlStore.ForEach(func(u *model.ShortURL) bool {
		if processed >= batch {
			return false
		}
		if u == nil {
			return true
		}
		if u.Disabled {
			processed++
			return true
		}
		exp := u.IsExpired(now)
		maxv := u.ExceedsMaxVisits()
		switch {
		case exp && maxv:
			candidates = append(candidates, markCandidate{code: u.Code, reason: reasonBoth})
		case exp:
			candidates = append(candidates, markCandidate{code: u.Code, reason: reasonExpired})
		case maxv:
			candidates = append(candidates, markCandidate{code: u.Code, reason: reasonMaxVisits})
		default:
			processed++
			return true
		}
		processed++
		return true
	})
	if err != nil {
		logger.Warn("janitor forEach error", logger.Fields{"err": err.Error()})
		return 0
	}

	return j.applyLifecycleMarks(candidates, now)
}

// applyLifecycleMarks 把候选记录逐条交给存储层在写锁下原子地完成失效变更。
//
// 这里不再直接修改任何 model.ShortURL 字段——所有变更都通过
// URLStore.ApplyLifecycle 在 s.mu.Lock 下完成，与 health.Check / Get / Stats 等
// 读路径共用同一把锁，彻底消除了「一边读一边写」的数据竞争。
func (j *JanitorService) applyLifecycleMarks(list []markCandidate, now time.Time) int {
	if len(list) == 0 {
		return 0
	}
	updated := 0
	for _, c := range list {
		if c.code == "" {
			continue
		}
		if _, err := j.urlStore.ApplyLifecycle(c.code, c.reason.toStoreReason(), now); err != nil {
			if !errors.Is(err, model.ErrCodeNotFound) {
				logger.Warn("janitor apply lifecycle error", logger.Fields{"err": err.Error(), "code": c.code})
			}
			continue
		}
		updated++
	}
	if updated > 0 {
		logger.Info("janitor sweep completed", logger.Fields{"updated": updated, "total_candidates": len(list)})
	}
	return updated
}
