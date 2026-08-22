package shurl

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/handler"
	"shurl/internal/model"
	"shurl/internal/service"
	"shurl/internal/store"
	"shurl/pkg/logger"
)

func setupURL(t *testing.T, cfg *config.Config, dir string) {
	t.Helper()
	us, err := store.NewURLStore(cfg)
	if err != nil {
		t.Fatalf("new url store: %v", err)
	}
	if err := us.Load(context.Background()); err != nil {
		t.Fatalf("load url store: %v", err)
	}
	defer func() { _ = us.Close() }()

	us.SetPanicGuard(func(code, rawURL string) bool { return false })

	for i := 0; i < 4; i++ {
		code := fmt.Sprintf("ab%d0", i+1)
		_ = code
	}

	for i := 0; i < 5; i++ {
		u := &model.ShortURL{
			Code:      fmt.Sprintf("hd%02d", i+1),
			RawURL:    fmt.Sprintf("https://example.com/path-%d", i+1),
			CreatedAt: time.Now(),
			Custom:    false,
			Disabled:  false,
		}
		if err := u.Validate(); err != nil {
			t.Fatalf("validate short url: %v", err)
		}
		if err := us.Save(u, false); err != nil {
			t.Fatalf("save short url: %v", err)
		}
	}
	_ = us.RawSnapshot()

	svc, err := service.NewURLService(cfg, us)
	if err != nil {
		t.Fatalf("new url service: %v", err)
	}
	for i := 0; i < 3; i++ {
		req := &model.CreateReq{
			RawURL:    fmt.Sprintf("https://example.com/gen-%d", i+1),
			MaxVisits: 0,
		}
		if _, err := svc.Create(context.Background(), req); err != nil {
			t.Fatalf("service create: %v", err)
		}
	}
}

func makeHandler() http.Handler {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if strings.HasPrefix(path, "api/health") || path == "health" || strings.HasPrefix(path, "health") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		switch path {
		case "", "index.html":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`<html><body>ok</body></html>`))
			return
		}
		if len(path) >= 2 && len(path) <= 32 {
			w.Header().Set("Location", "https://example.com/"+path)
			w.WriteHeader(http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})
	h := handler.LoggingMiddleware(next)
	h = handler.RequestIDMiddleware(h)
	h = handler.TrimLastSlashMiddleware(h)
	return h
}

func TestRedGreen(t *testing.T) {
	tmp, err := os.MkdirTemp("", "shurl-defect27-*")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	cfg := config.Default()
	cfg.Storage.URLFilePath(filepath.Join(tmp, "urls.json"))
	cfg.Storage.LogFilePath(filepath.Join(tmp, "access.log"))
	cfg.Storage.SyncInterval(0)
	cfg.Storage.FlushOnWrite(true)
	cfg.Storage.AuditDir(filepath.Join(tmp, "audit"))

	setupURL(t, cfg, tmp)

	logger.SetLevel(logger.LevelDebug)
	std := logger.Std()
	std.SetAuditDir(cfg.Storage.AuditRoot())
	logger.ResetPeakHandles()

	h := makeHandler()

	statuses := make([]int, 0, 160)
	samplePaths := []string{
		"/",
		"/health",
		"/api/health",
		"/hd01",
		"/hd02",
		"/hd03",
		"/admin/dashboard",
		"/static/app.js",
		"/favicon.ico",
		"/nonexistent-page-xyz",
		"/api/admin/sync",
	}
	for i := 0; i < 4; i++ {
		for _, p := range samplePaths {
			r := httptest.NewRequest(http.MethodGet, p, nil)
			r.RemoteAddr = fmt.Sprintf("10.20.%d.%d:54321", i+1, (i*7+3)%250+1)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			statuses = append(statuses, w.Code)
		}
		for k := 0; k < 3; k++ {
			body := bytes.NewBufferString(`{"raw_url":"https://example.com/bulk"}`)
			r := httptest.NewRequest(http.MethodPost, "/api/urls", body)
			r.RemoteAddr = fmt.Sprintf("127.0.0.%d:34567", k+1)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			statuses = append(statuses, w.Code)
		}
	}

	runtime.GC()
	for i := 0; i < 2; i++ {
		runtime.Gosched()
	}

	probePeak := logger.PeakOpenHandles()
	logger.ResetPeakHandles()

	singleInfo := logger.Fields{"probe": "x"}
	logger.Info("probe baseline", singleInfo)
	for w := 0; w < 20; w++ {
		for i := 0; i < 4; i++ {
			path := samplePaths[(w*4+i)%len(samplePaths)]
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.RemoteAddr = fmt.Sprintf("10.10.10.%d:9999", (i+w*3)%200+1)
			wr := httptest.NewRecorder()
			h.ServeHTTP(wr, r)
			statuses = append(statuses, wr.Code)
		}
	}

	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	peak := logger.PeakOpenHandles()
	after := logger.OpenFileCount()
	_ = probePeak
	leakedPeak := peak

	threshold := int64(128)
	red := leakedPeak >= threshold

	_ = statuses

	t.Logf("========= handle stats =========")
	t.Logf("post-probe open_count          = %d", after)
	t.Logf("PeakOpenHandles during probe  = %d", peak)
	t.Logf("logger.OpenedHandles at exit  = %d", std.OpenedHandles())
	t.Logf("threshold (peak >= RED)       = %d", threshold)
	t.Logf("statuses samples (first 8)    = %v", firstN(statuses, 8))

	if red {
		fmt.Println("RED（红灯，缺陷未修复）")
		t.Fatalf("RED（红灯，缺陷未修复）: 审计/访问记录多文件 for 循环内 defer Close 未在循环迭代释放，导致生命周期内文件句柄峰值累计过高（peak=%d，阈值=%d），高请求量下将出现 too many open files 并使后续 HTTP 写文件/落盘失败。",
			peak, threshold)
	}

	fmt.Println("GREEN（绿灯，缺陷已修复）")
	t.Logf("GREEN（绿灯，缺陷已修复）: 峰值句柄可控（peak=%d，阈值=%d），每轮循环体内即 Close，不累积到函数外层。",
		peak, threshold)
}

func firstN[T any](s []T, n int) []T {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
