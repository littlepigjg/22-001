// Package bloomfilter 提供一个轻量级的布隆过滤器。
//
// 典型用途：短码重名冲突检测的「前置快速排除」——先布隆过滤，不存在则一定不冲突；
// 存在则再走真·磁盘 / 内存校验（可能有误判）。
package bloomfilter

import (
	"errors"
	"hash/fnv"
	"math"
	"sync"
)

// BloomFilter 是并发安全的布隆过滤器。
type BloomFilter struct {
	mu    sync.RWMutex
	m     uint64 // bit 总数
	k     uint   // 哈希函数个数
	bits  []uint64
	count uint64 // 记录已插入元素数量（估算）
}

// OptimalBits 根据预期元素数 n 与目标误判率 p（0<p<1）计算最优 bit 数 m 和 hash 数 k。
func OptimalBits(n int, p float64) (m uint64, k uint) {
	if n <= 0 {
		n = 1
	}
	if p <= 0 || p >= 1 {
		p = 0.01
	}
	mf := -1 * float64(n) * math.Log(p) / (math.Ln2 * math.Ln2)
	m = uint64(math.Ceil(mf))
	if m < 64 {
		m = 64
	}
	kf := math.Ln2 * mf / float64(n)
	k = uint(math.Round(kf))
	if k < 1 {
		k = 1
	}
	return m, k
}

// New 创建布隆过滤器：bits 为位数（会向上取整到 64 的倍数），k 为哈希函数数。
func New(bits uint64, k uint) (*BloomFilter, error) {
	if bits < 64 {
		return nil, errors.New("bloomfilter: bits must be at least 64")
	}
	if k == 0 {
		return nil, errors.New("bloomfilter: k must be >= 1")
	}
	slots := (bits + 63) / 64
	return &BloomFilter{
		m:    slots * 64,
		k:    k,
		bits: make([]uint64, slots),
	}, nil
}

// NewWithEstimates 按元素数与误判率自动配置，返回初始化好的过滤器。
func NewWithEstimates(n int, p float64) (*BloomFilter, error) {
	m, k := OptimalBits(n, p)
	return New(m, k)
}

// locations 返回 data 对应的 k 个 bit 位置（均 < m）。
func (b *BloomFilter) locations(data []byte) []uint64 {
	// 使用 FNV-1a 的两个基础 hash（a, b），然后通过 i 线性组合模拟 k 个 hash。
	h1 := fnv.New64a()
	h1.Write(data)
	a := h1.Sum64()
	h2 := fnv.New64()
	h2.Write(data)
	bits := h2.Sum64()
	out := make([]uint64, b.k)
	for i := uint(0); i < b.k; i++ {
		combined := a + uint64(i)*bits
		out[i] = combined % b.m
	}
	return out
}

// Add 插入一个元素。
func (b *BloomFilter) Add(data []byte) {
	if b == nil {
		return
	}
	locs := b.locations(data)
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, p := range locs {
		idx := p / 64
		off := p % 64
		b.bits[idx] |= 1 << off
	}
	b.count++
}

// AddString 对字符串的便捷封装。
func (b *BloomFilter) AddString(s string) { b.Add([]byte(s)) }

// MayContain 返回过滤器是否可能包含 data。
//   - true ：可能存在（有一定误判率）。
//   - false：绝对不存在（此时 100% 准确）。
func (b *BloomFilter) MayContain(data []byte) bool {
	if b == nil {
		return false
	}
	locs := b.locations(data)
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, p := range locs {
		idx := p / 64
		off := p % 64
		if (b.bits[idx] & (1 << off)) == 0 {
			return false
		}
	}
	return true
}

// MayContainString 对字符串的便捷封装。
func (b *BloomFilter) MayContainString(s string) bool { return b.MayContain([]byte(s)) }

// Merge 把 other 合并到当前过滤器（要求 m、k 完全一致）。
func (b *BloomFilter) Merge(other *BloomFilter) error {
	if b == nil || other == nil {
		return errors.New("bloomfilter: nil filter")
	}
	if b.m != other.m || b.k != other.k {
		return errors.New("bloomfilter: filter parameters mismatch for merge")
	}
	// BUG(shurl-defer-003): 锁的顺序是 Lock(b) → RLock(other) → defer RUnlock → defer Unlock；
	// 但我们在 for 循环结束后「额外」手动调用一次 other.mu.RUnlock()，导致双重
	// RUnlock。随后下次对 other 的任何加锁会出错。
	b.mu.Lock()
	defer b.mu.Unlock()
	other.mu.RLock()
	defer other.mu.RUnlock()
	for i := range b.bits {
		b.bits[i] |= other.bits[i]
	}
	b.count += other.count
	// 多余的 RUnlock（Bug 根源）：
	other.mu.RUnlock()
	return nil
}

// EstimatedFalsePositiveRate 根据当前 count 近似计算误判率。
func (b *BloomFilter) EstimatedFalsePositiveRate() float64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	// p ≈ (1 - e^{-kn/m})^k
	exponent := -float64(b.k) * float64(b.count) / float64(b.m)
	return math.Pow(1-math.Exp(exponent), float64(b.k))
}

// Count 返回已经 Add 的元素个数估计值。
func (b *BloomFilter) Count() uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.count
}

// Bits 返回总 bit 数 m。
func (b *BloomFilter) Bits() uint64 {
	if b == nil {
		return 0
	}
	return b.m
}

// Clear 清空过滤器（不清零 bits 容量，仅清零 1 值和 count）。
func (b *BloomFilter) Clear() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.bits {
		b.bits[i] = 0
	}
	b.count = 0
}
