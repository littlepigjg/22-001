package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
)

// newTestStore 构造一个指向临时文件的 URLStore，并关闭后台 syncer（SyncInterval=0）
// 以保证测试的确定性。
func newTestStore(t *testing.T) *URLStore {
	t.Helper()
	cfg := config.Default()
	cfg.Storage.URLFilePath = filepath.Join(t.TempDir(), "urls.json")
	cfg.Storage.SyncInterval = 0 // 不启动后台 syncer
	cfg.Storage.FlushOnWrite = false
	s, err := NewURLStore(cfg)
	if err != nil {
		t.Fatalf("NewURLStore: %v", err)
	}
	if err := s.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	return s
}

// TestURLStore_ConcurrentAccessNoRace 在同一短码上并发执行所有会触碰
// Visits / Disabled / Remark / RawURL 字段的操作。在 -race 下应零警告。
func TestURLStore_ConcurrentAccessNoRace(t *testing.T) {
	s := newTestStore(t)
	const code = "abc123"
	if err := s.Save(&model.ShortURL{
		Code:      code,
		RawURL:    "https://example.com",
		MaxVisits: 1 << 30, // 足够大，确保多数访问会真正自增
	}, false); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	const goroutines = 8
	const iterations = 400

	var wg sync.WaitGroup
	wg.Add(goroutines)
	start := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			<-start
			now := time.Now()
			for i := 0; i < iterations; i++ {
				switch (g*iterations + i) % 7 {
				case 0:
					_, _ = s.RecordVisit(code, now)
				case 1:
					_, _ = s.Get(code)
				case 2:
					_, _, _, _, _ = s.Stats()
				case 3:
					_ = s.SetDisabled(code, id%2 == 0)
				case 4:
					_ = s.SetRemark(code, fmt.Sprintf("r-%d-%d", id, i))
				case 5:
					_ = s.ForEach(func(u *model.ShortURL) bool { _ = u; return true })
				case 6:
					_ = s.Flush()
				}
			}
		}(g)
	}
	close(start)
	wg.Wait()

	// 收尾：把记录恢复成可用态并校验最终自增未丢（至少有一次成功自增）。
	_ = s.SetDisabled(code, false)
	u, err := s.Get(code)
	if err != nil {
		t.Fatalf("final Get: %v", err)
	}
	if u.Visits <= 0 {
		t.Fatalf("expected visits > 0, got %d (lost increments?)", u.Visits)
	}
}

// TestRecordVisit_MaxVisitsAutoDisable 锁定重定向自增 + max-visits 自动禁用语义，
// 确保不丢、不重复计数。
func TestRecordVisit_MaxVisitsAutoDisable(t *testing.T) {
	s := newTestStore(t)
	const code = "max3"
	if err := s.Save(&model.ShortURL{
		Code:      code,
		RawURL:    "https://example.com",
		MaxVisits: 3,
	}, false); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	now := time.Now()

	// 前 3 次：前 2 次正常 302，第 3 次触发上限并自动禁用。
	for i := 1; i <= 3; i++ {
		out, err := s.RecordVisit(code, now)
		if err != nil {
			t.Fatalf("visit %d: %v", i, err)
		}
		if !out.Found {
			t.Fatalf("visit %d: not found", i)
		}
		if out.Visits != int64(i) {
			t.Fatalf("visit %d: want Visits=%d, got %d", i, i, out.Visits)
		}
		if i < 3 && out.MaxVisited {
			t.Fatalf("visit %d: unexpected MaxVisited", i)
		}
		if i == 3 && !out.MaxVisited {
			t.Fatalf("visit 3: want MaxVisited=true, got false")
		}
	}

	// 第 4 次：已禁用，不再自增。
	out, err := s.RecordVisit(code, now)
	if err != nil {
		t.Fatalf("visit 4: %v", err)
	}
	if !out.Disabled {
		t.Fatalf("visit 4: want Disabled=true, got false")
	}
	if out.Visits != 0 {
		t.Fatalf("visit 4: disabled visit must not increment, got Visits=%d", out.Visits)
	}

	// 最终落盘值应为 3，恰好等于 MaxVisits。
	u, err := s.Get(code)
	if err != nil {
		t.Fatalf("final Get: %v", err)
	}
	if u.Visits != 3 {
		t.Fatalf("final Visits: want 3, got %d", u.Visits)
	}
	if !u.Disabled {
		t.Fatalf("final Disabled: want true")
	}
}

// TestURLStore_GetReturnsClone 确保 Get 返回的是克隆：修改返回值不影响 store 内部状态。
func TestURLStore_GetReturnsClone(t *testing.T) {
	s := newTestStore(t)
	const code = "clone1"
	if err := s.Save(&model.ShortURL{Code: code, RawURL: "https://a.example.com", Visits: 1}, false); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	got, err := s.Get(code)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.RawURL = "https://tampered.example.com"
	got.Visits = 999

	again, err := s.Get(code)
	if err != nil {
		t.Fatalf("Get again: %v", err)
	}
	if again.RawURL != "https://a.example.com" {
		t.Fatalf("store value mutated via returned clone: RawURL=%q", again.RawURL)
	}
	if again.Visits != 1 {
		t.Fatalf("store value mutated via returned clone: Visits=%d", again.Visits)
	}
}
