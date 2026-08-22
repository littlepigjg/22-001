package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/store"
	"shurl/pkg/idgen"
)

// newStoresForTest 构造一组真实的 URLStore + AccessLogStore，使用临时目录，
// 开启后台 fsync（SyncInterval 较短，模拟线上后台落盘行为）。
func newStoresForTest(t *testing.T) (*store.URLStore, *store.AccessLogStore, string) {
	t.Helper()
	dir := t.TempDir()
	urlPath := filepath.Join(dir, "urls.json")
	logPath := filepath.Join(dir, "access.log")
	cfg := &config.Config{
		Storage: config.StorageCfg{
			URLFilePath:  urlPath,
			LogFilePath:  logPath,
			SyncInterval: 50 * time.Millisecond, // 后台 fsync 周期
			FlushOnWrite: false,
		},
		ShortCode: config.ShortCodeCfg{
			Length:     4,
			Alphabet:   "abcdefghijklmnopqrstuvwxyz",
			MaxRetries: 5,
		},
	}
	urlStore, err := store.NewURLStore(cfg)
	if err != nil {
		t.Fatalf("new url store: %v", err)
	}
	if err := urlStore.Load(context.Background()); err != nil {
		t.Fatalf("load url store: %v", err)
	}
	if err := urlStore.Save(&model.ShortURL{
		Code:      "abc1",
		RawURL:    "https://example.com/hello",
		CreatedAt: time.Now(),
	}, true); err != nil {
		t.Fatalf("save short url: %v", err)
	}
	logStore, err := store.NewAccessLogStore(cfg)
	if err != nil {
		t.Fatalf("new log store: %v", err)
	}
	if err := logStore.Open(context.Background()); err != nil {
		t.Fatalf("open log store: %v", err)
	}
	t.Cleanup(func() { _ = logStore.Close() })
	return urlStore, logStore, logPath
}

// runBackgroundFlusher 启动一个周期性调用 BackgroundFlush 的 goroutine，
// 返回停止并等待其退出的函数，模拟 RedirectHandler.runBackgroundFlush。
func runBackgroundFlusher(svc *RedirectService, interval time.Duration) func() {
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tk := time.NewTicker(interval)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				_, _ = svc.BackgroundFlush()
				return
			case <-tk.C:
				_, _ = svc.BackgroundFlush()
			}
		}
	}()
	return func() {
		close(stop)
		wg.Wait()
	}
}

// readValidLines 读取 access.log，断言：无空行、每行可解析、字段合法；
// 返回合法行数。
func readValidLines(t *testing.T, logPath string) int64 {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	trimmed := strings.TrimRight(string(data), "\n")
	if trimmed == "" {
		t.Fatalf("access log is empty")
	}
	lines := strings.Split(trimmed, "\n")
	var valid int64
	for i, line := range lines {
		if line == "" {
			t.Fatalf("empty line at %d; file must be clean NDJSON", i)
		}
		var l model.AccessLog
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("line %d unmarshal error: %v\nraw=%q", i, err, line)
		}
		if err := l.Validate(); err != nil {
			t.Fatalf("line %d invalid: %v\nraw=%q", i, err, line)
		}
		valid++
	}
	return valid
}

// TestHandleRedirect_ConcurrentNoLossNoRace 模拟线上压测场景：
// 数百路 goroutine 并发打 HandleRedirect（命中一个正常短码，触发
// IncrementVisits + appendLog），同时后台周期 BackgroundFlush，
// 持续一段时间后断言 access.log 的非空合法行数与成功重定向次数严格相等，
// 且每行都能通过 model.AccessLog.Validate。
// 以 -race 运行时同时验证并发追加/刷盘路径无数据竞争。
func TestHandleRedirect_ConcurrentNoLossNoRace(t *testing.T) {
	urlStore, logStore, logPath := newStoresForTest(t)
	svc, err := NewRedirectService(urlStore, logStore)
	if err != nil {
		t.Fatalf("new redirect service: %v", err)
	}

	stopFlush := runBackgroundFlusher(svc, 5*time.Millisecond)

	const workers = 200
	const perWorker = 75 // 总计 15000 次访问，远超 MaxVisits(0=不限) 与 100 的倍数边界
	var redirects int64

	var workWG sync.WaitGroup
	workWG.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer workWG.Done()
			for j := 0; j < perWorker; j++ {
				res, err := svc.HandleRedirect(context.Background(), &RedirectRequest{
					Code:       "abc1",
					RemoteAddr: "127.0.0.1:1234",
					Headers: map[string][]string{
						"User-Agent": {"Go-test/1.0"},
					},
					Timestamp: time.Now(),
				})
				if err != nil {
					t.Errorf("unexpected redirect error: %v", err)
					return
				}
				if res.Status == 302 {
					atomic.AddInt64(&redirects, 1)
				}
			}
		}()
	}
	workWG.Wait()

	stopFlush() // 停止后台 flush 并等待最后一次完成

	// 兜底：确保 pending 全部落盘。
	if _, err := svc.BackgroundFlush(); err != nil {
		t.Fatalf("final flush: %v", err)
	}
	_ = logStore.Sync()

	total := int64(workers * perWorker)
	if got := atomic.LoadInt64(&redirects); got != total {
		t.Fatalf("redirect count: got %d want %d", got, total)
	}
	if got := readValidLines(t, logPath); got != total {
		t.Fatalf("valid log lines: got %d want %d (lost %d)", got, total, total-got)
	}
}

