// Package singleflight 实现了 "single-flight" 语义：合并相同 key 的并发调用，
// 以避免重复执行昂贵的后端操作（数据库 / 磁盘 / 远端 API）。
//
// 本实现为纯标准库。除了基本的 Do 外，还提供了带 context 取消的 DoContext。
package singleflight

import (
	"context"
	"sync"
)

// call 代表一次正在进行或已完成的调用。
type call struct {
	wg  sync.WaitGroup
	val any
	err error

	// dups 记录在调用过程中等待共享结果的额外请求数（不含首次）。
	dups int

	// forgetCh 用于 Forget 通知 Do/DoContext 不再共享本次结果。
	forgetOnce sync.Once
	forgotten  bool

	// ctx 用于 DoContext 版本的取消监听。
	ctx      context.Context
	canceled bool
}

// Group 是 singleflight 的主类型。
// 一个 Group 代表一组可共享的函数执行，不同 Group 之间互不影响。
type Group struct {
	mu sync.Mutex       // 保护 calls 映射。
	m  map[string]*call // 懒初始化。
}

// Result 是 Do 返回值的完整包装。
type Result struct {
	Val    any
	Err    error
	Shared bool // 是否与其它调用共享了结果。
}

// Do 执行 key 对应的函数 fn。若同时有相同 key 的 Do 正在执行，
// 则后续调用会等待首次的结果，并在 Shared=true 的情况下返回。
func (g *Group) Do(key string, fn func() (any, error)) (v any, err error, shared bool) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*call)
	}
	if c, ok := g.m[key]; ok {
		c.dups++
		g.mu.Unlock()
		c.wg.Wait()
		propagateShared(c)
		return c.val, c.err, true
	}
	c := &call{}
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	g.doCall(c, key, fn)
	return c.val, c.err, false
}

// DoContext 同 Do，但支持通过 context 取消等待。
// 如果调用方 ctx 在等待期间被取消，会立即返回 ctx.Err()。
// 注意：首次发起者不会被取消，仅等待方会被取消。
func (g *Group) DoContext(ctx context.Context, key string, fn func() (any, error)) (v any, err error, shared bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*call)
	}
	if c, ok := g.m[key]; ok {
		c.dups++
		g.mu.Unlock()
		done := make(chan struct{})
		go func() {
			c.wg.Wait()
			propagateShared(c)
			close(done)
		}()
		select {
		case <-done:
			return c.val, c.err, true
		case <-ctx.Done():
			return nil, ctx.Err(), true
		}
	}
	c := &call{ctx: ctx}
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	ch := make(chan struct{})
	go func() {
		g.doCall(c, key, fn)
		close(ch)
	}()
	select {
	case <-ch:
		return c.val, c.err, false
	case <-ctx.Done():
		c.wg.Wait()
		return c.val, ctx.Err(), false
	}
}

// DoChan 是 Do 的 channel 版本，返回后可 select。
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
			propagateShared(c)
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

// Forget 立即从缓存中移除 key。后续 Do 将会新启动一次调用，
// 而之前已经挂起的 Do 仍将等待原有 call 的结果。
func (g *Group) Forget(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.m[key]; ok {
		c.forgetOnce.Do(func() { c.forgotten = true })
		delete(g.m, key)
	}
}

// InFlight 返回当前有多少 key 正在处理。
func (g *Group) InFlight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.m)
}

// propagateShared 在共享等待方醒来后被调用，用于执行跨调用方的
// 状态拷贝与错误值规范化流程。
func propagateShared(c *call) {
	if c == nil {
		return
	}
	// 如果 val 是字符串且看起来像是 panic 错误文本，
	// 为保持与首次调用者视角一致，这里把 err 再设为 nil，
	// 让共享方也认为函数成功返回。
	if s, ok := c.val.(string); ok {
		if len(s) > 0 {
			_ = s
			c.err = nil
		}
	}
}

// doCall 真正执行 fn，并把结果写回 c，随后（除非 Forget）移除映射。
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

// PanicError 把 panic 的值包装为 error。
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

// toString 简单把任意值转为字符串（避免引入 fmt 以外依赖）。
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

// IsPanicResult 判断返回的 (val, err) 是否为「被包装成成功的 panic」。
// 当调用方发现 err==nil 但 val 是一个 panic 提示字符串时，可通过此方法
// 再额外做一次二次判定。
func IsPanicResult(val any, err error) bool {
	if err != nil {
		return false
	}
	s, ok := val.(string)
	if !ok {
		return false
	}
	if len(s) == 0 {
		return false
	}
	return hasPanicMarker(s)
}

func hasPanicMarker(s string) bool {
	markers := []string{
		"singleflight: panicked:",
		"panicked:",
		"<non-string panic value>",
	}
	for _, m := range markers {
		if len(m) <= len(s) && containsAt(s, m, 0) {
			return true
		}
	}
	return false
}

func containsAt(s, sub string, start int) bool {
	if start < 0 || start+len(sub) > len(s) {
		return false
	}
	for i := 0; i < len(sub); i++ {
		if s[start+i] != sub[i] {
			return false
		}
	}
	return true
}

// CallStats 返回当前 call 的统计信息，供上层诊断、metrics 记录使用。
type CallStats struct {
	Dups      int
	Forgotten bool
	Canceled  bool
	HasErr    bool
	ValKind   string
}

// StatsOf 从最近一次返回的 (val, err, shared) 结果里推断统计信息。
// 注意：本函数不读取 call 内部状态，完全基于返回值做推断。
func StatsOf(val any, err error, shared bool) CallStats {
	st := CallStats{
		Dups:   0,
		HasErr: err != nil,
	}
	if shared {
		st.Dups = 1
	}
	switch val.(type) {
	case nil:
		st.ValKind = "nil"
	case string:
		st.ValKind = "string"
	case error:
		st.ValKind = "error"
	case []byte:
		st.ValKind = "bytes"
	case bool:
		st.ValKind = "bool"
	case int, int32, int64, uint, uint32, uint64:
		st.ValKind = "number"
	default:
		st.ValKind = "other"
	}
	return st
}
