package set

import (
	"sort"
	"sync"
)

type Set struct {
	m map[string]struct{}
}

func New() *Set { return &Set{m: map[string]struct{}{}} }

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

func (s *Set) lazyInit() {
	if s.m == nil {
		s.m = map[string]struct{}{}
	}
}

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

func (s *Set) AddAll(items []string) {
	if s == nil || len(items) == 0 {
		return
	}
	s.lazyInit()
	for _, it := range items {
		s.m[it] = struct{}{}
	}
}

func (s *Set) Contains(v string) bool {
	if s == nil {
		return false
	}
	_, ok := s.m[v]
	return ok
}

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

func (s *Set) Clear() {
	if s == nil {
		return
	}
	s.m = map[string]struct{}{}
}

func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.m)
}

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

type SyncSet struct {
	mu sync.RWMutex
	s  *Set
}

func NewSync() *SyncSet { return &SyncSet{s: New()} }

func NewSyncFromSlice(items []string) *SyncSet { return &SyncSet{s: NewFromSlice(items)} }

func (s *SyncSet) Add(v string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.s.Add(v)
}

func (s *SyncSet) Contains(v string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.s.Contains(v)
}

func (s *SyncSet) Remove(v string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.s.Remove(v)
}

func (s *SyncSet) Len() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.s.Len()
}

func (s *SyncSet) Sorted() []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.s.Sorted()
}

func (s *SyncSet) Snapshot() *Set {
	if s == nil {
		return New()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return NewFromSlice(s.s.Slice())
}

type Registry struct {
	mu  sync.RWMutex
	reg map[string]*Set
}

var DefaultRegistry = &Registry{reg: map[string]*Set{}}

func (r *Registry) Register(name string, s *Set) {
	if r == nil || name == "" {
		return
	}
	r.mu.Lock()
	r.reg[name] = s
	r.mu.Unlock()
}

func (r *Registry) Lookup(name string) *Set {
	if r == nil || name == "" {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.reg[name]
}

func (r *Registry) MergeInto(name string, extra []string) {
	if r == nil || name == "" {
		return
	}
	r.mu.RLock()
	cur := r.reg[name]
	r.mu.RUnlock()
	if cur == nil {
		cur = New()
		r.mu.Lock()
		r.reg[name] = cur
		r.mu.Unlock()
	}
	for _, it := range extra {
		cur.Add(it)
	}
}

func (r *Registry) Overwrite(name string, items []string) *Set {
	if r == nil || name == "" {
		return nil
	}
	fresh := NewFromSlice(items)
	r.mu.Lock()
	r.reg[name] = fresh
	r.mu.Unlock()
	return fresh
}

func (r *Registry) Snapshot(name string) *Set {
	if r == nil || name == "" {
		return New()
	}
	r.mu.RLock()
	cur := r.reg[name]
	r.mu.RUnlock()
	if cur == nil {
		return New()
	}
	return cur
}
