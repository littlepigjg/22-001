package shurl_test

import (
	"time"

	"shurl/internal/config"
)

func minimalConfig() *config.Config {
	cfg := config.Default()
	cfg.Server.Addr = "127.0.0.1:0"
	cfg.Server.ReadTimeout = 5 * time.Second
	cfg.Server.WriteTimeout = 5 * time.Second
	cfg.Server.IdleTimeout = 5 * time.Second
	cfg.Server.ShutdownTimeout = 3 * time.Second
	cfg.Stats.CacheTTL = 5 * time.Second
	cfg.Stats.MaxRecords = 1000
	cfg.Storage.URLFilePath = "/tmp/defect_verify_urls.json"
	cfg.Storage.LogFilePath = "/tmp/defect_verify_access.log"
	return cfg
}

func registerShortCodeForStats(t interface{ Helper() }) string {
	t.Helper()
	return "abcdef0"
}
