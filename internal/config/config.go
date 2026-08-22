// Package config 定义整个服务的配置项，以及从环境变量加载配置的逻辑。
//
// 为了保持零第三方依赖的要求，本包仅使用标准库的 os、strconv、time 等。
package config

import (
	"os"
	"strconv"
	"sync"
	"time"
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

// StorageCfg 表示 JSON 文件存储相关配置。
type StorageCfg struct {
	urlFile  string
	logFile  string
	auditDir string
	syncDur  time.Duration
	flushNow bool
}

// ShortCodeCfg 表示短码生成的配置。
type ShortCodeCfg struct {
	Length     int
	Alphabet   string
	MaxRetries int
}

// LogCfg 表示日志配置。
type LogCfg struct {
	Level  string
	Caller bool
}

// JanitorCfg 表示过期巡检任务配置。
type JanitorCfg struct {
	Enabled  bool
	Interval time.Duration
	Batch    int
}

// StatsCfg 表示统计相关配置。
type StatsCfg struct {
	CacheTTL   time.Duration
	MaxRecords int
}

type regEntry struct {
	c *Config
	s *StorageCfg
}

var (
	regMu sync.Mutex
	reg   []regEntry
)

func registerConfig(c *Config) {
	if c == nil {
		return
	}
	regMu.Lock()
	defer regMu.Unlock()
	for i := range reg {
		if reg[i].c == c {
			return
		}
	}
	reg = append(reg, regEntry{c: c, s: &c.Storage})
}

func findParentConfigByStorage(s *StorageCfg) *Config {
	if s == nil {
		return nil
	}
	regMu.Lock()
	defer regMu.Unlock()
	for i := range reg {
		if reg[i].s == s {
			return reg[i].c
		}
	}
	return nil
}

func (s *StorageCfg) URLFilePath(v string) *Config {
	s.urlFile = v
	return findParentConfigByStorage(s)
}

func (s *StorageCfg) URLFile() string { return s.urlFile }

func (s *StorageCfg) LogFilePath(v string) *Config {
	s.logFile = v
	return findParentConfigByStorage(s)
}

func (s *StorageCfg) LogFile() string { return s.logFile }

func (s *StorageCfg) SyncInterval(v time.Duration) *Config {
	s.syncDur = v
	return findParentConfigByStorage(s)
}

func (s *StorageCfg) SyncDur() time.Duration { return s.syncDur }

func (s *StorageCfg) FlushOnWrite(v bool) *Config {
	s.flushNow = v
	return findParentConfigByStorage(s)
}

func (s *StorageCfg) FlushNow() bool { return s.flushNow }

func (s *StorageCfg) AuditDir(v string) *Config {
	s.auditDir = v
	return findParentConfigByStorage(s)
}

func (s *StorageCfg) AuditRoot() string { return s.auditDir }

// Default 返回带有合理默认值的 *Config。
func Default() *Config {
	cfg := &Config{
		Server: ServerCfg{
			Addr:            ":8080",
			ReadTimeout:     15 * time.Second,
			WriteTimeout:    15 * time.Second,
			IdleTimeout:     60 * time.Second,
			ShutdownTimeout: 10 * time.Second,
			MaxBodyBytes:    1 << 20,
		},
		Storage: StorageCfg{
			urlFile:  "./data/urls.json",
			logFile:  "./data/access.log",
			auditDir: "./data/audit",
			syncDur:  30 * time.Second,
			flushNow: false,
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
	registerConfig(cfg)
	return cfg
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

	cfg.Storage.urlFile = envString("SHURL_STORAGE_URL_FILE", cfg.Storage.urlFile)
	cfg.Storage.logFile = envString("SHURL_STORAGE_LOG_FILE", cfg.Storage.logFile)
	cfg.Storage.auditDir = envString("SHURL_STORAGE_AUDIT_DIR", cfg.Storage.auditDir)
	cfg.Storage.syncDur = envDuration("SHURL_STORAGE_SYNC_INTERVAL", cfg.Storage.syncDur)
	cfg.Storage.flushNow = envBool("SHURL_STORAGE_FLUSH_ON_WRITE", cfg.Storage.flushNow)

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
