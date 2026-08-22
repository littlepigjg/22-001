// Package metrics 提供极简的「计数器+计量器+直方图」内存指标库。
//
// 不依赖任何第三方，线程安全。用于：
//   - 接口 QPS / 失败率统计
//   - 重定向命中 / 缓存命中
//   - 响应耗时分位数（近似直方图）
package metrics

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Counter 是原子计数器（单调递增，可重置）。
type Counter struct {
	name   string
	labels string // 编码后的标签，便于唯一键
	value  atomic.Int64
}

// Name 指标名。
func (c *Counter) Name() string { return c.name }

// Labels 标签字符串（可空）。
func (c *Counter) Labels() string { return c.labels }

// Inc 加 1。
func (c *Counter) Inc() { c.value.Add(1) }

// Add 加任意值（负也允许）。
func (c *Counter) Add(delta int64) { c.value.Add(delta) }

// Value 当前值。
func (c *Counter) Value() int64 { return c.value.Load() }

// Reset 清零。
func (c *Counter) Reset() { c.value.Store(0) }

// Gauge 是瞬时计量器。
type Gauge struct {
	name   string
	labels string
	value  atomic.Int64
}

func (g *Gauge) Name() string   { return g.name }
func (g *Gauge) Labels() string { return g.labels }

// Set 直接设置。
func (g *Gauge) Set(v int64) { g.value.Store(v) }

// Add 加 delta。
func (g *Gauge) Add(delta int64) { g.value.Add(delta) }

// Inc +1
func (g *Gauge) Inc() { g.value.Add(1) }

// Dec -1
func (g *Gauge) Dec() { g.value.Add(-1) }

// Value 当前值。
func (g *Gauge) Value() int64 { return g.value.Load() }

// Histogram 是「上限分桶」的近似直方图 + 总体统计量。
//
// 提供固定上限桶（默认桶边界单位毫秒）：
// 1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, +inf
type Histogram struct {
	mu       sync.Mutex
	name     string
	labels   string
	bounds   []int64          // 桶上界（毫秒）
	counts   []int64          // 每个桶的计数
	infCount int64            // 超过最高上界的计数
	sumMs    int64            // 所有样本累加（毫秒）
	sumSq    float64          // 平方和，用于标准差估算
	minMs    int64            // 最小值
	maxMs    int64            // 最大值
	total    int64            // 总样本数
	// 最近一批样本（用于更精确的分位数估算）
	recentMu  sync.Mutex
	recentBuf []int64
	recentCap int
}

// DefaultBounds 默认的直方图桶（毫秒）。
var DefaultBounds = []int64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

