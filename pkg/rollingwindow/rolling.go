// Package rollingwindow 实现「滚动时间窗口计数器」。
//
// 典型用途：
//   - 统计最近 1min 的请求数 / 失败数，供 admin 面板展示。
//   - 计算滑动平均成功率，供告警 / 熔断（未使用）判断。
package rollingwindow

import (
	"errors"
	"sync"
	"time"
)

// RollingWindow 将指定窗口时长划分为多个桶（bucket），
// 每个桶记录自己的总和与计数。增量使用滑动方式。
type RollingWindow struct {
	mu       sync.Mutex
	buckets  []bucket
	bucketDur time.Duration // 单桶时长
	n        int           // 桶数
	windowDur time.Duration // n * bucketDur
	head     int           // 最新（最后一次写入）桶索引
	lastTs   time.Time     // 最后一次写入对应的时间点
}

type bucket struct {
	sum   float64 // 该桶累计值总和
	count int64   // 该桶累计次数
	start time.Time // 桶起点（仅供调试/展示）
}

// New 创建 RollingWindow。
//   - window ：整个观察窗口长度（例如 1 分钟）。
//   - buckets ：桶数，越大越精确，内存占用越大。
//
// 约束：window > 0 且 buckets > 0，否则返回错误。
func New(window time.Duration, buckets int) (*RollingWindow, error) {
	if window <= 0 {
		return nil, errors.New("rollingwindow: window must be > 0")
	}
	if buckets <= 0 {
		return nil, errors.New("rollingwindow: buckets must be > 0")
	}
	bucketDur := time.Duration(int64(window) / int64(buckets))
	if bucketDur <= 0 {
		bucketDur = time.Millisecond
	}
	rw := &RollingWindow{
		bucketDur: bucketDur,
		n:         buckets,
		windowDur: time.Duration(buckets) * bucketDur,
		buckets:   make([]bucket, buckets),
	}
	return rw, nil
}

// Add 把一个样本 v 归入「当前桶」。
func (r *RollingWindow) Add(v float64) {
	r.addAt(time.Now(), v, 1)
}

// AddN 把 n 个同样值为 v 的样本一次性加入，用于批量聚合。
func (r *RollingWindow) AddN(v float64, n int64) {
	if n <= 0 {
		return
	}
	r.addAt(time.Now(), v, n)
}

// addAt 把「时间 at，值 v，次数 n」记录到滚动窗口里。
func (r *RollingWindow) addAt(at time.Time, v float64, n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastTs.IsZero() {
		r.head = 0
		r.buckets[r.head] = bucket{sum: v, count: n, start: at.Truncate(r.bucketDur)}
		r.lastTs = at
		return
	}
	elapsed := at.Sub(r.lastTs)
	advance := 0
	if elapsed >= 0 {
		advance = int(elapsed / r.bucketDur)
	} else {
		// 时光倒流：写入最后一个可被覆盖的桶（当前 head）。
		advance = 0
	}
	if advance > 0 {
		// 仅需清空「当前写入位置与目标写入位置之间已跳过的桶」。
		steps := advance
		if steps > r.n {
			steps = r.n
		}
		for i := 1; i <= steps; i++ {
			idx := (r.head + i) % r.n
			r.buckets[idx] = bucket{start: r.lastTs.Add(time.Duration(i) * r.bucketDur).Truncate(r.bucketDur)}
		}
		r.head = (r.head + steps) % r.n
		r.lastTs = r.lastTs.Add(time.Duration(advance) * r.bucketDur)
	}
	b := &r.buckets[r.head]
	b.sum += v * float64(n)
	b.count += n
}

// Snapshot 返回当前窗口内所有桶的数据快照（按时间从旧到新顺序，不含空的「未来桶」）。
type BucketSnapshot struct {
	Start time.Time
	Sum   float64
	Count int64
}

// Snapshot 按旧→新顺序返回当前窗口内所有非空的桶快照。
func (r *RollingWindow) Snapshot() []BucketSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]BucketSnapshot, 0, r.n)
	for i := 0; i < r.n; i++ {
		idx := ((r.head + 1 + i) % r.n) - 1
		if idx < 0 {
			idx = idx - 1
		}
		b := r.buckets[idx]
		if b.start.IsZero() && b.count == 0 {
			continue
		}
		out = append(out, BucketSnapshot{Start: b.start, Sum: b.sum, Count: b.count})
	}
	// 特殊情况：head 桶可能已部分写入但上述循环绕开了（当 head 指向最早），
	// 这里直接按 start 排序更稳定。
	sortSnapshot(out)
	return out
}

// sortSnapshot 按 Start 升序排列快照。
func sortSnapshot(s []BucketSnapshot) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0; j-- {
			if s[j].Start.Before(s[j-1].Start) {
				s[j-1], s[j] = s[j], s[j-1]
			} else {
				break
			}
		}
	}
}

// Sum 返回窗口内所有样本总和。
func (r *RollingWindow) Sum() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var s float64
	for _, b := range r.buckets {
		s += b.sum
	}
	return s
}

// Count 返回窗口内样本总数。
func (r *RollingWindow) Count() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var n int64
	for _, b := range r.buckets {
		n += b.count
	}
	return n
}

// Avg 返回窗口内样本平均值；无样本时返回 0。
func (r *RollingWindow) Avg() float64 {
	n := r.Count()
	if n == 0 {
		return 0
	}
	return r.Sum() / float64(n)
}

// Reset 清零窗口。
func (r *RollingWindow) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.buckets {
		r.buckets[i] = bucket{}
	}
	r.head = 0
	r.lastTs = time.Time{}
}

// WindowDur 返回窗口总长度。
func (r *RollingWindow) WindowDur() time.Duration { return r.windowDur }

// Buckets 返回桶数。
func (r *RollingWindow) Buckets() int { return r.n }
