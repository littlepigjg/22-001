package store

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
)

// newTestAccessLogStore 构建一个指向临时文件的 AccessLogStore。
func newTestAccessLogStore(t *testing.T) *AccessLogStore {
	t.Helper()
	cfg := config.Default()
	cfg.Storage.LogFilePath = t.TempDir() + "/access.log"
	cfg.Storage.SyncInterval = 0 // 不启动后台 fsync，避免与测试节奏耦合
	cfg.Storage.FlushOnWrite = false
	ls, err := NewAccessLogStore(cfg)
	if err != nil {
		t.Fatalf("NewAccessLogStore: %v", err)
	}
	if err := ls.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })
	return ls
}

func mustLog(i int) *model.AccessLog {
	return &model.AccessLog{
		ID:        fmt.Sprintf("id-%d", i),
		Code:      "abc1234",
		IP:        "203.0.113.1",
		UserAgent: "Mozilla/5.0 (store-test)",
		Referer:   "https://example.com/ref?x=1",
		Status:    302,
		OS:        "Linux",
		Browser:   "Firefox",
		Device:    "pc",
	}
}

// TestConcurrentWriteAndScan 验证并发追加写入的同时进行 Scan 读取，扫描必须看到
// 完整记录、不会被截断或读到错位（修复前 WriteBytes 与 ScanShared 共享同一 fd 会
// 因 offset 互相干扰而 decode error 后 break）。
func TestConcurrentWriteAndScan(t *testing.T) {
	ls := newTestAccessLogStore(t)

	const (
		writers     = 4
		writesEach  = 50
		totalWrites = writers * writesEach
	)

	var done atomic.Bool
	var wgWrite sync.WaitGroup
	var wgRead sync.WaitGroup

	// 并发读取方：周期性扫描并计数，直到写入全部完成。
	// 用一个独立的 ticker 控制扫描节奏，避免空转抢 CPU / 拖慢 -race。
	wgRead.Add(1)
	var lastCount int64
	go func() {
		defer wgRead.Done()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			n, err := ls.Scan(func(l *model.AccessLog) bool { return true }, 0)
			if err != nil {
				t.Errorf("Scan: %v", err)
				return
			}
			atomic.StoreInt64(&lastCount, int64(n))
			if done.Load() {
				return
			}
			<-ticker.C
		}
	}()

	// 并发写入方。
	wgWrite.Add(writers)
	for w := 0; w < writers; w++ {
		go func(base int) {
			defer wgWrite.Done()
			logs := make([]*model.AccessLog, writesEach)
			for i := 0; i < writesEach; i++ {
				logs[i] = mustLog(base*writesEach + i)
			}
			if err := ls.AppendMany(logs); err != nil {
				t.Errorf("AppendMany: %v", err)
			}
		}(w)
	}
	wgWrite.Wait()
	// 写入全部完成后，停止读取方并等待其退出。
	done.Store(true)
	wgRead.Wait()

	// 所有写入落盘后，最终扫描到的条数必须等于写入总数。
	got := atomic.LoadInt64(&lastCount)
	if got != int64(totalWrites) {
		t.Errorf("after concurrent writes, Scan saw %d records, want %d", got, totalWrites)
	}
}
