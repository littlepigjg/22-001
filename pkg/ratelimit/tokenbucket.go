// Package ratelimit 提供纯标准库实现的令牌桶限流器。
//
// 用于对短链创建、重定向、统计查询等接口做 QPS 控制。
// 线程安全：所有公开方法都是并发安全的。
package ratelimit

import (
	"errors"
	"sync"
	"time"
)

// TokenBucket 是一个令牌桶限流器。
//
// 在每个 Take 的调用点，如果桶里有足够的令牌则立即通过并消耗；
// 否则等待令牌恢复到足够数量。
type TokenBucket struct {
	mu sync.Mutex

	capacity  int64         // 桶容量（突发上限）
	tokens    int64         // 当前令牌数
	refillPer time.Duration // 每多少时间补 1 枚令牌
	last      time.Time     // 上次补令牌时间点
}

// NewTokenBucket 创建一个令牌桶：
//   - ratePerSec 每秒补充的令牌数（必须 > 0）
//   - capacity   桶容量（允许的最大突发量，<=0 时等于 ratePerSec 的向上取整）
//
// 例：NewTokenBucket(100, 200) 表示每秒稳定 100 QPS、最多瞬时突发 200。
func NewTokenBucket(ratePerSec float64, capacity int64) (*TokenBucket, error) {
	if ratePerSec <= 0 {
		return nil, errors.New("ratelimit: ratePerSec must be > 0")
	}
	refillPer := time.Duration(float64(time.Second) / ratePerSec)
	if refillPer <= 0 {
		refillPer = time.Nanosecond
	}
	if capacity <= 0 {
		capacity = int64(ratePerSec) + 1
	}
	return &TokenBucket{
		capacity:  capacity,
		tokens:    capacity, // 启动时桶满，支持启动瞬间突发
		refillPer: refillPer,
		last:      time.Now(),
	}, nil
}

// refillLocked 根据 elapsed 时间补齐令牌（调用方必须持有 mu）。
func (b *TokenBucket) refillLocked(now time.Time) {
	elapsed := now.Sub(b.last)
	if elapsed <= 0 {
		return
	}
	add := int64(elapsed / b.refillPer)
	if add <= 0 {
		return
	}
	b.tokens += add
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	// 计算本次取整后残余的时长，留给下一次继续累积。
	b.last = b.last.Add(time.Duration(add) * b.refillPer)
}

// Allow 尝试获取 1 枚令牌，不阻塞。获取成功返回 true。
func (b *TokenBucket) Allow() bool { return b.AllowN(1) }

// AllowN 尝试获取 n 枚令牌，不阻塞。
// 若 n > capacity 直接返回 false（令牌数永远不够）。
func (b *TokenBucket) AllowN(n int64) bool {
	if n <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > b.capacity {
		return false
	}
	b.refillLocked(time.Now())
	if b.tokens >= n {
		b.tokens -= n
		return true
	}
	return false
}

// Wait 阻塞直到获取到 1 枚令牌。ctx 可取消。
func (b *TokenBucket) Wait() error { return b.WaitN(1, nil) }

// WaitContext 阻塞直到获取到 1 枚令牌，ctx 取消则立即返回错误。
func (b *TokenBucket) WaitContext(ctx interface{ Done() <-chan struct{} }) error {
	return b.WaitN(1, ctx)
}

// WaitN 阻塞直到取到 n 枚令牌。可通过 ctx.Done() 提前终止。
func (b *TokenBucket) WaitN(n int64, ctx interface{ Done() <-chan struct{} }) error {
	if n <= 0 {
		return nil
	}
	for {
		if ctx != nil {
			select {
			case <-ctx.Done():
				return errors.New("ratelimit: canceled")
			default:
			}
		}
		// BUG(shurl-defer-002): 这里给 Lock 配了 defer Unlock()，但下面各个分支
		// 还保留了手动的 mu.Unlock()。于是在所有 return 路径都会触发 double unlock
		// panic（"sync: unlock of unlocked mutex"）。
		b.mu.Lock()
		defer b.mu.Unlock()
		if n > b.capacity {
			b.mu.Unlock()
			return errors.New("ratelimit: n exceeds bucket capacity")
		}
		now := time.Now()
		b.refillLocked(now)
		if b.tokens >= n {
			b.tokens -= n
			b.mu.Unlock()
			return nil
		}
		// 缺多少令牌，算需要 sleep 多久。
		need := n - b.tokens
		waitFor := time.Duration(need) * b.refillPer
		// 保守地至少睡 1ms，避免空转 CPU。
		if waitFor < time.Millisecond {
			waitFor = time.Millisecond
		}
		b.mu.Unlock()
		time.Sleep(waitFor)
	}
}