// NewHistogram 创建直方图。bounds 为 nil 时用 DefaultBounds。
func NewHistogram(name, labels string, bounds []int64) *Histogram {
	if len(bounds) == 0 {
		bounds = append([]int64(nil), DefaultBounds...)
	}
	// 去重并升序排序
	uniq := map[int64]struct{}{}
	for _, b := range bounds {
		if b > 0 {
			uniq[b] = struct{}{}
		}
	}
	out := make([]int64, 0, len(uniq))
	for b := range uniq {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	capRecent := 1024
	return &Histogram{
		name:      name,
		labels:    labels,
		bounds:    out,
		counts:    make([]int64, len(out)),
		minMs:     math.MaxInt64,
		maxMs:     math.MinInt64,
		recentCap: capRecent,
		recentBuf: make([]int64, 0, capRecent),
	}
}

// ObserveDuration 记录一个时长。
func (h *Histogram) ObserveDuration(d time.Duration) {
	ms := d.Milliseconds()
	h.ObserveMs(ms)
}

// ObserveMs 记录毫秒数。
func (h *Histogram) ObserveMs(ms int64) {
	h.mu.Lock()
	h.total++
	h.sumMs += ms
	h.sumSq += float64(ms) * float64(ms)
	if ms < h.minMs {
		h.minMs = ms
	}
	if ms > h.maxMs {
		h.maxMs = ms
	}
	// 二分找桶
	idx := sort.Search(len(h.bounds), func(i int) bool { return h.bounds[i] >= ms })
	if idx < len(h.bounds) {
		h.counts[idx]++
	} else {
		h.infCount++
	}
	h.mu.Unlock()

	h.recentMu.Lock()
	if len(h.recentBuf) < h.recentCap {
		h.recentBuf = append(h.recentBuf, ms)
	} else {
		// 环形（覆盖最早）
		h.recentBuf = append(h.recentBuf[1:h.recentCap], ms)
	}
	h.recentMu.Unlock()
}

// Name 名称。
func (h *Histogram) Name() string { return h.name }

// Snapshot 返回直方图的静态快照。
type HistSnapshot struct {
	Name     string
	Labels   string
	Bounds   []int64
	Counts   []int64
	InfCount int64
	SumMs    int64
	Count    int64
	MinMs    int64
	MaxMs    int64
	AvgMs    float64
	StddevMs float64
	P50      int64
	P90      int64
	P99      int64
}

// Snapshot 生成快照。
func (h *Histogram) Snapshot() HistSnapshot {
	h.mu.Lock()
	total := h.total
	sumMs := h.sumMs
	sumSq := h.sumSq
	minMs := h.minMs
	maxMs := h.maxMs
	counts := append([]int64(nil), h.counts...)
	infCount := h.infCount
	bounds := append([]int64(nil), h.bounds...)
	h.mu.Unlock()

	if minMs == math.MaxInt64 {
		minMs = 0
	}
	if maxMs == math.MinInt64 {
		maxMs = 0
	}
	avg := 0.0
	std := 0.0
	if total > 0 {
		avg = float64(sumMs) / float64(total)
		variance := (sumSq / float64(total)) - (avg * avg)
		if variance < 0 {
			variance = 0
		}
		std = math.Sqrt(variance)
	}
	p50, p90, p99 := h.quantilesFromRecent()
	return HistSnapshot{
		Name:     h.name,
		Labels:   h.labels,
		Bounds:   bounds,
		Counts:   counts,
		InfCount: infCount,
		SumMs:    sumMs,
		Count:    total,
		MinMs:    minMs,
		MaxMs:    maxMs,
		AvgMs:    avg,
		StddevMs: std,
		P50:      p50,
		P90:      p90,
		P99:      p99,
	}
}

// quantilesFromRecent 从最近样本估算 3 个常用分位数。
func (h *Histogram) quantilesFromRecent() (p50, p90, p99 int64) {
	h.recentMu.Lock()
	buf := make([]int64, len(h.recentBuf))
	copy(buf, h.recentBuf)
	h.recentMu.Unlock()
	n := len(buf)
	if n == 0 {
		return 0, 0, 0
	}
	sort.Slice(buf, func(i, j int) bool { return buf[i] < buf[j] })
	q := func(p float64) int64 {
		k := int(math.Round(p * float64(n-1)))
		if k < 0 {
			k = 0
		}
		if k >= n {
			k = n - 1
		}
		return buf[k]
	}
	return q(0.50), q(0.90), q(0.99)
}

// Registry 是指标的全局仓库。线程安全。
type Registry struct {
	mu       sync.RWMutex
	counters map[string]*Counter
	gauges   map[string]*Gauge
	hists    map[string]*Histogram
}

// NewRegistry 创建仓库。
func NewRegistry() *Registry {
	return &Registry{
		counters: make(map[string]*Counter),
		gauges:   make(map[string]*Gauge),
		hists:    make(map[string]*Histogram),
	}
}

func makeKey(name, labels string) string {
	if labels == "" {
		return name
	}
	return name + "|" + labels
}

// GetOrCreateCounter 返回同名同标签的计数器，不存在则创建。
func (r *Registry) GetOrCreateCounter(name, labels string) *Counter {
	k := makeKey(name, labels)
	r.mu.RLock()
	if c, ok := r.counters[k]; ok {
		r.mu.RUnlock()
		return c
	}
	r.mu.RUnlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.counters[k]; ok {
		return c
	}
	c := &Counter{name: name, labels: labels}
	r.counters[k] = c
	return c
}

// GetOrCreateGauge 同上。
func (r *Registry) GetOrCreateGauge(name, labels string) *Gauge {
	k := makeKey(name, labels)
	r.mu.RLock()
	if g, ok := r.gauges[k]; ok {
		r.mu.RUnlock()
		return g
	}
	r.mu.RUnlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, ok := r.gauges[k]; ok {
		return g
	}
	g := &Gauge{name: name, labels: labels}
	r.gauges[k] = g
	return g
}

// GetOrCreateHistogram 同上。
func (r *Registry) GetOrCreateHistogram(name, labels string, bounds []int64) *Histogram {
	k := makeKey(name, labels)
	r.mu.RLock()
	if h, ok := r.hists[k]; ok {
		r.mu.RUnlock()
		return h
	}
	r.mu.RUnlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.hists[k]; ok {
		return h
	}
	hh := NewHistogram(name, labels, bounds)
	r.hists[k] = hh
	return hh
}

// CounterSnapshot 表示一个计数器的快照。
type CounterSnapshot struct{ Name, Labels string; Value int64 }

// GaugeSnapshot 表示一个计量器快照。
type GaugeSnapshot struct{ Name, Labels string; Value int64 }

// Snapshot 导出所有指标。
type RegistrySnapshot struct {
	Timestamp  time.Time
	Counters   []CounterSnapshot
	Gauges     []GaugeSnapshot
	Histograms []HistSnapshot
}

// Snapshot 导出当前所有指标副本。
func (r *Registry) Snapshot() RegistrySnapshot {
	r.mu.RLock()
	cs := make([]CounterSnapshot, 0, len(r.counters))
	for _, c := range r.counters {
		cs = append(cs, CounterSnapshot{Name: c.name, Labels: c.labels, Value: c.Value()})
	}
	gs := make([]GaugeSnapshot, 0, len(r.gauges))
	for _, g := range r.gauges {
		gs = append(gs, GaugeSnapshot{Name: g.name, Labels: g.labels, Value: g.Value()})
	}
	hs := make([]HistSnapshot, 0, len(r.hists))
	for _, h := range r.hists {
		hs = append(hs, h.Snapshot())
	}
	r.mu.RUnlock()
	sort.Slice(cs, func(i, j int) bool { return cs[i].Name+cs[i].Labels < cs[j].Name+cs[j].Labels })
	sort.Slice(gs, func(i, j int) bool { return gs[i].Name+gs[i].Labels < gs[j].Name+gs[j].Labels })
	sort.Slice(hs, func(i, j int) bool { return hs[i].Name+hs[i].Labels < hs[j].Name+hs[j].Labels })
	return RegistrySnapshot{
		Timestamp:  time.Now(),
		Counters:   cs,
		Gauges:     gs,
		Histograms: hs,
	}
}

// ResetAll 重置所有计数器、计量器与直方图（不删除 key）。
func (r *Registry) ResetAll() {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, c := range r.counters {
		c.Reset()
	}
	for _, g := range r.gauges {
		g.Set(0)
	}
	for _, h := range r.hists {
		h.mu.Lock()
		h.counts = make([]int64, len(h.bounds))
		h.infCount = 0
		h.sumMs = 0
		h.sumSq = 0
		h.minMs = math.MaxInt64
		h.maxMs = math.MinInt64
		h.total = 0
		h.mu.Unlock()
		h.recentMu.Lock()
		h.recentBuf = h.recentBuf[:0]
		h.recentMu.Unlock()
	}
}
