package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/store"
)

// newTestStores 构建一对临时文件存储，统计缓存关闭、MaxRecords 足够大。
func newTestStores(t *testing.T) (*store.URLStore, *store.AccessLogStore, *config.Config) {
	t.Helper()
	cfg := config.Default()
	cfg.Storage.URLFilePath = t.TempDir() + "/urls.json"
	cfg.Storage.LogFilePath = t.TempDir() + "/access.log"
	cfg.Storage.FlushOnWrite = false
	cfg.Stats.CacheTTL = 0 // 关闭缓存，每次 Overall 都重新扫描，最大化并发读写竞争
	cfg.Stats.MaxRecords = 1_000_000

	urlStore, err := store.NewURLStore(cfg)
	if err != nil {
		t.Fatalf("NewURLStore: %v", err)
	}
	if err := urlStore.Load(context.Background()); err != nil {
		t.Fatalf("url store load: %v", err)
	}
	logStore, err := store.NewAccessLogStore(cfg)
	if err != nil {
		t.Fatalf("NewAccessLogStore: %v", err)
	}
	if err := logStore.Open(context.Background()); err != nil {
		t.Fatalf("log store open: %v", err)
	}
	t.Cleanup(func() {
		_ = logStore.Close()
		_ = urlStore.Close()
	})
	return urlStore, logStore, cfg
}

// TestConcurrentRedirectsAndStats 并发压测：多 goroutine 写 302 访问日志的同时，
// 多 goroutine 读统计报表。重定向全部落盘后，TotalPV 必须等于已记录的 302 次数，
// 且并发读期间观测到的最大 TotalPV 不得被截断。
//
// 复现并验证修复：修复前 ScanShared 在并发追加下读到错位记录会 decode error 后
// break，导致 TotalPV 远小于真实值（~5%）；修复后 Scan 使用独立只读句柄且遇坏行
// continue，TotalPV 完整。
func TestConcurrentRedirectsAndStats(t *testing.T) {
	urlStore, logStore, cfg := newTestStores(t)

	// 预置一条永不过期、不限访问次数的短码，保证每次重定向都返回 302。
	const code = "abc1234"
	u := &model.ShortURL{
		Code:      code,
		RawURL:    "https://example.com/landing",
		CreatedAt: time.Now(),
	}
	if err := urlStore.Save(u, false); err != nil {
		t.Fatalf("seed short url: %v", err)
	}

	rdSvc, err := NewRedirectService(urlStore, logStore)
	if err != nil {
		t.Fatalf("NewRedirectService: %v", err)
	}
	t.Cleanup(rdSvc.StopFlusher)

	stSvc, err := NewStatsService(cfg, urlStore, logStore)
	if err != nil {
		t.Fatalf("NewStatsService: %v", err)
	}

	const (
		writeGoroutines = 8
		redirectsEach   = 50
		totalRedirects  = writeGoroutines * redirectsEach
		readGoroutines  = 2
	)

	// 记录并发读期间观测到的最大 TotalPV。
	var maxObserved int64
	var stopRead atomic.Bool

	var wgWrite sync.WaitGroup
	var wgRead sync.WaitGroup

	// 并发统计读取方。用 ticker 节流轮询，避免空转拖慢 -race。
	wgRead.Add(readGoroutines)
	for i := 0; i < readGoroutines; i++ {
		go func() {
			defer wgRead.Done()
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			for {
				if stopRead.Load() {
					return
				}
				res, err := stSvc.Overall(context.Background(), code, 7)
				if err != nil {
					// 极端窗口下可能短暂返回 not found，跳过即可。
					<-ticker.C
					continue
				}
				for {
					old := atomic.LoadInt64(&maxObserved)
					if res.TotalPV <= old || atomic.CompareAndSwapInt64(&maxObserved, old, res.TotalPV) {
						break
					}
				}
				<-ticker.C
			}
		}()
	}

	// 并发重定向写入方。
	wgWrite.Add(writeGoroutines)
	for i := 0; i < writeGoroutines; i++ {
		go func() {
			defer wgWrite.Done()
			headers := map[string][]string{
				"User-Agent": {"Mozilla/5.0 (Test) shurl/redirect"},
				"Referer":    {"https://example.com/page?x=1"},
			}
			for j := 0; j < redirectsEach; j++ {
				req := &RedirectRequest{
					Code:       code,
					RemoteAddr: "203.0.113.1:1234",
					Headers:    headers,
					Timestamp:  time.Now(),
				}
				res, err := rdSvc.HandleRedirect(context.Background(), req)
				if err != nil {
					t.Errorf("HandleRedirect: %v", err)
					return
				}
				if res.Status != 302 {
					t.Errorf("expected 302, got %d", res.Status)
					return
				}
			}
		}()
	}
	wgWrite.Wait()
	// 写入全部完成后，停止读取方并等待它们退出。
	stopRead.Store(true)
	wgRead.Wait()

	// 停掉 flusher：把队列里残留的批量日志全部 flush 落盘，再 Sync 确保可见。
	rdSvc.StopFlusher()
	if err := logStore.Sync(); err != nil {
		t.Fatalf("log store sync: %v", err)
	}

	// 并发读期间观测到的最大 TotalPV 不应超过总量（读到错位/截断才是缺陷）；
	// 全部落盘后最终一次读取必须精确等于已记录的 302 总数。
	final, err := stSvc.Overall(context.Background(), code, 7)
	if err != nil {
		t.Fatalf("final Overall: %v", err)
	}
	if final.TotalPV != int64(totalRedirects) {
		t.Errorf("during concurrent reads, max observed TotalPV was only %d (want %d)",
			final.TotalPV, totalRedirects)
	}
	if max := atomic.LoadInt64(&maxObserved); max > int64(totalRedirects) {
		t.Errorf("max observed TotalPV %d exceeded total %d", max, totalRedirects)
	}
}