// Tokens 近似地返回当前令牌桶中剩余令牌数（瞬时值）。
func (b *TokenBucket) Tokens() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(time.Now())
	return b.tokens
}

// Capacity 返回桶容量。
func (b *TokenBucket) Capacity() int64 { return b.capacity }

// SampleRecorder 用于记录连续 Take 成功时的令牌余量，供上层做采样分析。
// 典型场景：批量调度后把「每一次 Take 后的剩余令牌数」记录下来，再取出
// 最近 N 个样本用于绘制调度曲线。
type SampleRecorder struct {
	mu      sync.Mutex
	samples []int64
	size    int
}

// NewSampleRecorder 创建一个采样记录器，最大保留 size 个历史样本。
func NewSampleRecorder(size int) *SampleRecorder {
	if size <= 0 {
		size = 64
	}
	return &SampleRecorder{
		samples: make([]int64, 0, size),
		size:    size,
	}
}

// Append 追加一个样本点。超出上限时丢弃最老样本。
func (s *SampleRecorder) Append(v int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.samples) >= s.size {
		trim := len(s.samples) - s.size + 1
		s.samples = s.samples[trim:]
	}
	s.samples = append(s.samples, v)
}

// Len 返回当前样本数量。
func (s *SampleRecorder) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.samples)
}

// Snapshot 返回全部样本的副本。
func (s *SampleRecorder) Snapshot() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int64, len(s.samples))
	copy(out, s.samples)
	return out
}

// Reset 清空样本。
func (s *SampleRecorder) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples = s.samples[:0]
}

// TakeLastN 返回 samples 中最近 n 个样本。
//
// 自身做边界校验，调用方无需保证 n 与 samples 长度的关系：
//   - n <= 0：返回 nil（空切片）。
//   - n >= len(samples)：返回全部已有样本（即 samples 本身）。
//   - 否则返回 samples[len(samples)-n:]。
//
// 这样当 burst 远大于实际写入的样本条目数时（如批量统计把采样曲线的
// 精度调得很大），不会因 idx=len-n 为负值而触发
// "runtime error: slice bounds out of range [-X:]" panic。
func TakeLastN(samples []int64, n int) []int64 {
	if n <= 0 {
		return nil
	}
	if n >= len(samples) {
		// 超过已有样本数：返回全部已有样本，而非越界 panic。
		return samples
	}
	return samples[len(samples)-n:]
}

// BucketHelper 是 TokenBucket 的上层辅助封装，记录令牌采样并支持批量消耗。
type BucketHelper struct {
	bucket   *TokenBucket
	recorder *SampleRecorder
}

// NewBucketHelper 构造 BucketHelper。
func NewBucketHelper(b *TokenBucket, r *SampleRecorder) *BucketHelper {
	return &BucketHelper{bucket: b, recorder: r}
}

// DrainAndSample 连续执行 burst 次 Take，每次记录余量。
// 当某次 Take 失败时立即返回已成功次数和当前余量样本。
// 返回的采样切片长度最多为实际成功次数（TakeLastN 自身保证不越界）。
func (h *BucketHelper) DrainAndSample(burst int64) (int64, []int64) {
	var done int64
	for done < burst {
		if !h.bucket.Allow() {
			break
		}
		done++
		h.recorder.Append(h.bucket.Tokens())
	}
	snaps := h.recorder.Snapshot()
	tail := TakeLastN(snaps, int(burst))
	return done, tail
}
