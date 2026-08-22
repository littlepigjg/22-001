package shurl_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/service"
	"shurl/internal/store"
)

// flushRaceReports gives the race runtime time to flush any asynchronously
// queued race reports and propagate the failed state before the defer in
// TestRedGreen inspects t.Failed().
func flushRaceReports() {
	for i := 0; i < 200; i++ {
		runtime.Gosched()
	}
	runtime.GC()
	time.Sleep(10 * time.Millisecond)
	for i := 0; i < 120; i++ {
		runtime.Gosched()
	}
	time.Sleep(8 * time.Millisecond)
	for i := 0; i < 80; i++ {
		runtime.Gosched()
	}
}

// TestRedGreen exercises the deliberately implanted cross-file concurrency
// defect between:
//
//   - internal/service/health_service.go : HealthService.Check performs
//     lock-free reads of model.ShortURL fields (Disabled, ExpireAt,
//     MaxVisits, Visits) on pointers captured from URLStore.ForEach.
//   - internal/service/janitor_service.go : JanitorService.applyLifecycleMarks
//     performs lock-free WRITES of the same ShortURL fields.
//
// Containment strategy used to make the defect detection deterministic:
//
//  1. Test spawns exactly ONE additional goroutine (the janitor writer) and
//     keeps ALL reads inside THIS (main) goroutine.  With only two actors
//     total and the read side running on TestRedGreen's stack, every data
//     race the Go race detector observes has the tracked test goroutine on
//     one of its two stack traces → testing.T.Failed() is set directly on
//     *this* T → t.Failed() == true → RED printed deterministically when
//     the defect is present.
//  2. model.RaceSyncStart (a timing-only atomic flag with no synchronisation
//     of the ShortURL fields themselves) is set by applyLifecycleMarks when
//     the write phase begins.  The main goroutine waits for this flag
//     before issuing any Check reads, guaranteeing read/write windows
//     overlap on every run regardless of scheduler jitter.
func TestRedGreen(t *testing.T) {
	green := true
	var sentinelTornStart int64
	defer func() {
		flushRaceReports()
		if r := recover(); r != nil {
			green = false
			fmt.Fprintf(os.Stderr, "panic: %v\n", r)
		}
		sentinelDelta := model.RaceSentinelTornCount.Load() - sentinelTornStart
		// RED decision – three independent witnesses, any one suffices:
		//   a) test body set green=false (logic error / stats mismatch)
		//   b) go test -race runtime set t.Failed() via TSAN goroutine→T trace
		//   c) software race-sentinel observed torn reads (B>A) during
		//      Check↔applyLifecycleMarks overlap → defect real, RED.
		// Sentinel (c) is deterministic under go test -count=N shared-
		// process reuse where TSAN dedupe / trace-labelling sometimes
		// skips per-T Fail marking even though races still occurred.
		if !green || t.Failed() || sentinelDelta > 0 {
			if sentinelDelta > 0 && !t.Failed() {
				// TSAN witness missed for this T.  Force Fail so that
				// go test's overall PASS/FAIL tally matches RED print.
				t.Errorf("race-sentinel detected %d torn reads (defect present, RED)", sentinelDelta)
			}
			fmt.Println("RED（红灯，缺陷未修复）")
		} else {
			fmt.Println("GREEN（绿灯，缺陷已修复）")
		}
	}()

	tmp, err := os.MkdirTemp("", "shurl-rg-*")
	if err != nil {
		t.Fatalf("mkdirtmp: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	cfg := config.Default()
	cfg.Storage.URLFilePath = filepath.Join(tmp, "urls.json")
	cfg.Storage.LogFilePath = filepath.Join(tmp, "access.log")
	cfg.Storage.SyncInterval = 0
	cfg.Storage.FlushOnWrite = false
	cfg.Janitor.Enabled = false

	urlStore, err := store.NewURLStore(cfg)
	if err != nil {
		t.Fatalf("new url store: %v", err)
	}
	if err := urlStore.Load(context.Background()); err != nil {
		t.Fatalf("load url store: %v", err)
	}
	defer func() { _ = urlStore.Close() }()

	logStore, err := store.NewAccessLogStore(cfg)
	if err != nil {
		t.Fatalf("new log store: %v", err)
	}
	if err := logStore.Open(context.Background()); err != nil {
		t.Fatalf("open log store: %v", err)
	}
	defer func() { _ = logStore.Close() }()

	now := time.Now()
	const N = 15
	for i := 0; i < N; i++ {
		u := &model.ShortURL{
			Code:      fmt.Sprintf("ex%04d", i),
			RawURL:    "https://example.com/ex/" + fmt.Sprintf("%d", i),
			CreatedAt: now.Add(-1 * time.Hour),
			ExpireAt:  now.Add(-time.Duration(i+1) * time.Second),
		}
		if err := urlStore.Save(u, false); err != nil {
			t.Fatalf("save ex: %v", err)
		}
	}
	for i := 0; i < N; i++ {
		u := &model.ShortURL{
			Code:      fmt.Sprintf("mx%04d", i),
			RawURL:    "https://example.com/mx/" + fmt.Sprintf("%d", i),
			CreatedAt: now.Add(-1 * time.Hour),
			MaxVisits: 10,
			Visits:    10,
		}
		if err := urlStore.Save(u, false); err != nil {
			t.Fatalf("save mx: %v", err)
		}
	}

	hs, err := service.NewHealthService(urlStore, logStore)
	if err != nil {
		t.Fatalf("health svc: %v", err)
	}
	hs.MarkStarted()

	js, err := service.NewJanitorService(cfg, urlStore)
	if err != nil {
		t.Fatalf("janitor svc: %v", err)
	}

	model.ResetRaceSync()
	sentinelTornStart = model.RaceSentinelTornCount.Load()

	var rendezvous sync.WaitGroup
	rendezvous.Add(1)
	var wg sync.WaitGroup
	var writerDone int32

	// === ONLY ONE extra goroutine (writer side) – test MAIN goroutine is
	// the reader side.  Keeping the actor count at exactly two ensures the
	// first race reported always has the tracked test goroutine on one
	// stack trace, so Fail() is delivered to TestRedGreen's T reliably.
	wg.Add(1)
	go func() {
		defer wg.Done()
		rendezvous.Wait()
		runtime.Gosched()
		runtime.Gosched()
		_ = js.RunOnce(10_000_000)
		atomic.StoreInt32(&writerDone, 1)
	}()

	// Start-line released: writer begins RunOnce.  Spin here until
	// applyLifecycleMarks raises the sync flag → at that precise moment the
	// janitor is mutating ShortURL fields, so every subsequent Check read
	// issued by the main goroutine races with those writes (the snapshots
	// captured inside Check still point at the OLD map clones that the
	// janitor's candidate list is writing to).
	rendezvous.Done()
	for atomic.LoadInt32(&model.RaceSyncStart) == 0 {
		runtime.Gosched()
	}
	for k := 0; k < 5; k++ {
		_ = hs.Check(context.Background())
		runtime.Gosched()
	}
	for atomic.LoadInt32(&writerDone) == 0 {
		_ = hs.Check(context.Background())
		runtime.Gosched()
	}

	wg.Wait()
	flushRaceReports()
	if _, _, _, _, err := urlStore.Stats(); err != nil {
		green = false
		t.Fatalf("final stats: %v", err)
	}
}
