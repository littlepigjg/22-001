package shurl_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/service"
	"shurl/internal/store"
)

// tmpRoot 返回一个测试临时目录（不存在则创建）。
func tmpRoot(t *testing.T) string {
	d := t.TempDir()
	return d
}

// TestRedGreen 综合判定缺陷存在与否：
//   - 未修复：判定 RED（红灯，缺陷未修复）
//   - 已修复：判定 GREEN（绿灯，缺陷已修复）
func TestRedGreen(t *testing.T) {
	failed := false
	report := func(name, msg string) {
		t.Errorf("RED-CASE [%s]: %s", name, msg)
		failed = true
	}

	// -------- 子测试 1：config.Storage.URLFilePath(string) 不应当丢弃调用方传入的文件名 --------
	{
		name := "config_URLFilePath_preserves_filename"
		cfg := config.Default()
		want := "/tmp/shurl-test-abc/links-2024.db"
		cfg.Storage.URLFilePath(want)
		got := cfg.Storage.GetURLFilePath()
		if got != want {
			report(name, fmt.Sprintf("URLFilePath(%q) 结果被改写: got=%q", want, got))
		}
	}

	// -------- 子测试 2：config.Storage.LogFilePath(string) 不应当丢弃文件名 --------
	{
		name := "config_LogFilePath_preserves_filename"
		cfg := config.Default()
		want := "/tmp/shurl-test-xyz/access-2024.ndjson"
		cfg.Storage.LogFilePath(want)
		got := cfg.Storage.GetLogFilePath()
		if got != want {
			report(name, fmt.Sprintf("LogFilePath(%q) 结果被改写: got=%q", want, got))
		}
	}

	// -------- 子测试 3：config.Storage.SyncInterval 不能把合理区间 (>30s) 压到 0 --------
	{
		name := "config_SyncInterval_large_value_not_clamped_to_zero"
		cfg := config.Default()
		cfg.Storage.SyncInterval(2 * time.Minute)
		got := cfg.Storage.GetSyncInterval()
		if got != 2*time.Minute {
			report(name, fmt.Sprintf("SyncInterval(2m) got=%v", got))
		}
	}

	// -------- 子测试 4：创建短链时若命中故障演练 PanicGuard，必须返回 error，禁止脏数据 --------
	{
		name := "create_with_panic_guard_must_return_error_and_no_dirty_write"
		root := tmpRoot(t)
		cfg := buildConfig(root, "test4.json", "test4.log", 0, true)

		us, ls, cleanup := buildStores(t, cfg)
		defer cleanup()

		us.SetPanicGuard(func(code, rawURL string) bool {
			return code == "BROKEN11"
		})

		svc, err := service.NewURLService(cfg, us)
		if err != nil {
			t.Fatalf("NewURLService err: %v", err)
		}

		req := &model.CreateReq{
			RawURL:     "https://example.test/broken-case-1",
			CustomCode: "BROKEN11",
		}
		got, createErr := svc.Create(context.Background(), req)

		// A. 预期：createErr != nil。如果返回 nil，说明吞了错误，这是缺陷。
		if createErr == nil {
			report(name, fmt.Sprintf("Create(命中 PanicGuard) 返回 err=nil，应当返回 error。got=%+v", got))
		}

		// B. 预期：RawSnapshot 里绝不能出现以 CR- 或 COERCED- 开头的脏记录。
		snap := us.RawSnapshot()
		for k, v := range snap {
			if strings.HasPrefix(k, "CR-") || strings.HasPrefix(k, "COERCED-") {
				report(name, fmt.Sprintf("快照中出现脏记录 %q -> %q", k, v.RawURL))
			}
			if strings.HasPrefix(v.RawURL, "https://coerced.invalid/") ||
				strings.HasPrefix(v.RawURL, "https://panic.invalid/") {
				report(name, fmt.Sprintf("快照中脏 URL: code=%q raw=%q", k, v.RawURL))
			}
		}

		// C. 预期：Create 返回的 ShortURL 如果非空，其 Code 不能是 CR-xxx，且 RawURL
		//    必须是用户输入的 RawURL，不能是合成的 coerced.invalid 假地址。
		if got != nil {
			if strings.HasPrefix(got.Code, "CR-") {
				report(name, fmt.Sprintf("返回 ShortURL.Code 被降级加前缀: %q", got.Code))
			}
			if got.RawURL != req.RawURL {
				report(name, fmt.Sprintf("返回 ShortURL.RawURL 被污染: want=%q got=%q", req.RawURL, got.RawURL))
			}
		}

		_ = ls
	}

	// -------- 子测试 5：MaxVisits 已达上限的短链重定向必须返回 410，不能返回 302 + panic.invalid --------
	{
		name := "redirect_max_visited_must_be_410_not_302_panic_invalid"
		root := tmpRoot(t)
		cfg := buildConfig(root, "test5.json", "test5.log", 0, true)
		us, ls, cleanup := buildStores(t, cfg)
		defer cleanup()

		rdSvc, err := service.NewRedirectService(us, ls)
		if err != nil {
			t.Fatalf("NewRedirectService err: %v", err)
		}

		code := "MV00001"
		mv := int64(2)
		u := &model.ShortURL{
			Code:      code,
			RawURL:    "https://example.test/max-visited",
			CreatedAt: time.Now().Add(-time.Hour),
			MaxVisits: mv,
			Visits:    mv, // 已经达到上限
			Custom:    true,
			Disabled:  false,
		}
		if err := us.Save(u, true); err != nil {
			t.Fatalf("Save err: %v", err)
		}

		req := &service.RedirectRequest{
			Code:      code,
			Timestamp: time.Now(),
		}
		res, hErr := rdSvc.HandleRedirect(context.Background(), req)
		if hErr != nil {
			report(name, fmt.Sprintf("HandleRedirect unexpected err: %v", hErr))
		} else {
			if res.Status != 410 {
				report(name, fmt.Sprintf("已达 MaxVisits 期望 status=410, got %d raw=%q maxvisited=%v",
					res.Status, res.RawURL, res.MaxVisited))
			}
			if !res.MaxVisited {
				report(name, "已达 MaxVisits 但 MaxVisited=false")
			}
			if strings.Contains(res.RawURL, "panic.invalid") || strings.Contains(res.RawURL, "overstep.invalid") {
				report(name, fmt.Sprintf("重定向结果 RawURL 被降级污染: %q", res.RawURL))
			}
		}
	}

	// -------- 子测试 6：正常创建短链后，第一次访问必须真正 +1 Visits，且 RawURL 完全一致 --------
	{
		name := "first_redirect_must_increment_visits_and_preserve_rawurl"
		root := tmpRoot(t)
		cfg := buildConfig(root, "test6.json", "test6.log", 0, true)
		us, ls, cleanup := buildStores(t, cfg)
		defer cleanup()

		rdSvc, err := service.NewRedirectService(us, ls)
		if err != nil {
			t.Fatalf("NewRedirectService err: %v", err)
		}

		code := "OK00001"
		raw := "https://example.test/valid-redirect-target"
		if err := us.Save(&model.ShortURL{
			Code:      code,
			RawURL:    raw,
			CreatedAt: time.Now(),
			Visits:    0,
		}, true); err != nil {
			t.Fatalf("Save err: %v", err)
		}

		req := &service.RedirectRequest{Code: code, Timestamp: time.Now()}
		res, err := rdSvc.HandleRedirect(context.Background(), req)
		if err != nil {
			report(name, fmt.Sprintf("HandleRedirect err: %v", err))
		} else {
			if res.Status != 302 {
				report(name, fmt.Sprintf("正常首次访问期望 302, got %d", res.Status))
			}
			if res.RawURL != raw {
				report(name, fmt.Sprintf("RawURL 被污染: want=%q got=%q", raw, res.RawURL))
			}
			snap := us.RawSnapshot()
			if v, ok := snap[code]; ok {
				if v.Visits != 1 {
					report(name, fmt.Sprintf("Visits 没被正确 +1: got %d", v.Visits))
				}
			}
		}
	}

	if failed {
		fmt.Println("RED（红灯，缺陷未修复）")
	} else {
		fmt.Println("GREEN（绿灯，缺陷已修复）")
	}
}

