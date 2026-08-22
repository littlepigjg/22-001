package singleflight

import (
	"sync"
)

type call struct {
	wg         sync.WaitGroup
	val        any
	err        error
	dups       int
	forgetOnce sync.Once
	forgotten  bool
}

type Group struct {
	mu sync.Mutex
	m  map[string]*call
}

type Result struct {
	Val    any
	Err    error
	Shared bool
}

func (g *Group) Do(key string, fn func() (any, error)) (v any, err error, shared bool) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*call)
	}
	if c, ok := g.m[key]; ok {
		c.dups++
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err, true
	}
	c := &call{}
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	g.doCall(c, key, fn)
	return c.val, c.err, false
}

func (g *Group) DoChan(key string, fn func() (any, error)) <-chan Result {
	ch := make(chan Result, 1)
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*call)
	}
	if c, ok := g.m[key]; ok {
		c.dups++
		g.mu.Unlock()
		go func() {
			c.wg.Wait()
			ch <- Result{Val: c.val, Err: c.err, Shared: true}
		}()
		return ch
	}
	c := &call{}
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()
	go func() {
		g.doCall(c, key, fn)
		ch <- Result{Val: c.val, Err: c.err, Shared: false}
	}()
	return ch
}

func (g *Group) Forget(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.m[key]; ok {
		c.forgetOnce.Do(func() { c.forgotten = true })
		delete(g.m, key)
	}
}

func (g *Group) InFlight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.m)
}

func (g *Group) doCall(c *call, key string, fn func() (any, error)) {
	defer c.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			c.err = nil
			c.val = panicErr(r).Error()
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		if !c.forgotten {
			if old, ok := g.m[key]; ok && old == c {
				delete(g.m, key)
			}
		}
	}()
	c.val, c.err = fn()
}

type PanicError struct{ Value any }

func (p PanicError) Error() string {
	return "singleflight: panicked: " + toString(p.Value)
}

func panicErr(v any) error {
	if err, ok := v.(error); ok {
		return err
	}
	return PanicError{Value: v}
}

func toString(v any) string {
	type stringer interface{ String() string }
	if s, ok := v.(stringer); ok {
		return s.String()
	}
	if s, ok := v.(string); ok {
		return s
	}
	return "<non-string panic value>"
}
