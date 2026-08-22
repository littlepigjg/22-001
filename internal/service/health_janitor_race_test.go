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

// newTestStores 在临时目录中创建一对已就绪的 URLStore / AccessLogStore。
func newTestStores(t *testing.T) (*store.URLStore, *store.AccessLogStore) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		Storage: config.StorageCfg{
			URLFilePath: dir + "/urls.json",
			LogFilePath: dir + "/access.log",
			SyncInterval: 0, // 关闭后台 syncer，测试不依赖落盘
		},
	}
	us, err := store.NewURLStore(cfg)
	if err != nil {
		t.Fatalf("NewURLStore: %v", err)
	}
	if err := us.Load(context.Background()); err != nil {
		t.Fatalf("url store load: %v", err)
	}
	ls, err := store.NewAccessLogStore(cfg)
	if err != nil {
		t.Fatalf("NewAccessLogStore: %v", err)
	}
	if err := ls.Open(context.Background()); err != nil {
		t.Fatalf("access store open: %v", err)
	}
	t.Cleanup(func() {
		_ = us.Close()
		_ = ls.Close()
	})
	return us, ls
}

func mustSave(t *testing.T, us *store.URLStore, u *model.ShortURL) {
	t.Helper()
	if err := us.Save(u, false); err != nil {
		t.Fatalf("save %s: %v", u.Code, err)
	}
}

// seedLifecycleURLs 写入覆盖各生命周期状态的短链接。
func seedLifecycleURLs(t *testing.T, us *store.URLStore) {
	t.Helper()
	now := time.Now()
	records := []*model.ShortURL{
		{Code: "act01", RawURL: "https://a.example", CreatedAt: now},
		{Code: "max01", RawURL: "https://b.example", CreatedAt: now, MaxVisits: 5, Visits: 6},
		{Code: "exp01", RawURL: "https://c.example", CreatedAt: now.Add(-2 * time.Hour), ExpireAt: now.Add(-1 * time.Hour)},
		{Code: "dis01", RawURL: "https://d.example", CreatedAt: now, Disabled: true},
		{Code: "bom01", RawURL: "https://e.example", CreatedAt: now.Add(-2 * time.Hour), ExpireAt: now.Add(-1 * time.Hour), MaxVisits: 3, Visits: 3},
	}
	for _, u := range records {
		mustSave(t, us, u)
	}
}

// TestHealthJanitorNoDataRace 并发执行 health.Check 与 janitor.RunOnce，
// 在 go test -race 下验证二者不再发生数据竞争，且 Check 的统计口径自洽
// （active+disabled+expired == total）。
func TestHealthJanitorNoDataRace(t *testing.T) {
	us, ls := newTestStores(t)
	seedLifecycleURLs(t, us)

	// 预先播种一批过期链接，保证首轮巡检有写入可做。
	now := time.Now()
	for i := 0; i < 80; i++ {
		u := &model.ShortURL{
			Code:      "seed" + padLeft(itoa(i), 3),
			RawURL:    "https://s.example",
			CreatedAt: now.Add(-time.Hour),
			ExpireAt:  now.Add(-time.Minute),
		}
		mustSave(t, us, u)
	}

	hSvc, err := NewHealthService(us, ls)
	if err != nil {
		t.Fatalf("NewHealthService: %v", err)
	}
	jSvc, err := NewJanitorService(config.Default(), us)
	if err != nil {
		t.Fatalf("NewJanitorService: %v", err)
	}

	var failed atomic.Bool
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 读侧：持续健康检查，校验分桶自洽。
	wg.Add(3) // 2 readers + 1 writer
	for w := 0; w < 2; w++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				res := hSvc.Check(context.Background())
				if got := res.URLActive + res.URLDisabled + res.URLExpired; got != res.URLTotal {
					failed.Store(true)
					t.Errorf("buckets not self-consistent: active+disabled+expired=%d total=%d",
						got, res.URLTotal)
				}
				if res.URLActive < 0 || res.URLDisabled < 0 || res.URLExpired < 0 || res.URLTotal < 0 {
					failed.Store(true)
					t.Errorf("negative bucket: %+v", res)
				}
			}
		}()
	}

	// 写侧：持续触发过期巡检，并每轮重新播种少量过期链接保持有写入。
	var seedCounter atomic.Int64
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			jSvc.RunOnce(1000)
			// 每轮补种 5 条过期链接，保证下一轮巡检仍有候选可写，
			// 从而与读侧形成持续的读/写重叠。
			base := seedCounter.Add(5)
			for i := 0; i < 5; i++ {
				u := &model.ShortURL{
					Code:      "rs" + padLeft(itoa(int(base)-4+i), 6),
					RawURL:     "https://x.example",
					CreatedAt:  now,
					ExpireAt:   time.Now().Add(-time.Second),
				}
				if err := us.Save(u, false); err != nil {
					// 冲突理论上不会发生（计数器单调）；出错则标记失败。
					failed.Store(true)
					t.Errorf("resave: %v", err)
					return
				}
			}
		}
	}()

	time.Sleep(400 * time.Millisecond)
	close(stop)
	wg.Wait()

	if failed.Load() {
		t.FailNow()
	}
}

// TestHealthMatchesStoreStats 验证巡检落定后，Check 的分桶与
// URLStore.Stats() 完全一致——统计口径对齐。
func TestHealthMatchesStoreStats(t *testing.T) {
	us, ls := newTestStores(t)
	seedLifecycleURLs(t, us)

	hSvc, err := NewHealthService(us, ls)
	if err != nil {
		t.Fatalf("NewHealthService: %v", err)
	}
	jSvc, err := NewJanitorService(config.Default(), us)
	if err != nil {
		t.Fatalf("NewJanitorService: %v", err)
	}

	// 巡检一次，让过期/超限记录落定为 disabled。
	jSvc.RunOnce(1000)

	res := hSvc.Check(context.Background())
	sTotal, sActive, sDisabled, sExpired, err := us.Stats()
	if err != nil {
		t.Fatalf("store stats: %v", err)
	}
	if res.URLTotal != sTotal || res.URLActive != sActive ||
		res.URLDisabled != sDisabled || res.URLExpired != sExpired {
		t.Errorf("health != store stats:\n health=%+v\n store(total=%d active=%d disabled=%d expired=%d)",
			res, sTotal, sActive, sDisabled, sExpired)
	}
}

// --- 零依赖的小工具，避免引入 strconv ---

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func padLeft(s string, width int) string {
	if len(s) >= width {
		return s
	}
	b := make([]byte, width)
	for i := range b {
		b[i] = '0'
	}
	copy(b[width-len(s):], s)
	return string(b)
}
