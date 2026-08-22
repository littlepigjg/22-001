package service_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/service"
	"shurl/internal/store"
	"shurl/pkg/cache"
	"shurl/pkg/safemap"
)

// newFixture 构造一套跑在临时目录里的 URLStore + AccessLogStore + StatsService +
// URLService，供并发压测使用。返回清理函数。
func newFixture(t *testing.T, logLines int) (*service.URLService, *service.StatsService, func()) {
	t.Helper()
	dir := t.TempDir()
	urlFile := filepath.Join(dir, "urls.json")
	logFile := filepath.Join(dir, "access.log")

	cfg := config.Default()
	cfg.Storage.URLFilePath = urlFile
	cfg.Storage.LogFilePath = logFile
	cfg.Storage.SyncInterval = 0 // 测试中关掉后台 syncer，避免与清理函数竞争
	cfg.Storage.FlushOnWrite = false
	cfg.Stats.CacheTTL = 50 * time.Millisecond
	cfg.Stats.MaxRecords = 100000

	urlStore, err := store.NewURLStore(cfg)
	if err != nil {
		t.Fatalf("new url store: %v", err)
	}
	if err := urlStore.Load(context.Background()); err != nil {
		t.Fatalf("load url store: %v", err)
	}
	logStore, err := store.NewAccessLogStore(cfg)
	if err != nil {
		t.Fatalf("new log store: %v", err)
	}
	if err := logStore.Open(context.Background()); err != nil {
		t.Fatalf("open log store: %v", err)
	}

	// 预置一批访问日志，让 Overall 的扫描有活干、超时才有意义。
	seedLogs(t, logStore, logLines)

	urlSvc, err := service.NewURLService(cfg, urlStore)
	if err != nil {
		t.Fatalf("new url service: %v", err)
	}
	statsSvc, err := service.NewStatsService(cfg, urlStore, logStore)
	if err != nil {
		t.Fatalf("new stats service: %v", err)
	}

	cleanup := func() {
		_ = logStore.Close()
		_ = urlStore.Close()
	}
	return urlSvc, statsSvc, cleanup
}

func seedLogs(t *testing.T, ls *store.AccessLogStore, n int) {
	t.Helper()
	if n <= 0 {
		return
	}
	now := time.Now()
	batch := make([]*model.AccessLog, 0, 256)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := ls.AppendMany(batch); err != nil {
			t.Fatalf("append logs: %v", err)
		}
		batch = batch[:0]
	}
	for i := 0; i < n; i++ {
		batch = append(batch, &model.AccessLog{
			ID:        fmt.Sprintf("log-%d", i),
			Code:      "racecode",
			IP:        fmt.Sprintf("10.0.%d.%d", (i/256)%256, i%256),
			Timestamp: now.Add(-time.Duration(i) * time.Second),
			Status:    302,
			OS:        "Linux",
			Browser:   "Chrome",
			Device:    "pc",
			Referer:   "https://example.com/page",
		})
		if len(batch) == 256 {
			flush()
		}
	}
	flush()
}

// TestOverallConcurrentCancel 压测统计接口：多个 goroutine 同时跑 Overall，
// 一部分用很短的 context 超时（模拟中途 cancel），一部分正常完成。
// 目标：-race 下无竞争，且超时能真正打断扫描。
func TestOverallConcurrentCancel(t *testing.T) {
	_, statsSvc, cleanup := newFixture(t, 20000)
	defer cleanup()

	var wg sync.WaitGroup
	const workers = 16
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				// 一半请求给极短超时（基本都会被 cancel 打断），
				// 一半给宽裕超时（正常完成）。两种路径都要安全。
				timeout := 300 * time.Millisecond
				if (i+seed)%2 == 0 {
					timeout = time.Millisecond
				}
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				_, err := statsSvc.Overall(ctx, "racecode", 7)
				cancel()
				_ = err // 取消或成功都可以，只要不 panic / 不 race
			}
		}(w)
	}
	wg.Wait()
}

// TestURLGetUpdateRemarkConcurrent 压测短码元信息接口：一边反复 Get，
// 一边反复 UpdateRemark，同时后台清理缓存。
// 目标：-race 下对共享 ShortURL 指针的字段读写不再竞争。
func TestURLGetUpdateRemarkConcurrent(t *testing.T) {
	urlSvc, _, cleanup := newFixture(t, 0)
	defer cleanup()

	ctx := context.Background()
	if _, err := urlSvc.Create(ctx, &model.CreateReq{
		RawURL: "https://example.org/x", CustomCode: "racecode", Remark: "init",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	var wg sync.WaitGroup
	const readers = 8
	const writers = 8
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				u, err := urlSvc.Get(ctx, "racecode")
				if err != nil {
					t.Errorf("get: %v", err)
					return
				}
				_ = u.Remark // 并发读字段
				_ = u.Visits
				_ = n
			}
		}(r)
	}
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				remark := fmt.Sprintf("r-%d-%d", n, i)
				if _, err := urlSvc.UpdateRemark(ctx, "racecode", remark); err != nil {
					t.Errorf("update remark: %v", err)
					return
				}
			}
		}(w)
	}
	// 后台并发清理 safemap 全局缓存 + LRU 过期清理，模拟压测时的过期清理压力。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 400; i++ {
			safemap.PurgeExpiredGlobal()
			if cache.SharedStats != nil {
				cache.SharedStats.PurgeExpired()
			}
			time.Sleep(time.Microsecond * 100)
		}
	}()
	wg.Wait()
}

// TestLRUPurgeExpiredConcurrent 专门压 LRU 的 PurgeExpired：一边塞过期条目，
// 一边 PurgeExpired，验证不再 nil pointer deref。
func TestLRUPurgeExpiredConcurrent(t *testing.T) {
	lru, err := cache.NewLRU(512)
	if err != nil {
		t.Fatalf("new lru: %v", err)
	}
	var wg sync.WaitGroup
	const producers = 8
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				// 大部分条目带极短 TTL，让 PurgeExpired 有大量目标。
				lru.Set(fmt.Sprintf("k-%d-%d", n, i), i, time.Microsecond*50)
			}
		}(p)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 4000; i++ {
			_ = lru.PurgeExpired()
		}
	}()
	wg.Wait()
}
