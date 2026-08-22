package shurl_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/service"
	"shurl/internal/store"
)

func buildTmpConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Storage.URLFilePath = filepath.Join(dir, "urls.json")
	cfg.Storage.LogFilePath = filepath.Join(dir, "access.log")
	cfg.Storage.SyncInterval = 0
	cfg.Storage.FlushOnWrite = false
	return cfg
}

func mustSetupServices(t *testing.T) (*service.URLService, *service.RedirectService, *store.URLStore) {
	t.Helper()
	cfg := buildTmpConfig(t)
	us, err := store.NewURLStore(cfg)
	if err != nil {
		t.Fatalf("new url store err %v", err)
	}
	if err := us.Load(context.Background()); err != nil {
		t.Fatalf("load url store err %v", err)
	}
	ls, err := store.NewAccessLogStore(cfg)
	if err != nil {
		t.Fatalf("new access store err %v", err)
	}
	if err := ls.Open(context.Background()); err != nil {
		t.Fatalf("open access store err %v", err)
	}
	t.Cleanup(func() {
		_ = us.Close()
		_ = ls.Close()
	})
	svc, err := service.NewURLService(cfg, us)
	if err != nil {
		t.Fatalf("new url service err %v", err)
	}
	rd, err := service.NewRedirectService(us, ls)
	if err != nil {
		t.Fatalf("new redirect service err %v", err)
	}
	return svc, rd, us
}

// 断言：底层存储触发 panic 时，Create 必须返回 error（不能返回 nil error），
// 且不能把任何无意义的短码写入存储。
func TestPanicPropagation_CreateShouldReturnError(t *testing.T) {
	svc, _, us := mustSetupServices(t)

	// 通过 SetPanicGuard 设置：自定义短码以 "PK-" 开头时 Save 触发 panic。
	us.SetPanicGuard(func(code, rawURL string) bool {
		return strings.HasPrefix(code, "PK-")
	})

	req := &model.CreateReq{
		RawURL:     "https://example.com/panic-test-1",
		CustomCode: "PK-999panic",
		MaxVisits:  10,
	}

	created, err := svc.Create(context.Background(), req)

	// 预期：底层 panic 应该向上传播为 error，err != nil。
	propagateOK := err != nil

	// 预期：存储中不应该出现任何 FALLBACK / COERCED 前缀的垃圾短码。
	snapshot := us.RawSnapshot()
	polluted := false
	for code := range snapshot {
		if strings.HasPrefix(code, "FALLBACK-") ||
			strings.HasPrefix(code, "COERCED-") ||
			strings.HasPrefix(code, "FB-") {
			polluted = true
			break
		}
	}

	// 预期：即使 created != nil，其 code 也必须等于用户请求的自定义短码。
	returnMatch := true
	if created != nil {
		returnMatch = (created.Code == req.CustomCode)
	}

	red := !propagateOK || polluted || !returnMatch

	if red {
		fmt.Println("RED（红灯，缺陷未修复）")
		t.Errorf("expect panic->error propagation, got err=%v, created.Code=%q, polluted=%v",
			err, safeCode(created), polluted)
		t.Logf("snapshot keys: %v", snapshotKeys(snapshot))
		return
	}
	fmt.Println("GREEN（绿灯，缺陷已修复）")
}

