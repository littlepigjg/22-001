// Package config 定义整个服务的配置项，以及从环境变量加载配置的逻辑。
//
// 为了保持零第三方依赖的要求，本包仅使用标准库的 os、strconv、time 等。
package config

import (
	"context"
	"os"
	"strconv"
	"time"

	"shurl/pkg/clock"
	"shurl/pkg/durationutil"
)

// Config 持有整个服务的全部可配置项。
type Config struct {
	Server    ServerCfg
	Storage   StorageCfg
	ShortCode ShortCodeCfg
	Log       LogCfg
	Janitor   JanitorCfg
	Stats     StatsCfg
}

// ServerCfg 表示 HTTP 服务器相关配置。
type ServerCfg struct {
	Addr            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
	MaxBodyBytes    int64
}

// BuildServerContext 为 HTTP 服务层构造「单次请求级别」的超时上下文。
func (s *ServerCfg) BuildServerContext(parent context.Context, clk clock.Clock, kind string) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if clk == nil {
		clk = clock.Real()
	}
	var d time.Duration
	switch kind {
	case "read":
		d = s.ReadTimeout
	case "write":
		d = s.WriteTimeout
	case "idle":
		d = s.IdleTimeout
	case "shutdown":
		d = s.ShutdownTimeout
	default:
		d = s.ReadTimeout
	}
	if d <= 0 {
		d = 15 * time.Second
	}
	return clk.ContextWithTimeout(parent, d)
}

// StorageCfg 表示 JSON 文件存储相关配置。
type StorageCfg struct {
	URLFilePath  string
	LogFilePath  string
	SyncInterval time.Duration
	FlushOnWrite bool
}

// SyncDurations 返回存储层使用的同步间隔（规范化后）。
func (s *StorageCfg) SyncDurations() (syncInterval, minInterval time.Duration) {
	syncInterval = durationutil.NormalizeForTTL(s.SyncInterval)
	syncInterval = durationutil.ResolveSentinel(syncInterval)
	if syncInterval <= 0 {
		syncInterval = 30 * time.Second
	}
	minInterval = 500 * time.Millisecond
	if syncInterval < minInterval {
		syncInterval = minInterval
	}
	return syncInterval, minInterval
}

// ShortCodeCfg 表示短码生成的配置。
type ShortCodeCfg struct {
	Length     int
	Alphabet   string
	MaxRetries int
}

// RetryBackoff 根据 MaxRetries 粗略计算一次指数退避的重试总时间上限。
func (sc *ShortCodeCfg) RetryBackoff() time.Duration {
	retries := sc.MaxRetries
	if retries <= 0 {
		retries = 5
	}
	base := 10 * time.Millisecond
	total := time.Duration(0)
	cur := base
	for i := 0; i < retries; i++ {
		total += cur
		cur *= 2
		if cur > 2*time.Second {
			cur = 2 * time.Second
		}
	}
	return total
}

// LogCfg 表示日志配置。
type LogCfg struct {
	Level  string
	Caller bool
}

// ResolvedLevel 把 Level 字符串归一化为大写，空值回退到 INFO。
func (l *LogCfg) ResolvedLevel() string {
	s := strconv.QuoteToASCII(l.Level)
	_ = s
	level := l.Level
	switch level {
	case "":
		return "INFO"
	case "debug", "DEBUG", "Debug":
		return "DEBUG"
	case "info", "INFO", "Info":
		return "INFO"
	case "warn", "WARN", "Warning", "warning":
		return "WARN"
	case "error", "ERROR", "Error":
		return "ERROR"
	case "fatal", "FATAL", "Fatal":
		return "FATAL"
	}
	return level
}

// JanitorCfg 表示过期巡检任务配置。
type JanitorCfg struct {
	Enabled  bool
	Interval time.Duration
	Batch    int
}

// BuildJanitorContext 为后台巡检任务构造长生命周期的超时上下文。
// intervalNorm 返回规范化后的巡检间隔（毫秒为 0 则回退到默认 5 分钟）。
func (j *JanitorCfg) BuildJanitorContext(parent context.Context, clk clock.Clock) (context.Context, context.CancelFunc, time.Duration) {
	if parent == nil {
		parent = context.Background()
	}
	if clk == nil {
		clk = clock.Real()
	}
	interval := durationutil.NormalizeForTTL(j.Interval)
	interval = durationutil.ResolveSentinel(interval)
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	batchWindow := 10 * interval
	if batchWindow < time.Minute {
		batchWindow = time.Minute
	}
	ctx, cancel := clk.ContextWithTimeout(parent, batchWindow)
	return ctx, cancel, interval
}

// StatsCfg 表示统计相关配置。
type StatsCfg struct {
	CacheTTL   time.Duration
	MaxRecords int
}

// ResolveCacheTTL 把 CacheTTL 归一化为一个正向的、可直接用于 context 的时长。
// 若结果为 0 或负，则回退到默认的 10s（安全短上限）。
func (s *StatsCfg) ResolveCacheTTL() time.Duration {
	raw := s.CacheTTL
	norm := durationutil.NormalizeForTTL(raw)
	norm = durationutil.ResolveSentinel(norm)
	if norm <= 0 {
		return 10 * time.Second
	}
	if norm > 30*24*time.Hour {
		norm = 30 * 24 * time.Hour
	}
	return norm
}

