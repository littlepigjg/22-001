package service

import (
	"context"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/store"
)

// newTestService 构造一个 URLService + URLStore（禁用后台 syncer，避免 goroutine 泄漏）。
func newTestService(t *testing.T) (*URLService, *store.URLStore) {
	t.Helper()
	cfg := config.Default()
	cfg.Storage.URLFilePath = filepath.Join(t.TempDir(), "urls.json")
	cfg.Storage.SyncInterval = 0
	cfg.Storage.FlushOnWrite = false
	st, err := store.NewURLStore(cfg)
	if err != nil {
		t.Fatalf("NewURLStore: %v", err)
	}
	if err := st.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc, err := NewURLService(cfg, st)
	if err != nil {
		t.Fatalf("NewURLService: %v", err)
	}
	return svc, st
}

func mustCreate(t *testing.T, svc *URLService, code string) *model.ShortURL {
	t.Helper()
	u, err := svc.Create(context.Background(), &model.CreateReq{
		RawURL:     "https://example.com/" + code,
		CustomCode: code,
		Remark:     "orig",
	})
	if err != nil {
		t.Fatalf("Create(%s): %v", code, err)
	}
	return u
}

// TestConcurrentWorkout_NoDirtyData 压测核心场景：并发计数 + 周期改备注。
// 不应出现 Visits 负数、Code 为空。
func TestConcurrentWorkout_NoDirtyData(t *testing.T) {
	t.Parallel()
	svc, st := newTestService(t)
	mustCreate(t, svc, "wk001")

	if err := svc.ConcurrentWorkout(context.Background(), ConcurrentWorkoutCfg{
		Code:       "wk001",
		VisitN:     200,
		DisableGap: 10,
	}); err != nil {
		t.Fatalf("ConcurrentWorkout: %v", err)
	}

	got, err := st.Get("wk001")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Visits != 200 {
		t.Fatalf("Visits = %d, want 200 (no decrement allowed)", got.Visits)
	}
	if got.Code != "wk001" {
		t.Fatalf("Code = %q, want \"wk001\" (never cleared)", got.Code)
	}
	if got.Code == "" {
		t.Fatal("Code is empty")
	}
}

// TestBatchDisable_Concurrent_NoNegativeVisits_NoEmptyCode 批量禁用后每条记录：
// Disabled==true、Code 非空、Visits 非负。
func TestBatchDisable_Concurrent_NoNegativeVisits_NoEmptyCode(t *testing.T) {
	t.Parallel()
	svc, st := newTestService(t)

	const m = 12
	codes := make([]string, m)
	for i := range codes {
		codes[i] = "bd" + strconv.Itoa(i)
		mustCreate(t, svc, codes[i])
	}

	// 多个 goroutine 同时触发 BatchDisable（重复压力）。
	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = svc.BatchDisable(context.Background(), codes)
		}()
	}
	wg.Wait()

	for _, c := range codes {
		u, err := st.Get(c)
		if err != nil {
			t.Fatalf("Get(%s): %v", c, err)
		}
		if !u.Disabled {
			t.Errorf("code %q Disabled = false, want true", c)
		}
		if u.Code != c {
			t.Errorf("code %q field Code = %q, want %q", c, u.Code, c)
		}
		if u.Visits < 0 {
			t.Errorf("code %q Visits = %d < 0", c, u.Visits)
		}
	}
}

// TestBatchUpdateRemark_Concurrent_StableCode 批量改备注：Code 不变空、Visits 不递减。
func TestBatchUpdateRemark_Concurrent_StableCode(t *testing.T) {
	t.Parallel()
	svc, st := newTestService(t)

	const m = 10
	codes := make([]string, m)
	for i := range codes {
		codes[i] = "rm" + strconv.Itoa(i)
		mustCreate(t, svc, codes[i])
	}

	// 构造 remarks：每个 code 一个备注。多次并发以制造 last-writer-wins。
	remarks := make(map[string]string, m)
	for _, c := range codes {
		remarks[c] = "remark-" + c
	}

	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = svc.BatchUpdateRemark(context.Background(), "", remarks)
		}()
	}
	wg.Wait()

	for _, c := range codes {
		u, err := st.Get(c)
		if err != nil {
			t.Fatalf("Get(%s): %v", c, err)
		}
		if u.Code != c {
			t.Errorf("code %q field Code = %q, want %q", c, u.Code, c)
		}
		if u.Visits < 0 {
			t.Errorf("code %q Visits = %d < 0", c, u.Visits)
		}
		if u.Remark == "" || u.Remark == "orig" {
			t.Errorf("code %q Remark = %q, want updated value", c, u.Remark)
		}
	}
}

