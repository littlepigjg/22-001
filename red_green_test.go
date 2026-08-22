package shurl

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/service"
	"shurl/internal/store"
	"shurl/pkg/ratelimit"
	"shurl/pkg/semaphore"
)

func createURLStoreForTest(t *testing.T, dir string) *store.URLStore {
	cfg := config.Default()
	cfg.Storage.URLFilePath = dir + "/urls.json"
	cfg.Storage.LogFilePath = dir + "/access.log"
	cfg.Storage.SyncInterval = 0
	us, err := store.NewURLStore(cfg)
	if err != nil {
		t.Fatalf("NewURLStore: %v", err)
	}
	if err := us.Load(context.Background()); err != nil {
		t.Fatalf("URLStore.Load: %v", err)
	}
	// 预置若干短码，保证 Overall 不会报 not found。
	for _, c := range []string{"tcd00001", "tcd00002", "tcd00003", "tcd00004", "tcd00005"} {
		u := &model.ShortURL{
			Code:      c,
			RawURL:    "https://example.com/page/" + c,
			CreatedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		}
		if err := us.Save(u, false); err != nil {
			t.Fatalf("Save %s: %v", c, err)
		}
	}
	return us
}

func createAccessLogStoreForTest(t *testing.T, dir string) *store.AccessLogStore {
	cfg := config.Default()
	cfg.Storage.URLFilePath = dir + "/urls.json"
	cfg.Storage.LogFilePath = dir + "/access.log"
	cfg.Storage.SyncInterval = 0
	ls, err := store.NewAccessLogStore(cfg)
	if err != nil {
		t.Fatalf("NewAccessLogStore: %v", err)
	}
	if err := ls.Open(context.Background()); err != nil {
		t.Fatalf("AccessLogStore.Open: %v", err)
	}
	return ls
}

func TestRedGreen(t *testing.T) {
	var anyPanic bool
	var lastMsg string

	wrap := func(name string, fn func()) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					anyPanic = true
					lastMsg = fmt.Sprintf("%v", r)
					t.Logf("case %s panicked: %s", name, lastMsg)
				}
			}()
			fn()
		})
	}

	wrap("direct_TakeLastN_oversize", func() {
		small := []int64{1, 2, 3}
		_ = ratelimit.TakeLastN(small, 50000)
	})

	wrap("WeightedBatch_ReleaseBurst_oversize", func() {
		w, err := semaphore.NewWeighted(32)
		if err != nil {
			t.Fatalf("NewWeighted: %v", err)
		}
		bh, err := semaphore.NewWeightedBatch(w, 32, 64)
		if err != nil {
			t.Fatalf("NewWeightedBatch: %v", err)
		}
		_, _ = bh.ReleaseBurst(1, 100000)
	})

	wrap("StatsService_BatchOverallAggregate_oversize", func() {
		dir := t.TempDir()
		us := createURLStoreForTest(t, dir)
		ls := createAccessLogStoreForTest(t, dir)
		defer func() {
			_ = ls.Close()
			_ = us.Close()
			_ = os.RemoveAll(dir)
		}()
		cfg := config.Default()
		cfg.Storage.URLFilePath = dir + "/urls.json"
		cfg.Storage.LogFilePath = dir + "/access.log"
		cfg.Storage.SyncInterval = 0
		cfg.Stats.CacheTTL = 0
		cfg.Stats.MaxRecords = 10000
		svc, err := service.NewStatsService(cfg, us, ls)
		if err != nil {
			t.Fatalf("NewStatsService: %v", err)
		}
		ctx := context.Background()
		codes := []string{"tcd00001", "tcd00002", "tcd00003", "tcd00004", "tcd00005"}
		// burst 远大于样本写入次数，触发越界。
		_, _, _ = svc.BatchOverallAggregate(ctx, codes, 7, 100000)
	})

	if anyPanic {
		fmt.Println("RED（红灯，缺陷未修复）")
		t.Errorf("RED（红灯，缺陷未修复）：发生切片越界 panic，最后一次信息: %s", lastMsg)
	} else {
		fmt.Println("GREEN（绿灯，缺陷已修复）")
		t.Logf("GREEN（绿灯，缺陷已修复）：所有用例均未越界")
	}
}
