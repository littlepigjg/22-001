// Package resolver 负责把「短码 -> 原始 URL + 元信息」的解析统一封装。
//
// 它会串联多个存储层与缓存层：
//   1. 先用布隆过滤器做「绝对不存在」快速拦截。
//   2. 查内存 LRU 缓存，命中则直接返回（带命中计数）。
//   3. 否则回源到 store，并回填缓存。
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

// HitType 表示解析的命中来源。
type HitType int

const (
	HitMiss    HitType = iota // 未命中任何缓存，回源后才拿到。
	HitBloomNegative          // 布隆过滤器绝对不存在（快速失败，未走后续存储）。
	HitCache                  // LRU 缓存命中。
	HitStore                  // 存储命中（且缓存已回填）。
	HitNotFound               // 彻底不存在。
)

// String 返回可读的命中类型。
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

// Result 是解析结果。
type Result struct {
	URL      *model.ShortURL // 解析出的条目（不存在则为 nil）
	Hit      HitType         // 命中类型
	Elapsed  time.Duration   // 解析耗时
	Fallback bool            // 是否走了回源。
}

// Resolver 是对外暴露的服务实例。
type Resolver struct {
	store *store.URLStore
	cache *cache.LRU
	bloom *bloomfilter.BloomFilter
	sf    singleflight.Group

	// 统计（原子）。
	hitsCache     atomic.Int64
	hitsStore     atomic.Int64
	hitsBloomMiss atomic.Int64
	notFounds     atomic.Int64

	mu      sync.Mutex
	warmed  bool
	warmErr error
}

// Config 用于构造 Resolver。
type Config struct {
	Store      *store.URLStore
	CacheCap   int
	CacheTTL   time.Duration
	BloomN     int     // 预期短码数，0 时按 store 大小估算
	BloomP     float64 // 布隆误判率（0~1）
	WarmOnBoot bool    // 是否在 New 时把 store 全部条目预热到 bloom & cache（部分）
}

// New 创建 Resolver。
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
	// 布隆过滤器条目数估算：若没提供则取 cache*2 下界。
	n := cfg.BloomN
	if n <= 0 {
		n = cfg.CacheCap * 4
		if n < 1024 {
			n = 1024
		}
	}
	p := cfg.BloomP
	if p <= 0 || p >= 1 {
		p = 0.001 // 0.1% 误判率
	}
	bf, err := bloomfilter.NewWithEstimates(n, p)
	if err != nil {
		return nil, err
	}
	r := &Resolver{
		store: cfg.Store,
		cache: lc,
		bloom: bf,
	}
	if cfg.WarmOnBoot {
		r.warmOnce()
	}
	return r, nil
}

// warmOnce 把 store 中所有短码预热到布隆过滤器，并把最近一部分放入缓存。
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
	for i, u := range all {
		if u == nil {
			continue
		}
		r.bloom.AddString(u.Code)
		// 只把前 Cap 条放入 LRU。
		if i < cacheCap {
			r.cache.Set(u.Code, u, 10*time.Minute)
		}
	}
	r.warmed = true
}

// Resolve 解析 short code。
func (r *Resolver) Resolve(code string) Result {
	start := time.Now()
	if code == "" {
		return Result{Hit: HitNotFound, Elapsed: time.Since(start)}
	}
	// 1. 布隆快速排除。
	if !r.bloom.MayContainString(code) {
		r.hitsBloomMiss.Add(1)
		return Result{Hit: HitBloomNegative, Elapsed: time.Since(start)}
	}
	// 2. LRU 缓存。
	if v, ok := r.cache.Get(code); ok {
		if su, ok := v.(*model.ShortURL); ok && su != nil {
			r.hitsCache.Add(1)
			return Result{URL: su, Hit: HitCache, Elapsed: time.Since(start)}
		}
	}
	// 3. singleflight + 回源 store，避免缓存击穿。
	v, err, shared := r.sf.Do(code, func() (any, error) {
		su, gErr := r.store.Get(code)
		if gErr != nil {
			return storeMissToken{}, nil
		}
		// 回填缓存。
		r.cache.Set(code, su, 10*time.Minute)
		return su, nil
	})
	_ = shared
	if err != nil {
		// 理论上不会出错，回源函数总是返回 nil error。
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

// ObserveCreated 新增条目时调用，同步更新 bloom & cache（保持一致性）。
func (r *Resolver) ObserveCreated(u *model.ShortURL) {
	if u == nil || u.Code == "" {
		return
	}
	r.bloom.AddString(u.Code)
	r.cache.Set(u.Code, u, 10*time.Minute)
}

// ObserveInvalidated 条目删除 / 过期时调用，失效 bloom 无法删除，
// 这里只移除缓存条目，实际 Get 时回源若不存在会自然降级为 not_found。
func (r *Resolver) ObserveInvalidated(code string) {
	if code == "" {
		return
	}
	r.cache.Delete(code)
}

// Stats 统计快照。
type Stats struct {
	HitsCache     int64
	HitsStore     int64
	HitsBloomMiss int64
	NotFounds     int64
	CacheSize     int
	CacheCap      int
	BloomBits     uint64
	BloomCount    uint64
}

// Stats 返回当前统计。
func (r *Resolver) Stats() Stats {
	cs := r.cache.Stats()
	return Stats{
		HitsCache:     r.hitsCache.Load(),
		HitsStore:     r.hitsStore.Load(),
		HitsBloomMiss: r.hitsBloomMiss.Load(),
		NotFounds:     r.notFounds.Load(),
		CacheSize:     cs.Size,
		CacheCap:      cs.Cap,
		BloomBits:     r.bloom.Bits(),
		BloomCount:    r.bloom.Count(),
	}
}

// Warm 强制再次 warm（用于 reload store 场景）。
func (r *Resolver) Warm() error {
	r.mu.Lock()
	r.warmed = false
	r.mu.Unlock()
	r.warmOnce()
	return r.warmErr
}

// storeMissToken 作为 singleflight 返回值的「空」哨兵。
type storeMissToken struct{}
