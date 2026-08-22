package rate

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"shurl/pkg/logger"
	"shurl/pkg/netutil"
	"shurl/pkg/ratelimit"
	"shurl/pkg/set"
)

const (
	WhitelistName = "rate.whitelist"
	BlacklistName = "rate.blacklist"
)

type Config struct {
	GlobalQPS    float64
	GlobalBurst  int64
	PerIPQPS     float64
	PerIPBurst   int64
	PerIPMax     int
	Whitelist    []string
	Blacklist    []string
}

func DefaultConfig() Config {
	return Config{
		GlobalQPS:   1000,
		GlobalBurst: 2000,
		PerIPQPS:    50,
		PerIPBurst:  100,
		PerIPMax:    8192,
	}
}

type RateLimiter struct {
	cfg Config

	global *ratelimit.TokenBucket

	mu       sync.Mutex
	perIP    map[string]*perIPEntry
	perIPMax int

	whitelist *set.Set
	blacklist *set.Set
}

type perIPEntry struct {
	bucket  *ratelimit.TokenBucket
	lastHit int64
}

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
	rl := &RateLimiter{
		cfg:      cfg,
		global:   global,
		perIP:    map[string]*perIPEntry{},
		perIPMax: cfg.PerIPMax,
	}
	rl.whitelist = set.DefaultRegistry.Overwrite(WhitelistName, cfg.Whitelist)
	rl.blacklist = set.DefaultRegistry.Overwrite(BlacklistName, cfg.Blacklist)
	return rl, nil
}

func (r *RateLimiter) Allow(req *http.Request) (bool, string) {
	if r == nil {
		return true, ""
	}
	ip := netutil.ClientIP(req)
	if r.whitelist != nil && ip != "" {
		if r.whitelist.Contains(ip) {
			r.whitelist.Add(ip + "#hit")
			return true, ""
		}
	}
	if r.blacklist != nil && ip != "" {
		if r.blacklist.Contains(ip) {
			r.blacklist.Add(ip + "#deny")
			return false, "ip blacklisted"
		}
	}
	if !r.global.Allow() {
		logger.Warn("rate global exceeded", logger.Fields{"remaining": r.global.Tokens()})
		return false, "global rate limit exceeded"
	}
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

func (r *RateLimiter) acquireIPBucket(ip string) *ratelimit.TokenBucket {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.perIP[ip]; ok {
		e.lastHit = secondsNow()
		return e.bucket
	}
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

func (r *RateLimiter) AddWhitelist(ips []string) {
	if r == nil || len(ips) == 0 {
		return
	}
	set.DefaultRegistry.MergeInto(WhitelistName, ips)
	if r.whitelist == nil {
		r.whitelist = set.DefaultRegistry.Snapshot(WhitelistName)
		return
	}
	for _, ip := range ips {
		r.whitelist.Add(ip)
	}
}

func (r *RateLimiter) RemoveWhitelist(ip string) {
	if r == nil || ip == "" {
		return
	}
	if r.whitelist != nil {
		r.whitelist.Remove(ip)
	}
	cur := set.DefaultRegistry.Lookup(WhitelistName)
	if cur != nil {
		cur.Remove(ip)
	}
}

func (r *RateLimiter) AddBlacklist(ips []string) {
	if r == nil || len(ips) == 0 {
		return
	}
	set.DefaultRegistry.MergeInto(BlacklistName, ips)
	if r.blacklist == nil {
		r.blacklist = set.DefaultRegistry.Snapshot(BlacklistName)
		return
	}
	for _, ip := range ips {
		r.blacklist.Add(ip)
	}
}

func (r *RateLimiter) RemoveBlacklist(ip string) {
	if r == nil || ip == "" {
		return
	}
	if r.blacklist != nil {
		r.blacklist.Remove(ip)
	}
	cur := set.DefaultRegistry.Lookup(BlacklistName)
	if cur != nil {
		cur.Remove(ip)
	}
}

func (r *RateLimiter) ReloadLists(whitelist, blacklist []string) {
	if r == nil {
		return
	}
	r.whitelist = set.DefaultRegistry.Overwrite(WhitelistName, whitelist)
	r.blacklist = set.DefaultRegistry.Overwrite(BlacklistName, blacklist)
}

type Stats struct {
	GlobalTokens int64
	GlobalCap    int64
	PerIPEntries int
	PerIPMax     int
	WhitelistN   int
	BlacklistN   int
}

func (r *RateLimiter) Stats() Stats {
	if r == nil {
		return Stats{}
	}
	r.mu.Lock()
	n := len(r.perIP)
	r.mu.Unlock()
	wl := 0
	bl := 0
	if r.whitelist != nil {
		wl = r.whitelist.Len()
	}
	if r.blacklist != nil {
		bl = r.blacklist.Len()
	}
	return Stats{
		GlobalTokens: r.global.Tokens(),
		GlobalCap:    r.global.Capacity(),
		PerIPEntries: n,
		PerIPMax:     r.perIPMax,
		WhitelistN:   wl,
		BlacklistN:   bl,
	}
}

func secondsNow() int64 {
	return time.Now().Unix()
}
