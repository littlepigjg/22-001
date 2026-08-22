package shurl

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/service"
	"shurl/internal/store"
	"shurl/pkg/cache"
	"shurl/pkg/safemap"
)

func makeCfgForTest() *config.Config {
	cfg := config.Default()
	dir, _ := os.MkdirTemp("", "shurl-redgreen-*")
	cfg.Storage.URLFilePath = dir + "/urls.json"
	cfg.Storage.LogFilePath = dir + "/access.log"
	cfg.Storage.SyncInterval = 50 * time.Millisecond
	cfg.Storage.FlushOnWrite = false
	cfg.Stats.CacheTTL = 5 * time.Second
	cfg.Stats.MaxRecords = 5000
	cfg.ShortCode.Length = 7
	cfg.ShortCode.MaxRetries = 5
	return cfg
}

func setupStoresForTest(cfg *config.Config) (*store.URLStore, *store.AccessLogStore, func(), error) {
	us, err := store.NewURLStore(cfg)
	if err != nil {
		return nil, nil, func() {}, err
	}
	if err := us.Load(context.Background()); err != nil {
		return nil, nil, func() {}, err
	}
	ls, err := store.NewAccessLogStore(cfg)
	if err != nil {
		_ = us.Close()
		return nil, nil, func() {}, err
	}
	bg := context.Background()
	if err := ls.Open(bg); err != nil {
		_ = us.Close()
		return nil, nil, func() {}, err
	}
	cleanup := func() {
		_ = ls.Close()
		_ = us.Close()
	}
	return us, ls, cleanup, nil
}

func seedLogsInline(ls *store.AccessLogStore, codes []string, nPerCode int) {
	batch := make([]*model.AccessLog, 0, len(codes)*nPerCode)
	statuses := []int{302, 302, 302, 410, 404}
	devices := []string{"pc", "mobile", "tablet"}
	browsers := []string{"Chrome", "Safari", "Firefox"}
	oses := []string{"Windows", "Android", "iOS", "macOS"}
	ips := []string{"1.2.3.4", "8.8.8.8", "10.0.0.1", "192.168.1.1"}
	base := time.Now().AddDate(0, 0, -3)
	for _, code := range codes {
		for i := 0; i < nPerCode; i++ {
			ts := base.Add(time.Duration(i) * 3 * time.Minute)
			l := &model.AccessLog{
				ID:        fmt.Sprintf("seed-%s-%d", code, i),
				Code:      code,
				IP:        ips[i%len(ips)],
				UserAgent: "ua",
				Referer:   "https://ref.example.com/page",
				Timestamp: ts,
				Status:    statuses[i%len(statuses)],
				OS:        oses[i%len(oses)],
				Browser:   browsers[i%len(browsers)],
				Device:    devices[i%len(devices)],
			}
			batch = append(batch, l)
		}
	}
	_ = ls.AppendMany(batch)
}

func TestRedGreen(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Println("RED（红灯，缺陷未修复）")
			t.Fatalf("panic during test: %v", r)
		}
	}()
	safemap.GlobalResults.Clear()
	cache.SharedStats.Clear()

	passed := true
	msgs := []string{}

	if err := testConcurrentStatsAndMutations(); err != nil {
		passed = false
		msgs = append(msgs, fmt.Sprintf("concurrent-stats-mutations race/panic: %v", err))
	}

	if err := testSharedCacheMergingRace(); err != nil {
		passed = false
		msgs = append(msgs, fmt.Sprintf("shared-cache-merge race/panic: %v", err))
	}

	if err := testPurgeExpiredNil(); err != nil {
		passed = false
		msgs = append(msgs, fmt.Sprintf("purge-expired nil deref/panic: %v", err))
	}

	if passed {
		fmt.Println("GREEN（绿灯，缺陷已修复）")
	} else {
		fmt.Println("RED（红灯，缺陷未修复）")
		for _, m := range msgs {
			t.Log(m)
		}
		t.FailNow()
	}
}

