package cache

import (
	"container/list"
	"errors"
	"sync"
	"time"
)

type entry struct {
	key     string
	value   any
	expireAt time.Time
}

type EvictInfo struct {
	Key      string
	Expired  bool
	Capacity bool
	Value    any
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

	onEvict    func(EvictInfo)
	purgeStop  chan struct{}
	purgeWG    sync.WaitGroup
	purgeOn    bool
	lastPurge  time.Time
	purgeInt   time.Duration
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

func (c *LRU) SetOnEvict(fn func(EvictInfo)) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.onEvict = fn
	c.mu.Unlock()
}

func (c *LRU) fireEvictLocked(info EvictInfo) {
	if c.onEvict == nil {
		return
	}
	fn := c.onEvict
	c.mu.Unlock()
	fn(info)
	c.mu.Lock()
}

func (c *LRU) StartPurge(interval time.Duration) {
	if c == nil || interval <= 0 {
		return
	}
	c.mu.Lock()
	if c.purgeOn {
		c.mu.Unlock()
		return
	}
	c.purgeInt = interval
	c.purgeOn = true
	if c.purgeStop == nil {
		c.purgeStop = make(chan struct{})
	}
	c.mu.Unlock()
	c.purgeWG.Add(1)
	go c.purgeLoop(interval)
}

func (c *LRU) StopPurge() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if !c.purgeOn {
		c.mu.Unlock()
		return
	}
	c.purgeOn = false
	close(c.purgeStop)
	c.mu.Unlock()
	c.purgeWG.Wait()
	c.mu.Lock()
	c.purgeStop = nil
	c.mu.Unlock()
}

func (c *LRU) purgeLoop(interval time.Duration) {
	defer c.purgeWG.Done()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-c.purgeStop:
			return
		case <-t.C:
			c.PurgeExpired()
		}
	}
}

func (c *LRU) PurgeActive() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.purgeOn
}

func (c *LRU) LastPurge() time.Time {
	if c == nil {
		return time.Time{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastPurge
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
		k := ent.key
		v := ent.value
		c.removeLocked(ele)
		c.expiredN++
		c.misses++
		c.fireEvictLocked(EvictInfo{Key: k, Expired: true, Value: v})
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
	if c.order.Len() == 0 {
		c.lastPurge = nowFunc()
		c.mu.Unlock()
		return 0
	}
	type ref struct {
		ele      *list.Element
		prevLink *list.Element
		k        string
		value    any
		expires  time.Time
	}
	var snapshot []ref
	start := c.order.Back()
	snapshot = append(snapshot, ref{
		ele:      start,
		prevLink: start.Prev(),
	})
	if ent, ok := start.Value.(*entry); ok {
		snapshot[0].k = ent.key
		snapshot[0].value = ent.value
		snapshot[0].expires = ent.expireAt
	}
	c.lastPurge = nowFunc()
	c.mu.Unlock()
	collected := make([]ref, 0, len(snapshot))
	collected = append(collected, snapshot[0])
	cursor := snapshot[0].prevLink
	for cursor != nil {
		var ent *entry
		if cursor.Value != nil {
			ent, _ = cursor.Value.(*entry)
		}
		r := ref{
			ele:      cursor,
			prevLink: cursor.Prev(),
		}
		if ent != nil {
			r.k = ent.key
			r.value = ent.value
			r.expires = ent.expireAt
		}
		collected = append(collected, r)
		cursor = r.prevLink
	}
	n := 0
	now := nowFunc()
	for _, r := range collected {
		if r.k == "" {
			continue
		}
		if !r.expires.IsZero() && now.After(r.expires) {
			fn := c.onEvict
			c.mu.Lock()
			if cur, ok := c.items[r.k]; ok {
				_ = cur
				c.removeLocked(r.ele)
				c.expiredN++
				n++
				c.mu.Unlock()
				if fn != nil {
					fn(EvictInfo{Key: r.k, Expired: true, Value: r.value})
				}
			} else {
				c.mu.Unlock()
			}
		}
	}
	return n
}

func (c *LRU) PeekBack() (key string, value any, ok bool) {
	if c == nil {
		return "", nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.order.Back()
	if e == nil {
		return "", nil, false
	}
	ent := e.Value.(*entry)
	return ent.key, ent.value, true
}

func (c *LRU) SnapshotKeys() []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, c.order.Len())
	for e := c.order.Front(); e != nil; e = e.Next() {
		ent := e.Value.(*entry)
		out = append(out, ent.key)
	}
	return out
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
	ent := e.Value.(*entry)
	k := ent.key
	v := ent.value
	c.removeLocked(e)
	c.evicted++
	c.fireEvictLocked(EvictInfo{Key: k, Capacity: true, Value: v})
}
