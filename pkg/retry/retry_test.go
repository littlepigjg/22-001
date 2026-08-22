package retry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// sentinelErr 用于断言返回的错误就是「最后一次」失败。
var (
	errFirst  = errors.New("boom-first")
	errSecond = errors.New("boom-second")
	errThird  = errors.New("boom-third")
)

// attemptsRecorder 记录每次调用的 attempt 编号并按配置返回错误。
type attemptsRecorder struct {
	// errs[attempt] 为该次调用返回的错误；nil 表示该次成功。
	errs map[int]error
	calls []int
}

func (r *attemptsRecorder) fn(attempt int) error {
	r.calls = append(r.calls, attempt)
	if e, ok := r.errs[attempt]; ok {
		return e
	}
	return nil
}

func TestDo_AllFailEvenReturnsLastError(t *testing.T) {
	// 历史缺陷：MaxAttempts 为偶数且全部失败时，曾因 lastErr 交替清空而误返回 nil。
	cfg := Config{MaxAttempts: 2, InitialBackoff: 0, Multiplier: 1, Jitter: false}
	r := &attemptsRecorder{errs: map[int]error{1: errFirst, 2: errSecond}}
	err := cfg.Do(context.Background(), r.fn)
	if err == nil {
		t.Fatalf("expected non-nil error for even-count all-fail, got nil")
	}
	if !errors.Is(err, errSecond) {
		t.Fatalf("expected last error %v, got %v", errSecond, err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(r.calls))
	}
}

func TestDo_AllFailOddReturnsLastError(t *testing.T) {
	cfg := Config{MaxAttempts: 3, InitialBackoff: 0, Multiplier: 1, Jitter: false}
	r := &attemptsRecorder{errs: map[int]error{1: errFirst, 2: errSecond, 3: errThird}}
	err := cfg.Do(context.Background(), r.fn)
	if err == nil {
		t.Fatalf("expected non-nil error for odd-count all-fail, got nil")
	}
	if !errors.Is(err, errThird) {
		t.Fatalf("expected last error %v, got %v", errThird, err)
	}
}

func TestDo_FirstAttemptSuccessReturnsNil(t *testing.T) {
	cfg := Config{MaxAttempts: 3, InitialBackoff: 0, Multiplier: 1, Jitter: false}
	r := &attemptsRecorder{errs: map[int]error{1: nil}} // 首试即成功
	err := cfg.Do(context.Background(), r.fn)
	if err != nil {
		t.Fatalf("expected nil error on first success, got %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("expected 1 attempt on first success, got %d", len(r.calls))
	}
}

func TestDo_FailThenSuccessReturnsNil(t *testing.T) {
	cfg := Config{MaxAttempts: 3, InitialBackoff: 0, Multiplier: 1, Jitter: false}
	r := &attemptsRecorder{errs: map[int]error{1: errFirst, 2: nil}} // 第1次失败、第2次成功
	err := cfg.Do(context.Background(), r.fn)
	if err != nil {
		t.Fatalf("expected nil error after retry success, got %v", err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(r.calls))
	}
}

func TestDo_SingleAttemptAllFailReturnsError(t *testing.T) {
	cfg := Config{MaxAttempts: 1, InitialBackoff: 0, Multiplier: 1, Jitter: false}
	r := &attemptsRecorder{errs: map[int]error{1: errFirst}}
	err := cfg.Do(context.Background(), r.fn)
	if !errors.Is(err, errFirst) {
		t.Fatalf("expected %v, got %v", errFirst, err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("expected 1 attempt, got %d", len(r.calls))
	}
}

func TestDo_ContextCancelMidRetry(t *testing.T) {
	cfg := Config{MaxAttempts: 3, InitialBackoff: 50 * time.Millisecond, Multiplier: 1, Jitter: false}
	ctx, cancel := context.WithCancel(context.Background())
	// 第 1 次失败触发退避，期间取消 ctx。
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	r := &attemptsRecorder{errs: map[int]error{1: errFirst, 2: errSecond, 3: errThird}}
	err := cfg.Do(ctx, r.fn)
	if err == nil {
		t.Fatalf("expected non-nil error on context cancel, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected error to wrap context.Canceled, got %v", err)
	}
}

func TestBatchDo_MixedSuccessAndFailure(t *testing.T) {
	cfg := Config{MaxAttempts: 2, InitialBackoff: 0, Multiplier: 1, Jitter: false}
	type item struct{ V int }
	// task1 全程失败，task2 首试即成功，task3 第1次失败第2次成功。
	tasks := []BatchTask[item]{
		{ID: "t1", Input: item{V: 1}, FailOn: []int{1, 2}},
		{ID: "t2", Input: item{V: 2}, FailOn: nil},
		{ID: "t3", Input: item{V: 3}, FailOn: []int{1}},
	}
	runner := func(_ context.Context, attempt int, task BatchTask[item]) (item, error) {
		for _, a := range task.FailOn {
			if a == attempt {
				return item{}, errors.New("synthetic fail for " + task.ID)
			}
		}
		return task.Input, nil
	}
	out, jerr := BatchDo(context.Background(), cfg, tasks, runner)
	if len(out) != 3 {
		t.Fatalf("expected 3 outcomes (one per task), got %d", len(out))
	}
	byID := map[string]BatchOutcome[item]{}
	for _, o := range out {
		byID[o.ID] = o
	}
	if byID["t1"].Ok {
		t.Fatalf("t1 should have failed")
	}
	if byID["t1"].Err == nil || !strings.Contains(byID["t1"].Err.Error(), "t1") {
		t.Fatalf("t1 failure should carry an error mentioning t1, got %v", byID["t1"].Err)
	}
	if !byID["t2"].Ok {
		t.Fatalf("t2 should have succeeded")
	}
	if byID["t2"].Result.V != 2 {
		t.Fatalf("t2 result mismatch: %v", byID["t2"].Result)
	}
	if !byID["t3"].Ok {
		t.Fatalf("t3 should have succeeded after retry")
	}
	if jerr == nil {
		t.Fatalf("expected joined error since t1 failed")
	}
}
