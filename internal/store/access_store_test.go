package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
)

func newAccessLogStoreForTest(t *testing.T, syncInt time.Duration) (*AccessLogStore, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")
	cfg := &config.Config{
		Storage: config.StorageCfg{
			LogFilePath:  logPath,
			SyncInterval: syncInt,
			FlushOnWrite: false,
		},
	}
	s, err := NewAccessLogStore(cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, logPath
}

func validLog(code string, status int, ts time.Time) *model.AccessLog {
	return &model.AccessLog{
		ID:        "id-" + code,
		Code:      code,
		IP:        "127.0.0.1",
		Timestamp: ts,
		Status:    status,
	}
}

// TestAppendMany_ConcurrentNoLossNoRace 并发向 AppendMany 写入并断言：
// 每条合法日志都完整落盘，且文件无空行、全部合法。-race 下验证无竞争。
func TestAppendMany_ConcurrentNoLossNoRace(t *testing.T) {
	s, logPath := newAccessLogStoreForTest(t, 50*time.Millisecond)

	// Open 使用一个会在测试中途被取消的 ctx，以验证后台 fsync goroutine
	// 不应随该 ctx 取消而退出（修复前会退出，导致后续落盘丢失）。
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.Open(ctx); err != nil {
		t.Fatalf("open: %v", err)
	}
	cancel() // 取消调用方 ctx，后台 syncer 应继续存活

	const workers = 50
	const perWorker = 300 // 总计 15000 条
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(g int) {
			defer wg.Done()
			logs := make([]*model.AccessLog, 0, perWorker)
			for j := 0; j < perWorker; j++ {
				logs = append(logs, validLog("abc1", 302, time.Now()))
			}
			if err := s.AppendMany(logs); err != nil {
				t.Errorf("append many: %v", err)
			}
		}(i)
	}
	wg.Wait()

	_ = s.Sync()

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	trimmed := strings.TrimRight(string(data), "\n")
	if trimmed == "" {
		t.Fatalf("access log empty")
	}
	lines := strings.Split(trimmed, "\n")
	for i, line := range lines {
		if line == "" {
			t.Fatalf("empty line at %d", i)
		}
		var l model.AccessLog
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("line %d unmarshal: %v", i, err)
		}
		if err := l.Validate(); err != nil {
			t.Fatalf("line %d invalid: %v", i, err)
		}
	}
	want := workers * perWorker
	if got := len(lines); got != want {
		t.Fatalf("valid log lines: got %d want %d (lost %d)", got, want, want-got)
	}
}

// TestAppend_RejectsInvalid 确保 Append 拒绝非法日志，文件保持干净。
func TestAppend_RejectsInvalid(t *testing.T) {
	s, logPath := newAccessLogStoreForTest(t, 0)
	if err := s.Open(context.Background()); err != nil {
		t.Fatalf("open: %v", err)
	}

	if err := s.Append(validLog("abc1", 999, time.Now())); err == nil {
		t.Fatalf("expected error for invalid status code")
	}
	if err := s.Append(validLog("abc1", 302, time.Time{})); err == nil {
		t.Fatalf("expected error for zero timestamp")
	}
	if err := s.Append(validLog("abc1", 302, time.Now())); err != nil {
		t.Fatalf("valid append failed: %v", err)
	}
	_ = s.Sync()

	data, _ := os.ReadFile(logPath)
	trimmed := strings.TrimRight(string(data), "\n")
	if trimmed == "" {
		t.Fatalf("expected 1 line, got empty file")
	}
	if got := len(strings.Split(trimmed, "\n")); got != 1 {
		t.Fatalf("expected exactly 1 line, got %d (invalid rows leaked)", got)
	}
}
