package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/pkg/retry"
)

// newReadyStore 构造一个已 Load 就绪的 URLStore，文件位于临时目录。
// FlushOnWrite=false（仅内存），SyncInterval=0（不启动后台 syncer）。
func newReadyStore(t *testing.T) *URLStore {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		Storage: config.StorageCfg{
			URLFilePath:  filepath.Join(dir, "sub", "urls.json"),
			SyncInterval: 0,
			FlushOnWrite: false,
		},
	}
	s, err := NewURLStore(cfg)
	if err != nil {
		t.Fatalf("NewURLStore: %v", err)
	}
	if err := s.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func makeItem(code, raw string) BatchSaveItem {
	return BatchSaveItem{
		ShortURL: &model.ShortURL{
			Code:      code,
			RawURL:    raw,
			CreatedAt: time.Now(),
		},
		Overwrite:    false,
		FailAttempts: 0,
	}
}

// SaveBatch 注入 FailAttempts=MaxAttempts（偶数 2）时：SuccessCount==0、无 SuccessCodes、
// 每条计入 Failures 且带原因、store 实际未写入（Count()==0、Exists 全 false）。
func TestSaveBatch_AllFailEvenReportsNoSuccess(t *testing.T) {
	s := newReadyStore(t)
	items := []BatchSaveItem{
		makeItem("code-a1", "https://example.com/a"),
		makeItem("code-a2", "https://example.com/b"),
	}
	for i := range items {
		items[i].FailAttempts = 2 // 注入每次尝试都失败
	}
	cfg := retry.Config{MaxAttempts: 2, InitialBackoff: 0, Multiplier: 1, Jitter: false}
	res, berr := s.SaveBatch(context.Background(), cfg, items)
	if berr == nil {
		t.Fatalf("expected non-nil joined error, got nil")
	}
	if res.SuccessCount != 0 {
		t.Fatalf("SuccessCount = %d, want 0", res.SuccessCount)
	}
	if len(res.SuccessCodes) != 0 {
		t.Fatalf("SuccessCodes = %v, want empty", res.SuccessCodes)
	}
	if len(res.Failures) != 2 {
		t.Fatalf("Failures len = %d, want 2", len(res.Failures))
	}
	for _, f := range res.Failures {
		if f.Err == nil {
			t.Fatalf("failure for %q missing error", f.Code)
		}
	}
	if s.Count() != 0 {
		t.Fatalf("store Count = %d, want 0 (nothing persisted)", s.Count())
	}
	for _, it := range items {
		if ok, _ := s.Exists(it.ShortURL.Code); ok {
			t.Fatalf("Exists(%q) = true, want false", it.ShortURL.Code)
		}
	}
}

// SaveBatch 注入 FailAttempts=MaxAttempts（奇数 3）时同样如实报失败（修复前此处偶/奇表现不同）。
func TestSaveBatch_AllFailOddReportsNoSuccess(t *testing.T) {
	s := newReadyStore(t)
	items := []BatchSaveItem{makeItem("code-b1", "https://example.com/c")}
	items[0].FailAttempts = 3
	cfg := retry.Config{MaxAttempts: 3, InitialBackoff: 0, Multiplier: 1, Jitter: false}
	res, berr := s.SaveBatch(context.Background(), cfg, items)
	if berr == nil {
		t.Fatalf("expected non-nil joined error, got nil")
	}
	if res.SuccessCount != 0 || len(res.SuccessCodes) != 0 || len(res.Failures) != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if s.Count() != 0 {
		t.Fatalf("store Count = %d, want 0", s.Count())
	}
}

// 正常路径：全部写入成功，SuccessCount 与实际落盘对齐，无 Failures。
func TestSaveBatch_HappyPathAllSucceed(t *testing.T) {
	s := newReadyStore(t)
	items := []BatchSaveItem{
		makeItem("code-c1", "https://example.com/d"),
		makeItem("code-c2", "https://example.com/e"),
	}
	cfg := retry.Config{MaxAttempts: 2, InitialBackoff: 0, Multiplier: 1, Jitter: false}
	res, berr := s.SaveBatch(context.Background(), cfg, items)
	if berr != nil {
		t.Fatalf("unexpected joined error: %v", berr)
	}
	if res.SuccessCount != 2 {
		t.Fatalf("SuccessCount = %d, want 2", res.SuccessCount)
	}
	if len(res.Failures) != 0 {
		t.Fatalf("Failures len = %d, want 0", len(res.Failures))
	}
	if s.Count() != 2 {
		t.Fatalf("store Count = %d, want 2", s.Count())
	}
	for _, it := range items {
		if ok, _ := s.Exists(it.ShortURL.Code); !ok {
			t.Fatalf("Exists(%q) = false, want true", it.ShortURL.Code)
		}
	}
}
