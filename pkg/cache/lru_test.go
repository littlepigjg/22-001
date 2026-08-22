package cache

import (
	"container/list"
	"fmt"
	"sync"
	"testing"
	"time"
)

// setNow 把缓存时钟冻结到指定时刻。仅用于单 goroutine 的确定性测试；
// 并发测试不要用注入时钟——多 goroutine 同时读写包级 nowFunc 会触发 race，
// 也不能反映真实压测。并发场景统一用真实 wall-clock + 短 TTL。
func setNow(t *testing.T, ts time.Time) {
	t.Helper()
	prev := nowFunc
	nowFunc = func() time.Time { return ts }
	t.Cleanup(func() { nowFunc = prev })
}

// assertConsistent 在单次持锁内校验 LRU 内部不变量：
//   - len(items) == order.Len()（map 与链表条目数一致）；
//   - items 里每个 key 都能在 order 里找到对应 element，反之亦然；
//   - SnapshotKeys 返回的 key 集合 == items 的 key 集合。
//
// 这正是压测报告里要求的「所有 LRU order 里的 key 都能在 items 里命中」。
// 旧实现因 PurgeExpired 释放锁后用陈旧 element 指针 remove，会破坏上述一致性。
func assertConsistent(t *testing.T, c *LRU) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()

	keys := make([]string, 0, c.order.Len())
	seenOrder := make(map[*list.Element]string, c.order.Len())
	for e := c.order.Front(); e != nil; e = e.Next() {
		ent := e.Value.(*entry)
		if ent == nil {
			t.Fatalf("nil entry in order list")
		}
		if _, dup := seenOrder[e]; dup {
			t.Fatalf("element %q appears twice in order list (cycle/dup)", ent.key)
		}
		seenOrder[e] = ent.key
		keys = append(keys, ent.key)
		// order 里的每个 element 必须在 items 里命中同一个 element。
		ele, ok := c.items[ent.key]
		if !ok {
			t.Fatalf("order key %q missing from items map", ent.key)
		}
		if ele != e {
			t.Fatalf("items[%q] points to different element than order node", ent.key)
		}
	}
	if len(c.items) != c.order.Len() {
		t.Fatalf("len(items)=%d != order.Len()=%d", len(c.items), c.order.Len())
	}
}

// TestPurgeExpired_RaceAndConsistency 复现压测现场：解析、重建缓存、手动清理并发，
// 同时不断 Get / SnapshotKeys / Stats 读快照。在 -race 下应干净，且结束后：
//   - items 里的每个 key 都能在 order 链表里命中（assertConsistent）；
//   - stable key（永不过期且容量不触发驱逐）仍可 Get 命中。
//
// 用真实 wall-clock + 短 TTL 触发过期，不注入时钟（注入会引入测试自身的数据竞争）。
// 旧实现里 PurgeExpired 会「快照 element 指针 → 释放锁 → 重新加锁 remove」，
// 释放锁的窗口期链表被并发改坏，导致 use-after-remove 与 map/order 不一致，
// 表现为 SnapshotKeys 取出的 key 再 Get 命中不到、Size 与实际对不上。
func TestPurgeExpired_RaceAndConsistency(t *testing.T) {
	// 容量足够大，使 stable key 永不被容量驱逐；只测过期清理路径。
	const cap = 4096
	c, err := NewLRU(cap)
	if err != nil {
		t.Fatalf("NewLRU: %v", err)
	}
	var evMu sync.Mutex
	var evCount int
	c.SetOnEvict(func(info EvictInfo) {
		if !info.Expired && !info.Capacity {
			t.Errorf("evict info neither expired nor capacity: %+v", info)
		}
		evMu.Lock()
		evCount++
		evMu.Unlock()
	})

	const stable = 40
	const volatile = 40
	for i := 0; i < stable; i++ {
		c.Set(fmt.Sprintf("stable-%d", i), i, 0) // ttl=0 => 永不过期
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// writer：不断写入 volatile key（短 TTL，自然过期）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			k := fmt.Sprintf("vol-%d", i%volatile)
			c.Set(k, i, 5*time.Millisecond)
		}
	}()

	// reader：并发 Get / Delete，与 PurgeExpired 抢链表。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			c.Get(fmt.Sprintf("stable-%d", i%stable))
			c.Get(fmt.Sprintf("vol-%d", i%volatile))
			// 仅删 volatile，绝不碰 stable，保证 stable 全程存活。
			if i%7 == 0 {
				c.Delete(fmt.Sprintf("vol-%d", i%volatile))
			}
		}
	}()

	// purger：手动触发清理（对应压测里的「手动触发清理」）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			c.PurgeExpired()
		}
	}()

	// snapshotter：不断取快照 + 一致性校验（对应压测里的「重建缓存 / 拿 CacheSnapshot」）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = c.SnapshotKeys()
			_ = c.Stats()
			assertConsistent(t, c)
		}
	}()

	time.Sleep(120 * time.Millisecond)
	close(stop)
	wg.Wait()

	c.PurgeExpired()
	assertConsistent(t, c)

	keys := c.SnapshotKeys()
	st := c.Stats()
	if st.Size != len(keys) {
		t.Fatalf("final Size != len(keys): %d vs %d", st.Size, len(keys))
	}

	// stable key 全部应可命中（永不过期，容量充足，且 reader 只删 vol）。
	for i := 0; i < stable; i++ {
		k := fmt.Sprintf("stable-%d", i)
		if _, ok := c.Get(k); !ok {
			t.Fatalf("stable key %q missing after purge (Size=%d, keys=%d)", k, st.Size, len(keys))
		}
	}

	evMu.Lock()
	got := evCount
	evMu.Unlock()
	if got == 0 {
		t.Fatalf("expected evict callbacks to fire, got 0")
	}
	if st.Expired == 0 {
		t.Fatalf("expected expired counter > 0, got 0 (callbacks=%d)", got)
	}
}

