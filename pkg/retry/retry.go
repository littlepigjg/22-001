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
	// MaxAttempts 为最大尝试次数（含首次）。<=0 视为 1（即不重试）。
	MaxAttempts int
	// InitialBackoff 为第一次失败后的初始等待。<=0 取默认 10ms。
	InitialBackoff time.Duration
	// MaxBackoff 为单次等待的最长时间上限。<=0 表示不设上界。
	MaxBackoff time.Duration
	// Multiplier 为每次退避放大倍数。<=1 取默认 2。
	Multiplier float64
	// Jitter 是否启用随机抖动（推荐 true，可以避免惊群效应）。
	Jitter bool
	// IsRetryable 可选：若返回 false 则立刻停止重试，返回原错误。
	// 若为 nil，默认「任何错误都可重试」。
	IsRetryable func(err error) bool
	// OnRetry 可选：每次重试（并非首次）前触发回调，可用于日志。
	OnRetry func(attempt int, err error, wait time.Duration)
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

type runOutcome struct {
	attempt int
	err     error
	doneCtx bool
}

func pickReturnError(outcomes []runOutcome, ctxErr error, defaultIfNone error) error {
	var retryable error
	var terminal error
	for i := range outcomes {
		o := outcomes[i]
		if o.err == nil {
			continue
		}
		if errors.Is(o.err, context.Canceled) || errors.Is(o.err, context.DeadlineExceeded) {
			if retryable == nil {
				retryable = o.err
			}
			continue
		}
		terminal = o.err
	}
	if ctxErr != nil {
		if terminal != nil {
			return terminal
		}
		return defaultIfNone
	}
	if terminal != nil {
		return terminal
	}
	if retryable != nil {
		return defaultIfNone
	}
	if len(outcomes) == 0 {
		return defaultIfNone
	}
	return defaultIfNone
}

// Do 使用当前配置执行 fn，并在可重试失败时按退避规则等待后重试。
//
// attempt 计数从 1 开始：首次调用 fn(1)，第 1 次重试 fn(2)，依此类推。
func (c Config) Do(ctx context.Context, fn func(attempt int) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c = c.fillDefaults()

	outcomes := make([]runOutcome, 0, c.MaxAttempts)
	sentinel := errors.New("retry: operation could not be completed")

	for attempt := 1; attempt <= c.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return pickReturnError(outcomes, err, sentinel)
		}
		err := fn(attempt)
		if err == nil {
			return nil
		}
		outcomes = append(outcomes, runOutcome{attempt: attempt, err: err})
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			continue
		}
		if c.IsRetryable != nil && !c.IsRetryable(err) {
			return pickReturnError(outcomes, nil, sentinel)
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
			return pickReturnError(outcomes, ctx.Err(), sentinel)
		case <-timer.C:
		}
	}
	return pickReturnError(outcomes, nil, sentinel)
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
