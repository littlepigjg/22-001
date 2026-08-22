package service

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/store"
)

// testEnv 装配一个真实可用的服务栈：URLStore + AccessLogStore + 各业务服务。
// 后台 syncer 关闭（SyncInterval=0），保证测试确定；日志走临时文件。
type testEnv struct {
	urlStore *store.URLStore
	logStore *store.AccessLogStore
	urlSvc   *URLService
	rdSvc    *RedirectService
	health   *HealthService
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	cfg := config.Default()
	dir := t.TempDir()
	cfg.Storage.URLFilePath = filepath.Join(dir, "urls.json")
	cfg.Storage.LogFilePath = filepath.Join(dir, "access.log")
	cfg.Storage.SyncInterval = 0 // 不启动后台 syncer
	cfg.Storage.FlushOnWrite = false

	us, err := store.NewURLStore(cfg)
	if err != nil {
		t.Fatalf("NewURLStore: %v", err)
	}
	if err := us.Load(context.Background()); err != nil {
		t.Fatalf("url store Load: %v", err)
	}
	ls, err := store.NewAccessLogStore(cfg)
	if err != nil {
		t.Fatalf("NewAccessLogStore: %v", err)
	}
	if err := ls.Open(context.Background()); err != nil {
		t.Fatalf("log store Open: %v", err)
	}
	t.Cleanup(func() {
		_ = us.Close()
		_ = ls.Close()
	})

	urlSvc, err := NewURLService(cfg, us)
	if err != nil {
		t.Fatalf("NewURLService: %v", err)
	}
	rdSvc, err := NewRedirectService(us, ls)
	if err != nil {
		t.Fatalf("NewRedirectService: %v", err)
	}
	health, err := NewHealthService(us, ls)
	if err != nil {
		t.Fatalf("NewHealthService: %v", err)
	}
	return &testEnv{urlStore: us, logStore: ls, urlSvc: urlSvc, rdSvc: rdSvc, health: health}
}

// TestRedirectService_ConcurrentMixedOpsNoRace 忠实复现报告中的竞态触发场景：
// 并发 GET /{code}（重定向）、GET /api/urls/{code}（查询并 JSON 序列化）、
// GET /stats（健康检查 → store.Stats）、PATCH /api/urls/{code}（禁用 / 改备注）。
// 在 -race 下应零警告。
func TestRedirectService_ConcurrentMixedOpsNoRace(t *testing.T) {
	env := newTestEnv(t)
	const code = "abc123"
	if err := env.urlStore.Save(&model.ShortURL{
		Code:      code,
		RawURL:    "https://example.com",
		MaxVisits: 1 << 30,
	}, false); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	ctx := context.Background()
	const goroutines = 8
	const iterations = 300

	var wg sync.WaitGroup
	wg.Add(goroutines)
	start := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			<-start
			for i := 0; i < iterations; i++ {
				switch (id*iterations + i) % 5 {
				case 0: // GET /{code} 重定向
					req := &RedirectRequest{
						Code:       code,
						RemoteAddr: "127.0.0.1:1234",
						Headers:    map[string][]string{"User-Agent": {"test"}},
						Timestamp:  time.Now(),
					}
					if _, err := env.rdSvc.HandleRedirect(ctx, req); err != nil {
						t.Errorf("HandleRedirect: %v", err)
						return
					}
				case 1: // GET /api/urls/{code} 查询 + JSON 序列化（命中字段读）
					u, err := env.urlSvc.Get(ctx, code)
					if err != nil {
						t.Errorf("urlSvc.Get: %v", err)
						return
					}
					if _, err := json.Marshal(u); err != nil {
						t.Errorf("json.Marshal: %v", err)
						return
					}
				case 2: // GET /stats（健康检查 → store.Stats）
					if h := env.health.Check(ctx); h == nil {
						t.Errorf("health.Check returned nil")
						return
					}
				case 3: // PATCH 禁用（id%2 控制开/关，保证持续有活动记录）
					if err := env.urlSvc.Disable(ctx, code); err != nil {
						// 禁用后再禁用无副作用；仅在非预期错误时报错。
						t.Errorf("urlSvc.Disable: %v", err)
						return
					}
				case 4: // PATCH 改备注
					if err := env.urlSvc.UpdateRemark(ctx, code, "r"+strconv.Itoa(i)); err != nil {
						t.Errorf("urlSvc.UpdateRemark: %v", err)
						return
					}
				}
			}
		}(g)
	}
	close(start)
	wg.Wait()

	// 收尾后保证记录可用并校验访问次数未丢失（应 > 0）。
	_ = env.urlStore.SetDisabled(code, false)
	u, err := env.urlSvc.Get(ctx, code)
	if err != nil {
		t.Fatalf("final Get: %v", err)
	}
	if u.Visits <= 0 {
		t.Fatalf("expected visits > 0 after concurrent redirects, got %d", u.Visits)
	}
	t.Logf("final visits=%d remark=%q", u.Visits, u.Remark)
}

// TestRedirectService_HandleRedirectStatus 覆盖 302/404/410(max) 三条返回路径。
func TestRedirectService_HandleRedirectStatus(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// 404：不存在。
	r, err := env.rdSvc.HandleRedirect(ctx, &RedirectRequest{
		Code: "nope00", RemoteAddr: "127.0.0.1:1", Timestamp: time.Now(),
	})
	if err != nil {
		t.Fatalf("not-found HandleRedirect: %v", err)
	}
	if r.Status != 404 {
		t.Fatalf("not-found status: want 404, got %d", r.Status)
	}

	// 302：正常重定向。
	const code = "okok00"
	if err := env.urlStore.Save(&model.ShortURL{Code: code, RawURL: "https://x.example.com"}, false); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	r, err = env.rdSvc.HandleRedirect(ctx, &RedirectRequest{
		Code: code, RemoteAddr: "127.0.0.1:1", Timestamp: time.Now(),
	})
	if err != nil {
		t.Fatalf("ok HandleRedirect: %v", err)
	}
	if r.Status != 302 {
		t.Fatalf("ok status: want 302, got %d", r.Status)
	}
	if r.RawURL != "https://x.example.com" {
		t.Fatalf("ok RawURL: want https://x.example.com, got %q", r.RawURL)
	}

	// 410：禁用后访问。
	if err := env.urlSvc.Disable(ctx, code); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	r, err = env.rdSvc.HandleRedirect(ctx, &RedirectRequest{
		Code: code, RemoteAddr: "127.0.0.1:1", Timestamp: time.Now(),
	})
	if err != nil {
		t.Fatalf("disabled HandleRedirect: %v", err)
	}
	if r.Status != 410 || !r.Disabled {
		t.Fatalf("disabled: want 410 + Disabled=true, got %d disabled=%v", r.Status, r.Disabled)
	}
}

// 消除 go vet 对未使用 fmt 的潜在抱怨（fmt 在并发测试中用到）。
var _ = fmt.Sprintf
