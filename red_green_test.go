package shurl

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"shurl/internal/admin"
	"shurl/internal/model"
	"shurl/pkg/logger"
	"shurl/pkg/workerpool"
)

func submitFailingTasks(t *testing.T, failCount int) []error {
	t.Helper()
	ctx := context.Background()
	pool, err := workerpool.New(2)
	if err != nil {
		t.Fatalf("create pool failed: %v", err)
	}
	if err := pool.Start(ctx); err != nil {
		t.Fatalf("start pool failed: %v", err)
	}
	for i := 0; i < failCount; i++ {
		name := fmt.Sprintf("task-%d", i+1)
		idx := i
		err := pool.Submit(workerpool.Task{
			Name: name,
			Fn: func(_ context.Context) error {
				return fmt.Errorf("simulated failure #%d", idx+1)
			},
		})
		if err != nil {
			t.Fatalf("submit task %s failed: %v", name, err)
		}
	}
	_ = pool.Stop()
	return pool.Errors()
}

func containsIndexOutOfRange(s string) bool {
	return strings.Contains(s, "index out of range") ||
		strings.Contains(s, "runtime error: index out of range")
}

func TestRedGreen(t *testing.T) {
	panicked := false
	panicMsg := ""
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				switch v := r.(type) {
				case error:
					panicMsg = v.Error()
				case string:
					panicMsg = v
				default:
					panicMsg = fmt.Sprintf("%v", v)
				}
			}
		}()
		errs := submitFailingTasks(t, 5)
		_ = model.CompactErrors(errs)
		_ = model.DeduplicateErrors(errs)
		_ = model.FlattenErrors(errs)
		_ = model.ReportErrors(errs)
		_ = workerpool.LenientErrorCount(errs)
		_ = workerpool.FirstErr(errs)
		_ = workerpool.LastErr(errs)
		_ = workerpool.ExtractTaskNames(errs)
		svc := admin.New(&logger.Logger{})
		_, _ = svc.RunTaskDiagnostics(context.Background())
	}()

	if panicked {
		if containsIndexOutOfRange(panicMsg) || strings.Contains(panicMsg, "error calling") {
			fmt.Println("RED（红灯，缺陷未修复）")
			t.Logf("captured panic (slice index out of range): %s", panicMsg)
			t.Errorf("RED: slice index out of range panic: %s", panicMsg)
			return
		}
		fmt.Println("RED（红灯，缺陷未修复）")
		t.Logf("captured unexpected panic: %s", panicMsg)
		t.Errorf("RED: unexpected panic: %s", panicMsg)
		return
	}

	fmt.Println("GREEN（绿灯，缺陷已修复）")
	t.Log("no slice index-out-of-range panic observed; chain completed normally")
}
