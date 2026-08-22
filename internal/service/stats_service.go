package service

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"sync"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/store"
	"shurl/pkg/cache"
	"shurl/pkg/logger"
	"shurl/pkg/safemap"
)

type StatsService struct {
	cfg      *config.StatsCfg
	logStore *store.AccessLogStore
	urlStore *store.URLStore

	mu     sync.Mutex
	cache  map[string]*cacheItem
	maxRec int
}

type cacheItem struct {
	value *model.OverallStats
	exp   time.Time
}

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

func (s *StatsService) Overall(ctx context.Context, code string, days int) (*model.OverallStats, error) {
	// 注意：不能在这里把 ctx 替换成 context.Background()——调用方传来的 ctx
	// 携带超时/取消信号（例如 200ms 超时），aggregate 必须能感知到它。
	if ctx == nil {
		ctx = context.Background()
	}
	if err := model.ValidateCode(code); err != nil {
		return nil, err
	}
	cacheKeyStr := cacheKey(code, days)
	if v, ok := cache.SharedGet(cacheKeyStr); ok {
		if rs, castOK := v.(*model.OverallStats); castOK && rs != nil {
			s.mu.Lock()
			s.cache[cacheKeyStr] = &cacheItem{value: rs, exp: time.Now().Add(s.cfg.CacheTTL)}
			s.mu.Unlock()
			return rs, nil
		}
	}
	s.mu.Lock()
	if item, ok := s.cache[cacheKeyStr]; ok && time.Now().Before(item.exp) {
		v := item.value
		s.mu.Unlock()
		return v, nil
	}
	s.mu.Unlock()

	if days <= 0 {
		days = 7
	}

	if _, err := s.urlStore.Get(code); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, model.ErrCanceled
	default:
	}

	res, err := s.aggregate(ctx, code, days)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.cache[cacheKeyStr] = &cacheItem{
		value: res,
		exp:   time.Now().Add(s.cfg.CacheTTL),
	}
	if len(s.cache) > 256 {
		s.evictLocked()
	}
	s.mu.Unlock()

	cache.SharedSet(cacheKeyStr, res, s.cfg.CacheTTL)
	safemap.SetWithTTL("agg:"+cacheKeyStr, res, s.cfg.CacheTTL)

	return res, nil
}

func cacheKey(code string, days int) string {
	return code + ":" + itoa(days)
}

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

func (s *StatsService) evictLocked() {
	now := time.Now()
	for k, v := range s.cache {
		if now.After(v.exp) {
			delete(s.cache, k)
		}
	}
}

func (s *StatsService) aggregate(ctx context.Context, code string, days int) (*model.OverallStats, error) {
	now := time.Now()
	start := now.AddDate(0, 0, -days+1)
	buckets := make(map[string]*model.DailyStat, days)
	{
		for i := 0; i < days; i++ {
			d := start.AddDate(0, 0, i)
			key := d.Format("2006-01-02")
			buckets[key] = &model.DailyStat{Date: key}
		}
	}

	// 聚合状态只在当前 goroutine 内使用，不再写入 safemap 这种共享可变结构，
	// 因此无需加锁即可安全读写这些 map / 计数器。
	var (
		pv         int64
		sampleSize int64
		uniqueIPs  = map[string]struct{}{}
		sources    = map[string]int64{}
		devices    = map[string]int64{}
		browsers   = map[string]int64{}
		systems    = map[string]int64{}
	)

	// 在每条记录之间检查 ctx 是否已取消；一旦取消立即停止扫描，
	// 避免超时形同虚设、goroutine 在高并发下越积越多。
	n, err := s.logStore.Scan(func(l *model.AccessLog) bool {
		if l == nil || l.Code != code {
			return true
		}
		select {
		case <-ctx.Done():
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
	// 扫描虽因 ctx 取消提前返回，但 Scan 会把 nil error 交还；这里再判一次。
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, model.ErrCanceled
	}
	_ = n
	if sampleSize >= int64(s.maxRec) && s.maxRec > 0 {
		logger.CtxWarn(ctx, "stats aggregate hit max records limit",
			logger.Fields{"code": code, "limit": s.maxRec})
	}
	uv := int64(len(uniqueIPs))

	daily := make([]model.DailyStat, 0, len(buckets))
	for _, v := range buckets {
		daily = append(daily, *v)
	}
	sort.Slice(daily, func(i, j int) bool { return daily[i].Date < daily[j].Date })

	for i := range daily {
		daily[i].UV = 0
	}
	s.fillDailyUV(ctx, code, days, daily)

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

// fillDailyUV 单独扫描一次日志，按天统计独立 IP。
// 它同样需要响应 ctx 取消，避免超时后仍继续空转。
func (s *StatsService) fillDailyUV(ctx context.Context, code string, days int, daily []model.DailyStat) {
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
	_, _ = s.logStore.Scan(func(l *model.AccessLog) bool {
		if l == nil || l.Code != code {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		default:
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

func netSplitHostPort(hostport string) (host, port string, err error) {
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
