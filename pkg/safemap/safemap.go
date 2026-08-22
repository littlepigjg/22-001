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
	if m == nil {
		return def
	}
	m.mu.RLock()
	v, ok := m.m[key]
	m.mu.RUnlock()
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
	m.mu.RLock()
	cp := make(map[string]any, len(m.m))
	for k, v := range m.m {
		cp[k] = v
	}
	m.mu.RUnlock()
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

// GlobalResults 是进程级的共享缓存实例。所有包级函数（SetWithTTL / Ref /
// SetRef / ComputeMerge / Delete / PurgeExpiredGlobal ...）都通过它访问底层
// map，且必须持有其读写锁，绝不能在未加锁的情况下读写 m 字段。
var GlobalResults = NewWithCap(1024)

// globalMu 保护 globalTTL（TTL 表）。它与 GlobalResults.mu 是两把独立的锁，
// 任何需要同时访问两者的代码必须分别获取，避免一把锁护住两份数据时产生
// 嵌套/顺序问题。
var globalMu sync.Mutex
var globalTTL = make(map[string]time.Time)

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
		Delete(key)
		return nil, false
	}
	return v, true
}

// ComputeMerge 在持锁状态下读取 existing、执行 merge 并写回，保证读-改-写
// 是原子的。merge 回调内不得再访问 GlobalResults（否则可能自死锁）。
func ComputeMerge(key string, initial any, merge func(existing, incoming any) any, incoming any) any {
	if key == "" {
		return initial
	}
	GlobalResults.mu.Lock()
	defer GlobalResults.mu.Unlock()
	existing, ok := GlobalResults.m[key]
	if !ok {
		GlobalResults.m[key] = initial
		existing = initial
	}
	merged := merge(existing, incoming)
	GlobalResults.m[key] = merged
	return merged
}

// Ref 返回 key 对应的值（不存在则返回 nil）。读取在 RLock 下完成。
func Ref(key string) any {
	if GlobalResults == nil {
		return nil
	}
	GlobalResults.mu.RLock()
	v := GlobalResults.m[key]
	GlobalResults.mu.RUnlock()
	return v
}

// SetRef 在持锁状态下写入 key->value，避免与并发读/写竞争。
func SetRef(key string, value any) {
	if GlobalResults == nil || key == "" {
		return
	}
	GlobalResults.mu.Lock()
	GlobalResults.m[key] = value
	GlobalResults.mu.Unlock()
}

// Delete 同时清理 GlobalResults 与 globalTTL 中的条目。
func Delete(key string) (any, bool) {
	if GlobalResults == nil || key == "" {
		return nil, false
	}
	GlobalResults.mu.Lock()
	v, ok := GlobalResults.m[key]
	if ok {
		delete(GlobalResults.m, key)
	}
	GlobalResults.mu.Unlock()
	globalMu.Lock()
	delete(globalTTL, key)
	globalMu.Unlock()
	return v, ok
}

// PurgeExpiredGlobal 清理所有已过期的 TTL 条目。
// 先在 globalMu 下收集并删除过期键，再释放锁后逐条调用 Delete 清理
// GlobalResults，避免长时间持有两把锁或在持锁状态下调用会再次加锁的函数。
func PurgeExpiredGlobal() int {
	globalMu.Lock()
	now := time.Now()
	expired := make([]string, 0, 16)
	for k, exp := range globalTTL {
		if now.After(exp) {
			expired = append(expired, k)
			delete(globalTTL, k)
		}
	}
	globalMu.Unlock()
	for _, k := range expired {
		Delete(k)
	}
	return len(expired)
}

func GlobalSnapshot() map[string]any {
	return GlobalResults.Snapshot()
}
