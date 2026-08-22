package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
)

// newTestStore builds a URLStore backed by a temp file, ready for use, with
// background sync disabled so it never races the test goroutines on disk.
func newTestStore(t *testing.T) *URLStore {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Storage.URLFilePath = filepath.Join(dir, "urls.json")
	cfg.Storage.SyncInterval = 0 // no background syncer
	cfg.Storage.FlushOnWrite = false
	s, err := NewURLStore(cfg)
	if err != nil {
		t.Fatalf("NewURLStore: %v", err)
	}
	if err := s.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seed(t *testing.T, s *URLStore, n int) []*model.ShortURL {
	t.Helper()
	out := make([]*model.ShortURL, 0, n)
	for i := 0; i < n; i++ {
		u := &model.ShortURL{
			Code:      fmt.Sprintf("code%03d", i),
			RawURL:     "https://example.com/" + fmt.Sprintf("code%03d", i),
			CreatedAt:  time.Now(),
			MaxVisits:  0,
			Visits:     0,
		}
		if err := s.Save(u, false); err != nil {
			t.Fatalf("Save %s: %v", u.Code, err)
		}
		out = append(out, u)
	}
	return out
}

// TestURLStoreConcurrency hammers Get / IncrementVisits / UpdateFields and the
// Bump* counters from many goroutines against the same shared store. Run with
// -race: must report no data races, no panic, and visits must be consistent.
func TestURLStoreConcurrency(t *testing.T) {
	s := newTestStore(t)
	codes := seed(t, s, 8)

	const goroutines = 120
	const iters = 2000

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				c := codes[(seed+i)%len(codes)].Code
				switch i % 4 {
				case 0:
					_, _ = s.Get(c)
				case 1:
					if u, err := s.IncrementVisits(c); err != nil {
						t.Errorf("IncrementVisits %s: %v", c, err)
						return
					} else if u == nil {
						t.Errorf("IncrementVisits %s: nil", c)
						return
					}
				case 2:
					_, err := s.UpdateFields(c, func(u *model.ShortURL) {
						u.SetRemark(fmt.Sprintf("r-%d", i))
					})
					if err != nil && !errors.Is(err, model.ErrCodeNotFound) {
						t.Errorf("UpdateFields %s: %v", c, err)
						return
					}
				case 3:
					s.BumpHits(1)
					s.BumpMisses(1)
					s.BumpUpdates(1)
				}
			}
		}(g)
	}

	// Concurrent stat reader.
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
				h, m, u := s.InternalStats()
				if h < 0 || m < 0 || u < 0 {
					t.Errorf("InternalStats negative: hits=%d misses=%d updates=%d", h, m, u)
					return
				}
			}
		}
	}()

	wg.Wait()
	close(stop)
	rWG.Wait()

	// Each IncrementVisits bumps visits exactly once; verify the aggregate.
	var total int64
	for _, c := range codes {
		u, err := s.Get(c.Code)
		if err != nil {
			t.Fatalf("Get %s: %v", c.Code, err)
		}
		snap := u.Snapshot()
		total += snap.Visits
	}
	incCount := int64(goroutines) * int64(iters) / 4
	if total != incCount {
		t.Fatalf("visits total=%d want=%d (IncrementVisits lost updates)", total, incCount)
	}
	h, m, u := s.InternalStats()
	if h < 0 || m < 0 || u < 0 {
		t.Fatalf("final InternalStats negative: hits=%d misses=%d updates=%d", h, m, u)
	}
	if got := s.Count(); got != len(codes) {
		t.Fatalf("Count=%d want=%d after concurrent run", got, len(codes))
	}
}

// TestIncrementVisitsCrossesBoundary reproduces the original double-unlock
// panic: the old IncrementVisits called a bare s.mu.Unlock() every time Visits
// hit a multiple of 100, then the deferred Unlock panicked with
// "sync: Unlock of unlocked RWMutex". Here we drive visits well past several
// multiples of 100 concurrently and assert no panic and an exact count.
func TestIncrementVisitsCrossesBoundary(t *testing.T) {
	s := newTestStore(t)
	const code = "bdy"
	if err := s.Save(&model.ShortURL{Code: code, RawURL: "https://x", CreatedAt: time.Now()}, false); err != nil {
		t.Fatalf("Save: %v", err)
	}

	const goroutines = 50
	const perG = 30 // 50*30 = 1500 >> several 100-boundaries, concurrent
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				if _, err := s.IncrementVisits(code); err != nil {
					t.Errorf("IncrementVisits: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	u, err := s.Get(code)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := u.Snapshot().Visits; got != int64(goroutines*perG) {
		t.Fatalf("visits=%d want=%d (IncrementVisits lost updates)", got, goroutines*perG)
	}
}
