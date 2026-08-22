package shurl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/handler"
	"shurl/internal/service"
	"shurl/internal/store"
)

func buildTestServer(t *testing.T) (*httptest.Server, *store.URLStore, func()) {
	t.Helper()
	tmp, err := os.MkdirTemp("", "shurl-rg-*")
	if err != nil {
		t.Fatalf("create temp dir failed: %v", err)
	}
	cfg := config.Default()
	cfg.Storage.URLFilePath = tmp + "/urls.json"
	cfg.Storage.LogFilePath = tmp + "/access.log"
	cfg.Storage.SyncInterval = 0
	us, err := store.NewURLStore(cfg)
	if err != nil {
		os.RemoveAll(tmp)
		t.Fatalf("new url store: %v", err)
	}
	if err := us.Load(context.Background()); err != nil {
		os.RemoveAll(tmp)
		t.Fatalf("load url store: %v", err)
	}
	svc, err := service.NewURLService(cfg, us)
	if err != nil {
		os.RemoveAll(tmp)
		t.Fatalf("new url service: %v", err)
	}
	uh, err := handler.NewURLHandler(svc)
	if err != nil {
		os.RemoveAll(tmp)
		t.Fatalf("new url handler: %v", err)
	}
	mux := http.NewServeMux()
	uh.Register(mux)
	var root http.Handler = mux
	root = handler.LoggingMiddleware(root)
	root = handler.RequestIDMiddleware(root)
	srv := httptest.NewUnstartedServer(root)
	srv.Start()
	cleanup := func() {
		srv.Close()
		us.Close()
		os.RemoveAll(tmp)
	}
	return srv, us, cleanup
}

type idCaptureRoundTripper struct {
	base        http.RoundTripper
	mu          sync.Mutex
	reqIDs      []string
	createAudit []string
}

func (c *idCaptureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqIDs = append(c.reqIDs, resp.Header.Get("X-Request-ID"))
	c.createAudit = append(c.createAudit, resp.Header.Get("X-Create-Audit"))
	return resp, err
}

func TestRedGreen(t *testing.T) {
	srv, _, cleanup := buildTestServer(t)
	defer cleanup()

	rt := &idCaptureRoundTripper{base: http.DefaultTransport}
	client := &http.Client{
		Transport: rt,
		Timeout:   30 * time.Second,
	}

	const goroutines = 60
	const perGoroutine = 8
	totalRequests := goroutines * perGoroutine

	var createdCount int64
	var failedCount int64
	var httpErrors int64

	payloadIdx := int64(0)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(gi int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				n := atomic.AddInt64(&payloadIdx, 1)
				raw := fmt.Sprintf("https://example.com/test/%d-%d?ref=%d", gi, i, n)
				body := map[string]any{
					"raw_url": raw,
				}
				buf, _ := json.Marshal(body)
				req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/urls", bytes.NewReader(buf))
				if err != nil {
					atomic.AddInt64(&httpErrors, 1)
					continue
				}
				req.Header.Set("Content-Type", "application/json")
				resp, err := client.Do(req)
				if err != nil {
					atomic.AddInt64(&httpErrors, 1)
					continue
				}
				if resp.StatusCode == http.StatusCreated {
					atomic.AddInt64(&createdCount, 1)
				} else {
					atomic.AddInt64(&failedCount, 1)
				}
				resp.Body.Close()
			}
		}(g)
	}
	wg.Wait()

	time.Sleep(300 * time.Millisecond)

	rt.mu.Lock()
	ids := make([]string, len(rt.reqIDs))
	copy(ids, rt.reqIDs)
	rt.mu.Unlock()

	t.Logf("requests: total=%d created=%d failed=%d http_err=%d collected_ids=%d",
		totalRequests, atomic.LoadInt64(&createdCount),
		atomic.LoadInt64(&failedCount), atomic.LoadInt64(&httpErrors), len(ids))

	defectPresent := false
	var defectReasons []string

	if len(ids) == 0 {
		defectPresent = true
		defectReasons = append(defectReasons, "no X-Request-ID headers captured (all requests dropped)")
	}

	seen := make(map[string]int, len(ids))
	var emptyCount int
	var pipeCount int
	var veryLongCount int
	for _, id := range ids {
		seen[id]++
		if id == "" {
			emptyCount++
		}
		if strings.Contains(id, "|") {
			pipeCount++
		}
		if len(id) > 80 {
			veryLongCount++
		}
	}
	duplicates := 0
	for _, c := range seen {
		if c > 1 {
			duplicates += c - 1
		}
	}
	if duplicates > 0 {
		defectPresent = true
		defectReasons = append(defectReasons, fmt.Sprintf("X-Request-ID 重复出现了 %d 次（池复用导致不同请求共享了同一个头部缓冲区）", duplicates))
	}
	if emptyCount > 0 {
		defectPresent = true
		defectReasons = append(defectReasons, fmt.Sprintf("有 %d 个响应的 X-Request-ID 为空串（缓冲区被并发 Reset 提前清空）", emptyCount))
	}
	if pipeCount > 0 {
		defectPresent = true
		defectReasons = append(defectReasons, fmt.Sprintf("有 %d 个响应的 X-Request-ID 混入了 '|created:...' 审计后缀（后台 goroutine 写入与中间件读取同一个 Builder）", pipeCount))
	}
	if veryLongCount > 0 {
		defectPresent = true
		defectReasons = append(defectReasons, fmt.Sprintf("有 %d 个响应的 X-Request-ID 长度超过 80 字符（多个请求共享缓冲区时发生拼接）", veryLongCount))
	}

	if atomic.LoadInt64(&httpErrors) > 0 {
		defectPresent = true
		defectReasons = append(defectReasons, fmt.Sprintf("有 %d 个 HTTP 请求发生传输级错误（竞态导致 ResponseWriter 头部异常）", atomic.LoadInt64(&httpErrors)))
	}

	t.Logf("id_analysis: unique=%d duplicates=%d empty=%d pipe_mark=%d long=%d",
		len(seen), duplicates, emptyCount, pipeCount, veryLongCount)

	for _, r := range defectReasons {
		t.Logf("DEFECT_REASON: %s", r)
	}

	if defectPresent {
		fmt.Println("RED（红灯，缺陷未修复）")
		t.Fatalf("RED（红灯，缺陷未修复）: %s", strings.Join(defectReasons, "; "))
	} else {
		fmt.Println("GREEN（绿灯，缺陷已修复）")
	}
}
