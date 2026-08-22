package shurl_test

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/handler"
	"shurl/internal/model"
	"shurl/internal/service"
	"shurl/internal/store"
)

func makeTempConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Storage.URLFilePath = filepath.Join(dir, "urls.json")
	cfg.Storage.LogFilePath = filepath.Join(dir, "access.log")
	cfg.Storage.SyncInterval = 5 * time.Minute
	cfg.Storage.FlushOnWrite = false
	cfg.Janitor.Enabled = false
	return cfg
}

func seedShortCodes(t *testing.T, us *store.URLStore, n int) []string {
	t.Helper()
	codes := make([]string, 0, n)
	now := time.Now()
	for i := 0; i < n; i++ {
		code := fmt.Sprintf("cd%04d", i)
		u := &model.ShortURL{
			Code:      code,
			RawURL:    fmt.Sprintf("https://example.com/p%d", i),
			CreatedAt: now,
			Visits:    0,
		}
		if err := us.Save(u, false); err != nil {
			t.Fatalf("save short url failed: %v", err)
		}
		codes = append(codes, code)
	}
	return codes
}

type lightRW struct {
	hdr    http.Header
	status int
}

func newLightRW() *lightRW        { return &lightRW{hdr: make(http.Header)} }
func (w *lightRW) Header() http.Header         { return w.hdr }
func (w *lightRW) Write(b []byte) (int, error) { return len(b), nil }
func (w *lightRW) WriteHeader(s int)           { w.status = s }

func TestRedGreen(t *testing.T) {
	cfg := makeTempConfig(t)

	us, err := store.NewURLStore(cfg)
	if err != nil {
		t.Fatalf("NewURLStore: %v", err)
	}
	ctxLoad, cancelLoad := context.WithTimeout(context.Background(), 5*time.Second)
	if err := us.Load(ctxLoad); err != nil {
		cancelLoad()
		t.Fatalf("URLStore.Load: %v", err)
	}
	cancelLoad()

	ls, err := store.NewAccessLogStore(cfg)
	if err != nil {
		t.Fatalf("NewAccessLogStore: %v", err)
	}
	if err := ls.Open(context.Background()); err != nil {
		t.Fatalf("AccessLogStore.Open: %v", err)
	}

	codes := seedShortCodes(t, us, 120)

	rdSvc, err := service.NewRedirectService(us, ls)
	if err != nil {
		t.Fatalf("NewRedirectService: %v", err)
	}

	rdH, err := handler.NewRedirectHandler(rdSvc)
	if err != nil {
		t.Fatalf("NewRedirectHandler: %v", err)
	}

	duration := 4 * time.Second
	concurrency := 128
	var expectedLogs atomic.Int64
	var wg sync.WaitGroup
	deadline := time.Now().Add(duration)
	rngPool := sync.Pool{
		New: func() any { return rand.New(rand.NewSource(time.Now().UnixNano())) },
	}
	uaHeaders := map[string][]string{
		"User-Agent": {"Mozilla/5.0 TestAgent"},
		"Referer":    {"https://ref.example.com/"},
	}

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			rnd := rngPool.Get().(*rand.Rand)
			defer rngPool.Put(rnd)
			lr := httptest.NewRequest(http.MethodGet, "/x", nil)
			lr.RemoteAddr = fmt.Sprintf("10.0.%d.%d:1234", gid&0xff, (gid>>8)&0xff)
			lw := newLightRW()
			for time.Now().Before(deadline) {
				idx := rnd.Intn(len(codes))
				code := codes[idx]
				if rnd.Intn(5) == 0 {
					code = "zz9999"
				}
				req := &service.RedirectRequest{
					Code:       code,
					RemoteAddr: lr.RemoteAddr,
					Headers:    uaHeaders,
					Timestamp:  time.Now(),
				}
				_, _ = rdSvc.HandleRedirect(context.Background(), req)
				lr.URL.Path = "/" + code
				for k, v := range uaHeaders {
					lr.Header[k] = v
				}
				rdH.Redirect(lw, lr, code)
				expectedLogs.Add(3)
			}
		}(i)
	}
	wg.Wait()

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = rdH.ShutdownFlush(shutCtx)
	shutCancel()
	_, _ = rdSvc.BackgroundFlush()
	_ = ls.Sync()
	_ = ls.Close()

	ls2, err := store.NewAccessLogStore(cfg)
	if err != nil {
		t.Fatalf("reopen NewAccessLogStore: %v", err)
	}
	if err := ls2.Open(context.Background()); err != nil {
		t.Fatalf("reopen log store: %v", err)
	}
	defer func() { _ = ls2.Close() }()

	var validLogs int64
	var decodeErrs int64
	_, scanErr := ls2.Scan(func(l *model.AccessLog) bool {
		if l == nil {
			decodeErrs++
			return true
		}
		if l.ID == "" || l.Code == "" || l.Timestamp.IsZero() {
			decodeErrs++
			return true
		}
		if l.Status < 100 || l.Status > 599 {
			decodeErrs++
			return true
		}
		validLogs++
		return true
	}, 0)
	if scanErr != nil {
		decodeErrs++
	}

	expected := expectedLogs.Load()
	t.Logf("expected logs: %d, valid logs: %d, decode errors: %d",
		expected, validLogs, decodeErrs)

	ratio := 0.0
	if expected > 0 {
		ratio = float64(validLogs) / float64(expected)
	}
	t.Logf("valid log ratio: %.4f", ratio)

	if decodeErrs > 0 {
		fmt.Println("RED（红灯，缺陷未修复）")
		t.Errorf("RED: NDJSON corrupted / bad records, decodeErrors=%d", decodeErrs)
		return
	}
	if expected > 0 && (ratio < 0.998 || ratio > 1.002) {
		fmt.Println("RED（红灯，缺陷未修复）")
		t.Errorf("RED: valid/expected ratio out of range, ratio=%.4f, valid=%d expected=%d",
			ratio, validLogs, expected)
		return
	}

	data, readErr := os.ReadFile(cfg.Storage.LogFilePath)
	if readErr == nil && len(data) > 0 {
		blankLines := 0
		for i := 0; i < len(data)-1; i++ {
			if data[i] == '\n' && data[i+1] == '\n' {
				blankLines++
			}
		}
		if blankLines > 8 {
			fmt.Println("RED（红灯，缺陷未修复）")
			t.Errorf("RED: suspicious blank lines in NDJSON: count=%d", blankLines)
			return
		}
	}

	fmt.Println("GREEN（绿灯，缺陷已修复）")
	t.Logf("GREEN: no corruption detected, valid=%d ratio=%.4f", validLogs, ratio)
}
