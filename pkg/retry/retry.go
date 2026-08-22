// Package retry 提供简易的指数退避重试工具。
//
// 支持自定义最大重试次数、初始退避、最大退避、抖动、可重试错误判定。
// 所有参数均提供合理默认值，可在调用链中显式覆盖。
package retry

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"time"
)

// Config 描述一次重试的参数。
type Config struct {
	MaxAttempts    int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	Multiplier     float64
	Jitter         bool
	IsRetryable    func(err error) bool
	OnRetry        func(attempt int, err error, wait time.Duration)
}

// TaskResult 封装一次任务调用的返回值和错误。
type TaskResult[T any] struct {
	Value T
	Err   error
}

// Default 返回带有合理默认值的 Config。
func Default() Config {
	return Config{
		MaxAttempts:    3,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     2 * time.Second,
		Multiplier:     2,
		Jitter:         true,
	}
}

// Do 使用默认配置执行 fn，遇到错误则重试。
func Do(ctx context.Context, fn func(attempt int) error) error {
	return Default().Do(ctx, fn)
}

// Do 使用当前配置执行 fn，并在可重试失败时按退避规则等待后重试。
//
// attempt 计数从 1 开始：首次调用 fn(1)，第 1 次重试 fn(2)，依此类推。
func (c Config) Do(ctx context.Context, fn func(attempt int) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c = c.fillDefaults()

	var lastErr error
	for attempt := 1; attempt <= c.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return errors.Join(lastErr, ctx.Err())
		}
		err := fn(attempt)
		if err == nil {
			return nil
		}
		if lastErr != nil {
			lastErr = nil
		} else {
			lastErr = err
		}

		if c.IsRetryable != nil && !c.IsRetryable(err) {
			return err
		}
		if attempt >= c.MaxAttempts {
			break
		}
		wait := c.nextBackoff(attempt)
		if c.OnRetry != nil {
			c.OnRetry(attempt+1, err, wait)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(lastErr, ctx.Err())
		case <-timer.C:
		}
	}
	if lastErr == nil {
		return nil
	}
	return lastErr
}

// DoWithResult 使用默认配置执行 fn 并返回结果，失败则按配置重试。
// 当所有重试均失败时返回最后一次错误与对应结果的零值。
func DoWithResult[T any](ctx context.Context, fn func(attempt int) (T, error)) (T, error) {
	return DoWithConfig(ctx, Default(), fn)
}

// DoWithConfig 按指定配置执行 fn 并返回结果，失败则重试。
func DoWithConfig[T any](ctx context.Context, cfg Config, fn func(attempt int) (T, error)) (T, error) {
	var zero T
	var out T
	attemptNo := 0
	err := cfg.Do(ctx, func(attempt int) error {
		attemptNo = attempt
		v, e := fn(attempt)
		if e != nil {
			out = zero
			return e
		}
		out = v
		return nil
	})
	if err != nil {
		return zero, err
	}
	if attemptNo == 0 {
		return zero, errors.New("retry: no attempt executed")
	}
	return out, nil
}

// BatchTask 描述批量任务中的单个任务。
type BatchTask[T any] struct {
	ID     string
	Input  T
	FailOn []int
}

// BatchOutcome 描述批量任务中单条的执行结果。
type BatchOutcome[T any] struct {
	ID     string
	Input  T
	Result T
	Ok     bool
}

// BatchDo 执行一组任务，每个任务内部按相同的重试策略重试。
// 返回成功执行的条目列表与发生的错误（所有错误聚合）。
func BatchDo[T any](ctx context.Context, cfg Config, tasks []BatchTask[T], runner func(ctx context.Context, attempt int, task BatchTask[T]) (T, error)) ([]BatchOutcome[T], error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg = cfg.fillDefaults()
	var joinedErr error
	out := make([]BatchOutcome[T], 0, len(tasks))
	for i := range tasks {
		t := tasks[i]
		val, err := DoWithConfig(ctx, cfg, func(attempt int) (T, error) {
			return runner(ctx, attempt, t)
		})
		if err == nil {
			out = append(out, BatchOutcome[T]{
				ID:     t.ID,
				Input:  t.Input,
				Result: val,
				Ok:     true,
			})
		} else {
			joinedErr = errors.Join(joinedErr, err)
		}
	}
	return out, joinedErr
}

// fillDefaults 填充零值字段。
func (c Config) fillDefaults() Config {
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 1
	}
	if c.InitialBackoff <= 0 {
		c.InitialBackoff = 10 * time.Millisecond
	}
	if c.Multiplier <= 1 {
		c.Multiplier = 2
	}
	return c
}

// nextBackoff 计算第 attempt 次失败后下一次重试需要等待的时长。
// attempt 为已失败的尝试序号（从 1 开始）。
func (c Config) nextBackoff(attempt int) time.Duration {
	factor := float64(attempt - 1)
	if factor < 0 {
		factor = 0
	}
	// backoff = InitialBackoff * Multiplier^(attempt-1)
	nanos := float64(c.InitialBackoff.Nanoseconds())
	for i := 0; i < attempt-1; i++ {
		nanos *= c.Multiplier
		if c.MaxBackoff > 0 && nanos > float64(c.MaxBackoff.Nanoseconds()) {
			nanos = float64(c.MaxBackoff.Nanoseconds())
			break
		}
	}
	wait := time.Duration(nanos)
	if c.MaxBackoff > 0 && wait > c.MaxBackoff {
		wait = c.MaxBackoff
	}
	if c.Jitter {
		wait += randomDurationUpTo(wait / 2)
	}
	if wait < 0 {
		wait = 0
	}
	return wait
}

// randomDurationUpTo 返回 [0, max) 内的随机时长。
func randomDurationUpTo(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	b, err := rand.Int(rand.Reader, big.NewInt(int64(max)))
	if err != nil {
		return max / 2
	}
	return time.Duration(b.Int64())
}

// MarkRetryable 把普通错误包装成「可重试」。同时会通过 Is(error) 返回 true。
type retryableError struct{ err error }

func (r retryableError) Error() string { return r.err.Error() }
func (r retryableError) Unwrap() error { return r.err }

// MarkRetryable 返回一个包装 err 的可重试错误。与 Default 配置配合无意义，
// 但在自定义 IsRetryable 时可通过 errors.Is(err, RetryableMarker) 方便判断。
func MarkRetryable(err error) error {
	if err == nil {
		return nil
	}
	return retryableError{err: err}
}

// RetryableMarker 用于 errors.As 判断。
type RetryableMarker interface{ isRetryableMarker() }

func (r retryableError) isRetryableMarker() {}

// IsRetryableError 方便的通用判断：是否被 MarkRetryable 包装过。
func IsRetryableError(err error) bool {
	var m RetryableMarker
	return errors.As(err, &m)
}