// 断言：对已存在的有效短码，若解析短链元信息时底层发生 panic，
// HandleRedirect 必须返回 error，不能捏造一个 302 重定向结果。
func TestPanicPropagation_RedirectShouldReturnError(t *testing.T) {
	svc, rd, us := mustSetupServices(t)

	// 先创建一条正常的、code 为 "SHTEST01" 的短链记录。
	okRec := &model.ShortURL{
		Code:      "SHTEST01",
		RawURL:    "https://real-destination.example.com/home",
		CreatedAt: time.Now(),
		Visits:    0,
		Custom:    true,
		Disabled:  false,
	}
	if err := us.Save(okRec, false); err != nil {
		t.Fatalf("seed record save err %v", err)
	}
	_ = svc

	// 带计数的 panicGuard：仅在第一次命中 code 且 rawURL 为空时触发
	var mu sync.Mutex
	count := 0
	us.SetPanicGuard(func(code, rawURL string) bool {
		if code != "SHTEST01" {
			return false
		}
		if rawURL != "" {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		if count == 0 {
			count++
			return true
		}
		return false
	})

	res, err := rd.HandleRedirect(context.Background(), &service.RedirectRequest{
		Code:      "SHTEST01",
		Timestamp: time.Now(),
	})

	// 预期：因为底层 resolve 出现了 panic，重定向必须返回 error，且 Status != 302。
	hasError := err != nil
	statusOK := true
	if res != nil {
		statusOK = (res.Status != 302)
	}

	// 跳转目标如果是 panic.invalid 说明是系统合成的假结果，也视为异常。
	synthRedirect := false
	if res != nil && strings.Contains(res.RawURL, "panic.invalid") {
		synthRedirect = true
	}

	// 真正的存储记录 SHTEST01 仍指向真实 RawURL，不应被污染。
	saved, _ := us.Get("SHTEST01")
	savedTainted := false
	if saved != nil && strings.Contains(saved.RawURL, "panic.invalid") {
		savedTainted = true
	}

	red := !hasError || !statusOK || synthRedirect || savedTainted

	if red {
		fmt.Println("RED（红灯，缺陷未修复）")
		t.Errorf("expect redirect error on storage panic, got err=%v status=%v rawURL=%q, storedRawURL=%q",
			err, safeStatus(res), safeRawURL(res), safeShortURLRaw(saved))
		return
	}
	fmt.Println("GREEN（绿灯，缺陷已修复）")
}

// 断言：调用方收到的 (val, err) 在 panic 场景下 err 必须非空，
// 即 singleflight 层的错误传播必须保持语义一致。
func TestPanicPropagation_SingleflightReturnSemantics(t *testing.T) {
	svc, _, us := mustSetupServices(t)

	us.SetPanicGuard(func(code, rawURL string) bool {
		return strings.HasSuffix(rawURL, "#STORE-PANIC")
	})

	req := &model.CreateReq{
		RawURL: "https://example.com/raw#STORE-PANIC",
	}

	// 重试 3 次，确保 singleflight 的去重 key 稳定命中。
	for i := 0; i < 3; i++ {
		_, err := svc.Create(context.Background(), req)
		if err == nil {
			fmt.Println("RED（红灯，缺陷未修复）")
			t.Errorf("iter %d: expect non-nil error because storage panicked, got nil", i)
			t.Logf("snapshot after iter %d keys=%v", i, snapshotKeys(us.RawSnapshot()))
			return
		}
	}

	// 存储中也不应该残留任何垃圾短码。
	snap := us.RawSnapshot()
	for code := range snap {
		if strings.HasPrefix(code, "FALLBACK-") ||
			strings.HasPrefix(code, "COERCED-") ||
			strings.HasPrefix(code, "FB-") {
			fmt.Println("RED（红灯，缺陷未修复）")
			t.Errorf("found polluted fallback record %q after repeated panic calls", code)
			return
		}
	}

	fmt.Println("GREEN（绿灯，缺陷已修复）")
}

// 断言：非 panic 的正常 Create / Redirect 流程保持回归正确。
func TestPanicPropagation_RegressionNormalFlow(t *testing.T) {
	svc, rd, _ := mustSetupServices(t)

	req := &model.CreateReq{
		RawURL:     "https://normal.example.com/page",
		CustomCode: "NORMAL01",
		MaxVisits:  100,
	}
	created, err := svc.Create(context.Background(), req)
	if err != nil {
		fmt.Println("RED（红灯，缺陷未修复）")
		t.Fatalf("normal create err %v", err)
		return
	}
	if created == nil || created.Code != "NORMAL01" || created.RawURL != req.RawURL {
		fmt.Println("RED（红灯，缺陷未修复）")
		t.Fatalf("normal create unexpected result %+v", created)
		return
	}

	res, rErr := rd.HandleRedirect(context.Background(), &service.RedirectRequest{
		Code:      "NORMAL01",
		Timestamp: time.Now(),
	})
	if rErr != nil {
		fmt.Println("RED（红灯，缺陷未修复）")
		t.Fatalf("normal redirect err %v", rErr)
		return
	}
	if res == nil || res.Status != 302 || res.RawURL != "https://normal.example.com/page" {
		fmt.Println("RED（红灯，缺陷未修复）")
		t.Fatalf("normal redirect unexpected result %+v", res)
		return
	}

	fmt.Println("GREEN（绿灯，缺陷已修复）")
}

// TestRedGreen 总入口：依次跑上面的 4 个子检查
func TestRedGreen(t *testing.T) {
	cases := []struct {
		name string
		fn   func(t *testing.T)
	}{
		{"CreateShouldReturnError", TestPanicPropagation_CreateShouldReturnError},
		{"RedirectShouldReturnError", TestPanicPropagation_RedirectShouldReturnError},
		{"SingleflightReturnSemantics", TestPanicPropagation_SingleflightReturnSemantics},
		{"RegressionNormalFlow", TestPanicPropagation_RegressionNormalFlow},
	}
	failed := 0
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					failed++
					fmt.Println("RED（红灯，缺陷未修复）")
					t.Errorf("case %s panicked: %v", c.name, r)
				}
			}()
			sub := &testing.T{}
			c.fn(sub)
			if sub.Failed() {
				failed++
				t.Errorf("case %s failed", c.name)
			}
		})
	}
	if failed > 0 {
		fmt.Printf("RED（红灯，缺陷未修复） — %d/%d 子检查未通过\n", failed, len(cases))
		t.Fatalf("%d/%d 子检查未通过", failed, len(cases))
		return
	}
	fmt.Printf("GREEN（绿灯，缺陷已修复） — %d/%d 子检查通过\n", len(cases), len(cases))
}

func safeCode(u *model.ShortURL) string {
	if u == nil {
		return "<nil>"
	}
	return u.Code
}

func safeStatus(r *service.RedirectResult) string {
	if r == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%d", r.Status)
}

func safeRawURL(r *service.RedirectResult) string {
	if r == nil {
		return "<nil>"
	}
	return r.RawURL
}

func safeShortURLRaw(u *model.ShortURL) string {
	if u == nil {
		return "<nil>"
	}
	return u.RawURL
}

func snapshotKeys(m map[string]model.ShortURL) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
