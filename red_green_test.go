package shurl_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/service"
	"shurl/internal/store"
)

const testShortCode = "abc1234"

func buildTestCfg(tmpDir string) *config.Config {
	cfg := config.Default()
	cfg.Storage.URLFilePath = filepath.Join(tmpDir, "urls.json")
	cfg.Storage.LogFilePath = filepath.Join(tmpDir, "access.log")
	cfg.Storage.SyncInterval = 0
	cfg.Storage.FlushOnWrite = false
	cfg.Stats.CacheTTL = 0
	cfg.Stats.MaxRecords = 1000000
	return cfg
}

func TestRedGreen(t *testing.T) {
	red := false
	reason := ""
	defer func() {
		if r := recover(); r != nil {
			red = true
			reason = fmt.Sprintf("panic during test: %v", r)
			fmt.Println("RED（红灯，缺陷未修复）:", reason)
			t.Fatalf("%s", reason)
		}
		if red {
			fmt.Println("RED（红灯，缺陷未修复）:", reason)
			t.Fatalf("%s", reason)
		} else {
			fmt.Println("GREEN（绿灯，缺陷已修复）")
		}
	}()

	tmpDir, err := os.MkdirTemp("", "shurl-race-*")
	if err != nil {
		red = true
		reason = "create tmp dir: " + err.Error()
		return
	}
	defer os.RemoveAll(tmpDir)

	cfg := buildTestCfg(tmpDir)
	ctx := context.Background()

	urlStore, err := store.NewURLStore(cfg)
	if err != nil {
		red = true
		reason = "new url store: " + err.Error()
		return
	}
	if err := urlStore.Load(ctx); err != nil {
		red = true
		reason = "load url store: " + err.Error()
		return
	}
	logStore, err := store.NewAccessLogStore(cfg)
	if err != nil {
		red = true
		reason = "new log store: " + err.Error()
		return
	}
	if err := logStore.Open(ctx); err != nil {
		red = true
		reason = "open log store: " + err.Error()
		return
	}
	defer logStore.Close()

	u := &model.ShortURL{
		Code:      testShortCode,
		RawURL:    "https://example.com/" + testShortCode,
		CreatedAt: time.Now(),
		Visits:    0,
	}
	if err := urlStore.Save(u, false); err != nil {
		red = true
		reason = "save url: " + err.Error()
		return
	}

	rdSvc, err := service.NewRedirectService(urlStore, logStore)
	if err != nil {
		red = true
		reason = "new redirect svc: " + err.Error()
		return
	}
	defer rdSvc.StopFlusher()

	stSvc, err := service.NewStatsService(cfg, urlStore, logStore)
	if err != nil {
		red = true
		reason = "new stats svc: " + err.Error()
		return
	}

	const (
		writerGoroutines = 16
		readerGoroutines = 16
		writesPerWriter  = 60
		readsPerReader   = 60
	)
	totalExpected := int64(writerGoroutines * writesPerWriter)

	var redirectProgress int64
	var redirectOK int64
	var statsOK int64
	var statsErr int64

	var worstUnderreads int64
	var maxObservedPV int64

	var wg sync.WaitGroup
	start := make(chan struct{})

	for w := 0; w < writerGoroutines; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			for i := 0; i < writesPerWriter; i++ {
				req := &service.RedirectRequest{
					Code:       testShortCode,
					RemoteAddr: fmt.Sprintf("10.0.%d.%d:1234", id, i%256),
					Headers: map[string][]string{
						"User-Agent": {fmt.Sprintf("writer-%d-agent-%d", id, i)},
						"Referer":    {fmt.Sprintf("https://ref%d.example.com/page%d", id%5, i%10)},
					},
					Timestamp: time.Now(),
				}
				res, e := rdSvc.HandleRedirect(ctx, req)
				if e == nil && res != nil {
					atomic.AddInt64(&redirectOK, 1)
					atomic.AddInt64(&redirectProgress, 1)
				}
			}
		}(w)
	}

	for r := 0; r < readerGoroutines; r++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			for i := 0; i < readsPerReader; i++ {
				ov, e := stSvc.Overall(ctx, testShortCode, 7)
				if e != nil {
					atomic.AddInt64(&statsErr, 1)
					time.Sleep(time.Microsecond * 200)
					continue
				}
				if ov != nil && ov.Code == testShortCode {
					atomic.AddInt64(&statsOK, 1)
					progress := atomic.LoadInt64(&redirectProgress)
					pv := ov.TotalPV
					for {
						old := atomic.LoadInt64(&maxObservedPV)
						if pv <= old || atomic.CompareAndSwapInt64(&maxObservedPV, old, pv) {
							break
						}
					}
					if progress > 30 && pv < progress/2 {
						atomic.AddInt64(&worstUnderreads, 1)
					}
					if progress > 50 && pv < 10 {
						atomic.AddInt64(&worstUnderreads, 10)
					}
				}
				time.Sleep(time.Microsecond * 150)
			}
		}(r)
	}

	close(start)
	wg.Wait()

	rdSvc.StopFlusher()
	_ = logStore.Sync()

	actualRedirects := atomic.LoadInt64(&redirectOK)
	if actualRedirects != totalExpected {
		red = true
		reason = fmt.Sprintf("redirect ok mismatch: expected %d got %d", totalExpected, actualRedirects)
		return
	}

	lines, err := logStore.CountLines()
	if err != nil {
		red = true
		reason = "count lines error: " + err.Error()
		return
	}
	if lines != actualRedirects {
		red = true
		reason = fmt.Sprintf("access log line count mismatch: expected %d (redirects) got %d (file lines)", actualRedirects, lines)
		return
	}

	final, err := stSvc.Overall(ctx, testShortCode, 7)
	if err != nil {
		red = true
		reason = "final stats error: " + err.Error()
		return
	}
	if final.TotalPV != actualRedirects {
		red = true
		reason = fmt.Sprintf("final stats TotalPV mismatch: expected %d (redirects) got %d (aggregated)", actualRedirects, final.TotalPV)
		return
	}

	pvSum := int64(0)
	for _, d := range final.Daily {
		pvSum += d.PV
	}
	if pvSum != final.TotalPV {
		red = true
		reason = fmt.Sprintf("daily bucket PV sum mismatch: total %d, buckets sum %d", final.TotalPV, pvSum)
		return
	}

	observedMax := atomic.LoadInt64(&maxObservedPV)
	expectedMinMax := int64(float64(actualRedirects) * 0.5)
	if observedMax < expectedMinMax {
		red = true
		reason = fmt.Sprintf("during concurrent reads, max observed TotalPV was only %d, expected at least %d (50%% of %d final entries) — shared-file cursor race truncated all scans",
			observedMax, expectedMinMax, actualRedirects)
		return
	}
	underreads := atomic.LoadInt64(&worstUnderreads)
	totalReads := int64(readerGoroutines * readsPerReader)
	if underreads > totalReads/5 {
		red = true
		reason = fmt.Sprintf("concurrent scan consistency failure: %d of %d stats reads produced severely undercounted PV (progress/2 gap or severe zero-read); this is caused by the shared file cursor being moved mid-scan",
			underreads, totalReads)
		return
	}
}
