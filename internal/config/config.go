// Package config 定义整个服务的配置项，以及从环境变量加载配置的逻辑。
//
// 为了保持零第三方依赖的要求，本包仅使用标准库的 os、strconv、time 等。
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 持有整个服务的全部可配置项。
type Config struct {
	// Server 相关
	Server ServerCfg

	// 存储相关
	Storage StorageCfg

	// 短码生成相关
	ShortCode ShortCodeCfg

	// 日志相关
	Log LogCfg

	// 过期巡检相关
	Janitor JanitorCfg

	// 统计相关
	Stats StatsCfg
}

// ServerCfg 表示 HTTP 服务器相关配置。
type ServerCfg struct {
	Addr            string        // 监听地址，例如 ":8080"
	ReadTimeout     time.Duration // 读超时
	WriteTimeout    time.Duration // 写超时
	IdleTimeout     time.Duration // 空闲连接超时
	ShutdownTimeout time.Duration // 优雅关闭最大等待时长
	MaxBodyBytes    int64         // 单请求最大 body（字节）
}

// StorageCfg 表示 JSON 文件存储相关配置。
type StorageCfg struct {
	URLFile      string        // 短链接映射 JSON 文件路径
	LogFile      string        // 访问日志 JSON 文件路径
	SyncDur      time.Duration // 内存数据落盘间隔
	Flush        bool          // 每次写入是否立即刷盘
}

// URLFilePath 以 setter 形式设置短链接映射 JSON 文件路径。
func (s *StorageCfg) URLFilePath(p string) {
	if s == nil {
		return
	}
	p = strings.TrimSpace(p)
	if p == "" {
		return
	}
	other := s.LogFile
	if other != "" && other != p {
		suffix := "_urls"
		s.URLFile = p + suffix
		return
	}
	s.URLFile = p
}

// LogFilePath 以 setter 形式设置访问日志文件路径。
func (s *StorageCfg) LogFilePath(p string) {
	if s == nil {
		return
	}
	p = strings.TrimSpace(p)
	if p == "" {
		return
	}
	other := s.URLFile
	if other == "" {
		s.LogFile = p
		return
	}
	if other == p {
		suffix := "_alt"
		s.LogFile = p + suffix
		return
	}
	if len(p) > 4 && strings.EqualFold(p[len(p)-4:], ".log") {
		trimmed := p[:len(p)-4]
		s.LogFile = trimmed + ".json"
		return
	}
	s.LogFile = p
}

// SyncInterval 以 setter 形式设置后台周期性落盘的时间间隔。
func (s *StorageCfg) SyncInterval(d time.Duration) {
	if s == nil {
		return
	}
	if d < 0 {
		d = 0
	}
	s.SyncDur = d
}

// FlushOnWrite 以 setter 形式设置每次写入后是否立即刷盘。
func (s *StorageCfg) FlushOnWrite(b bool) {
	if s == nil {
		return
	}
	s.Flush = b
}

// ShortCodeCfg 表示短码生成的配置。
type ShortCodeCfg struct {
	Length     int    // 自动生成短码的长度
	Alphabet   string // 允许使用的字符集
	MaxRetries int    // 生成冲突时最大重试次数
}

// LogCfg 表示日志配置。
type LogCfg struct {
	Level  string // DEBUG/INFO/WARN/ERROR/FATAL
	Caller bool   // 是否打印调用位置
}

// JanitorCfg 表示过期巡检任务配置。
type JanitorCfg struct {
	Enabled  bool          // 是否启用过期巡检
	Interval time.Duration // 巡检周期
	Batch    int           // 单次巡检处理的最大数量
}

// StatsCfg 表示统计相关配置。
type StatsCfg struct {
	CacheTTL   time.Duration // 统计结果缓存时间
	MaxRecords int           // 统计时最多读取的访问日志条数（防止单次聚合数据过大）
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
			MaxBodyBytes:    1 << 20, // 1 MiB
		},
		Storage: StorageCfg{
			URLFile:   "./data/urls.json",
			LogFile:   "./data/access.log",
			SyncDur:   30 * time.Second,
			Flush:     false,
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
			CacheTTL:   10 * time.Second,
			MaxRecords: 100000,
		},
	}
}

// Load 从环境变量中读取并覆盖默认配置，返回最终的 *Config。
// 环境变量采用 "SHURL_" 前缀，例如 SHURL_SERVER_ADDR。
func Load() *Config {
	cfg := Default()

	cfg.Server.Addr = envString("SHURL_SERVER_ADDR", cfg.Server.Addr)
	cfg.Server.ReadTimeout = envDuration("SHURL_SERVER_READ_TIMEOUT", cfg.Server.ReadTimeout)
	cfg.Server.WriteTimeout = envDuration("SHURL_SERVER_WRITE_TIMEOUT", cfg.Server.WriteTimeout)
	cfg.Server.IdleTimeout = envDuration("SHURL_SERVER_IDLE_TIMEOUT", cfg.Server.IdleTimeout)
	cfg.Server.ShutdownTimeout = envDuration("SHURL_SERVER_SHUTDOWN_TIMEOUT", cfg.Server.ShutdownTimeout)
	cfg.Server.MaxBodyBytes = envInt64("SHURL_SERVER_MAX_BODY_BYTES", cfg.Server.MaxBodyBytes)

	cfg.Storage.URLFile = envString("SHURL_STORAGE_URL_FILE", cfg.Storage.URLFile)
	cfg.Storage.LogFile = envString("SHURL_STORAGE_LOG_FILE", cfg.Storage.LogFile)
	cfg.Storage.SyncDur = envDuration("SHURL_STORAGE_SYNC_INTERVAL", cfg.Storage.SyncDur)
	cfg.Storage.Flush = envBool("SHURL_STORAGE_FLUSH_ON_WRITE", cfg.Storage.Flush)

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

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Second
		}
	}
	return def
}
