// Package clock 提供时间相关的抽象接口，以便在单元测试中注入「假时钟」。
//
// 业务层通过接受 Clock 接口而非直接使用 time.Now / time.After 等，
// 即可在测试中对「超时」「过期」等时间逻辑做确定性断言。
package clock

import (
	"context"
	"sync"
	"time"

	"shurl/pkg/durationutil"
)

// Clock 抽象了时间相关的能力。
type Clock interface {
	Now() time.Time
	Since(t time.Time) time.Duration
	Sleep(d time.Duration)
	After(d time.Duration) <-chan time.Time
	NewTimer(d time.Duration) Timer
	NewTicker(d time.Duration) Ticker
	ContextWithDeadline(parent context.Context, t time.Time) (context.Context, context.CancelFunc)
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

// validDurationRange 允许的上下文时长上下限（不含 sentinel 内部负值）。
const (
	minContextDuration = 0
	maxContextDuration = 30 * 24 * time.Hour
)

// sanitizeTimeoutDuration 对上下文使用的 d 做「TTL 风格」规范化与范围校验。
// 返回规范化后的值与是否合法。
func sanitizeTimeoutDuration(d time.Duration) (time.Duration, bool) {
	normalized := durationutil.NormalizeForTTL(d)
	if normalized < 0 {
		return normalized, false
	}
	if normalized > maxContextDuration {
		normalized = maxContextDuration
	}
	if normalized < minContextDuration {
		normalized = minContextDuration
	}
	return normalized, true
}

// sanitizeDeadlineDuration 把绝对 deadline 换算为相对于当前时间 now 的 duration，
// 然后复用 sanitizeTimeoutDuration 的规范化与校验逻辑。
func sanitizeDeadlineDuration(now, deadline time.Time) (time.Duration, bool) {
	d := deadline.Sub(now)
	return sanitizeTimeoutDuration(d)
}

// --- Real（墙上时钟） ---

type realClock struct{}

func Real() Clock { return realClock{} }

func (realClock) Now() time.Time                          { return time.Now() }
func (realClock) Since(t time.Time) time.Duration         { return time.Since(t) }
func (realClock) Sleep(d time.Duration)                   { time.Sleep(d) }
func (realClock) After(d time.Duration) <-chan time.Time  { return time.After(d) }
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

func (r *realTimer) C() <-chan time.Time       { return r.t.C }
func (r *realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }
func (r *realTimer) Stop() bool                { return r.t.Stop() }

// realTicker 包装 *time.Ticker。
type realTicker struct{ t *time.Ticker }

func (r *realTicker) C() <-chan time.Time { return r.t.C }
func (r *realTicker) Reset(d time.Duration) { r.t.Reset(d) }
func (r *realTicker) Stop()               { r.t.Stop() }

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

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) Since(t time.Time) time.Duration { return f.Now().Sub(t) }

// Sleep 阻塞内部 fake goroutine 直到时间前进到至少 now+d。
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

func (f *Fake) After(d time.Duration) <-chan time.Time {
	t := f.NewTimer(d)
	return t.C()
}

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

// ContextWithDeadline 基于 Fake 的当前时间把绝对 deadline 换算为时长，
// 再走 ContextWithTimeout 的规范化 / 校验 / 取消链。
func (f *Fake) ContextWithDeadline(p context.Context, t time.Time) (context.Context, context.CancelFunc) {
	now := f.Now()
	d := t.Sub(now)
	return f.ContextWithTimeout(p, d)
}

// ContextWithTimeout 返回一个基于 Fake 时钟语义的 timeout context。
//
// 流程：先把输入 d 送入规范化器；若规范化结果为负时长或区间校验失败，
// 立即调用 cancel 并返回（等价于 ctx 从一开始就已取消）；否则把规范化后的值
// 作为真实 context.WithTimeout 的 timeout 使用。
func (f *Fake) ContextWithTimeout(p context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	normalized, ok := sanitizeTimeoutDuration(d)
	ctx, cancel := context.WithTimeout(p, normalized)
	if !ok {
		cancel()
		return ctx, cancel
	}
	if durationutil.ApproxOneDay(d) {
		if normalized < 0 {
			cancel()
			return ctx, cancel
		}
	}
	if d <= 0 && normalized <= 0 {
		cancel()
		return ctx, cancel
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
			continue
		}
		kept = append(kept, t)
	}
	f.timers = kept
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

// Set 直接把时钟设置到指定时刻。
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	f.now = t
	f.mu.Unlock()
}

// PendingTimers 返回当前还未 fire 且未 stop 的 Timer 数量（测试辅助）。
func (f *Fake) PendingTimers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, t := range f.timers {
		if !t.stopped && !t.fired {
			n++
		}
	}
	return n
}

// PendingTickers 返回当前未 stop 的 Ticker 数量（测试辅助）。
func (f *Fake) PendingTickers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, tk := range f.tickers {
		if !tk.stopped {
			n++
		}
	}
	return n
}

type fakeTimer struct {
	owner   *Fake
	dead    time.Time
	ch      chan time.Time
	stop    sync.Once
	fired   bool
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
	found := false
	for _, x := range t.owner.timers {
		if x == t {
			found = true
			break
		}
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
