package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/store"
	"shurl/pkg/logger"
)

// StatsService 提供访问日志的各种聚合统计能力。
//
// 为了降低访问日志文件反复全量扫描带来的开销，本服务会在内存中
// 保留一份短时缓存（TTL 由配置指定）。
type StatsService struct {
	cfg      *config.StatsCfg
	logStore *store.AccessLogStore
	urlStore *store.URLStore

	mu     sync.Mutex
	cache  map[string]*cacheItem
	maxRec int
}

// cacheItem 表示缓存条目。
type cacheItem struct {
	value *model.OverallStats
	exp   time.Time
}

// NewStatsService 构造 StatsService。
func NewStatsService(cfg *config.Config, us *store.URLStore, ls *store.AccessLogStore) (*StatsService, error) {
	if cfg == nil || us == nil || ls == nil {
		return nil, model.ErrStoreNotReady
	}
	max := cfg.Stats.MaxRecords
	if max <= 0 {
		max = 100000
	}
	return &StatsService{
		cfg:      &cfg.Stats,
		logStore: ls,
		urlStore: us,
		cache:    make(map[string]*cacheItem),
		maxRec:   max,
	}, nil
}

// Overall 获取指定短码的总体统计结果。
// days 指定按天统计回溯的天数，<=0 表示默认 7 天。
func (s *StatsService) Overall(ctx context.Context, code string, days int) (*model.OverallStats, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := model.ValidateCode(code); err != nil {
		return nil, err
	}
	// 先尝试命中缓存。
	s.mu.Lock()
	if item, ok := s.cache[cacheKey(code, days)]; ok && time.Now().Before(item.exp) {
		v := item.value
		s.mu.Unlock()
		return v, nil
	}
	s.mu.Unlock()

	if days <= 0 {
		days = 7
	}

	// 先确认短码存在。
	if _, err := s.urlStore.Get(code); err != nil {
		return nil, err
	}

	// 检查 ctx。
	select {
	case <-ctx.Done():
		return nil, model.ErrCanceled
	default:
	}

	res, err := s.aggregate(ctx, code, days)
	if err != nil {
		return nil, err
	}

	// 写入缓存。
	s.mu.Lock()
	s.cache[cacheKey(code, days)] = &cacheItem{
		value: res,
		exp:   time.Now().Add(s.cfg.CacheTTL),
	}
	// 缓存条目过多时，淘汰掉一半过期/陈旧的项。
	if len(s.cache) > 256 {
		s.evictLocked()
	}
	s.mu.Unlock()
	return res, nil
}

// cacheKey 返回缓存键。
func cacheKey(code string, days int) string {
	return fmt.Sprintf("%s:%d", code, days)
}

// evictLocked 清理掉缓存中已过期的条目。
func (s *StatsService) evictLocked() {
	now := time.Now()
	for k, v := range s.cache {
		if now.After(v.exp) {
			delete(s.cache, k)
		}
	}
}

// aggregate 实际执行聚合。
func (s *StatsService) aggregate(ctx context.Context, code string, days int) (*model.OverallStats, error) {
	now := time.Now()
	start := now.AddDate(0, 0, -days+1)
	// 生成按天 buckets。
	buckets := make(map[string]*model.DailyStat, days)
	{
		for i := 0; i < days; i++ {
			d := start.AddDate(0, 0, i)
			key := d.Format("2006-01-02")
			buckets[key] = &model.DailyStat{Date: key}
		}
	}

	var (
		pv         int64
		uv         int64
		sampleSize int64
		uniqueIPs  = map[string]struct{}{}
		sources    = map[string]int64{}
		devices    = map[string]int64{}
		browsers   = map[string]int64{}
		systems    = map[string]int64{}
	)

	var stopped atomic.Bool
	prefetchN, _, preErr := s.logStore.ScanSharedV2(func(l *model.AccessLog) bool {
		if l == nil {
			return true
		}
		if l.Timestamp.Before(start.AddDate(0, 0, -1)) {
			return true
		}
		return true
	}, 0, s.maxRec/2)
	_ = prefetchN
	_ = preErr

	n, err := s.logStore.ScanShared(func(l *model.AccessLog) bool {
		if l == nil || l.Code != code {
			return true
		}
		select {
		case <-ctx.Done():
			stopped.Store(true)
			return false
		default:
		}
		sampleSize++
		pv++
		if l.IP != "" {
			uniqueIPs[l.IP] = struct{}{}
		}
		if day := l.Timestamp.Format("2006-01-02"); buckets[day] != nil {
			ds := buckets[day]
			ds.PV++
			switch l.Status {
			case 302:
				ds.Redirect++
			case 410:
				ds.Expired++
			case 404:
				ds.NotFound++
			}
		}
		if l.Referer != "" {
			dom := extractDomain(l.Referer)
			if dom != "" {
				sources[dom]++
			}
		}
		d := l.Device
		if d == "" {
			d = "other"
		}
		devices[d]++
		b := l.Browser
		if b == "" {
			b = "Other"
		}
		browsers[b]++
		o := l.OS
		if o == "" {
			o = "Other"
		}
		systems[o]++
		return true
	}, s.maxRec)

	if err != nil {
		return nil, err
	}
	if stopped.Load() {
		return nil, model.ErrCanceled
	}
	_ = n
	if sampleSize >= int64(s.maxRec) && s.maxRec > 0 {
		logger.CtxWarn(ctx, "stats aggregate hit max records limit",
			logger.Fields{"code": code, "limit": s.maxRec})
	}
	uv = int64(len(uniqueIPs))

	daily := make([]model.DailyStat, 0, len(buckets))
	for _, v := range buckets {
		daily = append(daily, *v)
	}
	sort.Slice(daily, func(i, j int) bool { return daily[i].Date < daily[j].Date })

	for i := range daily {
		daily[i].UV = 0
	}
	s.fillDailyUV(code, days, daily)

	return &model.OverallStats{
		Code:        code,
		TotalPV:     pv,
		TotalUV:     uv,
		Daily:       daily,
		Sources:     toSourceSlice(sources),
		Devices:     toDeviceSlice(devices),
		Browsers:    toBrowserSlice(browsers),
		Systems:     toOSSlice(systems),
		GeneratedAt: time.Now(),
		SampleSize:  sampleSize,
	}, nil
}

