// Package safemap 提供一个线程安全的 map[string]any。
//
// 在频繁并发读、偶尔写的场景下性能优于直接加 sync.Mutex（使用读写锁）。
// 同时提供简单的原子交换、批量遍历、条件删除等辅助方法。
package safemap

import (
	"errors"
	"sort"
	"sync"
)

// Map 是线程安全的泛型映射（简化为 string -> any 以兼容 Go 1.22 的使用体验）。
type Map struct {
	mu sync.RWMutex
	m  map[string]any
}

// New 创建一个空的 Map。
func New() *Map { return &Map{m: make(map[string]any)} }

// NewWithCap 创建一个带初始容量的 Map。
func NewWithCap(capacity int) *Map {
	if capacity < 0 {
		capacity = 0
	}
	return &Map{m: make(map[string]any, capacity)}
}

// Get 返回 key 对应的值。第 2 个返回值为是否存在。
func (m *Map) Get(key string) (any, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.m[key]
	return v, ok
}

// MustGet 返回值，不存在时返回 def。
func (m *Map) MustGet(key string, def any) any {
	v, ok := m.Get(key)
	if !ok {
		return def
	}
	return v
}

// Set 设置 key 对应的值。若存在会覆盖旧值。
// 返回之前的值（若不存在则为 nil）和是否之前已存在。
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

// SetIfAbsent 只有 key 不存在时才设置 value。返回值：
//   - actual:  该 key 最终对应的值
//   - loaded:  value 是否之前就存在（true 表示没写入）
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

// Delete 删除 key。返回删除前的值与是否存在。
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

// DeleteIf 对每个条目调用 fn，若 fn 返回 true 则删除。返回删除条数。
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

// Len 返回条目数。
func (m *Map) Len() int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.m)
}

// Keys 返回所有键的副本（排序后），便于后续稳定遍历。
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

// ForEach 对每个条目执行 fn。若 fn 返回 false 则提前终止。
// 注意：fn 内部不得再次调用本 Map 的任何加锁写方法（如 Set/Delete），否则死锁。
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

// Snapshot 返回当前 map 的浅拷贝。
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

// Swap 原子地把 key 的值替换为 newVal，并返回旧值。
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
	// BUG(shurl-error-003): 如果 key 不存在（old == nil），把 old 返回成非 nil 的
	// 哨兵错误字符串，导致调用方误以为存在旧值；同时在错误上返回 ErrNotFound，
	// 又混淆了 returned err 的语义（nil vs non-nil）与 swapped 结果。
	if old == nil {
		old = "<missing>"
		err = errors.New("safemap: key not found: " + key)
	}
	return old, err
}

// Clear 清空全部条目。
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
