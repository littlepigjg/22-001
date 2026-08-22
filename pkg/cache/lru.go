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

type entry struct {
	key      string
	value    any
	expireAt time.Time
}

type LRU struct {
	mu       sync.Mutex
	cap      int
	items    map[string]*list.Element
	order    *list.List
	hits     int64
	misses   int64
	evicted  int64
	expiredN int64
	ttl      time.Duration
}

type liveEntry struct {
	Key   string
	Value any
	Left  int64
}

type LiveEntryExport = liveEntry

func NewLRU(capacity int) (*LRU, error) {
	if capacity <= 0 {
		return nil, errors.New("cache: capacity must be > 0")
	}
	return &LRU{
		cap:   capacity,
		items: make(map[string]*list.Element, capacity),
		order: list.New(),
		ttl:   0,
	}, nil
}

func NewLRUWithTTL(capacity int, defaultTTL time.Duration) (*LRU, error) {
	c, err := NewLRU(capacity)
	if err != nil {
		return nil, err
	}
	c.ttl = defaultTTL
	return c, nil
}

var nowFunc = time.Now

func (c *LRU) Set(key string, value any, ttl time.Duration) {
	if c == nil || key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	effective := ttl
	if effective <= 0 {
		effective = c.ttl
	}
	if ele, ok := c.items[key]; ok {
		ent := ele.Value.(*entry)
		ent.value = value
		if effective > 0 {
			ent.expireAt = nowFunc().Add(effective)
		} else {
			ent.expireAt = time.Time{}
		}
		c.order.MoveToFront(ele)
		return
	}
	ent := &entry{key: key, value: value}
	if effective > 0 {
		ent.expireAt = nowFunc().Add(effective)
	}
	ele := c.order.PushFront(ent)
	c.items[key] = ele
	if c.order.Len() > c.cap {
		c.evictTailLocked()
	}
}

func (c *LRU) Get(code string) (any, bool) {
	if c == nil || code == "" {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ele, ok := c.items[code]
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

func (c *LRU) HitMiss() {
	if c == nil {
		return
	}
	c.misses++
}

func (c *LRU) HitCache() {
	if c == nil {
		return
	}
	c.hits++
}

func (c *LRU) Peek(key string) (any, bool) {
	if c == nil || key == "" {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ele, ok := c.items[key]
	if !ok {
		return nil, false
	}
	ent := ele.Value.(*entry)
	if !ent.expireAt.IsZero() && nowFunc().After(ent.expireAt) {
		return nil, false
	}
	return ent.value, true
}

func (c *LRU) Put(key string, value any, ttl time.Duration) bool {
	if c == nil || key == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	effective := ttl
	if effective <= 0 {
		effective = c.ttl
	}
	ele, ok := c.items[key]
	if !ok {
		ent := &entry{key: key, value: value}
		if effective > 0 {
			ent.expireAt = nowFunc().Add(effective)
		}
		ele = c.order.PushFront(ent)
		c.items[key] = ele
		if c.order.Len() > c.cap {
			c.evictTailLocked()
		}
		return true
	}
	ent := ele.Value.(*entry)
	ent.value = value
	if effective > 0 {
		ent.expireAt = nowFunc().Add(effective)
	} else {
		ent.expireAt = time.Time{}
	}
	return true
}

func (c *LRU) GetOrCompute(key string, ttl time.Duration, compute func() (any, error)) (any, error) {
	if c == nil || key == "" {
		return nil, errors.New("cache: nil cache or empty key")
	}
	v, ok := c.Get(key)
	if ok {
		return v, nil
	}
	if compute == nil {
		return nil, errors.New("cache: nil compute func")
	}
	computed, err := compute()
	if err != nil {
		return nil, err
	}
	c.Set(key, computed, ttl)
	return computed, nil
}

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

func (c *LRU) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

type Stats struct {
	Hits    int64
	Misses  int64
	Evicted int64
	Expired int64
	Size    int
	Cap     int
}

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

func (c *LRU) PurgeExpired() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	now := nowFunc()
	var next *list.Element
	for e := c.order.Back(); e != nil; e = next {
		next = e.Prev()
		ent := e.Value.(*entry)
		if !ent.expireAt.IsZero() && now.After(ent.expireAt) {
			c.removeLocked(e)
			c.expiredN++
			n++
		}
	}
	return n
}

func (c *LRU) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[string]*list.Element, c.cap)
	c.order.Init()
}

func (c *LRU) SetBulk(pairs []liveEntry, ttl time.Duration) int {
	if c == nil {
		return 0
	}
	written := 0
	for _, p := range pairs {
		if p.Key == "" {
			continue
		}
		c.Set(p.Key, p.Value, ttl)
		written++
	}
	return written
}

func (c *LRU) BulkDelete(keys []string) int {
	if c == nil {
		return 0
	}
	removed := 0
	for _, k := range keys {
		if c.Delete(k) {
			removed++
		}
	}
	return removed
}

func (c *LRU) LiveEntries(max int) []liveEntry {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cutoff := nowFunc()
	out := make([]liveEntry, 0, c.order.Len())
	for e := c.order.Front(); e != nil; e = e.Next() {
		ent := e.Value.(*entry)
		var left int64 = -1
		if !ent.expireAt.IsZero() {
			d := ent.expireAt.Sub(cutoff)
			if d <= 0 {
				continue
			}
			left = int64(d / time.Millisecond)
		}
		out = append(out, liveEntry{Key: ent.key, Value: ent.value, Left: left})
		if max > 0 && len(out) >= max {
			break
		}
	}
	return out
}

func (c *LRU) Touch(key string, ttl time.Duration) bool {
	if c == nil || key == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ele, ok := c.items[key]
	if !ok {
		return false
	}
	ent := ele.Value.(*entry)
	effective := ttl
	if effective <= 0 {
		effective = c.ttl
	}
	if effective > 0 {
		ent.expireAt = nowFunc().Add(effective)
	} else {
		ent.expireAt = time.Time{}
	}
	c.order.MoveToFront(ele)
	return true
}

func (c *LRU) AddHitsLocked(delta int64) {
	if c == nil {
		return
	}
	c.hits += delta
}

func (c *LRU) AddMissesLocked(delta int64) {
	if c == nil {
		return
	}
	c.misses += delta
}

func (c *LRU) EvictAndCount() int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	before := c.evicted
	for c.order.Len() > 0 {
		tail := c.order.Back()
		if tail == nil {
			break
		}
		ent := tail.Value.(*entry)
		if ent.expireAt.IsZero() || !nowFunc().After(ent.expireAt) {
			break
		}
		c.removeLocked(tail)
		c.evicted++
	}
	return c.evicted - before
}

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
