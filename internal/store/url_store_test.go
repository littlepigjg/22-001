package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
)

// newTestStore 构造一个写空文件、关闭后台 syncer 的 URLStore，
// 避免后台 goroutine 泄漏（SyncInterval=0 -> startSyncerLocked 为空操作）。
func newTestStore(t *testing.T) *URLStore {
	t.Helper()
	cfg := config.Default()
	cfg.Storage.URLFilePath = filepath.Join(t.TempDir(), "urls.json")
	cfg.Storage.SyncInterval = 0
	cfg.Storage.FlushOnWrite = false
	st, err := NewURLStore(cfg)
	if err != nil {
		t.Fatalf("NewURLStore: %v", err)
	}
	if err := st.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func mustSave(t *testing.T, st *URLStore, code string) *model.ShortURL {
	t.Helper()
	u := &model.ShortURL{
		Code:      code,
		RawURL:    "https://example.com/" + code,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Remark:    "orig",
		MaxVisits: 0,
	}
	if err := st.Save(u, false); err != nil {
		t.Fatalf("Save(%s): %v", code, err)
	}
	return u
}

// TestGetReturnsClone 校验读路径返回的是值拷贝：修改返回值不影响 store 内部状态。
func TestGetReturnsClone(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	mustSave(t, st, "abc123")

	got, err := st.Get("abc123")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.Remark = "mutated-by-caller"
	got.Visits = 999

	again, err := st.Get("abc123")
	if err != nil {
		t.Fatalf("Get(2): %v", err)
	}
	if again.Remark != "orig" {
		t.Fatalf("stored Remark changed to %q (clone leaked)", again.Remark)
	}
	if again.Visits != 0 {
		t.Fatalf("stored Visits changed to %d (clone leaked)", again.Visits)
	}
}

// TestUpdateReturnsClone 校验 Update 返回值同样是值拷贝。
func TestUpdateReturnsClone(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	mustSave(t, st, "abc123")

	got, err := st.Update("abc123", func(u *model.ShortURL) {
		u.Remark = "updated"
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	got.Remark = "caller-side-mutation"
	got.Visits = 4242

	again, err := st.Get("abc123")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if again.Remark != "updated" {
		t.Fatalf("stored Remark = %q, want \"updated\"", again.Remark)
	}
	if again.Visits != 0 {
		t.Fatalf("stored Visits = %d, want 0", again.Visits)
	}
}

// TestIncrementVisits_ConcurrentExactCount 并发自增计数应得到精确总数。
func TestIncrementVisits_ConcurrentExactCount(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	mustSave(t, st, "code1")

	const n = 200
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := st.IncrementVisits("code1"); err != nil {
				t.Errorf("IncrementVisits: %v", err)
			}
		}()
	}
	wg.Wait()

	got, err := st.Get("code1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Visits != n {
		t.Fatalf("Visits = %d, want %d", got.Visits, n)
	}
	if got.Code != "code1" {
		t.Fatalf("Code = %q, want \"code1\"", got.Code)
	}
}

// TestMixedOps_NoRaceNoPanic 并发混合：自增 / 新建 / 删除 / 批量查。
// 在 -race 下跑到结尾即通过（无 panic、无 data race）。
func TestMixedOps_NoRaceNoPanic(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)

	// 预置 20 个短码。
	for i := 0; i < 20; i++ {
		mustSave(t, st, fmt.Sprintf("seed%02d", i))
	}

	const n = 400
	var wg sync.WaitGroup
	wg.Add(n * 4)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			_, _ = st.IncrementVisits("seed00") // 自增已存在
		}()
		go func() {
			defer wg.Done()
			code := "fresh" + strconv.Itoa(i)
			_ = st.Save(&model.ShortURL{
				Code:      code,
				RawURL:    "https://example.com/" + code,
				CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			}, false)
		}()
		go func() {
			defer wg.Done()
			if i < 10 {
				_ = st.Delete("seed" + fmt.Sprintf("%02d", i)) // 删除部分种子
			}
		}()
		go func() {
			defer wg.Done()
			_, _ = st.GetMulti([]string{"seed00", "seed19", "nonexist", "fresh" + strconv.Itoa(i)})
		}()
		_ = i
	}
	wg.Wait()

	// 只断言一个不变式：Count 不 panic、与内部一致。
	_ = st.Count()
}

