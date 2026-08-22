package shurl_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/handler"
	"shurl/internal/model"
	"shurl/internal/service"
	"shurl/internal/store"
	"shurl/pkg/retry"
)

func mustTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if dir == "" {
		t.Fatalf("tempdir is empty")
	}
	return dir
}

func newCfgForCollision(tmpDir string) *config.Config {
	cfg := config.Default()
	cfg.Storage.URLFilePath = filepath.Join(tmpDir, "urls.json")
	cfg.Storage.LogFilePath = filepath.Join(tmpDir, "access.log")
	cfg.Storage.SyncInterval = 0
	cfg.ShortCode.Length = 3
	cfg.ShortCode.Alphabet = "0123456789"
	cfg.ShortCode.MaxRetries = 5
	cfg.Server.MaxBodyBytes = 1 << 20
	cfg.Janitor.Enabled = false
	cfg.Log.Level = "ERROR"
	return cfg
}

func buildFixtureWithCfg(t *testing.T, cfg *config.Config) (*store.URLStore, *service.URLService, *handler.URLHandler, *httptest.Server) {
	t.Helper()
	us, err := store.NewURLStore(cfg)
	if err != nil {
		t.Fatalf("new url store: %v", err)
	}
	if err := us.Load(context.Background()); err != nil {
		t.Fatalf("load url store: %v", err)
	}
	svc, err := service.NewURLService(cfg, us)
	if err != nil {
		t.Fatalf("new url service: %v", err)
	}
	uh, err := handler.NewURLHandler(svc)
	if err != nil {
		t.Fatalf("new url handler: %v", err)
	}
	mux := http.NewServeMux()
	uh.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { srv.Close() })
	return us, svc, uh, srv
}

// allCodes 枚举 len=3 + digits => 1000 条可能短码。
func allCodes() []string {
	alpha := "0123456789"
	out := make([]string, 0, 1000)
	for i := 0; i < len(alpha); i++ {
		for j := 0; j < len(alpha); j++ {
			for k := 0; k < len(alpha); k++ {
				out = append(out, string([]byte{alpha[i], alpha[j], alpha[k]}))
			}
		}
	}
	return out
}

func seedMany(t *testing.T, us *store.URLStore, codes []string) {
	t.Helper()
	now := time.Now()
	for _, c := range codes {
		u := &model.ShortURL{
			Code:      c,
			RawURL:    "https://example.com/" + c,
			CreatedAt: now,
			MaxVisits: 0,
			Custom:    false,
		}
		if err := us.Save(u, false); err != nil {
			// 允许冲突（重复调用时），只跳过。
			if !errors.Is(err, model.ErrCodeConflict) {
				t.Fatalf("seed %s: %v", c, err)
			}
		}
	}
}

func jsonCreateBody(rawURL, custom string, ttl int64) []byte {
	m := map[string]any{"raw_url": rawURL}
	if custom != "" {
		m["custom_code"] = custom
	}
	if ttl > 0 {
		m["ttl_seconds"] = ttl
	}
	b, _ := json.Marshal(m)
	return b
}

// ---- 四个子断言（同时作为独立 Top-level 测试可用） ----

// TestServiceContextCancelledCreate：在 service.Create 调用时传入 deadline，
// 存储 Exists 每次 2ms 延迟 + 字符空间已基本耗尽 -> generateUnique 必须
// 至少执行 2 次重试。若 context 被剥离则不会被取消，所有重试硬跑完
// (>=10ms) 并且最终返回 ErrShortCodeGenFailed 而非取消类错误。
func TestServiceContextCancelledCreate(t *testing.T) {
	tmp := mustTempDir(t)
	cfg := newCfgForCollision(tmp)
	us, svc, _, _ := buildFixtureWithCfg(t, cfg)
	t.Cleanup(func() {
		us.SetReadLatency(0)
		_ = us.Close()
	})

	// 预占 950/1000 条短码，保证 generateUnique 平均会撞很多次。
	all := allCodes()
	pre := all[:950]
	seedMany(t, us, pre)

	us.SetReadLatency(2 * time.Millisecond)

	deadline := 7 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	start := time.Now()
	req := &model.CreateReq{RawURL: "https://example.com/unique-x"}
	u, err := svc.Create(ctx, req)
	elapsed := time.Since(start)

	gotCancel := errors.Is(err, model.ErrCanceled) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	wrongMorph := errors.Is(err, model.ErrShortCodeGenFailed)
	tooLong := elapsed > 20*time.Millisecond
	createdOkNoCancel := err == nil && u != nil && u.Code != ""

	t.Logf("service.Create elapsed=%v err=%v code=%q", elapsed, err, codeOf(u))

	// 任一 bug 迹象 => RED
	switch {
	case wrongMorph:
		t.Fatalf("RED（红灯，缺陷未修复）：service.Create 将取消错误转换为 ErrShortCodeGenFailed（错误形态变形）")
	case tooLong && !gotCancel:
		t.Fatalf("RED（红灯，缺陷未修复）：service.Create 未响应 context deadline，耗时=%v 且未返回取消错误(err=%v)", elapsed, err)
	case createdOkNoCancel && tooLong:
		t.Fatalf("RED（红灯，缺陷未修复）：service.Create 在 deadline=%v 场景下依然跑完所有重试并返回成功 code=%q，耗时=%v", deadline, codeOf(u), elapsed)
	case !gotCancel:
		t.Fatalf("RED（红灯，缺陷未修复）：service.Create deadline=%v 场景未返回取消类错误，实际 err=%v，耗时=%v", deadline, err, elapsed)
	}

	fmt.Println("GREEN（绿灯，缺陷已修复）：TestServiceContextCancelledCreate 通过")
}