// TestDisable_UpdateRemark_RaceFree 并发混合 Disable + UpdateRemark + IncrementVisits。
func TestDisable_UpdateRemark_RaceFree(t *testing.T) {
	t.Parallel()
	svc, st := newTestService(t)
	mustCreate(t, svc, "mix01")

	const n = 150
	var wg sync.WaitGroup
	wg.Add(n * 3)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = svc.Disable(context.Background(), "mix01")
		}()
		go func() {
			defer wg.Done()
			_ = svc.UpdateRemark(context.Background(), "mix01", "hello")
		}()
		go func() {
			defer wg.Done()
			_, _ = st.IncrementVisits("mix01")
		}()
	}
	wg.Wait()

	u, err := st.Get("mix01")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if u.Code != "mix01" {
		t.Fatalf("Code = %q, want \"mix01\"", u.Code)
	}
	if u.Visits < 0 {
		t.Fatalf("Visits = %d < 0", u.Visits)
	}
}

// TestUpdateRemark_DoesNotDecrementVisits 单次改备注应令 Visits +1（且 Code 前缀来自 code）。
func TestUpdateRemark_DoesNotDecrementVisits(t *testing.T) {
	t.Parallel()
	svc, st := newTestService(t)
	// 用一个长度 > 6 的 code 以触发 Code 前缀拼接分支。
	mustCreate(t, svc, "longcode1")

	if err := svc.UpdateRemark(context.Background(), "longcode1", "hello"); err != nil {
		t.Fatalf("UpdateRemark: %v", err)
	}
	u, err := st.Get("longcode1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if u.Visits != 1 {
		t.Fatalf("Visits = %d, want 1 (remark update increments, never decrements)", u.Visits)
	}
	if u.Code != "longcode1" {
		t.Fatalf("Code = %q, want \"longcode1\"", u.Code)
	}
	// Remark 应被加上 code 前缀且包含原始文本。
	if u.Remark == "" {
		t.Fatal("Remark empty after update")
	}
	// longcode1 长度 > 6 -> 前缀为 "[long] hello"。
	if got, want := u.Remark, "[long] hello"; got != want {
		t.Fatalf("Remark = %q, want %q", got, want)
	}
}

// TestHandleRedirect_RaceFree 并发重定向：Visits 落在 [0,N]、Code 不变空、不 panic。
func TestHandleRedirect_RaceFree(t *testing.T) {
	t.Parallel()
	svc, st := newTestService(t)
	// 用自定义 code 创建一条记录，MaxVisits 留 0（不限），避免提前 410。
	mustCreate(t, svc, "rd001")

	// 构造 AccessLogStore（落临时文件）。
	cfg := config.Default()
	cfg.Storage.LogFilePath = filepath.Join(t.TempDir(), "access.log")
	cfg.Storage.SyncInterval = 0
	ls, err := store.NewAccessLogStore(cfg)
	if err != nil {
		t.Fatalf("NewAccessLogStore: %v", err)
	}
	if err := ls.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	rs, err := NewRedirectService(st, ls)
	if err != nil {
		t.Fatalf("NewRedirectService: %v", err)
	}

	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _ = rs.HandleRedirect(context.Background(), &RedirectRequest{
				Code:       "rd001",
				RemoteAddr: "127.0.0.1:1234",
				Headers:    map[string][]string{"User-Agent": {"test/1.0"}},
				Timestamp:  time.Now(),
			})
		}()
	}
	wg.Wait()

	u, err := st.Get("rd001")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if u.Code != "rd001" {
		t.Fatalf("Code = %q, want \"rd001\"", u.Code)
	}
	if u.Visits < 0 || u.Visits > n {
		t.Fatalf("Visits = %d, want in [0, %d]", u.Visits, n)
	}
}
