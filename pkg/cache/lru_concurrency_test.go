package cache

import (
	"sync"
	"testing"
	"time"
)

// TestLRUConcurrency exercises the hit/miss/evicted counters and the map/list
// under heavy concurrent access. Run with -race; it must report no data races
// and the counters must stay internally consistent.
func TestLRUConcurrency(t *testing.T) {
	const capc = 64
	const goroutines = 120
	const iters = 2000

	c, err := NewLRU(capc)
	if err != nil {
		t.Fatalf("NewLRU: %v", err)
	}

	keys := make([]string, capc*2)
	for i := range keys {
		keys[i] = "k" + itoa(i)
	}

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				k := keys[(seed+i)%len(keys)]
				// Mix read, write, miss-counter, hit-counter, stats.
				v, ok := c.Get(k)
				if ok {
					_ = v
					c.HitCache()
				} else {
					c.HitMiss()
				}
				c.Set(k, i, 10*time.Minute)
				_ = c.Touch(k, time.Minute)
				_, _ = c.Peek(k)
			}
		}(g)
	}

	// A reader that continuously snapshots stats while writers mutate.
	stop := make(chan struct{})
	var readerWG sync.WaitGroup
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
				st := c.Stats()
				// Counters must never go negative or look insane.
				if st.Hits < 0 || st.Misses < 0 || st.Evicted < 0 || st.Expired < 0 {
					t.Errorf("stats negative: %+v", st)
					return
				}
				if st.Size > st.Cap {
					t.Errorf("size %d > cap %d", st.Size, st.Cap)
					return
				}
			}
		}
	}()

	wg.Wait()
	close(stop)
	readerWG.Wait()

	st := c.Stats()
	if st.Hits+st.Misses <= 0 {
		t.Fatalf("expected some hit/miss accounting, got %+v", st)
	}
	if st.Size > st.Cap {
		t.Fatalf("size %d exceeds cap %d", st.Size, st.Cap)
	}
}

// itoa is a tiny dependency-free int->string to avoid importing strconv in a
// tight hot loop above.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