// TestHandlerTimeoutCancellation：HTTP 层在客户端超时或 deadline 已到时，
// 若 Handler 将错误吞成 200 queued=true 或未正确传递 ctx 导致提前成功，
// 均视为 RED。
func TestHandlerTimeoutCancellation(t *testing.T) {
	tmp := mustTempDir(t)
	cfg := newCfgForCollision(tmp)
	us, _, _, srv := buildFixtureWithCfg(t, cfg)
	t.Cleanup(func() {
		us.SetReadLatency(0)
		_ = us.Close()
	})

	seedMany(t, us, allCodes()[:950])
	us.SetReadLatency(2 * time.Millisecond)

	client := srv.Client()
	client.Timeout = 12 * time.Millisecond

	body := bytes.NewReader(jsonCreateBody("https://example.com/handler-timeout", "", 0))
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/urls", body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, doErr := client.Do(req)
	elapsed := time.Since(start)

	var status int
	var respBody []byte
	if doErr == nil {
		status = resp.StatusCode
		respBody, _ = io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}

	t.Logf("handler.Do elapsed=%v status=%d doErr=%v body=%s", elapsed, status, doErr, string(respBody))

	if doErr == nil && (status == http.StatusOK || status == http.StatusCreated) {
		decoded := map[string]any{}
		_ = json.Unmarshal(respBody, &decoded)
		if v, ok := decoded["queued"]; ok {
			if q, _ := v.(bool); q {
				t.Fatalf("RED（红灯，缺陷未修复）：Handler 返回 HTTP 200 queued=true 伪成功，取消语义被吞掉")
			}
		}
		if status == http.StatusCreated && len(respBody) > 0 {
			if elapsed > 20*time.Millisecond {
				t.Fatalf("RED（红灯，缺陷未修复）：Handler 未响应请求 deadline，耗时=%v 才完成创建", elapsed)
			}
		}
	}

	fmt.Println("GREEN（绿灯，缺陷已修复）：TestHandlerTimeoutCancellation 通过")
}

// TestRetryPropagatesContextCancel：retry.Do 在 context 取消时返回值必须
// 保留 context.Canceled / DeadlineExceeded 错误链，不能替换成无关 sentinel。
func TestRetryPropagatesContextCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Millisecond)
	defer cancel()

	err := retry.Do(ctx, func(attempt int) error {
		time.Sleep(2 * time.Millisecond)
		return context.DeadlineExceeded
	})

	hasCancel := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	t.Logf("retry.Do err=%v hasCancel=%v", err, hasCancel)

	sentinelMatch := err != nil && strings.Contains(err.Error(), "could not be completed")

	if sentinelMatch && !hasCancel {
		t.Fatalf("RED（红灯，缺陷未修复）：retry.Do 把 context 取消错误替换成了不相关的通用错误（%v），取消语义丢失", err)
	}
	if !hasCancel {
		t.Fatalf("RED（红灯，缺陷未修复）：retry.Do 未在返回错误中保留 context.Canceled/DeadlineExceeded，实际 err=%v", err)
	}

	fmt.Println("GREEN（绿灯，缺陷已修复）：TestRetryPropagatesContextCancel 通过")
}

// TestStoreExistsRespectsContextCancel：store.Exists 在存在延迟时每次都
// 必须响应调用方 ctx 取消；若内部统一替换为 Background 则每次延迟都会
// 硬睡到底，多次调用耗时远超 deadline。
func TestStoreExistsRespectsContextCancel(t *testing.T) {
	tmp := mustTempDir(t)
	cfg := newCfgForCollision(tmp)
	us, err := store.NewURLStore(cfg)
	if err != nil {
		t.Fatalf("new url store: %v", err)
	}
	if err := us.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Cleanup(func() {
		us.SetReadLatency(0)
		_ = us.Close()
	})
	us.SetReadLatency(15 * time.Millisecond)

	start := time.Now()
	_, _ = us.Exists("aaa")
	_, _ = us.Exists("bbb")
	observed := time.Since(start)

	t.Logf("store.Exists(2 次) observed=%v", observed)
	if observed > 22*time.Millisecond {
		t.Fatalf("RED（红灯，缺陷未修复）：store.Exists 延迟路径未响应 context 取消信号，两次调用耗时=%v（期望 ≤ ~17ms）", observed)
	}

	fmt.Println("GREEN（绿灯，缺陷已修复）：TestStoreExistsRespectsContextCancel 通过")
}

// ---- 聚合判定（被 verify_cmds 直接调用） ----