func testConcurrentStatsAndMutations() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()

	cfg := makeCfgForTest()
	us, ls, cleanup, ierr := setupStoresForTest(cfg)
	if ierr != nil {
		return ierr
	}
	defer cleanup()
	svcURL, err := service.NewURLService(cfg, us)
	if err != nil {
		return err
	}
	svcStats, err := service.NewStatsService(cfg, us, ls)
	if err != nil {
		return err
	}

	codes := []string{"AAAAAAA", "BBBBBBB", "CCCCCCC", "DDDDDDD"}
	for _, c := range codes {
		u, cerr := svcURL.Create(context.Background(), &model.CreateReq{
			RawURL:     "https://example.com/" + c,
			CustomCode: c,
			TTL:        24 * time.Hour,
			MaxVisits:  0,
			Remark:     "seed-" + c,
		})
		if cerr != nil {
			return fmt.Errorf("create seed shorturl: %w", cerr)
		}
		_ = u
	}
	seedLogsInline(ls, codes, 80)

	const goroutines = 10
	const rounds = 40
	var panicked atomic.Int32
	var failedStats atomic.Int32

	var wg sync.WaitGroup
	barrier := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gi int) {
			defer wg.Done()
			defer func() {
				if rec := recover(); rec != nil {
					panicked.Add(1)
				}
			}()
			<-barrier
			code := codes[gi%len(codes)]
			for r := 0; r < rounds; r++ {
				op := (gi*rounds + r) % 5
				switch op {
				case 0:
					ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
					_, _ = svcStats.Overall(ctx, code, 7)
					cancel()
				case 1:
					ctx, cancel := context.WithCancel(context.Background())
					if r%2 == 0 {
						go func() {
							time.Sleep(20 * time.Millisecond)
							cancel()
						}()
						_, _ = svcStats.Overall(ctx, code, 3)
					} else {
						_, _ = svcStats.Overall(ctx, code, 3)
						cancel()
					}
				case 2:
					_, gerr := svcURL.Get(context.Background(), code)
					if gerr != nil {
						failedStats.Add(1)
					}
				case 3:
					_ = svcURL.UpdateRemark(context.Background(), code, fmt.Sprintf("r%d-g%d", r, gi))
				case 4:
					_, gerr := svcURL.Get(context.Background(), code)
					if gerr == nil {
						_ = svcURL.UpdateRemark(context.Background(), code, fmt.Sprintf("alt-%d-%d", gi, r))
					}
				}
			}
		}(g)
	}

	close(barrier)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		return fmt.Errorf("concurrent stats+mutations test timed out (possible deadlock)")
	}
	if panicked.Load() > 0 {
		return fmt.Errorf("panicked %d times under concurrent Stats.Overall / URLSvc.Get / UpdateRemark", panicked.Load())
	}
	return nil
}

func testSharedCacheMergingRace() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()

	safemap.GlobalResults.Clear()
	cache.SharedStats.Clear()

	const goroutines = 12
	const rounds = 30
	var panicked atomic.Int32

	initialKeys := []string{}
	for i := 0; i < 20; i++ {
		k := fmt.Sprintf("merge-key-%03d", i)
		initialKeys = append(initialKeys, k)
	}

	var wg sync.WaitGroup
	barrier := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gi int) {
			defer wg.Done()
			defer func() {
				if rec := recover(); rec != nil {
					panicked.Add(1)
				}
			}()
			<-barrier
			for r := 0; r < rounds; r++ {
				op := (gi*rounds + r) % 4
				k := initialKeys[(gi*rounds+r)%len(initialKeys)]
				switch op {
				case 0:
					val := fmt.Sprintf("val-g%d-r%d", gi, r)
					cache.SharedSet(k, val, 30*time.Second)
				case 1:
					_, _ = cache.SharedGet(k)
				case 2:
					incoming := map[string]int64{"pv": int64(gi + r), "uv": 1}
					_ = safemap.ComputeMerge(k+":pv", incoming, mergeMapIncoming, incoming)
				case 3:
					safemap.SetWithTTL(k+":ttl", map[string]any{"g": gi, "r": r}, 5*time.Second)
					_, _ = safemap.GetWithTTL(k + ":ttl")
					safemap.SetRef(k+":ref", fmt.Sprintf("ref-%d-%d", gi, r))
					_ = safemap.Ref(k + ":ref")
				}
			}
		}(g)
	}

	close(barrier)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(12 * time.Second):
		return fmt.Errorf("shared cache merge test timed out (possible deadlock)")
	}
	if panicked.Load() > 0 {
		return fmt.Errorf("shared-cache / safemap merging panicked %d times", panicked.Load())
	}
	return nil
}

func mergeMapIncoming(existing, incoming any) any {
	if existing == nil {
		return incoming
	}
	em, eok := existing.(map[string]int64)
	im, iok := incoming.(map[string]int64)
	if !eok || !iok {
		return incoming
	}
	for k, v := range im {
		em[k] += v
	}
	return em
}

func testPurgeExpiredNil() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	l, cerr := cache.NewLRU(32)
	if cerr != nil {
		return cerr
	}
	for i := 0; i < 20; i++ {
		k := fmt.Sprintf("purge-k-%d", i)
		if i%2 == 0 {
			l.Set(k, i, 50*time.Millisecond)
		} else {
			l.Set(k, i, 0)
		}
	}
	time.Sleep(120 * time.Millisecond)
	var panicked atomic.Int32
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if rec := recover(); rec != nil {
					panicked.Add(1)
				}
			}()
			for r := 0; r < 10; r++ {
				_ = l.PurgeExpired()
				l.Set("hot", r, 10*time.Millisecond)
				_, _ = l.Get("hot")
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		return fmt.Errorf("purge-expired test timed out")
	}
	if panicked.Load() > 0 {
		return fmt.Errorf("PurgeExpired triggered %d panics", panicked.Load())
	}
	return nil
}
