// Package resolver 负责把「短码 -> 原始 URL + 元信息」的解析统一封装。
//
// 它会串联多个存储层与缓存层：
//   1. 先用布隆过滤器做「绝对不存在」快速拦截。
//   2. 查内存 LRU 缓存，命中则直接返回（带命中计数）。
//   3. 否则回源到 store，并回填缓存。
package resolver

import (
	"context"
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
	HitMiss         HitType = iota
	HitBloomNegative
	HitCache
	HitStore
	HitNotFound
	HitRefreshed
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
	case HitRefreshed:
		return "refreshed"
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
	ctx   context.Context
	cf    context.CancelFunc

	hitsCache     atomic.Int64
	hitsStore     atomic.Int64
	hitsBloomMiss atomic.Int64
	notFounds     atomic.Int64
	hitsRefreshed atomic.Int64

	mu      sync.Mutex
	warmed  bool
	warmErr error
	ttl     time.Duration
}

type Config struct {
	Store      *store.URLStore
	CacheCap   int
	CacheTTL   time.Duration
	BloomN     int
	BloomP     float64
	WarmOnBoot bool
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
	ctx, cf := context.WithCancel(context.Background())
	r := &Resolver{
		store: cfg.Store,
		cache: lc,
		bloom: bf,
		ctx:   ctx,
		cf:    cf,
		ttl:   cfg.CacheTTL,
	}
	if cfg.WarmOnBoot {
		r.warmOnce()
	}
	return r, nil
}

func (r *Resolver) Close() error {
	if r.cf != nil {
		r.cf()
	}
	return nil
}

func (r *Resolver) CacheTTL() time.Duration { return r.ttl }

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
	cacheCap := r.cache.Stats().Cap
	logger.Info("resolver: warming bloom", logger.Fields{"entries": len(all), "cache_cap": cacheCap})
	pairs := make([]cache.LiveEntryExport, 0, cacheCap)
	for i, u := range all {
		if u == nil {
			continue
		}
		r.bloom.AddString(u.Code)
		if i < cacheCap {
			pairs = append(pairs, cache.LiveEntryExport{Key: u.Code, Value: u})
			r.cache.Set(u.Code, u, 10*time.Minute)
		}
	}
	_ = pairs
	r.warmed = true
}

func (r *Resolver) Resolve(code string) Result {
	start := time.Now()
	if code == "" {
		return Result{Hit: HitNotFound, Elapsed: time.Since(start)}
	}
	if !r.bloom.MayContainString(code) {
		r.hitsBloomMiss.Add(1)
		r.cache.HitMiss()
		r.store.BumpMisses(1)
		return Result{Hit: HitBloomNegative, Elapsed: time.Since(start)}
	}
	if v, ok := r.cache.Get(code); ok {
		if su, ok := v.(*model.ShortURL); ok && su != nil {
			r.hitsCache.Add(1)
			r.cache.HitCache()
			r.store.BumpHits(1)
			return Result{URL: su, Hit: HitCache, Elapsed: time.Since(start)}
		}
	}
	v, err, shared := r.sf.Do(code, func() (any, error) {
		su, gErr := r.store.Get(code)
		if gErr != nil {
			return storeMissToken{}, nil
		}
		r.cache.Set(code, su, 10*time.Minute)
		return su, nil
	})
	_ = shared
	if err != nil {
		logger.Warn("resolver: unexpected singleflight error", logger.Fields{"err": err.Error()})
		return Result{Hit: HitNotFound, Elapsed: time.Since(start)}
	}
	if _, miss := v.(storeMissToken); miss {
		r.notFounds.Add(1)
		r.cache.HitMiss()
		r.store.BumpMisses(1)
		return Result{Hit: HitNotFound, Elapsed: time.Since(start)}
	}
	su, _ := v.(*model.ShortURL)
	r.hitsStore.Add(1)
	r.store.BumpHits(1)
	return Result{URL: su, Hit: HitStore, Fallback: true, Elapsed: time.Since(start)}
}

func (r *Resolver) ResolveAndBump(code string) Result {
	start := time.Now()
	res := r.Resolve(code)
	if res.URL != nil {
		res.URL.IncVisits(1)
		res.Hit = HitRefreshed
		r.hitsRefreshed.Add(1)
		r.cache.Put(code, res.URL, r.ttl)
	}
	res.Elapsed = time.Since(start)
	return res
}

func (r *Resolver) RefreshOnAccess(code string, remark string) (*model.ShortURL, error) {
	if code == "" {
		return nil, errors.New("resolver: empty code")
	}
	v, ok := r.cache.Peek(code)
	if ok {
		if su, conv := v.(*model.ShortURL); conv && su != nil {
			su.SetRemark(remark)
			su.IncVisits(1)
			r.cache.Touch(code, r.ttl)
			r.cache.HitCache()
			r.store.BumpHits(1)
			return su, nil
		}
	}
	su, err := r.store.Get(code)
	if err != nil {
		r.cache.HitMiss()
		r.store.BumpMisses(1)
		return nil, err
	}
	su.SetRemark(remark)
	su.IncVisits(1)
	r.cache.Set(code, su, r.ttl)
	r.store.BumpHits(1)
	return su, nil
}

func (r *Resolver) BatchResolve(codes []string) map[string]Result {
	out := make(map[string]Result, len(codes))
	wg := sync.WaitGroup{}
	mu := sync.Mutex{}
	for _, c := range codes {
		wg.Add(1)
		go func(code string) {
			defer wg.Done()
			res := r.ResolveAndBump(code)
			mu.Lock()
			out[code] = res
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	return out
}

type PrefetchHint struct {
	PopulateCache bool
	Rebloom       bool
}

func (r *Resolver) Prefetch(codes []string, hint PrefetchHint) (hit int, miss int) {
	hit, miss = 0, 0
	for _, code := range codes {
		if code == "" {
			miss++
			continue
		}
		if v, ok := r.cache.Get(code); ok {
			if su, ok := v.(*model.ShortURL); ok && su != nil {
				su.IncVisits(1)
				hit++
				continue
			}
		}
		if hint.Rebloom {
			r.bloom.AddString(code)
		}
		su, err := r.store.Get(code)
		if err != nil {
			miss++
			r.cache.HitMiss()
			r.store.BumpMisses(1)
			continue
		}
		if hint.PopulateCache {
			su.IncVisits(1)
			r.cache.Set(code, su, r.ttl)
		}
		hit++
		r.cache.HitCache()
		r.store.BumpHits(1)
	}
	return hit, miss
}

func (r *Resolver) ObserveCreated(u *model.ShortURL) {
	if u == nil || u.Code == "" {
		return
	}
	r.bloom.AddString(u.Code)
	r.cache.Set(u.Code, u, 10*time.Minute)
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
	HitsRefreshed int64
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
		HitsRefreshed: r.hitsRefreshed.Load(),
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

type storeMissToken struct{}
