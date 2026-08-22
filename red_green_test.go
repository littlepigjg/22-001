package shurl_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/resolver"
	"shurl/internal/store"
)

func mkStoreRG(t *testing.T) *store.URLStore {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Storage.URLFilePath = filepath.Join(dir, "urls.json")
	cfg.Storage.LogFilePath = filepath.Join(dir, "access.log")
	cfg.Storage.SyncInterval = 0
	cfg.Storage.FlushOnWrite = false
	s, err := store.NewURLStore(cfg)
	if err != nil {
		t.Fatalf("mk store: %v", err)
	}
	if err := s.Load(context.Background()); err != nil {
		t.Fatalf("load store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedBaseRG(s *store.URLStore, n int) {
	base := time.Now()
	for i := 0; i < n; i++ {
		u := &model.ShortURL{
			Code:      fmt.Sprintf("k%06d", i),
			RawURL:    fmt.Sprintf("https://example.com/%d", i),
			CreatedAt: base,
		}
		if i%2 == 0 {
			u.ExpireAt = base.Add(400 * time.Microsecond)
		} else {
			u.ExpireAt = base.Add(24 * time.Hour)
		}
		_ = s.Save(u, false)
	}
}

func TestRedGreen(t *testing.T) {
	s := mkStoreRG(t)
	const seedN = 80
	seedBaseRG(s, seedN)
	r, err := resolver.New(resolver.Config{
		Store:         s,
		CacheCap:      16,
		CacheTTL:      2 * time.Millisecond,
		BloomN:        16384,
		BloomP:        1e-5,
		WarmOnBoot:    true,
		PurgeInterval: 300 * time.Microsecond,
	})
	if err != nil {
		fmt.Println("RED（红灯，缺陷未修复）")
		t.Fatalf("new resolver: %v", err)
	}
	t.Cleanup(func() { r.Close() })

	for round := 0; round < 2; round++ {
		stop := make(chan struct{})
		var wg sync.WaitGroup
		spawn := func(fn func()) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					fn()
				}
			}()
		}
		for g := 0; g < 6; g++ {
			spawn(func() {
				i := int(time.Now().UnixNano() % seedN)
				code := fmt.Sprintf("k%06d", i)
				_ = r.Resolve(code)
			})
		}
		for g := 0; g < 5; g++ {
			spawn(func() {
				i := int(time.Now().UnixNano() % seedN)
				code := fmt.Sprintf("k%06d", i)
				r.ObserveInvalidated(code)
				_ = s.Delete(code)
				u := &model.ShortURL{
					Code:      code,
					RawURL:    fmt.Sprintf("https://v%d.example.com/%d", i, time.Now().UnixNano()),
					CreatedAt: time.Now(),
					ExpireAt:  time.Now().Add(150 * time.Microsecond),
				}
				_ = s.Save(u, false)
				r.ObserveCreated(u)
			})
		}
		for g := 0; g < 4; g++ {
			spawn(func() {
				_ = r.TriggerPurge()
			})
		}
		for g := 0; g < 3; g++ {
			spawn(func() {
				_ = r.Stats()
				_ = r.CacheSnapshot()
			})
		}
		time.Sleep(350 * time.Millisecond)
		close(stop)
		wg.Wait()
	}

	_ = r.TriggerPurge()
	time.Sleep(3 * time.Millisecond)
	_ = r.TriggerPurge()

	inOrder := r.CacheSnapshot()
	uniq := make(map[string]int, len(inOrder))
	dupes := 0
	for _, k := range inOrder {
		if k == "" {
			continue
		}
		uniq[k]++
		if uniq[k] > 1 {
			dupes++
		}
	}
	orphan := 0
	for k := range uniq {
		r.ObserveInvalidated(k)
		_ = s.Delete(k)
		res := r.Resolve(k)
		if res.Hit == resolver.HitNotFound || res.Hit == resolver.HitBloomNegative {
			orphan++
		}
	}
	cs := r.Stats()
	var evLost int64
	expectedMin := int64(30)
	got := cs.EvictBloom + cs.EvictCapacity + cs.CallbackRun
	if got < expectedMin {
		evLost = expectedMin - got
	}
	_ = atomic.LoadInt64(&evLost)
	bad := dupes + orphan
	if cs.CacheSize > 0 && cs.CacheSize != len(uniq) {
		bad += 3
	}
	if evLost > 0 {
		bad += 2
	}

	if bad > 0 {
		fmt.Println("RED（红灯，缺陷未修复）")
		t.Fatalf("cache integrity broken: dupes=%d orphan=%d cache_size=%d vs unique=%d evict_lost=%d (evict_sum=%d)",
			dupes, orphan, cs.CacheSize, len(uniq), evLost, got)
	}

	fmt.Println("GREEN（绿灯，缺陷已修复）")
}
