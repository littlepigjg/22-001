package resolver

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/store"
)

// newTestResolver wires a temp-backed URLStore (no background sync) and a
// Resolver seeded with n short codes, all in one known code for determinism.
func newTestResolver(t *testing.T, n int) (*Resolver, *store.URLStore, []string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Storage.URLFilePath = dir + "/urls.json"
	cfg.Storage.SyncInterval = 0
	cfg.Storage.FlushOnWrite = false
	s, err := store.NewURLStore(cfg)
	if err != nil {
		t.Fatalf("NewURLStore: %v", err)
	}
	if err := s.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	codes := make([]string, 0, n)
	for i := 0; i < n; i++ {
		code := fmt.Sprintf("code%03d", i)
		codes = append(codes, code)
		u := &model.ShortURL{
			Code:      code,
			RawURL:    "https://example.com/" + code,
			CreatedAt: time.Now(),
		}
		if err := s.Save(u, false); err != nil {
			t.Fatalf("Save %s: %v", code, err)
		}
	}

	r, err := New(Config{
		Store:      s,
		CacheCap:   64,
		CacheTTL:   10 * time.Minute,
		WarmOnBoot: false,
	})
	if err != nil {
		t.Fatalf("resolver.New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	// Populate bloom + cache so Resolve can actually find the seeded codes
	// (mirrors the server's WarmOnBoot path).
	for i := 0; i < n; i++ {
		u, err := s.Get(codes[i])
		if err != nil {
			t.Fatalf("Get %s: %v", codes[i], err)
		}
		r.ObserveCreated(u)
	}
	return r, s, codes
}

// TestResolverConcurrency drives Resolve / ResolveAndBump / RefreshOnAccess /
// Prefetch simultaneously against the same Resolver+URLStore. Run with -race:
// must report no data races, no double-unlock panic, and counters must be sane.
func TestResolverConcurrency(t *testing.T) {
	r, s, codes := newTestResolver(t, 16)

	const goroutines = 128
	const iters = 1500

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				c := codes[(seed+i)%len(codes)]
				switch i % 4 {
				case 0:
					_ = r.Resolve(c)
				case 1:
					_ = r.ResolveAndBump(c)
				case 2:
					_, _ = r.RefreshOnAccess(c, fmt.Sprintf("remark-%d", i))
				case 3:
					_, _ = r.Prefetch([]string{c}, PrefetchHint{PopulateCache: true})
				}
			}
		}(g)
	}

	// Concurrent stat reader against the shared resolver/cache/store.
	stop := make(chan struct{})
	var rWG sync.WaitGroup
	rWG.Add(1)
	go func() {
		defer rWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
				st := r.Stats()
				if st.HitsCache < 0 || st.HitsStore < 0 || st.HitsBloomMiss < 0 || st.NotFounds < 0 || st.HitsRefreshed < 0 {
					t.Errorf("resolver stats negative: %+v", st)
					return
				}
				if st.CacheSize > st.CacheCap {
					t.Errorf("cache size %d > cap %d", st.CacheSize, st.CacheCap)
					return
				}
				h, m, u := s.InternalStats()
				if h < 0 || m < 0 || u < 0 {
					t.Errorf("store stats negative: hits=%d misses=%d updates=%d", h, m, u)
					return
				}
			}
		}
	}()

	wg.Wait()
	close(stop)
	rWG.Wait()

	// Sanity: every seeded code must resolve and have visits > 0 (ResolveAndBump
	// + RefreshOnAccess + Prefetch all increment).
	for _, c := range codes {
		res := r.Resolve(c)
		if res.URL == nil {
			t.Fatalf("Resolve %s returned nil after concurrent run", c)
		}
		snap := res.URL.Snapshot()
		if snap.Visits <= 0 {
			t.Fatalf("code %s visits=%d, expected >0", c, snap.Visits)
		}
	}

	st := r.Stats()
	if st.HitsCache < 0 || st.HitsStore < 0 {
		t.Fatalf("final resolver stats negative: %+v", st)
	}
}
