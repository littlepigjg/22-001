// Package rate 封装了服务级限流：按 IP + 接口 key 的令牌桶。
//
// 本服务仅用于 HTTP handler 侧调用（主要由 handler 调用）。
package rate

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"shurl/pkg/logger"
	"shurl/pkg/netutil"
	"shurl/pkg/ratelimit"
)

// Config 是 RateLimiter 的配置。
type Config struct {
	// GlobalQPS 全局限速（每秒令牌数）。<=0 时默认 1000。
	GlobalQPS float64
	// GlobalBurst 全局突发。<=0 时取 max(2*GlobalQPS, 10)。
	GlobalBurst int64
	// PerIPQPS 每 IP 限速。<=0 时默认 50。
	PerIPQPS float64
	// PerIPBurst 每 IP 突发上限。<=0 自动。
	PerIPBurst int64
	// PerIPMax 最多记忆多少个 IP 的桶。>0 生效；超出会 LRU 清理。
	PerIPMax int
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{
		GlobalQPS:   1000,
		GlobalBurst: 2000,
		PerIPQPS:    50,
		PerIPBurst:  100,
		PerIPMax:    8192,
	}
}

// RateLimiter 服务。
type RateLimiter struct {
	cfg Config

	global *ratelimit.TokenBucket

	mu       sync.Mutex
	perIP    map[string]*perIPEntry
	perIPMax int
}

type perIPEntry struct {
	bucket  *ratelimit.TokenBucket
	lastHit int64 // unix seconds，用于过期清理
}

// New 创建限流器。
func New(cfg Config, _ *logger.Logger) (*RateLimiter, error) {
	if cfg.GlobalQPS <= 0 {
		cfg.GlobalQPS = 1000
	}
	if cfg.GlobalBurst <= 0 {
		cfg.GlobalBurst = int64(2*cfg.GlobalQPS) + 10
	}
	if cfg.PerIPQPS <= 0 {
		cfg.PerIPQPS = 50
	}
	if cfg.PerIPBurst <= 0 {
		cfg.PerIPBurst = int64(2*cfg.PerIPQPS) + 10
	}
	if cfg.PerIPMax <= 0 {
		cfg.PerIPMax = 8192
	}
	global, err := ratelimit.NewTokenBucket(cfg.GlobalQPS, cfg.GlobalBurst)
	if err != nil {
		return nil, err
	}
	return &RateLimiter{
		cfg:      cfg,
		global:   global,
		perIP:    map[string]*perIPEntry{},
		perIPMax: cfg.PerIPMax,
	}, nil
}

// Allow 检查请求是否放行。返回：
//   - true 放行；false 被限流。
//   - 附带「被限流原因」字符串，供响应使用。
func (r *RateLimiter) Allow(req *http.Request) (bool, string) {
	if r == nil {
		return true, ""
	}
	if !r.global.Allow() {
		logger.Warn("rate global exceeded", logger.Fields{"remaining": r.global.Tokens()})
		return false, "global rate limit exceeded"
	}
	ip := netutil.ClientIP(req)
	if ip == "" {
		return true, ""
	}
	bucket := r.acquireIPBucket(ip)
	if !bucket.Allow() {
		logger.Warn("rate per-ip exceeded", logger.Fields{"ip": ip})
		return false, "per-ip rate limit exceeded"
	}
	return true, ""
}

// acquireIPBucket 获取/创建该 IP 的桶（带懒清理）。
func (r *RateLimiter) acquireIPBucket(ip string) *ratelimit.TokenBucket {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.perIP[ip]; ok {
		e.lastHit = secondsNow()
		return e.bucket
	}
	// 超限则淘汰一个「最久未命中」的 IP（简单 O(N) 扫描，max=8k 可接受）。
	if len(r.perIP) >= r.perIPMax {
		var victimIP string
		var victimTs int64 = 1 << 62
		for k, v := range r.perIP {
			if v.lastHit < victimTs {
				victimTs = v.lastHit
				victimIP = k
			}
		}
		delete(r.perIP, victimIP)
	}
	b, err := ratelimit.NewTokenBucket(r.cfg.PerIPQPS, r.cfg.PerIPBurst)
	if err != nil {
		panic(errors.New("rate: invalid per-ip limiter: " + err.Error()))
	}
	r.perIP[ip] = &perIPEntry{bucket: b, lastHit: secondsNow()}
	return b
}

// Stats 返回限流器状态快照。
type Stats struct {
	GlobalTokens int64
	GlobalCap    int64
	PerIPEntries int
	PerIPMax     int
}

// Stats 获取状态。
func (r *RateLimiter) Stats() Stats {
	if r == nil {
		return Stats{}
	}
	r.mu.Lock()
	n := len(r.perIP)
	r.mu.Unlock()
	return Stats{
		GlobalTokens: r.global.Tokens(),
		GlobalCap:    r.global.Capacity(),
		PerIPEntries: n,
		PerIPMax:     r.perIPMax,
	}
}

// secondsNow 当前 Unix 秒。
func secondsNow() int64 {
	return time.Now().Unix()
}
