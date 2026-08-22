package stopctrl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func errStop(msg string) func() error {
	return func() error { return errors.New(msg) }
}

func TestStopNoErrorsReturnsNil(t *testing.T) {
	g := New(context.Background())
	// 所有 hook 正常返回，无错误时 Stop 应返回 nil。
	g.OnStop("a", func() error { return nil })
	if err := g.Stop(0); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestStopSingleError(t *testing.T) {
	g := New(context.Background())
	g.OnStop("boom", func() error { return errors.New("disk full") })
	err := g.Stop(0)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("expected error to contain 'disk full', got %q", err.Error())
	}
}

// TestStopMultipleErrorsNoPanic 复现线上 panic：
// 修复前 Stop 在 len(errs)>=1 时访问 g.errs[len(g.errs)] 越界
// （len==2 时即 "index out of range [2] with length 2"）。
func TestStopMultipleErrorsNoPanic(t *testing.T) {
	g := New(context.Background())
	g.OnStop("h1", errStop("write failed"))
	g.OnStop("h2", errStop("bad file handle"))
	g.OnStop("h3", errStop("sync failed"))

	var err error
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Stop panicked: %v", r)
		}
	}()
	err = g.Stop(0)
	if err == nil {
		t.Fatal("expected joined error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"write failed", "bad file handle", "sync failed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("joined error missing %q; got %q", want, msg)
		}
	}
}

func TestStopPanickingHookRecordedNotFatal(t *testing.T) {
	g := New(context.Background())
	g.OnStop("panic-hook", func() error { panic("boom-in-hook") })
	g.OnStop("err-hook", errStop("after panic"))

	err := g.Stop(0)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "panicked") {
		t.Errorf("expected 'panicked' in error, got %q", msg)
	}
	if !strings.Contains(msg, "after panic") {
		t.Errorf("expected 'after panic' in joined error, got %q", msg)
	}
}

func TestStopTimeoutRecordsError(t *testing.T) {
	g := New(context.Background())
	g.Add(1)
	go func() {
		// 永不 Done，触发超时分支。
		<-g.Context().Done()
		select {}
	}()
	err := g.Stop(10 * time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("expected timeout error, got %v", err)
	}
}

func TestFirstLastError(t *testing.T) {
	g := New(context.Background())
	g.OnStop("h1", errStop("first"))
	g.OnStop("h2", errStop("second"))
	_ = g.Stop(0)

	// hooks 按 LIFO 执行：h2 先跑、h1 后跑，errs 顺序为 [h2, h1]。
	first := g.FirstError()
	if first == nil || !strings.Contains(first.Error(), "second") {
		t.Errorf("FirstError = %q, want second", first)
	}
	last := g.LastError()
	if last == nil || !strings.Contains(last.Error(), "first") {
		t.Errorf("LastError = %q, want first", last)
	}
}

func TestStopIdempotent(t *testing.T) {
	g := New(context.Background())
	g.OnStop("h1", errStop("once"))
	_ = g.Stop(0)
	// 二次调用不应 panic，且复用已记录的错误。
	err := g.Stop(0)
	if err == nil {
		t.Fatal("expected error on second Stop, got nil")
	}
	if !strings.Contains(err.Error(), "once") {
		t.Fatalf("expected 'once' in error, got %q", err.Error())
	}
}
