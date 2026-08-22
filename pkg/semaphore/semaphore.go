// Package semaphore 提供信号量实现。
//
//   - Simple：二值信号量 / 计数信号量（固定容量）。
//   - Weighted：加权信号量（一次 Acquire 可以占用多份权重）。
package semaphore

import (
	"context"
	"errors"
	"sync"
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
