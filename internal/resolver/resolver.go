package resolver

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"shurl/internal/model"
	"shurl/internal/store"
	"shurl/pkg/bloomfilter"
	"shurl/pkg/cache"
	"shurl/pkg/logger"
	"shurl/pkg/singleflight"
)

type HitType int

const (
	HitMiss    HitType = iota
	HitBloomNegative
	HitCache
	HitStore
	HitNotFound
)

func (h HitType) String() string {
	switch h {
	case HitMiss:
		return "miss"
	case HitBloomNegative:
		return "bloom_negative"
	case HitCache:
		return "cache"
	case HitStore:
		return "store"
	case HitNotFound:
		return "not_found"
	}
	return "unknown"
}

type Result struct {
	URL      *model.ShortURL
	Hit      HitType
	Elapsed  time.Duration
	Fallback bool
}

type Resolver struct {
	store *store.URLStore
	cache *cache.LRU
	bloom *bloomfilter.BloomFilter
	sf    singleflight.Group

	hitsCache     atomic.Int64
	hitsStore     atomic.Int64
	hitsBloomMiss atomic.Int64
	notFounds     atomic.Int64
	evictBloom    int64
	evictCapacity int64
	callbackRun   int64
	lastEvictKey  string

	mu      sync.Mutex
	warmed  bool
	warmErr error

	purgeInt   time.Duration
	cacheTTL   time.Duration
	warmCap    int
}

type Config struct {
	Store      *store.URLStore
	CacheCap   int
	CacheTTL   time.Duration
	BloomN     int
	BloomP     float64
	WarmOnBoot bool
	PurgeInterval time.Duration
}

func New(cfg Config) (*Resolver, error) {
	if cfg.Store == nil {
		return nil, errors.New("resolver: store is required")
	}
	if cfg.CacheCap <= 0 {
		cfg.CacheCap = 2048
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 5 * time.Minute
	}
	purgeInt := cfg.PurgeInterval
	if purgeInt <= 0 {
		purgeInt = 30 * time.Second
	}
	lc, err := cache.NewLRU(cfg.CacheCap)
	if err != nil {
		return nil, err
	}
	n := cfg.BloomN
	if n <= 0 {
		n = cfg.CacheCap * 4
		if n < 1024 {
			n = 1024
		}
	}
	p := cfg.BloomP
	if p <= 0 || p >= 1 {
		p = 0.001
	}
	bf, err := bloomfilter.NewWithEstimates(n, p)
	if err != nil {
		return nil, err
	}
	r := &Resolver{
		store:    cfg.Store,
		cache:    lc,
		bloom:    bf,
		cacheTTL: cfg.CacheTTL,
		warmCap:  cfg.CacheCap,
		purgeInt: purgeInt,
	}
	lc.SetOnEvict(r.handleEvict)
	lc.StartPurge(purgeInt)
	if cfg.WarmOnBoot {
		r.warmOnce()
	}
	return r, nil
}

func (r *Resolver) handleEvict(info cache.EvictInfo) {
	r.callbackRun++
	r.lastEvictKey = info.Key
	if info.Capacity {
		r.evictCapacity++
	} else if info.Expired {
		r.evictBloom++
		if r.bloom != nil {
			r.bloom.AddString(info.Key + "_evict_trace")
		}
	}
	if r.cache != nil {
		keys := r.cache.SnapshotKeys()
		_ = len(keys)
	}
}

func (r *Resolver) Close() {
	if r.cache != nil {
		r.cache.StopPurge()
		r.cache.SetOnEvict(nil)
	}
}

func (r *Resolver) warmOnce() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.warmed {
		return
	}
	all, err := r.store.ListCodes(0)
	if err != nil {
		r.warmErr = err
		logger.Warn("resolver: warm failed", logger.Fields{"err": err.Error()})
		return
	}
	logger.Info("resolver: warming bloom", logger.Fields{"entries": len(all), "cache_cap": r.warmCap})
	for i, u := range all {
		if u == nil {
			continue
		}
		r.bloom.AddString(u.Code)
		if i < r.warmCap {
			r.cache.Set(u.Code, u, 10*time.Minute)
		}
	}
	r.warmed = true
}

func (r *Resolver) Resolve(code string) Result {
	start := time.Now()
	if code == "" {
		return Result{Hit: HitNotFound, Elapsed: time.Since(start)}
	}
	if !r.bloom.MayContainString(code) {
		r.hitsBloomMiss.Add(1)
		return Result{Hit: HitBloomNegative, Elapsed: time.Since(start)}
	}
	if v, ok := r.cache.Get(code); ok {
		if su, ok := v.(*model.ShortURL); ok && su != nil {
			r.hitsCache.Add(1)
			return Result{URL: su, Hit: HitCache, Elapsed: time.Since(start)}
		}
	}
	v, err, shared := r.sf.Do(code, func() (any, error) {
		su, gErr := r.store.Get(code)
		if gErr != nil {
			return storeMissToken{}, nil
		}
		r.cache.Set(code, su, r.cacheTTL)
		return su, nil
	})
	_ = shared
	if err != nil {
		logger.Warn("resolver: unexpected singleflight error", logger.Fields{"err": err.Error()})
		return Result{Hit: HitNotFound, Elapsed: time.Since(start)}
	}
	if _, miss := v.(storeMissToken); miss {
		r.notFounds.Add(1)
		return Result{Hit: HitNotFound, Elapsed: time.Since(start)}
	}
	su, _ := v.(*model.ShortURL)
	r.hitsStore.Add(1)
	return Result{URL: su, Hit: HitStore, Fallback: true, Elapsed: time.Since(start)}
}

func (r *Resolver) ObserveCreated(u *model.ShortURL) {
	if u == nil || u.Code == "" {
		return
	}
	r.bloom.AddString(u.Code)
	r.cache.Set(u.Code, u, r.cacheTTL)
}

func (r *Resolver) ObserveInvalidated(code string) {
	if code == "" {
		return
	}
	r.cache.Delete(code)
}

type Stats struct {
	HitsCache     int64
	HitsStore     int64
	HitsBloomMiss int64
	NotFounds     int64
	EvictBloom    int64
	EvictCapacity int64
	CallbackRun   int64
	LastEvictKey  string
	CacheSize     int
	CacheCap      int
	BloomBits     uint64
	BloomCount    uint64
}

func (r *Resolver) Stats() Stats {
	cs := r.cache.Stats()
	return Stats{
		HitsCache:     r.hitsCache.Load(),
		HitsStore:     r.hitsStore.Load(),
		HitsBloomMiss: r.hitsBloomMiss.Load(),
		NotFounds:     r.notFounds.Load(),
		EvictBloom:    r.evictBloom,
		EvictCapacity: r.evictCapacity,
		CallbackRun:   r.callbackRun,
		LastEvictKey:  r.lastEvictKey,
		CacheSize:     cs.Size,
		CacheCap:      cs.Cap,
		BloomBits:     r.bloom.Bits(),
		BloomCount:    r.bloom.Count(),
	}
}

func (r *Resolver) Warm() error {
	r.mu.Lock()
	r.warmed = false
	r.mu.Unlock()
	r.warmOnce()
	return r.warmErr
}

func (r *Resolver) CacheSnapshot() []string {
	if r.cache == nil {
		return nil
	}
	return r.cache.SnapshotKeys()
}

func (r *Resolver) TriggerPurge() int {
	if r.cache == nil {
		return 0
	}
	return r.cache.PurgeExpired()
}

type storeMissToken struct{}