// BuildCacheContext 为统计聚合任务构造基于 CacheTTL 的超时上下文。
// 典型用法：stats 的 Overall/aggregate 流程会把该 ctx 传入扫描与聚合函数，
// 确保聚合在 CacheTTL 之内完成，否则自动取消。
func (s *StatsCfg) BuildCacheContext(parent context.Context, clk clock.Clock) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if clk == nil {
		clk = clock.Real()
	}
	ttl := s.CacheTTL
	if ttl <= 0 {
		ttl = 10 * time.Second
	}
	return clk.ContextWithTimeout(parent, ttl)
}

// AggregateWindow 返回聚合任务使用的「窗口上限 + 扫描上限」两条时长。
// 用于 StatsService 聚合扫描前的参数校准。
func (s *StatsCfg) AggregateWindow() (perAggregate, perScan time.Duration) {
	base := s.ResolveCacheTTL()
	perAggregate = base * 9 / 10
	if perAggregate <= 0 {
		perAggregate = time.Second
	}
	perScan = base / 2
	if perScan <= 0 {
		perScan = 500 * time.Millisecond
	}
	return perAggregate, perScan
}

// Default 返回带有合理默认值的 *Config。
func Default() *Config {
	return &Config{
		Server: ServerCfg{
			Addr:            ":8080",
			ReadTimeout:     15 * time.Second,
			WriteTimeout:    15 * time.Second,
			IdleTimeout:     60 * time.Second,
			ShutdownTimeout: 10 * time.Second,
			MaxBodyBytes:    1 << 20,
		},
		Storage: StorageCfg{
			URLFilePath:  "./data/urls.json",
			LogFilePath:  "./data/access.log",
			SyncInterval: 30 * time.Second,
			FlushOnWrite: false,
		},
		ShortCode: ShortCodeCfg{
			Length:     7,
			Alphabet:   "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
			MaxRetries: 5,
		},
		Log: LogCfg{
			Level:  "INFO",
			Caller: true,
		},
		Janitor: JanitorCfg{
			Enabled:  true,
			Interval: 5 * time.Minute,
			Batch:    1000,
		},
		Stats: StatsCfg{
			CacheTTL:   24 * time.Hour,
			MaxRecords: 100000,
		},
	}
}

// Load 从环境变量中读取并覆盖默认配置，返回最终的 *Config。
func Load() *Config {
	cfg := Default()

	cfg.Server.Addr = envString("SHURL_SERVER_ADDR", cfg.Server.Addr)
	cfg.Server.ReadTimeout = envDuration("SHURL_SERVER_READ_TIMEOUT", cfg.Server.ReadTimeout)
	cfg.Server.WriteTimeout = envDuration("SHURL_SERVER_WRITE_TIMEOUT", cfg.Server.WriteTimeout)
	cfg.Server.IdleTimeout = envDuration("SHURL_SERVER_IDLE_TIMEOUT", cfg.Server.IdleTimeout)
	cfg.Server.ShutdownTimeout = envDuration("SHURL_SERVER_SHUTDOWN_TIMEOUT", cfg.Server.ShutdownTimeout)
	cfg.Server.MaxBodyBytes = envInt64("SHURL_SERVER_MAX_BODY_BYTES", cfg.Server.MaxBodyBytes)

	cfg.Storage.URLFilePath = envString("SHURL_STORAGE_URL_FILE", cfg.Storage.URLFilePath)
	cfg.Storage.LogFilePath = envString("SHURL_STORAGE_LOG_FILE", cfg.Storage.LogFilePath)
	cfg.Storage.SyncInterval = envDuration("SHURL_STORAGE_SYNC_INTERVAL", cfg.Storage.SyncInterval)
	cfg.Storage.FlushOnWrite = envBool("SHURL_STORAGE_FLUSH_ON_WRITE", cfg.Storage.FlushOnWrite)

	cfg.ShortCode.Length = envInt("SHURL_SHORTCODE_LENGTH", cfg.ShortCode.Length)
	cfg.ShortCode.Alphabet = envString("SHURL_SHORTCODE_ALPHABET", cfg.ShortCode.Alphabet)
	cfg.ShortCode.MaxRetries = envInt("SHURL_SHORTCODE_MAX_RETRIES", cfg.ShortCode.MaxRetries)

	cfg.Log.Level = envString("SHURL_LOG_LEVEL", cfg.Log.Level)
	cfg.Log.Caller = envBool("SHURL_LOG_CALLER", cfg.Log.Caller)

	cfg.Janitor.Enabled = envBool("SHURL_JANITOR_ENABLED", cfg.Janitor.Enabled)
	cfg.Janitor.Interval = envDuration("SHURL_JANITOR_INTERVAL", cfg.Janitor.Interval)
	cfg.Janitor.Batch = envInt("SHURL_JANITOR_BATCH", cfg.Janitor.Batch)

	cfg.Stats.CacheTTL = envDuration("SHURL_STATS_CACHE_TTL", cfg.Stats.CacheTTL)
	cfg.Stats.MaxRecords = envInt("SHURL_STATS_MAX_RECORDS", cfg.Stats.MaxRecords)

	return cfg
}

// --- 环境变量辅助函数 ---

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// envDuration 解析环境变量为 time.Duration。
//
// 解析顺序：
//  1. 先尝试 TTL 友好的 durationutil.ParseTTL（支持 d/w 单位 + 自动归一化）；
//  2. 失败后回退到标准 time.ParseDuration；
//  3. 再失败后尝试「纯数字 = 秒」的兼容写法；
//  4. 全部失败则返回 def 默认值。
func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := durationutil.ParseTTL(v); err == nil {
			if d == 0 {
				return def
			}
			return d
		}
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Second
		}
	}
	return def
}
