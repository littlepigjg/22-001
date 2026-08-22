package shurl_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/service"
	"shurl/internal/store"
)

func tDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "shurl-defect-*")
	if err != nil {
		t.Fatalf("mk temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func tCfg(dir string, syncInt time.Duration, flushOnWrite bool) *config.Config {
	cfg := config.Default()
	cfg.Storage.URLFilePath(filepath.Join(dir, "urls.json")).
		LogFilePath(filepath.Join(dir, "access.log")).
		SyncInterval(syncInt).
		FlushOnWrite(flushOnWrite)
	cfg.Janitor.Enabled = false
	return cfg
}

type res struct {
	name   string
	ok     bool
	detail string
}

func TestRedGreen(t *testing.T) {
	results := make([]res, 0, 5)

	// 场景 1：FlushOnWrite=true 时，Append 应该每条都触发 Sync。
	// 如果条件写反（!FlushOnWrite），就会一条都不触发。
	results = append(results, func() res {
		name := "flush_on_write_true_access_log"
		dir := tDir(t)
		// SyncInterval 设为很长，所以后台 sync 不会在测试期间触发；FlushOnWrite=true
		cfg := tCfg(dir, 24*time.Hour, true)
		ls, err := store.NewAccessLogStore(cfg)
		if err != nil {
			return res{name, false, fmt.Sprintf("NewAccessLogStore: %v", err)}
		}
		if err := ls.Open(context.Background()); err != nil {
			return res{name, false, fmt.Sprintf("Open: %v", err)}
		}
		defer func() { _ = ls.Close() }()

		n := 500
		ts := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		for i := 0; i < n; i++ {
			l := &model.AccessLog{
				ID:        fmt.Sprintf("F%d", i),
				Code:      fmt.Sprintf("ab%04d", i),
				Timestamp: ts.Add(time.Duration(i) * time.Second),
				Status:    302,
				IP:        "10.0.0.1",
			}
			if err := ls.Append(l); err != nil {
				return res{name, false, fmt.Sprintf("Append #%d: %v", i, err)}
			}
		}
		dc := ls.DiagnosticCounters()
		// FlushOnWrite=true => 每条 Append 都应该走 FlushOnWriteSync 分支
		if dc.AppendCalls != int64(n) {
			return res{name, false, fmt.Sprintf("AppendCalls=%d, want %d", dc.AppendCalls, n)}
		}
		// 正常：FlushOnWriteSyncs ~= n（允许极少量误差 0）
		// 缺陷（条件写反）：FlushOnWriteSyncs == 0（因为 !true=false，分支不进）
		const minFOW = 450
		if dc.FlushOnWriteSyncs < minFOW {
			return res{name, false, fmt.Sprintf(
				"FlushOnWrite=true but FlushOnWriteSyncs=%d (AppendCalls=%d); expected >=%d — condition likely inverted",
				dc.FlushOnWriteSyncs, dc.AppendCalls, minFOW)}
		}
		return res{name, true, fmt.Sprintf(
			"FlushOnWrite=true ok: AppendCalls=%d FlushOnWriteSyncs=%d",
			dc.AppendCalls, dc.FlushOnWriteSyncs)}
	}())

	// 场景 2：FlushOnWrite=false 时，Append 不应该触发 Sync。
	// 如果条件写反（!false=true），就会每条都错误地触发 Sync。
	results = append(results, func() res {
		name := "flush_on_write_false_access_log"
		dir := tDir(t)
		cfg := tCfg(dir, 24*time.Hour, false)
		ls, err := store.NewAccessLogStore(cfg)
		if err != nil {
			return res{name, false, fmt.Sprintf("NewAccessLogStore: %v", err)}
		}
		if err := ls.Open(context.Background()); err != nil {
			return res{name, false, fmt.Sprintf("Open: %v", err)}
		}
		defer func() { _ = ls.Close() }()

		n := 200
		ts := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
		for i := 0; i < n; i++ {
			l := &model.AccessLog{
				ID:        fmt.Sprintf("G%d", i),
				Code:      fmt.Sprintf("cd%04d", i),
				Timestamp: ts.Add(time.Duration(i) * time.Second),
				Status:    302,
				IP:        "10.0.0.2",
			}
			if err := ls.Append(l); err != nil {
				return res{name, false, fmt.Sprintf("Append #%d: %v", i, err)}
			}
		}
		dc := ls.DiagnosticCounters()
		// 正常：FlushOnWrite=false => FlushOnWriteSyncs == 0（允许 0）
		// 缺陷（条件写反 !false=true）：FlushOnWriteSyncs == n（错误地每条都 Sync）
		const maxFOW = 20
		if dc.FlushOnWriteSyncs > maxFOW {
			return res{name, false, fmt.Sprintf(
				"FlushOnWrite=false but FlushOnWriteSyncs=%d (AppendCalls=%d); expected <=%d — condition likely inverted",
				dc.FlushOnWriteSyncs, dc.AppendCalls, maxFOW)}
		}
		return res{name, true, fmt.Sprintf(
			"FlushOnWrite=false ok: FlushOnWriteSyncs=%d (AppendCalls=%d)",
			dc.FlushOnWriteSyncs, dc.AppendCalls)}
	}())

	// 场景 3：AccessLogStore 后台 Sync goroutine 生命周期。
	// SyncInterval=50ms => BuildSyncContext limit 也会被压到 50ms。
	// 正常情况下（用 Background + WithCancel 派生）：后台 goroutine 应该一直活到 Close。
	// 缺陷情况下（50ms WithTimeout）：goroutine 50ms 后就退出，BackgroundSyncs 不再增长。
	results = append(results, func() res {
		name := "bg_sync_lifecycle_access_log"
		dir := tDir(t)
		syncInt := 50 * time.Millisecond
		cfg := tCfg(dir, syncInt, false)
		ls, err := store.NewAccessLogStore(cfg)
		if err != nil {
			return res{name, false, fmt.Sprintf("NewAccessLogStore: %v", err)}
		}
		if err := ls.Open(context.Background()); err != nil {
			return res{name, false, fmt.Sprintf("Open: %v", err)}
		}
		defer func() { _ = ls.Close() }()

		// 先写 1 条激活写路径
		_ = ls.Append(&model.AccessLog{ID: "bg1", Code: "AB", Timestamp: time.Now(), Status: 302, IP: "1.1.1.1"})

		// 阶段一：等待 600ms（= 50ms × 12 个 ticker 周期）
		time.Sleep(600 * time.Millisecond)
		bgPhase1 := ls.DiagnosticCounters().BackgroundSyncs

		// 再写 1 条保持 dirty
		_ = ls.Append(&model.AccessLog{ID: "bg2", Code: "AC", Timestamp: time.Now(), Status: 302, IP: "1.1.1.1"})

		// 阶段二：再等待 1500ms（= 30+ 个 ticker 周期）
		time.Sleep(1500 * time.Millisecond)
		bgPhase2 := ls.DiagnosticCounters().BackgroundSyncs

		// 正常：phase1 应该已经有很多次（至少 5 次），phase2 又会增加更多（至少 15 次增加）
		// 缺陷：context 50ms 就死了，所以 phase1 只有 0~1 次，phase2 也不再增加（delta 很小，<2）
		if bgPhase1 < 3 {
			return res{name, false, fmt.Sprintf(
				"early stage: BackgroundSyncs=%d after 600ms (ticker every 50ms, ~12 ticks expected >=5); bg syncer likely died due to 50ms context timeout",
				bgPhase1)}
		}
		delta := bgPhase2 - bgPhase1
		if delta < 10 {
			return res{name, false, fmt.Sprintf(
				"late stage: BackgroundSyncs phase1=%d phase2=%d (delta=%d after ~1500ms, ~30 ticks expected >=10 increase); bg syncer stopped entirely after short context timeout",
				bgPhase1, bgPhase2, delta)}
		}
		return res{name, true, fmt.Sprintf(
			"bg sync alive: phase1=%d phase2=%d (delta=%d) after ~2.1s with 50ms ticker",
			bgPhase1, bgPhase2, delta)}
	}())

	// 场景 4：URLStore 后台 Syncer 生命周期 —— 同样用 BuildSyncContext 的短超时。
	results = append(results, func() res {
		name := "bg_sync_lifecycle_url_store"
		dir := tDir(t)
		syncInt := 50 * time.Millisecond
		cfg := tCfg(dir, syncInt, false)
		us, err := store.NewURLStore(cfg)
		if err != nil {
			return res{name, false, fmt.Sprintf("NewURLStore: %v", err)}
		}
		if err := us.Load(context.Background()); err != nil {
			return res{name, false, fmt.Sprintf("Load: %v", err)}
		}
		defer func() { _ = us.Close() }()

		now := time.Now()
		// 先保存一些数据制造 dirty
		for i := 0; i < 10; i++ {
			_ = us.Save(&model.ShortURL{
				Code:     fmt.Sprintf("BG%03d", i),
				RawURL:    fmt.Sprintf("https://b.com/%d", i),
				CreatedAt: now.Add(time.Duration(i) * time.Millisecond),
			}, false)
		}

		// phase1：等待 600ms
		time.Sleep(600 * time.Millisecond)
		diag1 := us.DiagnosticCounters()
		bgPhase1 := diag1.BackgroundSyncs

		// 再制造更多 dirty（后面更多 Save）
		for i := 10; i < 25; i++ {
			_ = us.Save(&model.ShortURL{
				Code:     fmt.Sprintf("BG%03d", i),
				RawURL:    fmt.Sprintf("https://b.com/%d", i),
				CreatedAt: now.Add(time.Duration(1000+i) * time.Millisecond),
			}, false)
		}

		// phase2：再等 1500ms
		time.Sleep(1500 * time.Millisecond)
		diag2 := us.DiagnosticCounters()
		bgPhase2 := diag2.BackgroundSyncs

		// 正常：BackgroundSyncs 至少 phase1>=2，phase2-phase1>=10
		// 缺陷：BuildSyncContext 50ms 就超时，phase1 约 0~1，phase2 - phase1 < 2
		if bgPhase1 < 2 {
			return res{name, false, fmt.Sprintf(
				"URLStore phase1 bg syncs=%d after 600ms (expected >=2 with 50ms ticker); bg syncer likely exited early on short context timeout",
				bgPhase1)}
		}
		delta := bgPhase2 - bgPhase1
		if delta < 8 {
			return res{name, false, fmt.Sprintf(
				"URLStore phase1=%d phase2=%d delta=%d after extra 1500ms (expected delta>=8); bg syncer didn't continue periodic syncs between phases",
				bgPhase1, bgPhase2, delta)}
		}
		return res{name, true, fmt.Sprintf(
			"URLStore bg syncs alive: phase1=%d phase2=%d delta=%d",
			bgPhase1, bgPhase2, delta)}
	}())

	// 场景 5：CloseFinalSync + 生命周期顺序验证。
	// 在 FlushOnWrite=false、SyncInterval=2小时 的情况下，所有同步都依赖 Close 的 final sync。
	// 如果 Close 顺序正确（先 cancel goroutine → wg.Wait → file Sync/Close），CloseFinalSyncs 应该等于 1。
	// 同时对于 URLStore，final sync 必须实际执行（syncCounter 增加）。
	results = append(results, func() res {
		name := "close_final_sync_both_stores"
		dir := tDir(t)
		cfg := tCfg(dir, 24*time.Hour, false)

		us, err := store.NewURLStore(cfg)
		if err != nil {
			return res{name, false, fmt.Sprintf("NewURLStore: %v", err)}
		}
		if err := us.Load(context.Background()); err != nil {
			return res{name, false, fmt.Sprintf("Load: %v", err)}
		}
		ls, err := store.NewAccessLogStore(cfg)
		if err != nil {
			return res{name, false, fmt.Sprintf("NewAccessLogStore: %v", err)}
		}
		if err := ls.Open(context.Background()); err != nil {
			return res{name, false, fmt.Sprintf("Open: %v", err)}
		}

		rdSvc, err := service.NewRedirectService(us, ls)
		if err != nil {
			return res{name, false, fmt.Sprintf("NewRedirectService: %v", err)}
		}

		// 创建种子 URL + 重定向（产生访问日志）
		now := time.Now()
		for i := 0; i < 20; i++ {
			_ = us.Save(&model.ShortURL{
				Code:     fmt.Sprintf("Z%03d", i),
				RawURL:    fmt.Sprintf("https://z.com/%d", i),
				CreatedAt: now,
			}, false)
		}
		hdrs := map[string][]string{"User-Agent": {"test"}}
		for i := 0; i < 300; i++ {
			_, _ = rdSvc.HandleRedirect(context.Background(), &service.RedirectRequest{
				Code:       fmt.Sprintf("Z%03d", i%20),
				RemoteAddr: "127.0.0.1:9999",
				Headers:    hdrs,
				Timestamp:  now.Add(time.Duration(i) * time.Millisecond),
			})
		}

		// SyncInterval=24h，所以后台 sync 在测试期间不会触发。
		// FlushOnWrite=false，所以 Append/Save 内部也不会立即 Sync。
		// 所有落盘都依赖 Close 时的 final sync。

		errL := ls.Close()
		errU := us.Close()
		if errL != nil || errU != nil {
			return res{name, false, fmt.Sprintf("Close errors: log=%v url=%v", errL, errU)}
		}
		lsDiag := ls.DiagnosticCounters()
		usDiag := us.DiagnosticCounters()

		// 至少 CloseFinalSyncs 应该各有 1 次
		if lsDiag.CloseFinalSyncs < 1 {
			return res{name, false, fmt.Sprintf(
				"AccessLogStore CloseFinalSyncs=%d; expected >=1 — close order or final sync missing",
				lsDiag.CloseFinalSyncs)}
		}
		if usDiag.CloseFinalSyncs < 1 {
			return res{name, false, fmt.Sprintf(
				"URLStore CloseFinalSyncs=%d; expected >=1 — close final sync (flushLocked/sync) was not executed properly",
				usDiag.CloseFinalSyncs)}
		}

		// URLStore SyncCalls 至少应该包括 Close 的那次（如果 BG sync 一次都没跑，则 SyncCalls == 1）
		if usDiag.SyncCalls < 1 {
			return res{name, false, fmt.Sprintf(
				"URLStore total SyncCalls=%d (SaveCalls=%d); expected >=1 final sync via Close",
				usDiag.SyncCalls, usDiag.SaveCalls)}
		}
		return res{name, true, fmt.Sprintf(
			"close final sync ok: URL(Save=%d Sync=%d CloseSync=%d) | Log(Append=%d Sync=%d CloseSync=%d)",
			usDiag.SaveCalls, usDiag.SyncCalls, usDiag.CloseFinalSyncs,
			lsDiag.AppendCalls, lsDiag.SyncCalls, lsDiag.CloseFinalSyncs)}
	}())

	total := len(results)
	pass := 0
	for _, r := range results {
		if r.ok {
			pass++
		}
		t.Logf("[%-36s] %s", r.name, r.detail)
	}
	fmt.Println()
	if pass == total {
		fmt.Printf("GREEN（绿灯，缺陷已修复） — 全部 %d 个场景通过 (%d/%d)\n", total, pass, total)
		t.Logf("GREEN — %d/%d scenarios passed", pass, total)
	} else {
		fmt.Printf("RED（红灯，缺陷未修复） — %d/%d 个场景失败 (%d/%d 通过)\n", total-pass, total, pass, total)
		t.Logf("RED — %d/%d scenarios passed", pass, total)
		t.Fatalf("RED: %d/%d scenarios FAILED", total-pass, total)
	}
}
