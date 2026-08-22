package safemap

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

type Map struct {
	mu sync.RWMutex
	m  map[string]any
}

func New() *Map { return &Map{m: make(map[string]any)} }

func NewWithCap(capacity int) *Map {
	if capacity < 0 {
		capacity = 0
	}
	return &Map{m: make(map[string]any, capacity)}
}

func (m *Map) Get(key string) (any, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.m[key]
	return v, ok
}

func (m *Map) MustGet(key string, def any) any {
	v, ok := m.Get(key)
	if !ok {
		return def
	}
	return v
}

func (m *Map) Set(key string, value any) (prev any, existed bool) {
	if m == nil {
		return nil, false
	}
	if key == "" {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	prev, existed = m.m[key]
	m.m[key] = value
	return prev, existed
}

func (m *Map) SetIfAbsent(key string, value any) (actual any, loaded bool) {
	if m == nil || key == "" {
		return value, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.m[key]; ok {
		return v, true
	}
	m.m[key] = value
	return value, false
}

func (m *Map) Delete(key string) (prev any, existed bool) {
	if m == nil || key == "" {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.m[key]
	if !ok {
		return nil, false
	}
	delete(m.m, key)
	return v, true
}

func (m *Map) DeleteIf(fn func(key string, value any) bool) int {
	if m == nil || fn == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, v := range m.m {
		if fn(k, v) {
			delete(m.m, k)
			n++
		}
	}
	return n
}

func (m *Map) Len() int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.m)
}

func (m *Map) Keys() []string {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.m))
	for k := range m.m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (m *Map) ForEach(fn func(key string, value any) bool) {
	if m == nil || fn == nil {
		return
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for k, v := range m.m {
		if !fn(k, v) {
			return
		}
	}
}

func (m *Map) Snapshot() map[string]any {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	cp := make(map[string]any, len(m.m))
	for k, v := range m.m {
		cp[k] = v
	}
	return cp
}

// Swap atomically stores newVal for key and returns the previous value together
// with whether the key already existed. A first insert is NOT an error: old is
// the zero value (nil) and existed is false, so callers can distinguish a true
// replace from a first insert. err is non-nil only for invalid input (nil map
// or empty key).
func (m *Map) Swap(key string, newVal any) (old any, existed bool, err error) {
	if m == nil {
		return nil, false, errors.New("safemap: nil map")
	}
	if key == "" {
		return nil, false, errors.New("safemap: empty key")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old, existed = m.m[key]
	m.m[key] = newVal
	return old, existed, nil
}

type SwapResult struct {
	Key    string
	Old    any
	New    any
	Err    error
	Exists bool
}

// SwapMany applies items atomically under a single lock. For each entry it
// reports the previous value in Old, whether the key already existed in Exists,
// and an Err only for invalid input (empty key). A first insert yields Old=nil,
// Exists=false, Err=nil — distinct from a replace of an existing empty value.
func (m *Map) SwapMany(items map[string]any) []SwapResult {
	if m == nil || items == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(items))
	for k := range items {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]SwapResult, 0, len(keys))
	for _, k := range keys {
		if k == "" {
			out = append(out, SwapResult{
				Key:    k,
				Old:    nil,
				New:    items[k],
				Err:    errors.New("safemap: empty key"),
				Exists: false,
			})
			continue
		}
		old, existed := m.m[k]
		nv := items[k]
		m.m[k] = nv
		out = append(out, SwapResult{
			Key:    k,
			Old:    old,
			New:    nv,
			Err:    nil,
			Exists: existed,
		})
	}
	return out
}

// MustSwap is a string-oriented convenience over Swap. It returns the previous
// value coerced to a string, replaced reports whether a prior value existed
// (false on first insert), and opErr is non-nil only for invalid input.
// replaced is therefore the authoritative ADD-vs-REPLACE signal: it is true
// iff the key already existed, regardless of whether the old value was empty.
func (m *Map) MustSwap(key string, newVal any) (oldValue string, replaced bool, opErr error) {
	o, existed, err := m.Swap(key, newVal)
	if o == nil {
		oldValue = ""
	} else if s, ok := o.(string); ok {
		oldValue = s
	} else {
		oldValue = fmt.Sprintf("%v", o)
	}
	if err != nil {
		opErr = err
		return oldValue, existed, opErr
	}
	return oldValue, existed, nil
}

func (m *Map) Clear() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.m {
		delete(m.m, k)
	}
}
