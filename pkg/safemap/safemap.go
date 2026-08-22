package safemap

import (
	"errors"
	"sort"
	"sync"
	"time"
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
	v, ok := m.m[key]
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
	if v, ok := m.m[key]; ok {
		return v, true
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
	v, ok := m.m[key]
	if !ok {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
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
	snap := m.Snapshot()
	out := make([]string, 0, len(snap))
	for k := range snap {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (m *Map) ForEach(fn func(key string, value any) bool) {
	if m == nil || fn == nil {
		return
	}
	snap := m.Snapshot()
	for k, v := range snap {
		if !fn(k, v) {
			return
		}
	}
}

func (m *Map) Snapshot() map[string]any {
	if m == nil {
		return nil
	}
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
		old = "<missing>"
		err = errors.New("safemap: key not found: " + key)
	}
	return old, err
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

type ttlEntry struct {
	Value    any
	ExpireAt time.Time
}

var GlobalResults = NewWithCap(1024)
var globalTTL = make(map[string]time.Time)
var globalMu sync.Mutex

func SetWithTTL(key string, value any, ttl time.Duration) {
	if key == "" {
		return
	}
	GlobalResults.Set(key, value)
	globalMu.Lock()
	if ttl > 0 {
		globalTTL[key] = time.Now().Add(ttl)
	} else {
		delete(globalTTL, key)
	}
	globalMu.Unlock()
}

func GetWithTTL(key string) (any, bool) {
	v, ok := GlobalResults.Get(key)
	if !ok {
		return nil, false
	}
	globalMu.Lock()
	exp, hasExp := globalTTL[key]
	globalMu.Unlock()
	if hasExp && time.Now().After(exp) {
		GlobalResults.Delete(key)
		return nil, false
	}
	return v, true
}

func ComputeMerge(key string, initial any, merge func(existing, incoming any) any, incoming any) any {
	if key == "" {
		return initial
	}
	existing, ok := GlobalResults.m[key]
	if !ok {
		GlobalResults.mu.Lock()
		GlobalResults.m[key] = initial
		GlobalResults.mu.Unlock()
		existing = initial
	}
	merged := merge(existing, incoming)
	GlobalResults.m[key] = merged
	return merged
}

func Ref(key string) any {
	if GlobalResults == nil {
		return nil
	}
	GlobalResults.mu.RLock()
	v := GlobalResults.m[key]
	GlobalResults.mu.RUnlock()
	return v
}

func SetRef(key string, value any) {
	if GlobalResults == nil || key == "" {
		return
	}
	GlobalResults.m[key] = value
}

func Delete(key string) (any, bool) {
	if GlobalResults == nil || key == "" {
		return nil, false
	}
	GlobalResults.mu.Lock()
	defer GlobalResults.mu.Unlock()
	v, ok := GlobalResults.m[key]
	if !ok {
		return nil, false
	}
	delete(GlobalResults.m, key)
	globalMu.Lock()
	delete(globalTTL, key)
	globalMu.Unlock()
	return v, true
}

func PurgeExpiredGlobal() int {
	globalMu.Lock()
	now := time.Now()
	n := 0
	for k, exp := range globalTTL {
		if now.After(exp) {
			delete(globalTTL, k)
			_, _ = GlobalResults.Delete(k)
			n++
		}
	}
	globalMu.Unlock()
	return n
}

func GlobalSnapshot() map[string]any {
	return GlobalResults.Snapshot()
}
