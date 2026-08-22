package shurl

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

func makeTestCfg(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Storage.URLFilePath(filepath.Join(dir, "urls.json"))
	cfg.Storage.LogFilePath(filepath.Join(dir, "access.log"))
	cfg.Storage.SyncInterval(0)
	cfg.Storage.FlushOnWrite(true)
	return cfg
}

func setupStores(t *testing.T, cfg *config.Config) (*store.URLStore, *store.AccessLogStore) {
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
	t.Cleanup(func() {
		_ = us.Close()
		_ = ls.Close()
		_ = os.Remove(cfg.Storage.GetURLFilePath())
		_ = os.Remove(cfg.Storage.GetLogFilePath())
	})
	return us, ls
}

func TestRedGreen(t *testing.T) {
	isRed := false
	var reasons []string

	appendReason := func(format string, a ...any) {
		reasons = append(reasons, fmt.Sprintf(format, a...))
	}

	cfg := makeTestCfg(t)
	us, ls := setupStores(t, cfg)

	us.SetPanicGuard(func(code, rawURL string) bool {
		return strings.HasPrefix(code, "BAD-")
	})

	usvc, err := service.NewURLService(cfg, us)
	if err != nil {
		t.Fatalf("NewURLService err: %v", err)
	}
	rdsvc, err := service.NewRedirectService(us, ls)
	if err != nil {
		t.Fatalf("NewRedirectService err: %v", err)
	}

	created, errCreate := usvc.Create(context.Background(), &model.CreateReq{
		RawURL:     "https://example.test/normal-case",
		CustomCode: "GOOD-01",
		MaxVisits:  0,
	})
	if errCreate != nil {
		appendReason("正常场景 Create 意外返回错误：%v", errCreate)
		isRed = true
	} else if created == nil {
		appendReason("正常场景 Create 返回 nil ShortURL")
		isRed = true
	} else if created.RawURL != "https://example.test/normal-case" {
		appendReason("正常场景 Create 后 ShortURL.RawURL 被错误篡改：got %q want %q",
			created.RawURL, "https://example.test/normal-case")
		isRed = true
	}

	badReq := &model.CreateReq{
		RawURL:     "https://example.test/will-trigger-guard",
		CustomCode: "BAD-01",
		MaxVisits:  0,
	}
	badCreated, errBadCreate := usvc.Create(context.Background(), badReq)

	if errBadCreate == nil {
		appendReason("Guard触发场景下 Create 返回错误为 nil（应返回非 nil error）")
		isRed = true
	}
	if badCreated != nil {
		if strings.Contains(badCreated.RawURL, "panic.invalid") {
			appendReason("Guard触发场景下 Create 返回 ShortURL.RawURL 包含 panic.invalid 占位（%q），应返回错误而非合成的假成功记录",
				badCreated.RawURL)
			isRed = true
		}
		if badCreated.Code == "BAD-01" {
			appendReason("Guard触发场景下 BAD-01 仍被登记为成功记录，存在脏数据")
			isRed = true
		}
	}

	snap := us.RawSnapshot()
	for key, val := range snap {
		if strings.HasPrefix(key, "COERCED-") {
			appendReason("RawSnapshot 中检测到 COERCED- 前缀脏记录：%q -> %q", key, val.RawURL)
			isRed = true
		}
		if key == "BAD-01" && strings.Contains(val.RawURL, "panic.invalid") {
			appendReason("RawSnapshot 中 BAD-01 的 RawURL 被污染为 panic.invalid：%q", val.RawURL)
			isRed = true
		}
	}

	if badCreated != nil && badCreated.Code != "" {
		code := badCreated.Code
		rreq := &service.RedirectRequest{Code: code, Timestamp: time.Now()}
		rres, rerr := rdsvc.HandleRedirect(context.Background(), rreq)
		_ = rerr
		if rres != nil && rres.Status == 302 && strings.Contains(rres.RawURL, "panic.invalid") {
			appendReason("Guard污染记录命中重定向时返回 302->panic.invalid（%q），应走 4xx/error 分支而非降级 302",
				rres.RawURL)
			isRed = true
		}
	}

	rreq2 := &service.RedirectRequest{Code: "BAD-01", Timestamp: time.Now()}
	rres2, rerr2 := rdsvc.HandleRedirect(context.Background(), rreq2)
	_ = rerr2
	if rres2 != nil && rres2.Status == 302 && strings.Contains(rres2.RawURL, "panic.invalid") {
		appendReason("BAD-01 直接重定向返回 302->panic.invalid（%q），应走 4xx/error 分支而非降级 302",
			rres2.RawURL)
		isRed = true
	}

	if created != nil {
		rreqGood := &service.RedirectRequest{Code: "GOOD-01", Timestamp: time.Now()}
		rresGood, rerrGood := rdsvc.HandleRedirect(context.Background(), rreqGood)
		if rerrGood != nil {
			appendReason("正常记录重定向意外返回错误：%v", rerrGood)
			isRed = true
		} else if rresGood == nil {
			appendReason("正常记录重定向返回 nil result")
			isRed = true
		} else if rresGood.Status != 302 {
			appendReason("正常记录重定向 Status 应为 302，got %d", rresGood.Status)
			isRed = true
		} else if rresGood.RawURL != "https://example.test/normal-case" {
			appendReason("正常记录重定向 RawURL 不匹配：got %q want %q",
				rresGood.RawURL, "https://example.test/normal-case")
			isRed = true
		}
	}

	if isRed {
		fmt.Println("RED（红灯，缺陷未修复）")
		for i, r := range reasons {
			fmt.Printf("  RED-REASON-%d: %s\n", i+1, r)
		}
		t.Fatalf("RED（红灯，缺陷未修复）：共 %d 项异常", len(reasons))
	} else {
		fmt.Println("GREEN（绿灯，缺陷已修复）")
	}
}
