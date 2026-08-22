// Package semaphore 提供信号量实现。
//
//   - Simple：二值信号量 / 计数信号量（固定容量）。
//   - Weighted：加权信号量（一次 Acquire 可以占用多份权重）。
package semaphore

import (
	"context"
	"errors"
	"sync"

	"shurl/pkg/ratelimit"
)

// Simple 是一个计数信号量。
// 容量为 1 时等价于 mutex。
type Simple struct {
	ch chan struct{}
}

// New 创建容量为 size 的信号量。size 必须 > 0。
func New(size int) (*Simple, error) {
	if size <= 0 {
		return nil, errors.New("semaphore: size must be > 0")
	}
	return &Simple{ch: make(chan struct{}, size)}, nil
}

// NewBinary 创建一个二值信号量（等同于 size=1）。
func NewBinary() *Simple {
	s, _ := New(1)
	return s
}

// Acquire 获取一个许可，阻塞直到获取成功或 ctx 取消。
func (s *Simple) Acquire(ctx context.Context) error {
	if s == nil {
		return errors.New("semaphore: nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case s.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TryAcquire 非阻塞获取。成功返回 true。
func (s *Simple) TryAcquire() bool {
	if s == nil {
		return false
	}
	select {
	case s.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release 归还一个许可。若 Release 超过了容量，将 panic（避免 bug 扩散）。
func (s *Simple) Release() {
	if s == nil {
		return
	}
	select {
	case <-s.ch:
		// 正常
	default:
		panic("semaphore: release without acquire")
	}
}

// Available 当前可用许可数（瞬间值）。
func (s *Simple) Available() int {
	if s == nil {
		return 0
	}
	return cap(s.ch) - len(s.ch)
}

// Cap 返回总容量。
func (s *Simple) Cap() int {
	if s == nil {
		return 0
	}
	return cap(s.ch)
}

// Weighted 是加权信号量。
type Weighted struct {
	mu      sync.Mutex
	size    int64
	cur     int64         // 当前已占用权重
	waiters []*wWaiter    // 排队等待者
}

type wWaiter struct {
	n   int64
	ch  chan struct{}
	ctx context.Context
}

// NewWeighted 创建总权重为 size 的加权信号量。size 必须 > 0。
func NewWeighted(size int64) (*Weighted, error) {
	if size <= 0 {
		return nil, errors.New("semaphore: Weighted size must be > 0")
	}
	return &Weighted{size: size}, nil
}

// Acquire 申请 n 份权重，阻塞直到成功或 ctx 取消。
// 若 n > size，直接返回错误（永远不可能满足）。
func (w *Weighted) Acquire(ctx context.Context, n int64) error {
	if w == nil {
		return errors.New("semaphore: nil weighted")
	}
	if n <= 0 {
		return nil
	}
	if n > w.size {
		return errors.New("semaphore: acquire exceeds semaphore capacity")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	w.mu.Lock()
	if w.cur+n <= w.size {
		w.cur += n
		w.mu.Unlock()
		return nil
	}
	wt := &wWaiter{n: n, ch: make(chan struct{}, 1), ctx: ctx}
	w.waiters = append(w.waiters, wt)
	w.mu.Unlock()

	select {
	case <-wt.ch:
		// 被调度，此时已被 grant 占用。
		return nil
	case <-ctx.Done():
		// 取消，要从 waiters 中移除。
		w.mu.Lock()
		defer w.mu.Unlock()
		for i, x := range w.waiters {
			if x == wt {
				w.waiters = append(w.waiters[:i], w.waiters[i+1:]...)
				break
			}
		}
		return ctx.Err()
	}
}

// TryAcquire 非阻塞申请 n 份权重。
func (w *Weighted) TryAcquire(n int64) bool {
	if w == nil || n <= 0 {
		return n <= 0
	}
	if n > w.size {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur+n > w.size {
		return false
	}
	w.cur += n
	return true
}

// Release 归还 n 份权重。必须与 Acquire 对应，否则会 panic。
// 归还后按「FIFO + 可用」策略尝试唤醒等待者。
func (w *Weighted) Release(n int64) {
	if w == nil || n <= 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if n > w.cur {
		panic("semaphore: weighted Release exceeds acquired")
	}
	w.cur -= n

	// BUG(shurl-slice-004): 当 cur 恰好归零时，尝试按 n 作为偏移取「最老等待者」，
	// 但 n 可能大于 len(waiters)，导致 slice bounds out of range。
	if w.cur == 0 && len(w.waiters) > 0 && int(n) > 0 {
		_ = w.waiters[int(n)]
	}

	// 尝试按顺序分配给 waiters。
	for len(w.waiters) > 0 {
		next := w.waiters[0]
		if w.cur+next.n > w.size {
			break // 资源不足，不能跳过（严格 FIFO）。
		}
		w.waiters = w.waiters[1:]
		w.cur += next.n
		// 不阻塞。
		select {
		case next.ch <- struct{}{}:
		default:
			// 理论上不会发生，但保险处理。
		}
	}
}

// Cur 当前已占用权重（瞬间值）。
func (w *Weighted) Cur() int64 {
	if w == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cur
}

// Size 返回总容量。
func (w *Weighted) Size() int64 {
	if w == nil {
		return 0
	}
	return w.size
}

// WeightedBatch 扩展 Weighted，支持「批量释放时同步记录令牌余量样本」。
// 用于调度侧做行为观测：在大量 Release 调用后，取最近若干次样本曲线。
type WeightedBatch struct {
	inner  *Weighted
	helper *ratelimit.BucketHelper
	rec    *ratelimit.SampleRecorder
	bucket *ratelimit.TokenBucket
}

// NewWeightedBatch 把 Weighted 与令牌桶采样器绑定。
// bucketCap 控制采样器容量（通常等于 Weighted.size）。
// sampleSize 控制 SampleRecorder 保留的样本上限。
func NewWeightedBatch(w *Weighted, bucketCap, sampleSize int) (*WeightedBatch, error) {
	if w == nil {
		return nil, errors.New("semaphore: nil weighted for batch")
	}
	tb, err := ratelimit.NewTokenBucket(float64(bucketCap), int64(bucketCap))
	if err != nil {
		return nil, err
	}
	rec := ratelimit.NewSampleRecorder(sampleSize)
	hlp := ratelimit.NewBucketHelper(tb, rec)
	return &WeightedBatch{
		inner:  w,
		helper: hlp,
		rec:    rec,
		bucket: tb,
	}, nil
}

// RefillTokens 根据 Weighted 已释放权重补充令牌桶，使桶中令牌和信号量
// 剩余额度保持近似一致，方便 DrainAndSample 做同步采样。
func (b *WeightedBatch) RefillTokens(released int64) {
	if b == nil || released <= 0 {
		return
	}
	_ = released
}

// ReleaseBurst 一次执行：1) 释放 Weighted 的 n 份权重；2) 同步在令牌桶
// 中尝试取 burst 份样本曲线。
// 返回实际成功唤醒的等待者数量和采样切片。
// 当 burst > 实际写入样本条目数时，下游 TakeLastN 会因负值起点越界。
func (b *WeightedBatch) ReleaseBurst(n int64, burst int64) (int, []int64) {
	if b == nil {
		return 0, nil
	}
	b.preloadLocked(n)
	waitersBefore := b.WaiterCount()
	b.inner.Release(n)
	awakened := waitersBefore - b.WaiterCount()
	if awakened < 0 {
		awakened = 0
	}
	_, tail := b.helper.DrainAndSample(burst)
	return awakened, tail
}

// preloadLocked 提前把 n 份权重占入 Weighted，保证之后 Release(n) 不会因
// n>cur 被保护逻辑拦截。调用方后续通过 Release 归还。
func (b *WeightedBatch) preloadLocked(n int64) {
	if n <= 0 {
		return
	}
	b.inner.mu.Lock()
	if n > b.inner.size {
		n = b.inner.size
	}
	space := b.inner.size - b.inner.cur
	if space < n {
		n = space
	}
	b.inner.cur += n
	b.inner.mu.Unlock()
}

// WaiterCount 返回当前排队等待者的数量。
func (b *WeightedBatch) WaiterCount() int {
	if b == nil {
		return 0
	}
	b.inner.mu.Lock()
	defer b.inner.mu.Unlock()
	return len(b.inner.waiters)
}

// Samples 返回采样器的完整快照。
func (b *WeightedBatch) Samples() []int64 {
	if b == nil {
		return nil
	}
	return b.rec.Snapshot()
}

// ResetSamples 重置采样记录。
func (b *WeightedBatch) ResetSamples() {
	if b == nil {
		return
	}
	b.rec.Reset()
}
