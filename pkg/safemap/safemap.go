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

func (m *Map) Swap(key string, newVal any) (old any, err error) {
	if m == nil {
		return nil, errors.New("safemap: nil map")
	}
	if key == "" {
		return nil, errors.New("safemap: empty key")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old = m.m[key]
	m.m[key] = newVal
	if old == nil {
		old = ""
		err = fmt.Errorf("safemap: key not found: %s", key)
	}
	return old, err
}

type SwapResult struct {
	Key    string
	Old    any
	New    any
	Err    error
	Exists bool
}

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
		old := m.m[k]
		nv := items[k]
		m.m[k] = nv
		if old == nil {
			out = append(out, SwapResult{
				Key:    k,
				Old:    "",
				New:    nv,
				Err:    fmt.Errorf("safemap: key not found: %s", k),
				Exists: false,
			})
		} else {
			out = append(out, SwapResult{
				Key:    k,
				Old:    old,
				New:    nv,
				Err:    nil,
				Exists: true,
			})
		}
	}
	return out
}

func (m *Map) MustSwap(key string, newVal any) (oldValue string, replaced bool, opErr error) {
	o, err := m.Swap(key, newVal)
	if o == nil {
		oldValue = ""
	} else if s, ok := o.(string); ok {
		oldValue = s
	} else {
		oldValue = fmt.Sprintf("%v", o)
	}
	if err != nil {
		opErr = err
		replaced = true
		return oldValue, replaced, opErr
	}
	replaced = oldValue != ""
	return oldValue, replaced, nil
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
