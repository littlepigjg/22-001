package model

import "time"

// MetricsOverview 是对外导出的精简指标概览（供首页 / 统计面板调用方使用）。
type MetricsOverview struct {
	GeneratedAt time.Time `json:"generated_at"`

	// 接口级指标（键：method:path）。
	EndpointHits  map[string]int64   `json:"endpoint_hits,omitempty"`
	EndpointErrs  map[string]int64   `json:"endpoint_errs,omitempty"`
	EndpointLatMs map[string]float64 `json:"endpoint_latency_ms_avg,omitempty"`

	// 全局指标。
	TotalRequests int64   `json:"total_requests"`
	TotalErrors   int64   `json:"total_errors"`
	AvgLatencyMs  float64 `json:"avg_latency_ms"`
	P95LatencyMs  int64   `json:"p95_latency_ms"`

	// Resolver 相关。
	ResolverHitsCache     int64   `json:"resolver_hits_cache"`
	ResolverHitsStore     int64   `json:"resolver_hits_store"`
	ResolverBloomNegative int64   `json:"resolver_bloom_negative"`
	ResolverHitRate       float64 `json:"resolver_hit_rate"`

	// 业务核心。
	ShortCodesCount int64 `json:"short_codes_count"`
	AccessLogsCount int64 `json:"access_logs_count"`

	// 扩展字段（可选）。
	TopShortCodes []TopShortCodeRow `json:"top_short_codes,omitempty"`
}

// TopShortCodeRow 表示按访问量排序的短码排行条目。
type TopShortCodeRow struct {
	Code      string    `json:"code"`
	Original  string    `json:"original_url,omitempty"`
	Views     int64     `json:"views"`
	LastSeen  time.Time `json:"last_seen"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// RateLimitStatus 是限流器状态的对外模型。
type RateLimitStatus struct {
	GlobalTokens int64 `json:"global_tokens"`
	GlobalCap    int64 `json:"global_cap"`
	PerIPEntries int   `json:"per_ip_entries"`
	PerIPMax     int   `json:"per_ip_max"`
}

// ResolverStatsEx 是 Resolver 统计的对外结构（带缓存命中率）。
type ResolverStatsEx struct {
	HitsCache        int64   `json:"hits_cache"`
	HitsStore        int64   `json:"hits_store"`
	BloomNegatives   int64   `json:"bloom_negatives"`
	NotFounds        int64   `json:"not_founds"`
	TotalLookups     int64   `json:"total_lookups"`
	CacheHitRate     float64 `json:"cache_hit_rate"`
	EffectiveHitRate float64 `json:"effective_hit_rate"` // cache+store 除以 total（>=1）
	CacheSize        int     `json:"cache_size"`
	CacheCap         int     `json:"cache_cap"`
	BloomBits        uint64  `json:"bloom_bits"`
	BloomEntries     uint64  `json:"bloom_entries"`
}

// CalcRates 填充命中率字段。
func (r *ResolverStatsEx) CalcRates() *ResolverStatsEx {
	total := r.HitsCache + r.HitsStore + r.BloomNegatives + r.NotFounds
	r.TotalLookups = total
	if total > 0 {
		r.CacheHitRate = float64(r.HitsCache) / float64(total)
		r.EffectiveHitRate = float64(r.HitsCache+r.HitsStore) / float64(total)
	}
	return r
}

// ServerRuntimeMeta 用于管理面板展示「服务元信息摘要」。
type ServerRuntimeMeta struct {
	Version    string    `json:"version"`
	BuildTime  string    `json:"build_time,omitempty"`
	CommitID   string    `json:"commit_id,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	UptimeSec  float64   `json:"uptime_sec"`
	Mode       string    `json:"mode"` // prod / dev
	ListenAddr string    `json:"listen_addr"`
	DataDir    string    `json:"data_dir"`
	URLFile    string    `json:"url_file"`
	AccessFile string    `json:"access_file"`
}
