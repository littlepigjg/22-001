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

// newTestStores 构造一组指向临时目录的真实 store，供并发测试使用。
// SyncInterval=0 关闭后台 syncer，FlushOnWrite=false 避免每次写落盘。
func newTestStores(t *testing.T) (*store.URLStore, *store.AccessLogStore) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Storage.URLFilePath = dir + "/urls.json"
	cfg.Storage.LogFilePath = dir + "/access.log"
	cfg.Storage.SyncInterval = 0
	cfg.Storage.FlushOnWrite = false
	cfg.Janitor.Enabled = false

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
	t.Cleanup(func() {
		_ = urlStore.Close()
		_ = logStore.Close()
	})
	return urlStore, logStore
}

// TestRedirectConcurrent_NoRace 并发压测同一短码的重定向 + 快照 + 改备注，
// 修复前应稳定复现 go test -race 的 DATA RACE（IncrementVisits 写 vs HandleRedirect/SnapshotCached 读）
// 及字段撕裂 / 偶发 panic；修复后应干净通过。
func TestRedirectConcurrent_NoRace(t *testing.T) {
	urlStore, logStore := newTestStores(t)

	const (
		code    = "racetest"
		rawURL  = "https://example.com/original"
		remark  = "v0"
		workers = 24
		iters   = 120
	)
	if err := urlStore.Save(&model.ShortURL{
		Code:      code,
		RawURL:    rawURL,
		CreatedAt: time.Now(),
		Remark:    remark,
		MaxVisits: 0, // 不限；主路径恒为 302 直到被并发写改状态
	}, false); err != nil {
		t.Fatalf("seed url: %v", err)
	}

	rdSvc, err := NewRedirectService(urlStore, logStore)
	if err != nil {
		t.Fatalf("new redirect service: %v", err)
	}
	urlSvc, err := NewURLService(config.Default(), urlStore)
	if err != nil {
		t.Fatalf("new url service: %v", err)
	}
	// 注入解析层缓存失效，使并发写后重定向能回源拿到最新状态。
	urlSvc.SetInvalidator(func(c string) { rdSvc.Resolver().ObserveInvalidated(c) })
	// 预热 resolver 的布隆过滤器与缓存：登记真实条目。
	// （生产由 Create/启动预热覆盖；测试直接 Save 不经过 resolver，需补登记，
	// 否则 bloom 首次否定会短路成 404。）
	if seed, sErr := urlStore.Get(code); sErr != nil {
		t.Fatalf("seed get: %v", sErr)
	} else {
		rdSvc.Resolver().ObserveCreated(seed)
	}

	var (
		badN   atomic.Int64
		panicN atomic.Int64
	)
	var wg sync.WaitGroup

	// worker：重定向风暴。
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < iters; k++ {
				func() {
					defer func() {
						if r := recover(); r != nil {
							panicN.Add(1)
						}
					}()
					res, rErr := rdSvc.HandleRedirect(context.Background(), &RedirectRequest{
						Code:      code,
						Timestamp: time.Now(),
					})
					if rErr != nil {
						// 仅日志写入失败等可容忍；重定向本身不应报错。
						return
					}
					// 不变量：要么 302 且 RawURL 正确，要么 410。
					switch res.Status {
					case 302:
						if res.RawURL != rawURL {
							badN.Add(1)
							t.Errorf("bad 302: RawURL=%q want=%q", res.RawURL, rawURL)
						}
					case 410:
						if !(res.Disabled || res.Expired || res.MaxVisited) {
							badN.Add(1)
							t.Errorf("bad 410: no reason flag set (disabled=%v expired=%v maxvisited=%v)", res.Disabled, res.Expired, res.MaxVisited)
						}
					default:
						badN.Add(1)
						t.Errorf("bad status=%d (raw=%q disabled=%v expired=%v maxvisited=%v)", res.Status, res.RawURL, res.Disabled, res.Expired, res.MaxVisited)
					}
				}()
			}
		}()
	}

	// worker：SnapshotCached 快照读，与写并发。
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < iters; k++ {
				func() {
					defer func() {
						if r := recover(); r != nil {
							panicN.Add(1)
						}
					}()
					su, sErr := rdSvc.SnapshotCached(code)
					if sErr != nil {
						return
					}
					if su != nil && su.RawURL != rawURL {
						badN.Add(1)
					}
				}()
			}
		}()
	}

	// worker：并发改备注（写路径），与 IncrementVisits 竞争 Remark 字段。
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for k := 0; k < iters; k++ {
				_ = urlSvc.UpdateRemark(context.Background(), code, "remark")
			}
			_ = id
		}(i)
	}

	// worker：并发禁用 + 重置，验证写路径原子性与缓存失效。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for k := 0; k < iters; k++ {
			_ = urlSvc.Disable(context.Background(), code)
		}
	}()

	wg.Wait()

	if panicN.Load() > 0 {
		t.Fatalf("detected %d panic(s) during concurrent redirect", panicN.Load())
	}
	if badN.Load() > 0 {
		t.Fatalf("%d results violated consistency invariants (torn RawURL/status)", badN.Load())
	}
}

// TestIncrementVisitsConcurrent_StoreLevel 直接压 store 层：
// IncrementVisits 写 vs Get 读，验证返回拷贝后无 DATA RACE 且计数不丢。
func TestIncrementVisitsConcurrent_StoreLevel(t *testing.T) {
	urlStore, _ := newTestStores(t)
	const code = "svc1"
	if err := urlStore.Save(&model.ShortURL{
		Code:   code,
		RawURL: "https://example.com",
		Remark: "r0",
	}, false); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const writers, readers = 16, 16
	const iters = 200
	var wg sync.WaitGroup

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < iters; k++ {
				u, err := urlStore.IncrementVisits(code)
				if err != nil || u == nil {
					t.Errorf("increment: err=%v u=%v", err, u)
					return
				}
				_ = u.Visits // 读返回拷贝的字段，与写不再竞争
				_ = u.Remark
			}
		}()
	}
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < iters; k++ {
				if u, err := urlStore.Get(code); err == nil && u != nil {
					_ = u.Visits
					_ = u.Remark
					_ = u.ExpireAt
				}
			}
		}()
	}
	wg.Wait()

	// 计数不应丢失：16 * 200 = 3200 次自增（IncrementVisits 自身不阻断于 MaxVisits）。
	final, err := urlStore.Get(code)
	if err != nil {
		t.Fatalf("final get: %v", err)
	}
	if final.Visits != 3200 {
		t.Fatalf("lost updates: visits=%d want=3200", final.Visits)
	}
}
