package shurl_test

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"shurl/internal/admin"
	"shurl/pkg/stopctrl"
)

type failingFlusher struct {
	name    string
	called  *atomic.Int32
	failMsg string
}

func newFailingFlusher(name string, msg string) *failingFlusher {
	return &failingFlusher{name: name, called: &atomic.Int32{}, failMsg: msg}
}

func (f *failingFlusher) Flush() error {
	f.called.Add(1)
	return errors.New(f.name + ": " + f.failMsg)
}

type failingSyncer struct {
	name    string
	called  *atomic.Int32
	failMsg string
}

func newFailingSyncer(name string, msg string) *failingSyncer {
	return &failingSyncer{name: name, called: &atomic.Int32{}, failMsg: msg}
}

func (s *failingSyncer) Sync() error {
	s.called.Add(1)
	return errors.New(s.name + ": " + s.failMsg)
}

type failingCloser struct {
	name    string
	called  *atomic.Int32
	failMsg string
}

func newFailingCloser(name string, msg string) *failingCloser {
	return &failingCloser{name: name, called: &atomic.Int32{}, failMsg: msg}
}

func (c *failingCloser) Close() error {
	c.called.Add(1)
	return errors.New(c.name + ": " + c.failMsg)
}

type goodFlusher struct{}

func (goodFlusher) Flush() error { return nil }

type goodSyncer struct{}

func (goodSyncer) Sync() error { return nil }

type goodCloser struct{}

func (goodCloser) Close() error { return nil }

func capturePanic(fn func()) (recovered any) {
	defer func() {
		recovered = recover()
	}()
	fn()
	return nil
}

func assertIndexOutOfRange(t *testing.T, recovered any) bool {
	t.Helper()
	if recovered == nil {
		return false
	}
	s := fmt.Sprintf("%v", recovered)
	return strings.Contains(s, "index out of range") || strings.Contains(s, "runtime error")
}

func TestRedGreen(t *testing.T) {
	total := 0
	passed := 0

	t.Run("GroupStop_SingleErrorHook_TriggersSlicePanic", func(t *testing.T) {
		total++
		g := stopctrl.New(nil)
		g.OnStop("bad_hook", func() error {
			return errors.New("always fail single")
		})
		rec := capturePanic(func() {
			_ = g.Stop(50 * time.Millisecond)
		})
		if !assertIndexOutOfRange(t, rec) {
			t.Fatalf("expected slice index out of range panic on single error hook, got %v", rec)
		}
		passed++
	})

	t.Run("GroupStop_MultiErrorHooks_TriggersSlicePanic", func(t *testing.T) {
		total++
		g := stopctrl.New(nil)
		g.OnStop("hook_one", func() error {
			return errors.New("first failure")
		})
		g.OnStop("hook_two", func() error {
			return errors.New("second failure")
		})
		g.OnStop("hook_three", func() error {
			return errors.New("third failure")
		})
		rec := capturePanic(func() {
			_ = g.Stop(50 * time.Millisecond)
		})
		if !assertIndexOutOfRange(t, rec) {
			t.Fatalf("expected slice index out of range panic on multi error hooks, got %v", rec)
		}
		passed++
	})

	t.Run("GroupStop_NoErrors_ReturnsNil_NoPanic", func(t *testing.T) {
		total++
		g := stopctrl.New(nil)
		g.OnStop("good_hook", func() error {
			return nil
		})
		var rec any
		func() {
			defer func() { rec = recover() }()
			err := g.Stop(50 * time.Millisecond)
			if err != nil {
				t.Fatalf("expected nil error on clean stop, got %v", err)
			}
		}()
		if rec != nil {
			t.Fatalf("expected no panic on clean stop, got %v", rec)
		}
		passed++
	})

	t.Run("GroupStop_TimeoutPlusErrorHook_TriggersSlicePanic", func(t *testing.T) {
		total++
		g := stopctrl.New(nil)
		_ = g.Add(1)
		g.OnStop("err", func() error {
			return errors.New("hook failed")
		})
		go func() {
			time.Sleep(200 * time.Millisecond)
			g.Done()
		}()
		rec := capturePanic(func() {
			_ = g.Stop(50 * time.Millisecond)
		})
		if !assertIndexOutOfRange(t, rec) {
			t.Fatalf("expected slice index out of range panic on timeout + error, got %v", rec)
		}
		passed++
	})

	t.Run("AdminFlushAll_FailingComponent_TriggersSlicePanic", func(t *testing.T) {
		total++
		svc := admin.New(nil)
		ff := newFailingFlusher("disk_flusher", "disk full")
		svc.RegisterFlusher("disk_flusher", ff)
		fs := newFailingSyncer("log_syncer", "permission denied")
		svc.RegisterSyncer("log_syncer", fs)
		rec := capturePanic(func() {
			_ = svc.FlushAll()
		})
		if !assertIndexOutOfRange(t, rec) {
			t.Fatalf("expected slice index out of range panic on admin FlushAll with failing component, got %v", rec)
		}
		passed++
	})

	t.Run("AdminCloseAll_FailingComponent_TriggersSlicePanic", func(t *testing.T) {
		total++
		svc := admin.New(nil)
		fc1 := newFailingCloser("db_conn", "connection reset")
		fc2 := newFailingCloser("file_handle", "bad file descriptor")
		svc.RegisterCloser("db_conn", fc1)
		svc.RegisterCloser("file_handle", fc2)
		rec := capturePanic(func() {
			_ = svc.CloseAll()
		})
		if !assertIndexOutOfRange(t, rec) {
			t.Fatalf("expected slice index out of range panic on admin CloseAll with failing component, got %v", rec)
		}
		passed++
	})

	t.Run("AdminShutdownAll_FailingFlusher_TriggersSlicePanic", func(t *testing.T) {
		total++
		svc := admin.New(nil)
		ff := newFailingFlusher("store_flusher", "flush failed persistently")
		svc.RegisterFlusher("store_flusher", ff)
		rec := capturePanic(func() {
			_ = svc.ShutdownAll(100 * time.Millisecond)
		})
		if !assertIndexOutOfRange(t, rec) {
			t.Fatalf("expected slice index out of range panic on admin ShutdownAll, got %v", rec)
		}
		passed++
	})

	t.Run("GroupStop_OnlyTimeoutError_TriggersSlicePanic", func(t *testing.T) {
		total++
		g := stopctrl.New(nil)
		_ = g.Add(1)
		go func() {
			time.Sleep(300 * time.Millisecond)
			g.Done()
		}()
		rec := capturePanic(func() {
			_ = g.Stop(50 * time.Millisecond)
		})
		if !assertIndexOutOfRange(t, rec) {
			t.Fatalf("expected slice index out of range panic on timeout-only error, got %v", rec)
		}
		passed++
	})

	t.Run("AdminFlushAll_GoodComponents_NoPanic", func(t *testing.T) {
		total++
		svc := admin.New(nil)
		svc.RegisterFlusher("good", goodFlusher{})
		svc.RegisterSyncer("good_sync", goodSyncer{})
		svc.RegisterCloser("good_close", goodCloser{})
		var rec any
		func() {
			defer func() { rec = recover() }()
			_ = svc.FlushAll()
		}()
		if rec != nil {
			t.Fatalf("expected no panic on admin FlushAll with good components, got %v", rec)
		}
		passed++
	})

	if passed == total {
		fmt.Println("RED（红灯，缺陷未修复）")
		t.Fatalf("RED: defect present — all %d trigger cases hit index-out-of-range panic, fix required", total)
	} else {
		fmt.Printf("GREEN（绿灯，缺陷已修复） — passed %d/%d\n", passed, total)
	}
}
