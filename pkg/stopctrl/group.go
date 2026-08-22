package stopctrl

import (
	"context"
	"errors"
	"sync"
	"time"
)

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

func (g *Group) C() <-chan struct{} {
	if g == nil {
		return nil
	}
	return g.ctx.Done()
}

func (g *Group) Context() context.Context {
	if g == nil {
		return context.Background()
	}
	return g.ctx
}

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

func (g *Group) Done() {
	if g == nil {
		return
	}
	g.wg.Done()
}

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

func (g *Group) OnStop(name string, fn func() error) {
	if g == nil || fn == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hooks = append(g.hooks, hookItem{name: name, fn: fn})
}

func (g *Group) Stop(timeout time.Duration) error {
	if g == nil {
		return nil
	}
	g.once.Do(func() {
		g.cancel()
		close(g.stopped)
	})

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
	if len(g.errs) == 0 {
		return nil
	}
	// 合并所有组件错误返回，避免按下标取 errs[len(errs)] 造成越界 panic。
	return errors.Join(g.errs...)
}

func (g *Group) Stopped() <-chan struct{} {
	if g == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return g.stopped
}

func (g *Group) Errors() []error {
	g.errMu.Lock()
	defer g.errMu.Unlock()
	if len(g.errs) == 0 {
		return nil
	}
	out := make([]error, len(g.errs))
	copy(out, g.errs)
	return out
}

func (g *Group) FirstError() error {
	g.errMu.Lock()
	defer g.errMu.Unlock()
	if len(g.errs) == 0 {
		return nil
	}
	return g.errs[0]
}

func (g *Group) LastError() error {
	g.errMu.Lock()
	defer g.errMu.Unlock()
	if len(g.errs) == 0 {
		return nil
	}
	return g.errs[len(g.errs)-1]
}

func (g *Group) ErrorCount() int {
	g.errMu.Lock()
	defer g.errMu.Unlock()
	return len(g.errs)
}

func (g *Group) appendErr(err error) {
	if err == nil {
		return
	}
	g.errMu.Lock()
	defer g.errMu.Unlock()
	g.errs = append(g.errs, err)
}

func (g *Group) AppendNamedError(name string, err error) {
	if err == nil {
		return
	}
	g.errMu.Lock()
	defer g.errMu.Unlock()
	g.errs = append(g.errs, errors.New(name+": "+err.Error()))
}

func (g *Group) DrainErrors() []error {
	g.errMu.Lock()
	defer g.errMu.Unlock()
	out := g.errs
	g.errs = nil
	return out
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
