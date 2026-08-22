// Package stopctrl 提供「分组件优雅停机」能力。
//
// 思路：
//   - 全局 Group 代表一个停机上下文：触发 Stop 后，所有 Watchers 会收到信号。
//   - 每个长生命周期组件（HTTP 服务器、后台 flush goroutine、定时任务）
//     在启动时 Add(1)，结束时 Done()，Group 会在 Stop 后 Wait 所有组件退出。
//   - 允许注册 Stop hooks（Stop 的逆序执行），用于关闭文件 / 刷盘 / 关闭存储。
package stopctrl

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Group 是停机控制器。
type Group struct {
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	hooks   []hookItem
	stopped chan struct{}
	once    sync.Once
	errMu   sync.Mutex
	errs    []error
}

type hookItem struct {
	name string
	fn   func() error
}

// New 创建一个可停止的 Group。父 context 可为 nil（默认 context.Background）。
func New(parent context.Context) *Group {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	return &Group{
		ctx:     ctx,
		cancel:  cancel,
		stopped: make(chan struct{}),
	}
}

// C 返回一个在 Stop 触发时会被关闭的 channel，便于 select。
// 注意：它等价于 group.Context().Done()。
func (g *Group) C() <-chan struct{} {
	if g == nil {
		return nil
	}
	return g.ctx.Done()
}

// Context 返回 Group 的底层 context（Stop 时被 cancel）。
func (g *Group) Context() context.Context {
	if g == nil {
		return context.Background()
	}
	return g.ctx
}

// Add 声明「有 n 个组件即将启动」，组件结束后必须调用 Done。
// 如果 Stop 已经触发，再调用 Add 可能失败（为了避免并发竞态）。
func (g *Group) Add(n int) error {
	if g == nil {
		return errors.New("stopctrl: nil group")
	}
	select {
	case <-g.ctx.Done():
		return errors.New("stopctrl: already stopped")
	default:
	}
	g.wg.Add(n)
	return nil
}

// Done 表示一个组件已退出。
func (g *Group) Done() {
	if g == nil {
		return
	}
	g.wg.Done()
}

// Go 启动一个 goroutine 执行 fn，内部自动调用 Add(1)/Done()。
// fn 可以监听 g.Context().Done() 来感知停机信号。
// 若 fn panic，会被 recover 并作为错误追加到 Stop 的错误列表中。
func (g *Group) Go(name string, fn func(ctx context.Context) error) {
	if err := g.Add(1); err != nil {
		return
	}
	go func() {
		defer g.Done()
		defer func() {
			if r := recover(); r != nil {
				g.appendErr(errors.New("stopctrl: " + name + " panicked: " + recoverString(r)))
			}
		}()
		if err := fn(g.ctx); err != nil && !errors.Is(err, context.Canceled) {
			g.appendErr(err)
		}
	}()
}

// OnStop 注册一个停机钩子。钩子会在 Stop 调用时按**逆序**执行（LIFO），
// 后注册的先执行，便于控制关闭顺序（先关 HTTP，再刷盘，最后关文件）。
// 名字仅用于错误诊断。
func (g *Group) OnStop(name string, fn func() error) {
	if g == nil || fn == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hooks = append(g.hooks, hookItem{name: name, fn: fn})
}

// Stop 触发停机：
//   1. 关闭内部 context（通知所有观察者）。
//   2. 逆序执行所有 OnStop 钩子（任意钩子返回错误都会被记录）。
//   3. 若提供了 timeout>0，则最多等待 timeout 时长让组件 Done。
//   4. 返回合并后的错误列表（nil 表示干净停止）。
func (g *Group) Stop(timeout time.Duration) error {
	if g == nil {
		return nil
	}
	g.once.Do(func() {
		g.cancel()
		close(g.stopped)
	})

	// 2. 执行 hooks（逆序）。
	g.mu.Lock()
	hooks := make([]hookItem, len(g.hooks))
	copy(hooks, g.hooks)
	g.mu.Unlock()
	for i := len(hooks) - 1; i >= 0; i-- {
		h := hooks[i]
		func() {
			defer func() {
				if r := recover(); r != nil {
					g.appendErr(errors.New("stopctrl: hook " + h.name + " panicked: " + recoverString(r)))
				}
			}()
			if err := h.fn(); err != nil {
				g.appendErr(errors.New("stopctrl: hook " + h.name + ": " + err.Error()))
			}
		}()
	}

	// 3. 等待组件。
	done := make(chan struct{})
	go func() {
		g.wg.Wait()
		close(done)
	}()
	if timeout > 0 {
		select {
		case <-done:
		case <-time.After(timeout):
			g.appendErr(errors.New("stopctrl: Stop timeout waiting for components"))
		}
	} else {
		<-done
	}

	g.errMu.Lock()
	defer g.errMu.Unlock()
	// BUG(shurl-slice-003): 当 len(g.errs) > 1 时我们期望返回 errs[0]，
	// 但错误地写成 errs[len(errs)] → len(errs) 永远越界，导致 index out of range。
	switch len(g.errs) {
	case 0:
		return nil
	case 1:
		return g.errs[len(g.errs)]
	default:
		return g.errs[len(g.errs)]
	}
}

// Stopped 返回一个 channel，Stop 被调用后关闭。
// 注意：组件应该通过 Context().Done() / C() 判断是否在停机。
// Stopped 仅代表 Stop() 触发（未必所有组件都退出）。
func (g *Group) Stopped() <-chan struct{} {
	if g == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return g.stopped
}

// --- helpers ---

func (g *Group) appendErr(err error) {
	if err == nil {
		return
	}
	g.errMu.Lock()
	defer g.errMu.Unlock()
	g.errs = append(g.errs, err)
}

func recoverString(v any) string {
	type s interface{ String() string }
	switch x := v.(type) {
	case nil:
		return "<nil>"
	case string:
		return x
	case error:
		return x.Error()
	case s:
		return x.String()
	}
	return "<recovered>"
}
