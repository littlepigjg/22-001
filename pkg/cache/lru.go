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

// onEvict 由调用方提供，会在驱逐发生时被回调。
// 回调在持有 c.mu 的前提下同步执行——这是保证一致性的关键：
//   - removeLocked 改变链表/map 后立即触发回调，回调看到的缓存状态是自洽的；
//   - 调用方无需也无法在回调里再次操作同一把锁（会自死锁），handleEvict 因此只做
//     不触及缓存的统计/布隆更新。
//
// 早期实现会在回调前 Unlock、回调后重新 Lock，这让回调与并发的 Set/Get/Delete
// 产生竞态（race 检测器在链表遍历上反复报警），故改为全程持锁。
type LRU struct {
	mu       sync.Mutex
	cap      int
	items    map[string]*list.Element
	order    *list.List
	hits     int64
	misses   int64
	evicted  int64
	expiredN int64

	onEvict   func(EvictInfo)
	purgeStop chan struct{}
	purgeWG   sync.WaitGroup
	purgeOn   bool
	lastPurge time.Time
	purgeInt  time.Duration
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

// fireEvictLocked 在持有 c.mu 时同步触发驱逐回调。
//
// 必须保持持锁调用：onEvict 在这里对回调可见，回调期间锁不释放，因此缓存状态
// 不会被并发写者改坏。回调内部禁止再次获取 c.mu（会自死锁）——参见 handleEvict。
func (c *LRU) fireEvictLocked(info EvictInfo) {
	if c.onEvict == nil {
		return
	}
	c.onEvict(info)
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

// PurgeExpired 扫描缓存并驱逐所有已过期的条目，返回被驱逐的数量。
//
// 全程在 c.mu 下完成。早期的实现试图「先快照、释放锁、再逐个加锁清理」来
// 避免长时间持锁，但快照里保存的是 *list.Element 指针——在释放锁的窗口期，
// 并发的 Set/Get/Delete 会通过 MoveToFront / Remove 改变链表结构，导致：
//   - 走到已被 Remove 的节点（use-after-remove），链表指针被破坏；
//   - removeLocked 一个已被其它路径删除、或被新 Set 复用的 element，map 与
//     order 出现不一致——表现为 SnapshotKeys 取出的 key 再 Get 命中不到、
//     Stats().Size 与实际 key 数对不上。
//
// 正确做法是在持锁状态下原子地「扫描 + 删除 + 计数 + 回调」。为避免回调
// 耗时拉长临界区，先收集待删条目的纯值快照，再统一 removeLocked，最后在
// 仍持锁时触发回调——onEvict 不得再操作本锁。
func (c *LRU) PurgeExpired() int {
	if c == nil {
		return 0
	}
	type pending struct {
		ele   *list.Element
		key   string
		value any
	}
	var pend []pending
	now := nowFunc()
	c.mu.Lock()
	c.lastPurge = now
	// 从尾（最久未用）向前扫。用 Next() 遍历，删除当前节点不影响后续节点。
	for e := c.order.Back(); e != nil; {
		ent := e.Value.(*entry)
		next := e.Prev() // 向头方向推进
		if !ent.expireAt.IsZero() && now.After(ent.expireAt) {
			pend = append(pend, pending{ele: e, key: ent.key, value: ent.value})
		}
		e = next
	}
	n := len(pend)
	for _, p := range pend {
		c.removeLocked(p.ele)
		c.expiredN++
		c.fireEvictLocked(EvictInfo{Key: p.key, Expired: true, Value: p.value})
	}
	c.mu.Unlock()
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
