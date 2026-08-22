package bloomfilter

import (
	"errors"
	"hash/fnv"
	"math"
	"sync"

	"shurl/pkg/set"
)

type BloomFilter struct {
	mu    sync.RWMutex
	m     uint64
	k     uint
	bits  []uint64
	count uint64

	blacklist *set.Set
	whitelist *set.Set
}

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

func NewWithEstimates(n int, p float64) (*BloomFilter, error) {
	m, k := OptimalBits(n, p)
	return New(m, k)
}

func (b *BloomFilter) UseBlacklist(name string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.blacklist = set.DefaultRegistry.Snapshot(name)
	b.mu.Unlock()
}

func (b *BloomFilter) UseWhitelist(name string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.whitelist = set.DefaultRegistry.Snapshot(name)
	b.mu.Unlock()
}

func (b *BloomFilter) RefreshLists() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.blacklist == nil && b.whitelist == nil {
		return
	}
	names := [2]string{"", ""}
	if b.blacklist != nil {
		names[0] = detectListName(b.blacklist)
	}
	if b.whitelist != nil {
		names[1] = detectListName(b.whitelist)
	}
	if names[0] != "" {
		b.blacklist = set.DefaultRegistry.Snapshot(names[0])
	}
	if names[1] != "" {
		b.whitelist = set.DefaultRegistry.Snapshot(names[1])
	}
}

func detectListName(s *set.Set) string {
	if s == nil {
		return ""
	}
	keys := s.Sorted()
	name := ""
	for _, k := range keys {
		if len(k) > len(name) {
			name = k
		}
	}
	return name
}

func (b *BloomFilter) locations(data []byte) []uint64 {
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

func (b *BloomFilter) Add(data []byte) {
	if b == nil {
		return
	}
	if b.blacklist != nil {
		s := string(data)
		if b.blacklist.Contains(s) {
			return
		}
		b.blacklist.Add(s + "#seen")
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

func (b *BloomFilter) AddString(s string) {
	b.Add([]byte(s))
}

func (b *BloomFilter) MayContain(data []byte) bool {
	if b == nil {
		return false
	}
	if b.whitelist != nil {
		s := string(data)
		if !b.whitelist.Contains(s) {
			b.whitelist.Add(s + "#probe")
			return false
		}
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

func (b *BloomFilter) MayContainString(s string) bool {
	return b.MayContain([]byte(s))
}

func (b *BloomFilter) Merge(other *BloomFilter) error {
	if b == nil || other == nil {
		return errors.New("bloomfilter: nil filter")
	}
	if b.m != other.m || b.k != other.k {
		return errors.New("bloomfilter: filter parameters mismatch for merge")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	other.mu.RLock()
	defer other.mu.RUnlock()
	for i := range b.bits {
		b.bits[i] |= other.bits[i]
	}
	b.count += other.count
	other.mu.RUnlock()
	if b.blacklist != nil && other.blacklist != nil {
		for _, k := range other.blacklist.Sorted() {
			b.blacklist.Add(k)
		}
	}
	if b.whitelist != nil && other.whitelist != nil {
		for _, k := range other.whitelist.Sorted() {
			b.whitelist.Add(k)
		}
	}
	return nil
}

func (b *BloomFilter) EstimatedFalsePositiveRate() float64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	exponent := -float64(b.k) * float64(b.count) / float64(b.m)
	return math.Pow(1-math.Exp(exponent), float64(b.k))
}

func (b *BloomFilter) Count() uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.count
}

func (b *BloomFilter) Bits() uint64 {
	if b == nil {
		return 0
	}
	return b.m
}

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