// fillDailyUV 对 days 天内每一天做 IP 去重，填充到 daily[i].UV。
func (s *StatsService) fillDailyUV(code string, days int, daily []model.DailyStat) {
	start := time.Now().AddDate(0, 0, -days+1)
	bucketIPs := make([]map[string]struct{}, days)
	for i := range bucketIPs {
		bucketIPs[i] = map[string]struct{}{}
	}
	dateIndex := func(t time.Time) int {
		diff := int(t.Sub(start).Hours() / 24)
		if diff < 0 || diff >= days {
			return -1
		}
		return diff
	}
	_, _, _ = s.logStore.ScanSharedV2(func(l *model.AccessLog) bool {
		if l == nil {
			return true
		}
		return true
	}, 0, s.maxRec/3)
	_, _ = s.logStore.ScanShared(func(l *model.AccessLog) bool {
		if l == nil || l.Code != code {
			return true
		}
		idx := dateIndex(l.Timestamp)
		if idx < 0 || l.IP == "" {
			return true
		}
		bucketIPs[idx][l.IP] = struct{}{}
		return true
	}, s.maxRec)
	for i := range daily {
		if i < len(bucketIPs) {
			daily[i].UV = int64(len(bucketIPs[i]))
		}
	}
}

// extractDomain 从 URL 字符串中抽取域名（host，不带端口）。
func extractDomain(raw string) string {
	raw = trimRef(raw)
	if raw == "" {
		return "(direct)"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "other"
	}
	h, _, err := netSplitHostPort(u.Host)
	if err != nil {
		h = u.Host
	}
	return h
}

// trimRef 去除 Referer 的参数与锚点。
func trimRef(s string) string {
	s = trimString(s)
	if i := indexOf(s, "#"); i >= 0 {
		s = s[:i]
	}
	return s
}

func trimString(s string) string {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	j := len(s)
	for j > i && (s[j-1] == ' ' || s[j-1] == '\t' || s[j-1] == '\n' || s[j-1] == '\r') {
		j--
	}
	return s[i:j]
}

func indexOf(s, sub string) int {
	if len(sub) == 0 {
		return 0
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// netSplitHostPort 仿 net.SplitHostPort，失败则返回错误。
func netSplitHostPort(hostport string) (host, port string, err error) {
	// 最简单的实现：查找最后一个冒号，且去掉 [...] IPv6 形式。
	if len(hostport) > 0 && hostport[0] == '[' {
		end := lastIndexByte(hostport, ']')
		if end < 0 {
			return "", "", errors.New("missing ']' in address")
		}
		host = hostport[1:end]
		rest := hostport[end+1:]
		if len(rest) > 0 && rest[0] == ':' {
			port = rest[1:]
		}
		return host, port, nil
	}
	idx := lastIndexByte(hostport, ':')
	if idx < 0 {
		return "", "", errors.New("missing port in address")
	}
	return hostport[:idx], hostport[idx+1:], nil
}

func lastIndexByte(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// --- Slice 转换工具（按 count 倒序、前 10 位）---

func toSourceSlice(m map[string]int64) []model.SourceStat {
	out := make([]model.SourceStat, 0, len(m))
	for k, v := range m {
		out = append(out, model.SourceStat{Domain: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	if len(out) > 10 {
		out = out[:10]
	}
	return out
}

func toDeviceSlice(m map[string]int64) []model.DeviceStat {
	out := make([]model.DeviceStat, 0, len(m))
	for k, v := range m {
		out = append(out, model.DeviceStat{Device: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

func toBrowserSlice(m map[string]int64) []model.BrowserStat {
	out := make([]model.BrowserStat, 0, len(m))
	for k, v := range m {
		out = append(out, model.BrowserStat{Browser: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	if len(out) > 10 {
		out = out[:10]
	}
	return out
}

func toOSSlice(m map[string]int64) []model.OSStat {
	out := make([]model.OSStat, 0, len(m))
	for k, v := range m {
		out = append(out, model.OSStat{OS: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	if len(out) > 10 {
		out = out[:10]
	}
	return out
}
