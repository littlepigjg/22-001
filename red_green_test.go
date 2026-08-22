package shurl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/service"
	"shurl/internal/store"
	"shurl/pkg/clock"
	"shurl/pkg/durationutil"
)

func buildTempConfig(t *testing.T, cacheTTL time.Duration) (*config.Config, func()) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Storage.URLFilePath = filepath.Join(dir, "urls.json")
	cfg.Storage.LogFilePath = filepath.Join(dir, "access.log")
	cfg.Storage.SyncInterval = 0
	cfg.Storage.FlushOnWrite = false
	cfg.Stats.CacheTTL = cacheTTL
	return cfg, func() { _ = os.RemoveAll(dir) }
}

func makeStores(t *testing.T, cfg *config.Config) (*store.URLStore, *store.AccessLogStore, func()) {
	t.Helper()
	us, err := store.NewURLStore(cfg)
	if err != nil {
		t.Fatalf("NewURLStore: %v", err)
	}
	ls, err := store.NewAccessLogStore(cfg)
	if err != nil {
		t.Fatalf("NewAccessLogStore: %v", err)
	}
	ctx := context.Background()
	if err := us.Load(ctx); err != nil {
		t.Fatalf("URLStore.Load: %v", err)
	}
	if err := ls.Open(ctx); err != nil {
		t.Fatalf("AccessLogStore.Open: %v", err)
	}
	cleanup := func() {
		_ = us.Close()
		_ = ls.Close()
	}
	return us, ls, cleanup
}

func failRed(t *testing.T, reason string) {
	t.Helper()
	t.Errorf("RED: %s", reason)
	fmt.Printf("  -> RED（红灯，缺陷未修复）: %s\n", reason)
}

