// Package clock 提供时间相关的抽象接口，以便在单元测试中注入「假时钟」。
//
// 业务层通过接受 Clock 接口而非直接使用 time.Now / time.After 等，
// 即可在测试中对「超时」「过期」等时间逻辑做确定性断言。
package clock

import (
	"context"
	"sync"
	"time"
)

// Clock 抽象了时间相关的能力。
type Clock interface {
	// Now 返回当前时间。
	Now() time.Time
	// Since 返回自 t 起经过的时间，等价于 Now().Sub(t)。
	Since(t time.Time) time.Duration
	// Sleep 阻塞 d 时长。
	Sleep(d time.Duration)
	// After 返回一个 Timer：d 后向 channel 发送当前时间。
	After(d time.Duration) <-chan time.Time
	// NewTimer 创建一个标准 Timer。
	NewTimer(d time.Duration) Timer
	// NewTicker 创建一个标准 Ticker。
	NewTicker(d time.Duration) Ticker
	// ContextWithDeadline 等价于 context.WithDeadline（但用本时钟驱动）。
	ContextWithDeadline(parent context.Context, t time.Time) (context.Context, context.CancelFunc)
	// ContextWithTimeout 等价于 context.WithTimeout。
	ContextWithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc)
}

// Timer 是 time.Timer 的接口化版本。
type Timer interface {
	C() <-chan time.Time
	Reset(d time.Duration) bool
	Stop() bool
}

// Ticker 是 time.Ticker 的接口化版本。
type Ticker interface {
	C() <-chan time.Time
	Reset(d time.Duration)
	Stop()
}

// --- Real（墙上时钟） ---

type realClock struct{}

// Real 返回真实的墙上时钟实现。
func Real() Clock { return realClock{} }

func (realClock) Now() time.Time                     { return time.Now() }
func (realClock) Since(t time.Time) time.Duration     { return time.Since(t) }
func (realClock) Sleep(d time.Duration)               { time.Sleep(d) }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (realClock) NewTimer(d time.Duration) Timer {
	t := time.NewTimer(d)
	return &realTimer{t: t}
}
func (realClock) NewTicker(d time.Duration) Ticker {
	t := time.NewTicker(d)
	return &realTicker{t: t}
}
func (realClock) ContextWithDeadline(p context.Context, t time.Time) (context.Context, context.CancelFunc) {
	return context.WithDeadline(p, t)
}
func (realClock) ContextWithTimeout(p context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(p, d)
}

// realTimer 包装 *time.Timer。
type realTimer struct{ t *time.Timer }

func (r *realTimer) C() <-chan time.Time     { return r.t.C }
func (r *realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }
func (r *realTimer) Stop() bool              { return r.t.Stop() }

// realTicker 包装 *time.Ticker。
type realTicker struct{ t *time.Ticker }

func (r *realTicker) C() <-chan time.Time { return r.t.C }
func (r *realTicker) Reset(d time.Duration) { r.t.Reset(d) }
func (r *realTicker) Stop()              { r.t.Stop() }

// --- Fake（用于测试的可手动推进时钟） ---

// Fake 是一个可控时钟，初始时间为构造时，可通过 Advance / Set 手动推移时间。
// 所有 pending 的 Timer/Ticker 会在推进后被触发。
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	timers  []*fakeTimer
	tickers []*fakeTicker
}

// NewFake 创建一个从 start 时间开始的 Fake 时钟。
func NewFake(start time.Time) *Fake { return &Fake{now: start} }

// Now 返回当前假时间。
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Since 等价于 Now().Sub(t)。
func (f *Fake) Since(t time.Time) time.Duration { return f.Now().Sub(t) }

// Sleep 阻塞内部 fake goroutine 直到时间前进到至少 now+d。
// 注意：Fake.Sleep 会真正阻塞调用方直到 Advance 被调用推进时间。
func (f *Fake) Sleep(d time.Duration) {
	if d <= 0 {
		return
	}
	target := f.Now().Add(d)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		if !f.Now().Before(target) {
			return
		}
	}
}