func TestRedGreen(t *testing.T) {
	var red atomic.Int32

	run := func(name string, fn func(t *testing.T)) {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					red.Add(1)
					_ = fmt.Sprintf("子测试 %s panic: %v", name, r)
					t.Errorf("子测试 %s panic recovered", name)
				}
			}()
			fn(t)
		})
	}

	run("ServiceContextCancelledCreate", func(t *testing.T) {
		tmp := t.TempDir()
		cfg := newCfgForCollision(tmp)
		us, svc, _, _ := buildFixtureWithCfg(t, cfg)
		t.Cleanup(func() {
			us.SetReadLatency(0)
			_ = us.Close()
		})
		seedMany(t, us, allCodes()[:950])
		us.SetReadLatency(2 * time.Millisecond)

		deadline := 7 * time.Millisecond
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()

		start := time.Now()
		req := &model.CreateReq{RawURL: "https://example.com/s-x"}
		u, err := svc.Create(ctx, req)
		elapsed := time.Since(start)

		gotCancel := errors.Is(err, model.ErrCanceled) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		wrongMorph := errors.Is(err, model.ErrShortCodeGenFailed)
		tooLong := elapsed > 20*time.Millisecond

		t.Logf("svc elapsed=%v err=%v code=%q", elapsed, err, codeOf(u))
		if wrongMorph || tooLong || !gotCancel {
			red.Add(1)
			t.Fatalf("wrongMorph=%v tooLong=%v gotCancel=%v => RED", wrongMorph, tooLong, gotCancel)
		}
	})

	run("HandlerTimeoutCancellation", func(t *testing.T) {
		tmp := t.TempDir()
		cfg := newCfgForCollision(tmp)
		us, _, _, srv := buildFixtureWithCfg(t, cfg)
		t.Cleanup(func() {
			us.SetReadLatency(0)
			_ = us.Close()
		})
		seedMany(t, us, allCodes()[:950])
		us.SetReadLatency(2 * time.Millisecond)

		client := srv.Client()
		client.Timeout = 12 * time.Millisecond

		body := bytes.NewReader(jsonCreateBody("https://example.com/h-x", "", 0))
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/urls", body)
		req.Header.Set("Content-Type", "application/json")

		start := time.Now()
		resp, doErr := client.Do(req)
		elapsed := time.Since(start)

		var status int
		var respBody []byte
		if doErr == nil {
			status = resp.StatusCode
			respBody, _ = io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
		}
		t.Logf("handler elapsed=%v status=%d doErr=%v body=%s", elapsed, status, doErr, string(respBody))

		if doErr == nil && (status == http.StatusOK || status == http.StatusCreated) {
			decoded := map[string]any{}
			_ = json.Unmarshal(respBody, &decoded)
			if v, ok := decoded["queued"]; ok {
				if q, _ := v.(bool); q {
					red.Add(1)
					t.Fatalf("handler queued=true 伪成功响应 => RED")
				}
			}
		}
	})

	run("RetryPropagatesContextCancel", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Millisecond)
		defer cancel()
		err := retry.Do(ctx, func(attempt int) error {
			time.Sleep(2 * time.Millisecond)
			return context.DeadlineExceeded
		})
		hasCancel := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		sentinelMatch := err != nil && strings.Contains(err.Error(), "could not be completed")
		t.Logf("retry err=%v hasCancel=%v sentinel=%v", err, hasCancel, sentinelMatch)
		if (sentinelMatch && !hasCancel) || !hasCancel {
			red.Add(1)
			t.Fatalf("retry 取消语义丢失 => RED")
		}
	})

	run("StoreExistsRespectsContextCancel", func(t *testing.T) {
		tmp := t.TempDir()
		cfg := newCfgForCollision(tmp)
		us, err := store.NewURLStore(cfg)
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		if err := us.Load(context.Background()); err != nil {
			t.Fatalf("load: %v", err)
		}
		t.Cleanup(func() {
			us.SetReadLatency(0)
			_ = us.Close()
		})
		us.SetReadLatency(15 * time.Millisecond)

		start := time.Now()
		_, _ = us.Exists("aaa")
		_, _ = us.Exists("bbb")
		observed := time.Since(start)
		t.Logf("store exists observed=%v", observed)
		if observed > 22*time.Millisecond {
			red.Add(1)
			t.Fatalf("store.Exists 延迟未按 ctx 中止，耗时=%v => RED", observed)
		}
	})

	if red.Load() > 0 {
		fmt.Printf("RED（红灯，缺陷未修复）：共有 %d 个子断言失败\n", red.Load())
		runtime.Gosched()
		// 保证至少有一次 RED 输出被 stdout 捕获；同时按字母顺序排前输出
		msgs := []string{"RED（红灯，缺陷未修复）"}
		sort.Strings(msgs)
		for _, m := range msgs {
			fmt.Println(m)
		}
		t.Fatalf("RED（红灯，缺陷未修复）")
	}
	fmt.Println("GREEN（绿灯，缺陷已修复）：全部子断言通过（TestRedGreen 聚合）")
}

func codeOf(u *model.ShortURL) string {
	if u == nil {
		return ""
	}
	return u.Code
}
