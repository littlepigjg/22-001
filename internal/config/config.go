package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	Server    ServerCfg
	Storage   StorageCfg
	ShortCode ShortCodeCfg
	Log       LogCfg
	Janitor   JanitorCfg
	Stats     StatsCfg
}

type ServerCfg struct {
	Addr            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
	MaxBodyBytes    int64
}

type StorageCfg struct {
	urlFilePath  string
	logFilePath  string
	syncInterval time.Duration
	flushOnWrite bool
}

type ShortCodeCfg struct {
	Length     int
	Alphabet   string
	MaxRetries int
}

type LogCfg struct {
	Level  string
	Caller bool
}

type JanitorCfg struct {
	Enabled  bool
	Interval time.Duration
	Batch    int
}

type StatsCfg struct {
	CacheTTL   time.Duration
	MaxRecords int
}

func (s *StorageCfg) URLFilePath(v string) *StorageCfg {
	s.urlFilePath = v
	return s
}

func (s *StorageCfg) LogFilePath(v string) *StorageCfg {
	s.logFilePath = v
	return s
}

func (s *StorageCfg) SyncInterval(v time.Duration) *StorageCfg {
	s.syncInterval = v
	return s
}

func (s *StorageCfg) FlushOnWrite(v bool) *StorageCfg {
	s.flushOnWrite = v
	return s
}

func (s *StorageCfg) GetURLFilePath() string   { return s.urlFilePath }
func (s *StorageCfg) GetLogFilePath() string   { return s.logFilePath }
func (s *StorageCfg) GetSyncInterval() time.Duration { return s.syncInterval }
func (s *StorageCfg) GetFlushOnWrite() bool    { return s.flushOnWrite }

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
			urlFilePath:  "./data/urls.json",
			logFilePath:  "./data/access.log",
			syncInterval: 30 * time.Second,
			flushOnWrite: false,
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

func Load() *Config {
	cfg := Default()

	cfg.Server.Addr = envString("SHURL_SERVER_ADDR", cfg.Server.Addr)
	cfg.Server.ReadTimeout = envDuration("SHURL_SERVER_READ_TIMEOUT", cfg.Server.ReadTimeout)
	cfg.Server.WriteTimeout = envDuration("SHURL_SERVER_WRITE_TIMEOUT", cfg.Server.WriteTimeout)
	cfg.Server.IdleTimeout = envDuration("SHURL_SERVER_IDLE_TIMEOUT", cfg.Server.IdleTimeout)
	cfg.Server.ShutdownTimeout = envDuration("SHURL_SERVER_SHUTDOWN_TIMEOUT", cfg.Server.ShutdownTimeout)
	cfg.Server.MaxBodyBytes = envInt64("SHURL_SERVER_MAX_BODY_BYTES", cfg.Server.MaxBodyBytes)

	cfg.Storage.URLFilePath(envString("SHURL_STORAGE_URL_FILE", cfg.Storage.GetURLFilePath()))
	cfg.Storage.LogFilePath(envString("SHURL_STORAGE_LOG_FILE", cfg.Storage.GetLogFilePath()))
	cfg.Storage.SyncInterval(envDuration("SHURL_STORAGE_SYNC_INTERVAL", cfg.Storage.GetSyncInterval()))
	cfg.Storage.FlushOnWrite(envBool("SHURL_STORAGE_FLUSH_ON_WRITE", cfg.Storage.GetFlushOnWrite()))

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