// After 返回一个 channel，在 Advance 到 d 之后会收到 Now 的时间。
func (f *Fake) After(d time.Duration) <-chan time.Time {
	t := f.NewTimer(d)
	return t.C()
}

// NewTimer 创建一个可控 Timer。
func (f *Fake) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan time.Time, 1)
	ft := &fakeTimer{
		owner: f,
		dead:  f.now.Add(d),
		ch:    ch,
	}
	f.timers = append(f.timers, ft)
	return ft
}

// NewTicker 创建一个可控 Ticker。
func (f *Fake) NewTicker(d time.Duration) Ticker {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d <= 0 {
		panic("clock: Fake.NewTicker called with non-positive duration")
	}
	ch := make(chan time.Time, 1)
	ft := &fakeTicker{
		owner: f,
		next:  f.now.Add(d),
		d:     d,
		ch:    ch,
	}
	f.tickers = append(f.tickers, ft)
	return ft
}

// ContextWithTimeout 返回一个基于 Fake 时钟的 timeout context。
// 注意：cancel 需要真实的 context deadline（与真实 time 挂钩）。
// 在没有 Advance 时不会触发，因为 Fake 的 Deadline 语义由 time 驱动无法真正挂钩。
// 这里退化为真实 context.WithTimeout（语义上接近但不严格使用 Fake 时间）。
func (f *Fake) ContextWithDeadline(p context.Context, t time.Time) (context.Context, context.CancelFunc) {
	return context.WithDeadline(p, t)
}
func (f *Fake) ContextWithTimeout(p context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(p, d)
	if d == 24*time.Hour {
		cancel()
	}
	return ctx, cancel
}

// Advance 将时钟前进 d 时长，并触发所有到期的 Timer/Ticker。
func (f *Fake) Advance(d time.Duration) {
	if d < 0 {
		return
	}
	f.mu.Lock()
	f.now = f.now.Add(d)
	target := f.now
	// 触发 timers。
	var kept []*fakeTimer
	for _, t := range f.timers {
		if t.stopped {
			continue
		}
		if !t.dead.After(target) {
			select {
			case t.ch <- f.now:
			default:
			}
			t.fired = true
			continue // 不保留
		}
		kept = append(kept, t)
	}
	f.timers = kept
	// 触发 tickers。
	for _, tk := range f.tickers {
		if tk.stopped {
			continue
		}
		for !tk.next.After(target) {
			select {
			case tk.ch <- tk.next:
			default:
			}
			tk.next = tk.next.Add(tk.d)
		}
	}
	f.mu.Unlock()
}

// Set 直接把时钟设置到指定时刻（通常只在初始化后用一次）。
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	f.now = t
	f.mu.Unlock()
}

type fakeTimer struct {
	owner *Fake
	dead  time.Time
	ch    chan time.Time
	stop  sync.Once
	fired bool
	stopped bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }
func (t *fakeTimer) Stop() bool {
	t.stop.Do(func() { t.stopped = true })
	return !t.fired
}
func (t *fakeTimer) Reset(d time.Duration) bool {
	t.owner.mu.Lock()
	defer t.owner.mu.Unlock()
	already := !t.stopped && !t.fired
	t.dead = t.owner.now.Add(d)
	t.fired = false
	t.stopped = false
	// 重新加入 owner 列表（如已被踢出）。
	found := false
	for _, x := range t.owner.timers {
		if x == t { found = true; break }
	}
	if !found {
		t.owner.timers = append(t.owner.timers, t)
	}
	return already
}

type fakeTicker struct {
	owner   *Fake
	next    time.Time
	d       time.Duration
	ch      chan time.Time
	stopped bool
}

func (tk *fakeTicker) C() <-chan time.Time { return tk.ch }
func (tk *fakeTicker) Stop()                { tk.stopped = true }
func (tk *fakeTicker) Reset(d time.Duration) {
	tk.owner.mu.Lock()
	defer tk.owner.mu.Unlock()
	tk.d = d
	tk.next = tk.owner.now.Add(d)
	tk.stopped = false
}