func TestRedGreen(t *testing.T) {
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	failed := false

	check := func(name string, fn func(t *testing.T) bool) {
		ok := t.Run(name, func(t *testing.T) {
			if !fn(t) {
				t.FailNow()
			}
		})
		if !ok {
			failed = true
		}
	}

	check("A_TTLParsing_24h_sentinel_roundtrip", func(t *testing.T) bool {
		d, err := durationutil.ParseTTL("24h")
		if err != nil {
			t.Fatalf("ParseTTL(24h) err: %v", err)
		}
		resolved := durationutil.ResolveSentinel(d)
		if resolved != 24*time.Hour {
			failRed(t, fmt.Sprintf("ParseTTL(\"24h\") resolved=%v, 期望 24h", resolved))
			return false
		}
		return true
	})

	check("B_FakeClock_ContextWithTimeout_1h_OK", func(t *testing.T) bool {
		fc := clock.NewFake(base)
		ctx, cancel := fc.ContextWithTimeout(context.Background(), 1*time.Hour)
		defer cancel()
		select {
		case <-ctx.Done():
			failRed(t, fmt.Sprintf("Fake+1h 上下文立即被取消, err=%v", ctx.Err()))
			return false
		default:
		}
		return true
	})

	check("C_FakeClock_ContextWithTimeout_24h_not_canceled", func(t *testing.T) bool {
		fc := clock.NewFake(base)
		ctx, cancel := fc.ContextWithTimeout(context.Background(), 24*time.Hour)
		defer cancel()
		select {
		case <-ctx.Done():
			err := ctx.Err()
			failRed(t, fmt.Sprintf("Fake+24h 上下文立即被取消, err=%v (期望保持 ctx 直到真正超时)", err))
			return false
		default:
		}
		return true
	})

	check("D_Config_ParseTTL_1d_resolves_24h", func(t *testing.T) bool {
		d, err := durationutil.ParseTTL("1d")
		if err != nil {
			t.Fatalf("ParseTTL(1d) err: %v", err)
		}
		okDur := durationutil.ResolveSentinel(d)
		if okDur <= 0 {
			failRed(t, fmt.Sprintf("ParseTTL(\"1d\") 解析后正向值为 %v, 期望 > 0", okDur))
			return false
		}
		if okDur != 24*time.Hour {
			failRed(t, fmt.Sprintf("ParseTTL(\"1d\") resolved=%v, 期望 24h", okDur))
			return false
		}
		return true
	})

	check("E_StatsCfg_BuildCacheContext_24h_Fake_not_cancel", func(t *testing.T) bool {
		cfg, cleanupCfg := buildTempConfig(t, 24*time.Hour)
		defer cleanupCfg()
		us, ls, cleanupStores := makeStores(t, cfg)
		defer cleanupStores()

		svc, err := service.NewStatsService(cfg, us, ls)
		if err != nil {
			t.Fatalf("NewStatsService err: %v", err)
		}
		fc := clock.NewFake(base)
		svc.SetClock(fc)

		parent := context.Background()
		ctx, cancel := cfg.Stats.BuildCacheContext(parent, fc)
		defer cancel()

		select {
		case <-ctx.Done():
			cerr := ctx.Err()
			failRed(t, fmt.Sprintf("BuildCacheContext(CacheTTL=24h)+Fake 返回立即取消 ctx, err=%v", cerr))
			return false
		default:
		}
		return true
	})

	check("F_StatsService_Overall_24h_TTL_Fake_not_canceled", func(t *testing.T) bool {
		cfg, cleanupCfg := buildTempConfig(t, 24*time.Hour)
		defer cleanupCfg()
		us, ls, cleanupStores := makeStores(t, cfg)
		defer cleanupStores()

		u := &model.ShortURL{
			Code:      "test123",
			RawURL:    "https://example.com/",
			CreatedAt: base,
		}
		if err := u.Validate(); err != nil {
			t.Fatalf("u.Validate err: %v", err)
		}
		if err := us.Save(u, false); err != nil {
			t.Fatalf("URLStore.Save err: %v", err)
		}

		svc, err := service.NewStatsService(cfg, us, ls)
		if err != nil {
			t.Fatalf("NewStatsService err: %v", err)
		}
		fc := clock.NewFake(base)
		svc.SetClock(fc)

		result, err := svc.Overall(context.Background(), "test123", 7)
		if err != nil {
			if errors.Is(err, model.ErrCanceled) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				failRed(t, fmt.Sprintf("Stats.Overall(24h TTL+Fake) 立即返回取消错误: %v (期望正常完成)", err))
				return false
			}
			t.Fatalf("Stats.Overall 非预期错误: %v", err)
		}
		if result == nil {
			failRed(t, "Stats.Overall 返回 nil 结果，无 err (异常)")
			return false
		}
		return true
	})

	check("G_Default_Stats_CacheTTL_BuildCacheContext_sane", func(t *testing.T) bool {
		cfg := config.Default()
		resolved := cfg.Stats.ResolveCacheTTL()
		if resolved <= 0 {
			failRed(t, fmt.Sprintf("Default().Stats.ResolveCacheTTL()=%v, 期望 > 0", resolved))
			return false
		}
		fc := clock.NewFake(base)
		ctx, cancel := cfg.Stats.BuildCacheContext(context.Background(), fc)
		defer cancel()
		select {
		case <-ctx.Done():
			failRed(t, fmt.Sprintf("默认 CacheTTL=%v 经 BuildCacheContext(Fake) 立即取消, err=%v", cfg.Stats.CacheTTL, ctx.Err()))
			return false
		default:
		}
		return true
	})

	check("H_FakeClock_ContextWithDeadline_24h_not_canceled", func(t *testing.T) bool {
		fc := clock.NewFake(base)
		deadline := base.Add(24 * time.Hour)
		ctx, cancel := fc.ContextWithDeadline(context.Background(), deadline)
		defer cancel()
		select {
		case <-ctx.Done():
			failRed(t, fmt.Sprintf("Fake+Deadline(now+24h) 上下文立即被取消, err=%v", ctx.Err()))
			return false
		default:
		}
		return true
	})

	if failed {
		fmt.Println()
		fmt.Println("===============================================")
		fmt.Println("RED（红灯，缺陷未修复）")
		fmt.Println("===============================================")
		t.Fail()
	} else {
		fmt.Println()
		fmt.Println("===============================================")
		fmt.Println("GREEN（绿灯，缺陷已修复）")
		fmt.Println("===============================================")
	}
}