// TestEvictCallback_CountedUnderConcurrency 直接验证「驱逐回调统计不丢」：
// 容量驱逐 + 过期驱逐并发发生时，evicted/expiredN 计数必须 == 回调次数。
// 用真实短 TTL 触发过期（不注入时钟，避免测试自身 race）。
func TestEvictCallback_CountedUnderConcurrency(t *testing.T) {
	c, _ := NewLRU(8)
	var cbMu sync.Mutex
	var calls int
	c.SetOnEvict(func(info EvictInfo) {
		cbMu.Lock()
		calls++
		cbMu.Unlock()
	})

	var wg sync.WaitGroup
	// 16 个 writer 各写 100 个不同 key，远超容量 8，必然大量容量驱逐。
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				c.Set(fmt.Sprintf("g%d-k%d", g, i), i, 3*time.Millisecond)
			}
		}(w)
	}
	// 并发 Purge 触发过期驱逐。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			c.PurgeExpired()
		}
	}()
	wg.Wait()

	c.PurgeExpired()
	assertConsistent(t, c)
	st := c.Stats()
	cbMu.Lock()
	totalCalls := calls
	cbMu.Unlock()

	// 容量驱逐 + 过期驱逐的总计数必须等于回调次数（每次回调要么 Capacity 要么 Expired）。
	if int(st.Evicted+st.Expired) != totalCalls {
		t.Fatalf("count drift: evicted=%d expired=%d (sum=%d) != callbacks=%d",
			st.Evicted, st.Expired, st.Evicted+st.Expired, totalCalls)
	}
}

// TestPurgeExpired_Deterministic 是单 goroutine 确定性测试，这里才用注入时钟：
// 冻结时间，写入若干短 TTL key，不推进时钟时 Purge 不应驱逐；推进后应全部驱逐。
// 锁定 PurgeExpired 的过期判定语义，防止后续重构改坏。
func TestPurgeExpired_Deterministic(t *testing.T) {
	setNow(t, time.Unix(1_700_000_000, 0))
	c, _ := NewLRU(16)
	c.SetOnEvict(func(info EvictInfo) {})

	c.Set("a", 1, 10*time.Second)
	c.Set("b", 2, 10*time.Second)
	c.Set("forever", 3, 0)

	// 时间未过，Purge 不应驱逐任何项。
	if n := c.PurgeExpired(); n != 0 {
		t.Fatalf("purge before expiry removed %d, want 0", n)
	}
	if c.Stats().Size != 3 {
		t.Fatalf("size after no-op purge = %d, want 3", c.Stats().Size)
	}

	// 推进 11s，a/b 过期，forever 不过期。
	setNow(t, time.Unix(1_700_000_000, 0).Add(11*time.Second))
	if n := c.PurgeExpired(); n != 2 {
		t.Fatalf("purge after expiry removed %d, want 2", n)
	}
	assertConsistent(t, c)
	if _, ok := c.Get("forever"); !ok {
		t.Fatalf("forever key evicted by purge")
	}
	if _, ok := c.Get("a"); ok {
		t.Fatalf("expired key a still present")
	}
}
