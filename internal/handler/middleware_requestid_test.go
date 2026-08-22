package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestRequestIDMiddleware_ConcurrentUnique 复现压测场景：并发请求穿过 RequestIDMiddleware，
// 每个响应头的 X-Request-ID 必须唯一、非空、且不被其它字段污染。
// 用 -race 运行可验证中间件内部不再有跨请求复用的可变对象产生 data race。
func TestRequestIDMiddleware_ConcurrentUnique(t *testing.T) {
	const n = 256
	h := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 模拟 POST /api/urls 创建链路：异步审计 goroutine 的生命周期可能长于请求本身。
		reqID := RequestID(r.Context())
		go func(id string) {
			// 仅消费不可变的 ID 字符串，不触碰任何共享可变对象。
			_ = "created:demo:" + id
		}(reqID)
		_, _ = io.WriteString(w, "ok")
	}))

	srv := httptest.NewServer(h)
	defer srv.Close()

	seen := make(map[string]int, n)
	var mu sync.Mutex
	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/urls", strings.NewReader(`{"raw_url":"https://example.com"}`))
			if err != nil {
				errs <- err
				return
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			id := resp.Header.Get("X-Request-ID")
			if id == "" {
				errs <- io.EOF // 借用：表示空 ID
				return
			}
			if strings.Contains(id, "|created:") {
				errs <- &badIDError{id: id, reason: "audit field leaked into req_id"}
				return
			}
			if len(id) > 80 {
				errs <- &badIDError{id: id, reason: "req_id suspiciously long"}
				return
			}
			mu.Lock()
			seen[id]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent request failed: %v", err)
	}

	// 每个响应的 req_id 都必须唯一。
	if len(seen) != n {
		// 找出重复项以便定位。
		var dupes []string
		for id, c := range seen {
			if c > 1 {
				dupes = append(dupes, id)
			}
		}
		t.Fatalf("expected %d unique request ids, got %d; duplicates=%v", n, len(seen), dupes)
	}
}

type badIDError struct {
	id     string
	reason string
}

func (e *badIDError) Error() string { return e.reason + ": " + e.id }

// TestRequestID_ContextRoundTrip 确认 WithRequestID/RequestID 读写的是不可变 string，
// 不会被并发请求相互覆盖。
func TestRequestID_ContextRoundTrip(t *testing.T) {
	ctx := WithRequestID(context.Background(), "abc-123")
	if got := RequestID(ctx); got != "abc-123" {
		t.Fatalf("RequestID = %q, want %q", got, "abc-123")
	}
	if got := RequestID(context.Background()); got != "" {
		t.Fatalf("empty context RequestID = %q, want empty", got)
	}
}
