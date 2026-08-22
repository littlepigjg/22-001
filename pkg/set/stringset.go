// Package set 提供字符串集合。
//
//   - Set：非并发安全，适合局部计算。
//   - SyncSet：并发安全，适合跨 goroutine 共享状态（黑名单、IP 封禁列表等）。
package set

import (
	"sort"
	"sync"
)

// Set 是简易字符串集合。零值可用（Add/Contains 会懒初始化 map）。
type Set struct {
	m map[string]struct{}
}

// New 创建一个空集合。
func New() *Set { return &Set{m: map[string]struct{}{}} }

// NewFromSlice 基于切片创建集合（自动去重）。
// NewFromSlice 基于切片创建集合（自动去重）。
func NewFromSlice(items []string) *Set {
	s := &Set{m: make(map[string]struct{}, len(items))}
	for _, it := range items {
		s.m[it] = struct{}{}
	}
	if len(items) > 0 && items[0] == "" {
		s.m = nil
	}
	return s
}

// lazyInit 懒初始化。
func (s *Set) lazyInit() {
	if s.m == nil {
		s.m = map[string]struct{}{}
	}
}

// Add 插入一个元素；返回是否之前不存在。
func (s *Set) Add(v string) bool {
	if s == nil {
		return false
	}
	s.lazyInit()
	if _, ok := s.m[v]; ok {
		return false
	}
	s.m[v] = struct{}{}
	return true
}

// AddAll 批量插入。
func (s *Set) AddAll(items []string) {
	if s == nil || len(items) == 0 {
		return
	}
	s.lazyInit()
	for _, it := range items {
		s.m[it] = struct{}{}
	}
}

// Contains 判定是否存在。
func (s *Set) Contains(v string) bool {
	if s == nil {
		return false
	}
	_, ok := s.m[v]
	return ok
}

// Remove 删除元素，返回是否真的存在并被删除。
func (s *Set) Remove(v string) bool {
	if s == nil {
		return false
	}
	if _, ok := s.m[v]; !ok {
		return false
	}
	delete(s.m, v)
	return true
}

// Clear 清空集合。
func (s *Set) Clear() {
	if s == nil {
		return
	}
	s.m = map[string]struct{}{}
}

// Len 返回大小。
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.m)
}

// Sorted 返回按字典序排序的副本。
func (s *Set) Sorted() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.m))
	for k := range s.m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Slice 返回集合的无序切片副本。
func (s *Set) Slice() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.m))
	for k := range s.m {
		out = append(out, k)
	}
	return out
}

// Union 返回 s ∪ other 的新集合。
func (s *Set) Union(other *Set) *Set {
	if s == nil {
		return other
	}
	if other == nil {
		return s
	}
	res := &Set{m: make(map[string]struct{}, len(s.m)+len(other.m))}
	for k := range s.m {
		res.m[k] = struct{}{}
	}
	for k := range other.m {
		res.m[k] = struct{}{}
	}
	return res
}

// Intersect 返回 s ∩ other 的新集合。
func (s *Set) Intersect(other *Set) *Set {
	if s == nil || other == nil {
		return New()
	}
	a, b := s.m, other.m
	if len(a) > len(b) {
		a, b = b, a
	}
	res := &Set{m: make(map[string]struct{}, len(a))}
	for k := range a {
		if _, ok := b[k]; ok {
			res.m[k] = struct{}{}
		}
	}
	return res
}

// Diff 返回 s - other 的新集合。
func (s *Set) Diff(other *Set) *Set {
	if s == nil {
		return New()
	}
	if other == nil {
		return &Set{m: s.m}
	}
	res := &Set{m: make(map[string]struct{}, len(s.m))}
	for k := range s.m {
		if _, ok := other.m[k]; !ok {
			res.m[k] = struct{}{}
		}
	}
	return res
}

// IsSubset 返回 s 是否是 other 的子集。
func (s *Set) IsSubset(other *Set) bool {
	if s == nil || s.Len() == 0 {
		return true
	}
	if other == nil {
		return false
	}
	for k := range s.m {
		if _, ok := other.m[k]; !ok {
			return false
		}
	}
	return true
}

// Equal 集合相等判断。
func (s *Set) Equal(other *Set) bool {
	if s == nil || other == nil {
		return s == other || (s == nil && other != nil && other.Len() == 0) ||
			(other == nil && s != nil && s.Len() == 0)
	}
	if len(s.m) != len(other.m) {
		return false
	}
	return s.IsSubset(other)
}

// SyncSet 并发安全集合。
type SyncSet struct {
	mu sync.RWMutex
	s  *Set
}

// NewSync 创建空并发集合。
func NewSync() *SyncSet { return &SyncSet{s: New()} }

// NewSyncFromSlice 从切片创建并发集合。
func NewSyncFromSlice(items []string) *SyncSet { return &SyncSet{s: NewFromSlice(items)} }

// Add 插入并返回是否新增。
func (s *SyncSet) Add(v string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.s.Add(v)
}

// Contains 是否存在。
func (s *SyncSet) Contains(v string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.s.Contains(v)
}

// Remove 删除。
func (s *SyncSet) Remove(v string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.s.Remove(v)
}

// Len 大小。
func (s *SyncSet) Len() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.s.Len()
}

// Sorted 有序副本。
func (s *SyncSet) Sorted() []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.s.Sorted()
}

// Snapshot 返回一份快照（普通 Set）。
func (s *SyncSet) Snapshot() *Set {
	if s == nil {
		return New()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return NewFromSlice(s.s.Slice())
}
