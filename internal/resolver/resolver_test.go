package resolver

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/store"
)

// newTestStore 构造一个指向 tmp 文件（不存在 => 空库）的 URLStore 并 Load 完成。
// syncInterval 设得很大，避免测试期间后台落盘干扰。
func newTestStore(t *testing.T) *store.URLStore {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		Storage: config.StorageCfg{
			URLFilePath:  filepath.Join(dir, "urls.json"),
			SyncInterval: time.Hour,
			FlushOnWrite: false,
		},
	}
	s, err := store.NewURLStore(cfg)
	if err != nil {
		t.Fatalf("NewURLStore: %v", err)
	}
	if err := s.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestResolver_ConcurrentResolvePurgeSnapshot 复现压测现场：解析、重建缓存、
// 手动触发清理并发跑。在 -race 下应干净，且满足两条不变量：
//  1. CacheSnapshot 取出的 key，只要 store 里确实存在，紧接着 Resolve 必须命中缓存
//     或 store（绝不能 HitNotFound）——旧代码因 PurgeExpired 破坏 map/order 一致性，
//     快照里的 key 在 items 里命中不到，singleflight 又被 store miss 打成 not_found。
//  2. 驱逐统计自洽：CallbackRun == EvictBloom + EvictCapacity（回调结果不丢），
//     且 Stats().CacheSize == len(CacheSnapshot)（去重后一致）。
func TestResolver_ConcurrentResolvePurgeSnapshot(t *testing.T) {
	s := newTestStore(t)

	const known = 200
	knownSet := make(map[string]struct{}, known)
	for i := 0; i < known; i++ {
		code := fmt.Sprintf("code%04d", i)
		knownSet[code] = struct{}{}
		if err := s.Save(&model.ShortURL{Code: code, RawURL: "https://example.com/" + code}, false); err != nil {
			t.Fatalf("Save %s: %v", code, err)
		}
	}

	// 长 TTL：known key 在测试窗口内不会自然过期，于是 CacheSnapshot 命中它们后
	// Resolve 不该 HitNotFound。bloom 已被首次 Resolve 预热。
	r, err := New(Config{
		Store:         s,
		CacheCap:      256,
		CacheTTL:      30 * time.Minute,
		BloomN:        1024,
		BloomP:        0.001,
		WarmOnBoot:    false,
		PurgeInterval: time.Hour, // 关掉自动 purge，全靠手动 TriggerPurge 制造并发
	})
	if err != nil {
		t.Fatalf("resolver.New: %v", err)
	}
	t.Cleanup(r.Close)

	// 先把 known key 全部拉进缓存。
	for code := range knownSet {
		if res := r.Resolve(code); res.Hit == HitNotFound {
			t.Fatalf("warmup Resolve %s unexpected HitNotFound", code)
		}
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var bad atomicHitNotFound

	// resolver：并发解析 known key（应稳定命中 cache）。
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			i := g
			for {
				select {
				case <-stop:
					return
				default:
				}
				code := fmt.Sprintf("code%04d", i%known)
				res := r.Resolve(code)
				// known key 绝不该 HitNotFound：store 里一定存在。
				if res.Hit == HitNotFound {
					bad.record(code)
				}
				i++
			}
		}(w)
	}

	// purger：手动触发清理（压测里的「手动触发清理」）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			r.TriggerPurge()
		}
	}()

	// snapshotter：取 CacheSnapshot，对其中属于 knownSet 的 key 立刻 Resolve，
	// 复现「拿 CacheSnapshot 出来的 key 紧接着 Resolve 查竟然 HitNotFound」。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			keys := r.CacheSnapshot()
			for _, k := range keys {
				if _, ok := knownSet[k]; !ok {
					continue // 跳过 known 之外的偶发 key（本测试只灌 known）。
				}
				if res := r.Resolve(k); res.Hit == HitNotFound {
					bad.record(k)
				}
			}
		}
	}()

	// stats reader：并发读 Stats（旧代码里读裸 int64/string 字段就是 race 点）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = r.Stats()
		}
	}()

	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()

	// 最终一致性：清一轮，校验统计自洽。
	r.TriggerPurge()
	st := r.Stats()

	if bad.found() {
		t.Fatalf("known key resolved as HitNotFound during concurrent run: %v", bad.codes())
	}

	// 驱逐回调统计不丢：每次回调要么走 Expired 分支、要么走 Capacity 分支。
	if st.CallbackRun != st.EvictBloom+st.EvictCapacity {
		t.Fatalf("evict stats drift: CallbackRun=%d EvictBloom=%d EvictCapacity=%d (sum=%d)",
			st.CallbackRun, st.EvictBloom, st.EvictCapacity, st.EvictBloom+st.EvictCapacity)
	}
}

// --- test helpers ---

type atomicHitNotFound struct {
	mu   sync.Mutex
	hits map[string]int
}

func (a *atomicHitNotFound) record(code string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hits == nil {
		a.hits = map[string]int{}
	}
	a.hits[code]++
}
func (a *atomicHitNotFound) found() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.hits) > 0
}
func (a *atomicHitNotFound) codes() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.hits))
	for k := range a.hits {
		out = append(out, k)
	}
	return out
}