// TestAppendToPending_ConcurrentNoRace 直接对 AppendToPending 与
// BackgroundFlush 做并发压测，确保共享 pending 批次在互斥锁下无竞争，
// 且所有追加的合法日志最终都完整落盘、无丢失。
func TestAppendToPending_ConcurrentNoRace(t *testing.T) {
	urlStore, logStore, logPath := newStoresForTest(t)
	svc, err := NewRedirectService(urlStore, logStore)
	if err != nil {
		t.Fatalf("new redirect service: %v", err)
	}

	stopFlush := runBackgroundFlusher(svc, 2*time.Millisecond)

	const workers = 64
	const perWorker = 200 // 总计 12800 条
	var workWG sync.WaitGroup
	workWG.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer workWG.Done()
			for j := 0; j < perWorker; j++ {
				svc.AppendToPending(&model.AccessLog{
					ID:        idgen.NewString(),
					Code:      "abc1",
					IP:        "127.0.0.1",
					Timestamp: time.Now(),
					Status:    302,
				})
			}
		}()
	}
	workWG.Wait()

	stopFlush()
	if _, err := svc.BackgroundFlush(); err != nil {
		t.Fatalf("final flush: %v", err)
	}
	_ = logStore.Sync()

	total := int64(workers * perWorker)
	if got := readValidLines(t, logPath); got != total {
		t.Fatalf("valid log lines: got %d want %d (lost %d)", got, total, total-got)
	}
}

// TestAppendToPending_SkipsInvalidLines 确保非法日志（空时间戳/非法状态码）
// 不会写入文件，保证 NDJSON 文件干净合法。
func TestAppendToPending_SkipsInvalidLines(t *testing.T) {
	_, logStore, logPath := newStoresForTest(t)
	svc, err := NewRedirectService(nil, logStore)
	if err == nil {
		t.Fatalf("expected error for nil urlStore")
	}
	urlStore, logStore, logPath := newStoresForTest(t)
	svc, err = NewRedirectService(urlStore, logStore)
	if err != nil {
		t.Fatalf("new redirect service: %v", err)
	}

	// 合法日志应落盘，非法日志（空时间戳、非法状态码）应被跳过。
	logs := []*model.AccessLog{
		{ID: idgen.NewString(), Code: "abc1", IP: "127.0.0.1", Timestamp: time.Now(), Status: 302},
		nil,
		{ID: idgen.NewString(), Code: "abc1", IP: "127.0.0.1", Timestamp: time.Time{}, Status: 302}, // 空时间戳
		{ID: idgen.NewString(), Code: "abc1", IP: "127.0.0.1", Timestamp: time.Now(), Status: 999},   // 非法状态码
		{ID: idgen.NewString(), Code: "abc1", IP: "127.0.0.1", Timestamp: time.Now(), Status: 404},
	}
	for _, l := range logs {
		svc.AppendToPending(l)
	}
	if _, err := svc.BackgroundFlush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	_ = logStore.Sync()

	// 期望只有 2 条合法行落盘（第一条与最后一条）。
	if got := readValidLines(t, logPath); got != 2 {
		t.Fatalf("valid log lines: got %d want 2 (invalid rows leaked)", got)
	}
}
