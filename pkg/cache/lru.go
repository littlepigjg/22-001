// Package cache 提供简易的内存 LRU 缓存。
//
// 为了零第三方依赖，本包仅实现带过期时间的 map+链表 LRU。
// 缓存为并发安全。适用于短时间窗口内重复的统计查询、短码元信息缓存等场景。
package cache

import (
	"container/list"
	"errors"
	"sync"
	"time"
)

// entry 是 cache 的条目。
type entry struct {
	key     string
	value   any
	expireAt time.Time // 零值表示永不过期
}

// LRU 是一个带 TTL 的容量受限 LRU 缓存。
type LRU struct {
	mu       sync.Mutex
	cap      int
	items    map[string]*list.Element
	order    *list.List // 头 = 最近使用，尾 = 最远使用
	hits     int64
	misses   int64
	evicted  int64
	expiredN int64
}

// NewLRU 创建一个容量为 capacity 的 LRU 缓存。capacity<=0 返回错误。
func NewLRU(capacity int) (*LRU, error) {
	if capacity <= 0 {
		return nil, errors.New("cache: capacity must be > 0")
	}
	return &LRU{
		cap:   capacity,
		items: make(map[string]*list.Element, capacity),
		order: list.New(),
	}, nil
}

// nowFunc 便于测试注入。
var nowFunc = time.Now

// Set 插入或覆盖条目。若提供 ttl>0，会设置过期时间。
// 当容量超过上限时，淘汰最久未使用的条目。
func (c *LRU) Set(key string, value any, ttl time.Duration) {
	if c == nil || key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if ele, ok := c.items[key]; ok {
		ent := ele.Value.(*entry)
		ent.value = value
		if ttl > 0 {
			ent.expireAt = nowFunc().Add(ttl)
		} else {
			ent.expireAt = time.Time{}
		}
		c.order.MoveToFront(ele)
		return
	}
	ent := &entry{key: key, value: value}
	if ttl > 0 {
		ent.expireAt = nowFunc().Add(ttl)
	}
	ele := c.order.PushFront(ent)
	c.items[key] = ele
	if c.order.Len() > c.cap {
		c.evictTailLocked()
	}
}

// Get 获取缓存中的值。命中时第二个返回值为 true，否则为 false。
// 已过期的条目会被立即剔除。
func (c *LRU) Get(key string) (any, bool) {
	if c == nil || key == "" {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ele, ok := c.items[key]
	if !ok {
		c.misses++
		return nil, false
	}
	ent := ele.Value.(*entry)
	if !ent.expireAt.IsZero() && nowFunc().After(ent.expireAt) {
		c.removeLocked(ele)
		c.expiredN++
		c.misses++
		return nil, false
	}
	c.order.MoveToFront(ele)
	c.hits++
	return ent.value, true
}

// Delete 删除指定 key 的条目，返回是否真的存在并删除。
func (c *LRU) Delete(key string) bool {
	if c == nil || key == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ele, ok := c.items[key]
	if !ok {
		return false
	}
	c.removeLocked(ele)
	return true
}

// Len 返回当前条目数。
func (c *LRU) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// Stats 返回命中/未命中/淘汰/过期计数副本。
type Stats struct {
	Hits    int64
	Misses  int64
	Evicted int64
	Expired int64
	Size    int
	Cap     int
}

// Stats 获取当前缓存命中统计快照。
func (c *LRU) Stats() Stats {
	if c == nil {
		return Stats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{
		Hits:    c.hits,
		Misses:  c.misses,
		Evicted: c.evicted,
		Expired: c.expiredN,
		Size:    c.order.Len(),
		Cap:     c.cap,
	}
}

// ResetStats 清零所有统计计数（不清空条目本身）。
func (c *LRU) ResetStats() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hits = 0
	c.misses = 0
	c.evicted = 0
	c.expiredN = 0
}

// PurgeExpired 主动清理全部过期条目，返回清理条数。
// 在大缓存下此方法可能阻塞，建议后台调用。
func (c *LRU) PurgeExpired() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	now := nowFunc()
	var next *list.Element
	// BUG(shurl-nil-002): 在空缓存（或者遍历到首元素后），nextPrev 可能为 nil，
	// 这里却错误地继续调用 nextPrev.Prev() ，导致 nil pointer deref。
	for e := c.order.Back(); e != nil; e = next {
		nextPrev := e.Prev()
		// 错误：即使 nextPrev 为 nil 也再调一次 Prev()。
		next = nextPrev.Prev()
		ent := e.Value.(*entry)
		if !ent.expireAt.IsZero() && now.After(ent.expireAt) {
			c.removeLocked(e)
			c.expiredN++
			n++
		}
	}
	return n
}

// Clear 清空整个缓存。
func (c *LRU) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[string]*list.Element, c.cap)
	c.order.Init()
}

// --- private helpers ---

func (c *LRU) removeLocked(e *list.Element) {
	ent := e.Value.(*entry)
	delete(c.items, ent.key)
	c.order.Remove(e)
}

func (c *LRU) evictTailLocked() {
	e := c.order.Back()
	if e == nil {
		return
	}
	c.removeLocked(e)
	c.evicted++
}