// TestUpdate_ConcurrentFieldWrites_NoRace 并发写不同字段，Code 永不变空、Visits 不被改。
func TestUpdate_ConcurrentFieldWrites_NoRace(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	mustSave(t, st, "c0dE12")

	const n = 300
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			_, err := st.Update("c0dE12", func(u *model.ShortURL) {
				u.Remark = "r" + strconv.Itoa(i%7)
				u.MaxVisits++
			})
			if err != nil {
				t.Errorf("Update: %v", err)
			}
		}()
	}
	wg.Wait()

	got, err := st.Get("c0dE12")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Code != "c0dE12" {
		t.Fatalf("Code = %q, want \"c0dE12\"", got.Code)
	}
	if got.Visits != 0 {
		t.Fatalf("Visits = %d, want 0 (mutator never touches Visits)", got.Visits)
	}
	if got.MaxVisits != n {
		t.Fatalf("MaxVisits = %d, want %d", got.MaxVisits, n)
	}
}

// TestBulkStatsReport_ReadOnly 统计接口只读：并发自增时，每条返回 Code 非空、Visits 非负。
func TestBulkStatsReport_ReadOnly(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	mustSave(t, st, "aaa")
	// 另置一条已超限的记录用于覆盖统计分支。
	over := &model.ShortURL{
		Code:      "bbb",
		RawURL:    "https://example.com/bbb",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		MaxVisits: 5,
		Visits:    100,
	}
	if err := st.Save(over, false); err != nil {
		t.Fatalf("Save bbb: %v", err)
	}

	codes := []string{"aaa", "bbb", "missing"}
	const n = 200
	var wg sync.WaitGroup
	wg.Add(n * 2)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			entries := st.BulkStatsReport(codes)
			for _, e := range entries {
				if e.Code == "" {
					t.Errorf("BulkStatsReport returned empty Code")
				}
				if e.Visits < 0 {
					t.Errorf("BulkStatsReport returned Visits=%d < 0 for %q", e.Visits, e.Code)
				}
			}
		}()
		go func() {
			defer wg.Done()
			_, _ = st.IncrementVisits("aaa")
		}()
	}
	wg.Wait()

	// 校验 store 内部记录未被统计接口篡改：Code 仍非空、Visits 仍非负。
	for _, c := range []string{"aaa", "bbb"} {
		u, err := st.Get(c)
		if err != nil {
			t.Fatalf("Get(%s): %v", c, err)
		}
		if u.Code == "" {
			t.Fatalf("stored %q Code became empty (BulkStatsReport mutated it?)", c)
		}
		if u.Visits < 0 {
			t.Fatalf("stored %q Visits=%d < 0", c, u.Visits)
		}
	}
}

// TestUpdate_NonExistent_ReturnsNotFound Update 不存在的 code 应返回 ErrCodeNotFound。
func TestUpdate_NonExistent_ReturnsNotFound(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	_, err := st.Update("nope", func(u *model.ShortURL) { u.Remark = "x" })
	if !errors.Is(err, model.ErrCodeNotFound) {
		t.Fatalf("err = %v, want ErrCodeNotFound", err)
	}
}

// TestUpdate_NilMutator 校验 nil mutator 的防御。
func TestUpdate_NilMutator(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	mustSave(t, st, "abc123")
	_, err := st.Update("abc123", nil)
	if err == nil {
		t.Fatalf("Update with nil mutator should error")
	}
}

// TestSave_OverwriteSemantics 覆盖语义：重复 Save(false) 冲突；Save(true) 成功。
func TestSave_OverwriteSemantics(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	mustSave(t, st, "dup")

	// 再以 overwrite=false 存同一 code -> 冲突。
	if err := st.Save(&model.ShortURL{Code: "dup", RawURL: "https://x", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}, false); !errors.Is(err, model.ErrCodeConflict) {
		t.Fatalf("second Save(false) err = %v, want ErrCodeConflict", err)
	}

	// overwrite=true 成功，且存储的是新值。
	v2 := &model.ShortURL{Code: "dup", RawURL: "https://new", CreatedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Remark: "v2"}
	if err := st.Save(v2, true); err != nil {
		t.Fatalf("Save(true): %v", err)
	}
	got, err := st.Get("dup")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.RawURL != "https://new" || got.Remark != "v2" {
		t.Fatalf("stored = %+v, want overwritten v2", got)
	}
	// 改 v2 不应影响 store（克隆隔离）。
	v2.Remark = "caller-mutation"
	again, _ := st.Get("dup")
	if again.Remark != "v2" {
		t.Fatalf("stored Remark changed to %q (clone leaked)", again.Remark)
	}
}

// TestDeleteClearsRecord 删除后再 Get 应找不到（验证 fastCache 移除后 Delete 完整）。
func TestDeleteClearsRecord(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	mustSave(t, st, "gone")
	if err := st.Delete("gone"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.Get("gone"); !errors.Is(err, model.ErrCodeNotFound) {
		t.Fatalf("Get after Delete err = %v, want ErrCodeNotFound", err)
	}
}