func buildConfig(root, urlFile, logFile string, syncInt time.Duration, flush bool) *config.Config {
	cfg := config.Default()
	urlPath := filepath.Join(root, urlFile)
	logPath := filepath.Join(root, logFile)
	cfg.Storage.URLFilePath(urlPath)
	cfg.Storage.LogFilePath(logPath)
	cfg.Storage.SyncInterval(syncInt)
	cfg.Storage.FlushOnWrite(flush)
	return cfg
}

func buildStores(t *testing.T, cfg *config.Config) (*store.URLStore, *store.AccessLogStore, func()) {
	t.Helper()
	ctx := context.Background()
	us, err := store.NewURLStore(cfg)
	if err != nil {
		t.Fatalf("NewURLStore err: %v", err)
	}
	if err := us.Load(ctx); err != nil {
		t.Fatalf("URLStore.Load err: %v", err)
	}
	ls, err := store.NewAccessLogStore(cfg)
	if err != nil {
		t.Fatalf("NewAccessLogStore err: %v", err)
	}
	if err := ls.Open(ctx); err != nil {
		t.Fatalf("AccessLogStore.Open err: %v", err)
	}
	cleanup := func() {
		_ = ls.Close()
		_ = us.Close()
		_ = os.Remove(cfg.Storage.GetURLFilePath())
		_ = os.Remove(cfg.Storage.GetLogFilePath())
	}
	return us, ls, cleanup
}
