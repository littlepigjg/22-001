package service

import (
	"context"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/store"
)

func newTestStatsService(t *testing.T) (*StatsService, *store.URLStore, *store.AccessLogStore) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Storage.URLFilePath = dir + "/urls.json"
	cfg.Storage.LogFilePath = dir + "/access.log"
	cfg.Storage.SyncInterval = 0 // 关闭后台 syncer，避免与测试 ctx 生命周期耦合
	cfg.Stats.MaxRecords = 100000

	us, err := store.NewURLStore(cfg)
	if err != nil {
		t.Fatalf("NewURLStore: %v", err)
	}
	ctx := context.Background()
	if err := us.Load(ctx); err != nil {
		t.Fatalf("URLStore.Load: %v", err)
	}
	ls, err := store.NewAccessLogStore(cfg)
	if err != nil {
		t.Fatalf("NewAccessLogStore: %v", err)
	}
	if err := ls.Open(ctx); err != nil {
		t.Fatalf("AccessLogStore.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = ls.Close()
		_ = us.Close()
	})
	svc, err := NewStatsService(cfg, us, ls)
	if err != nil {
		t.Fatalf("NewStatsService: %v", err)
	}
	return svc, us, ls
}

func seedShortCode(t *testing.T, us *store.URLStore, code string) {
	t.Helper()
	if err := us.Save(&model.ShortURL{
		Code:      code,
		CreatedAt: time.Now().Add(-48 * time.Hour),
	}, true); err != nil {
		t.Fatalf("Save %s: %v", code, err)
	}
}

func seedAccess(t *testing.T, ls *store.AccessLogStore, code string, n int) {
	t.Helper()
	logs := make([]*model.AccessLog, 0, n)
	for i := 0; i < n; i++ {
		logs = append(logs, &model.AccessLog{
			Code:      code,
			IP:        "127.0.0.1",
			Referer:   "https://example.com/page",
			Timestamp: time.Now().Add(-24 * time.Hour),
			Status:    302,
			Device:    "pc",
			Browser:   "Chrome",
			OS:        "Linux",
		})
	}
	if err := ls.AppendMany(logs); err != nil {
		t.Fatalf("AppendMany %s: %v", code, err)
	}
}

// TestBatchOverallAggregate_LargeBurstNoPanic 复现用户报告：约 5 个短码、
// 7 天统计、burst=100000，旧实现在 TakeLastN 处 panic
// (slice bounds out of range [-99936:])。修复后应平稳返回。
func TestBatchOverallAggregate_LargeBurstNoPanic(t *testing.T) {
	svc, us, ls := newTestStatsService(t)
	codes := []string{"abc123", "def456", "ghi789", "jkl012", "mno345"}
	for _, c := range codes {
		seedShortCode(t, us, c)
		seedAccess(t, ls, c, 3)
	}

	ctx := context.Background()
	var (
		results []*model.OverallStats
		curves  [][]int64
		err     error
	)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("BatchOverallAggregate panicked: %v", r)
		}
	}()
	results, curves, err = svc.BatchOverallAggregate(ctx, codes, 7, 100000)
	if err != nil {
		t.Fatalf("BatchOverallAggregate: %v", err)
	}
	if len(results) != len(codes) {
		t.Fatalf("len(results)=%d, want %d", len(results), len(codes))
	}
	if len(curves) != 1 {
		t.Fatalf("len(curves)=%d, want 1", len(curves))
	}
	// curve 在 burst 远大于样本数时由 TakeLastN 兜底，长度不超过实际写入样本数。
	for _, c := range curves {
		if c == nil {
			continue
		}
		// 仅断言不 panic、返回切片合法；样本数量受令牌桶容量限制。
		_ = len(c)
	}
}

// TestBatchOverallAggregate_SmallBurst 正常小 burst 路径也应工作。
func TestBatchOverallAggregate_SmallBurst(t *testing.T) {
	svc, us, ls := newTestStatsService(t)
	codes := []string{"alpha01", "beta002"}
	for _, c := range codes {
		seedShortCode(t, us, c)
		seedAccess(t, ls, c, 2)
	}
	ctx := context.Background()
	results, curves, err := svc.BatchOverallAggregate(ctx, codes, 7, 4)
	if err != nil {
		t.Fatalf("BatchOverallAggregate: %v", err)
	}
	if len(results) != len(codes) {
		t.Fatalf("len(results)=%d, want %d", len(results), len(codes))
	}
	if len(curves) != 1 {
		t.Fatalf("len(curves)=%d, want 1", len(curves))
	}
	for i, r := range results {
		if r.Code != codes[i] {
			t.Fatalf("results[%d].Code=%q, want %q", i, r.Code, codes[i])
		}
	}
}

// TestOverall_SingleCode 单短码 Overall 路径不受 batch 修复影响。
func TestOverall_SingleCode(t *testing.T) {
	svc, us, ls := newTestStatsService(t)
	seedShortCode(t, us, "solo999")
	seedAccess(t, ls, "solo999", 5)
	ctx := context.Background()
	res, err := svc.Overall(ctx, "solo999", 7)
	if err != nil {
		t.Fatalf("Overall: %v", err)
	}
	if res.Code != "solo999" {
		t.Fatalf("Code=%q, want solo999", res.Code)
	}
	if res.TotalPV != 5 {
		t.Fatalf("TotalPV=%d, want 5", res.TotalPV)
	}
}
