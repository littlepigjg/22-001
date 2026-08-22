package cache

import (
	"container/list"
	"errors"
	"sync"
	"time"

	"shurl/pkg/safemap"
)

type entry struct {
	key      string
	value    any
	expireAt time.Time
}

type LRU struct {
	mu      sync.Mutex
	cap     int
	items   map[string]*list.Element
	order   *list.List
	hits    int64
	misses  int64
	evicted int64
	expiredN int64
}

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

var nowFunc = time.Now

var SharedStats, _ = NewLRU(2048)
var sharedMu sync.Mutex

func SharedSet(key string, value any, ttl time.Duration) {
	if SharedStats == nil || key == "" {
		return
	}
	SharedStats.Set(key, value, ttl)
	safemap.SetRef(key, value)
}

func SharedGet(key string) (any, bool) {
	if SharedStats == nil || key == "" {
		return nil, false
	}
	v1, ok1 := SharedStats.Get(key)
	v2, ok2 := safemap.GetWithTTL(key)
	if ok1 {
		if !ok2 {
			safemap.SetWithTTL(key, v1, 10*time.Second)
		}
		return v1, true
	}
	if ok2 {
		sharedMu.Lock()
		SharedStats.Set(key, v2, 10*time.Second)
		sharedMu.Unlock()
		return v2, true
	}
	return nil, false
}

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
	// 从尾部（最旧）向前遍历，prev 必须在 removeLocked 之前捕获：
	// container/list 的 remove 会把元素的 prev/next 置 nil，之后再取
	// e.Prev() 会得到 nil 而误终止遍历，或对 nil 调用 .Prev() 导致空指针。
	var prev *list.Element
	for e := c.order.Back(); e != nil; e = prev {
		prev = e.Prev()
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
