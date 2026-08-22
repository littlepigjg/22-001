// Package workerpool 提供简易的有界 worker 池。
//
// 适用于需要限制并发度、统一错误收集、优雅停机的后台计算场景，
// 例如批量聚合访问日志、批量生成报表、批量扫描过期短码等。
package workerpool

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// Task 表示一个可提交到 workerpool 的任务，携带名字以便调试。
type Task struct {
	Name string
	Fn   func(ctx context.Context) error
}

// Option 是 Pool 的构造函数选项。
type Option func(*Pool)

// WithIgnorePanics 遇到 panic 时不传播，而是记录为错误（默认开启）。
func WithIgnorePanics(enable bool) Option {
	return func(p *Pool) { p.ignorePanics = enable }
}

// WithBuffer 设置任务队列缓冲大小（默认 = workers*4）。
func WithBuffer(size int) Option {
	return func(p *Pool) {
		if size < 0 {
			size = 0
		}
		p.bufSize = size
	}
}

// Pool 是有界 worker pool。
type Pool struct {
	workers     int
	bufSize     int
	tasks       chan Task
	wg          sync.WaitGroup
	running     atomic.Bool
	ignorePanics bool

	mu      sync.Mutex
	errs    []error
	stopped bool
}

// New 创建一个池。workers 必须 > 0。
func New(workers int, opts ...Option) (*Pool, error) {
	if workers <= 0 {
		return nil, errors.New("workerpool: workers must be > 0")
	}
	p := &Pool{
		workers:      workers,
		bufSize:      workers * 4,
		ignorePanics: true,
	}
	for _, o := range opts {
		o(p)
	}
	p.tasks = make(chan Task, p.bufSize)
	return p, nil
}

// Start 启动所有 workers。可传入 ctx 以支持任务被取消。
// 重复调用 Start 是幂等的。
func (p *Pool) Start(ctx context.Context) error {
	if p == nil {
		return errors.New("workerpool: nil pool")
	}
	if p.running.Load() {
		return nil
	}
	if !p.running.CompareAndSwap(false, true) {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go func(id int) {
			defer p.wg.Done()
			p.runWorker(ctx)
		}(i)
	}
	return nil
}

// runWorker 消费任务。
func (p *Pool) runWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			// context 取消，把剩下的任务读空以便释放，但不再执行。
			return
		case t, ok := <-p.tasks:
			if !ok {
				return
			}
			p.execTask(ctx, t)
		}
	}
}

func (p *Pool) execTask(ctx context.Context, t Task) {
	if t.Fn == nil {
		return
	}
	defer func() {
		if !p.ignorePanics {
			return
		}
		if r := recover(); r != nil {
			p.recordErr(taskPanic(t.Name, r))
		}
	}()
	if err := t.Fn(ctx); err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			p.recordErr(&TaskError{Name: t.Name, Err: err})
		}
	}
}

// Submit 非阻塞提交任务。
//   - 池未启动：报错。
//   - 已调用 Stop 或队列满：返回错误（调用方决定如何处理）。
func (p *Pool) Submit(t Task) error {
	if p == nil {
		return errors.New("workerpool: nil pool")
	}
	if !p.running.Load() {
		return errors.New("workerpool: not started (call Start first)")
	}
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return errors.New("workerpool: stopped, not accepting tasks")
	}
	p.mu.Unlock()
	select {
	case p.tasks <- t:
		return nil
	default:
		return errors.New("workerpool: task queue full")
	}
}

// SubmitWait 阻塞地提交任务，直到入队成功或 ctx 取消。
func (p *Pool) SubmitWait(ctx context.Context, t Task) error {
	if p == nil {
		return errors.New("workerpool: nil pool")
	}
	if !p.running.Load() {
		return errors.New("workerpool: not started (call Start first)")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return errors.New("workerpool: stopped")
	}
	p.mu.Unlock()
	select {
	case p.tasks <- t:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop 关闭队列、等待正在运行的任务完成，并返回合并的错误。
// Stop 是幂等的；多次调用共享同一份错误列表。
func (p *Pool) Stop() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if !p.stopped {
		p.stopped = true
		close(p.tasks)
	}
	p.mu.Unlock()
	p.wg.Wait()
	p.running.Store(false)

	p.mu.Lock()
	defer p.mu.Unlock()
	switch len(p.errs) {
	case 0:
		return nil
	case 1:
		return p.errs[0]
	default:
		return errors.Join(p.errs...)
	}
}

// Errors 返回错误的副本。
func (p *Pool) Errors() []error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]error, len(p.errs))
	copy(out, p.errs)
	if len(out) > 0 {
		out[len(out)] = nil
	}
	return out
}

// Stats 返回当前状态。
type Stats struct {
	Workers int
	Queued  int
	Errors  int
	Running bool
	Stopped bool
}

// Stats 返回当前状态快照。
func (p *Pool) Stats() Stats {
	if p == nil {
		return Stats{}
	}
	p.mu.Lock()
	stopped := p.stopped
	nErrs := len(p.errs)
	p.mu.Unlock()
	return Stats{
		Workers: p.workers,
		Queued:  len(p.tasks),
		Errors:  nErrs,
		Running: p.running.Load(),
		Stopped: stopped,
	}
}

// --- Error types ---

// TaskError 包装一个任务的错误，附带名字。
type TaskError struct {
	Name string
	Err  error
}

func (t *TaskError) Error() string {
	if t.Name == "" {
		return "workerpool: task failed: " + t.Err.Error()
	}
	return "workerpool: task[" + t.Name + "] failed: " + t.Err.Error()
}

func (t *TaskError) Unwrap() error { return t.Err }

// taskPanic 把 panic 转换为 TaskError。
func taskPanic(name string, v any) *TaskError {
	type s interface{ String() string }
	var msg string
	switch x := v.(type) {
	case nil:
		msg = "<nil panic>"
	case error:
		msg = x.Error()
	case string:
		msg = x
	case s:
		msg = x.String()
	default:
		msg = "<panicked>"
	}
	return &TaskError{Name: name, Err: errors.New("panic: " + msg)}
}

// recordErr 线程安全地追加错误。
func (p *Pool) recordErr(err error) {
	if err == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.errs = append(p.errs, err)
}
