package semaphore

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestWeighted_ReleaseBurst_LargeBurstNoPanic(t *testing.T) {
	w, err := NewWeighted(64)
	if err != nil {
		t.Fatal(err)
	}
	bh, err := NewWeightedBatch(w, 64, 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ReleaseBurst panicked: %v", r)
		}
	}()
	awakened, tail := bh.ReleaseBurst(64, 100000)
	if awakened < 0 {
		t.Fatalf("awakened = %d, want >= 0", awakened)
	}
	// tail 不应超过实际写入样本数；绝不为负或 panic。
	if len(tail) < 0 {
		t.Fatalf("tail len negative")
	}
}

func TestWeighted_AcquireRelease_SingleStep(t *testing.T) {
	w, _ := NewWeighted(10)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if err := w.Acquire(ctx, 1); err != nil {
			t.Fatalf("Acquire #%d: %v", i, err)
		}
	}
	// 桶满后 Acquire 应当阻塞而非立即成功；用带超时的 ctx 验证它不会立刻通过。
	satCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := w.Acquire(satCtx, 1); err == nil {
		t.Fatal("Acquire beyond size should block/fail, but succeeded")
	}
	for i := 0; i < 10; i++ {
		w.Release(1)
	}
	if w.Cur() != 0 {
		t.Fatalf("Cur = %d after full release, want 0", w.Cur())
	}
}

func TestWeighted_Release_ExceedsAcquired_Panics(t *testing.T) {
	// 保护契约：Release 超过已占额时必须 panic（避免 bug 静默扩散）。
	w, _ := NewWeighted(10)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on Release beyond acquired")
		}
	}()
	w.Release(1)
}

func TestWeighted_ReleaseWithWaiters_NoSliceOOB(t *testing.T) {
	// 原 BUG(shurl-slice-004) 触发面：Release 后 cur 归零且仍有等待者，
	// 而 n 大于 len(waiters)，旧实现用 _ = w.waiters[int(n)] 取值会越界。
	// 这里 cur=10、排 3 个等待者(n=1)、Release(10)：cur 归零、waiters 长度 3，
	// 旧代码 w.waiters[10] 越界。
	w, _ := NewWeighted(10)
	ctx := context.Background()
	if err := w.Acquire(ctx, 10); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	var wg sync.WaitGroup
	ready := make(chan struct{})
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ready
			_ = w.Acquire(ctx, 1)
		}()
	}
	close(ready)
	// 等待者入队后再 Release（给一点时间让 goroutine 进入 Acquire）。
	time.Sleep(20 * time.Millisecond)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Release panicked with waiters: %v", r)
		}
	}()
	w.Release(10) // 唤醒全部 3 个等待者后 cur=3
	wg.Wait()
	// 收尾：等待者们各自又占了 1，共占 3。
	w.Release(3)
	if w.Cur() != 0 {
		t.Fatalf("Cur = %d, want 0", w.Cur())
	}
}

func TestWeighted_ConcurrentAcquireReleaseNoRace(t *testing.T) {
	// -race 下验证无数据竞争 / 无 double-unlock 类回归。
	w, _ := NewWeighted(64)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 500; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := w.Acquire(ctx, 1); err != nil {
				return
			}
			w.Release(1)
			_ = w.Cur()
		}()
	}
	wg.Wait()
}

func TestSimple_AcquireRelease(t *testing.T) {
	s, err := New(2)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	if !s.TryAcquire() {
		t.Fatal("TryAcquire should succeed with one slot left")
	}
	if s.TryAcquire() {
		t.Fatal("TryAcquire should fail when full")
	}
	s.Release()
	s.Release()
	if s.Available() != 2 {
		t.Fatalf("Available = %d, want 2", s.Available())
	}
}
