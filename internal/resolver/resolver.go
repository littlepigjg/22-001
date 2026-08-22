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
	// 以下计数在驱逐回调（可能来自 PurgeExpired / evictTail / Get 过期分支，
	// 多个 goroutine 并发触发）里被 ++ 修改，又在 Stats() 里被读取。
	// 早期用裸 int64，非原子自增会丢更新——这正是「驱逐计数明显偏小」的根因。改为原子。
	evictBloom    atomic.Int64
	evictCapacity atomic.Int64
	callbackRun   atomic.Int64
	// lastEvictKey 是 string，本工具链没有 atomic.String，用一把专用锁保护。
	// 注意锁序：handleEvict 在 cache.mu 持锁中获取 evMu；Stats 在 *未* 持 cache.mu
	// 时获取 evMu。绝不出现 evMu→cache.mu 的反向嵌套，故无死锁。warmOnce 是
	// r.mu→cache.mu，与 evMu 无交集。
	lastEvictKey string
	evMu         sync.Mutex

	mu      sync.Mutex
	warmed  bool
	warmErr error

	purgeInt time.Duration
	cacheTTL time.Duration
	warmCap  int
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

// handleEvict 是注册给 LRU 的驱逐回调，由 LRU 在持有 cache.mu 时同步调用。
//
// 关键约束：**禁止**在此回调里再访问同一个 LRU（会自死锁，因为缓存回调持锁中），
// 也禁止任何会阻塞的 IO。这里只做：
//   - 原子累加统计计数；
//   - 过期类驱逐时把 key 追加进布隆过滤器用于事后追溯（不读取缓存状态）。
//
// 早期实现里那句 r.cache.SnapshotKeys() 必须删除——它会在持锁回调里再次争用
// 同一把锁，既会死锁也属于无意义开销。
func (r *Resolver) handleEvict(info cache.EvictInfo) {
	r.callbackRun.Add(1)
	r.evMu.Lock()
	r.lastEvictKey = info.Key
	r.evMu.Unlock()
	if info.Capacity {
		r.evictCapacity.Add(1)
	} else if info.Expired {
		r.evictBloom.Add(1)
		if r.bloom != nil {
			r.bloom.AddString(info.Key + "_evict_trace")
		}
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
	r.evMu.Lock()
	lastKey := r.lastEvictKey
	r.evMu.Unlock()
	return Stats{
		HitsCache:     r.hitsCache.Load(),
		HitsStore:     r.hitsStore.Load(),
		HitsBloomMiss: r.hitsBloomMiss.Load(),
		NotFounds:     r.notFounds.Load(),
		EvictBloom:    r.evictBloom.Load(),
		EvictCapacity: r.evictCapacity.Load(),
		CallbackRun:   r.callbackRun.Load(),
		LastEvictKey:  lastKey,
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
