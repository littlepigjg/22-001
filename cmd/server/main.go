// Command server 启动 SHURL 短链接 HTTP 服务。
//
// 纯 Go 标准库实现，使用 net/http 内置的 mux 做路由，通过中间件实现
// 请求 ID、日志、恢复、跨域、body 限制、限流、指标记录等通用能力。
//
// 用法：
//
//	go run ./cmd/server
//	# 或
//	go build -o shurl ./cmd/server && ./shurl
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"shurl/internal/admin"
	"shurl/internal/config"
	"shurl/internal/handler"
	"shurl/internal/metrics"
	"shurl/internal/rate"
	"shurl/internal/resolver"
	"shurl/internal/service"
	"shurl/internal/store"
	"shurl/pkg/logger"
	"shurl/web"
)

// Version 由构建脚本注入（默认 dev）。
var Version = "dev"

// BuildTime 由构建脚本注入。
var BuildTime = ""

// CommitID 由构建脚本注入。
var CommitID = ""

func main() {
	conf := config.Load()

	// 命令行参数覆盖部分配置。
	flag.StringVar(&conf.Server.Addr, "addr", conf.Server.Addr, "listen address")
	flag.StringVar(&conf.Log.Level, "log-level", conf.Log.Level, "log level (DEBUG/INFO/WARN/ERROR/FATAL)")
	flag.BoolVar(&conf.Storage.FlushOnWrite, "flush", conf.Storage.FlushOnWrite, "flush storage on every write")
	flag.Parse()

	// 初始化日志。
	logger.SetLevel(logger.ParseLevel(conf.Log.Level))
	logger.Info("shurl starting", logger.Fields{
		"addr":      conf.Server.Addr,
		"logLevel":  conf.Log.Level,
		"version":   Version,
		"buildTime": BuildTime,
		"commit":    CommitID,
	})

	// 根 context：在信号到来时取消。
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// 构建存储依赖。
	urlStore, err := store.NewURLStore(conf)
	if err != nil {
		logger.Fatal("create url store failed", logger.Fields{"err": err.Error()})
	}
	if err := urlStore.Load(ctx); err != nil {
		logger.Fatal("load url store failed", logger.Fields{"err": err.Error()})
	}
	logStore, err := store.NewAccessLogStore(conf)
	if err != nil {
		logger.Fatal("create log store failed", logger.Fields{"err": err.Error()})
	}
	if err := logStore.Open(ctx); err != nil {
		logger.Fatal("open log store failed", logger.Fields{"err": err.Error()})
	}

	// 构建 resolver（短码解析：布隆 + LRU + singleflight 回源）。
	rsv, err := resolver.New(resolver.Config{
		Store:      urlStore,
		CacheCap:   4096,
		CacheTTL:   10 * time.Minute,
		WarmOnBoot: true,
	})
	if err != nil {
		logger.Fatal("create resolver failed", logger.Fields{"err": err.Error()})
	}

	// 构建业务服务。
	urlSvc, err := service.NewURLService(conf, urlStore)
	if err != nil {
		logger.Fatal("create url service failed", logger.Fields{"err": err.Error()})
	}
	rdSvc, err := service.NewRedirectService(urlStore, logStore)
	if err != nil {
		logger.Fatal("create redirect service failed", logger.Fields{"err": err.Error()})
	}
	stSvc, err := service.NewStatsService(conf, urlStore, logStore)
	if err != nil {
		logger.Fatal("create stats service failed", logger.Fields{"err": err.Error()})
	}
	healthSvc, err := service.NewHealthService(urlStore, logStore)
	if err != nil {
		logger.Fatal("create health service failed", logger.Fields{"err": err.Error()})
	}
	janitor, err := service.NewJanitorService(conf, urlStore)
	if err != nil {
		logger.Fatal("create janitor service failed", logger.Fields{"err": err.Error()})
	}
	if err := janitor.Start(ctx); err != nil {
		logger.Fatal("start janitor failed", logger.Fields{"err": err.Error()})
	}

	// 构建限流服务。
	rlCfg := rate.DefaultConfig()
	limiter, err := rate.New(rlCfg, nil)
	if err != nil {
		logger.Fatal("create rate limiter failed", logger.Fields{"err": err.Error()})
	}

	// 构建指标服务（JSON + prom-text 导出）。
	metricsSvc := metrics.New(nil, nil)
	metricsSvc.SetExtra("version", Version)
	metricsSvc.SetExtra("build_time", BuildTime)
	metricsSvc.SetExtra("commit_id", CommitID)
	metricsSvc.SetExtra("listen_addr", conf.Server.Addr)
	metricsSvc.SetExtra("url_file", conf.Storage.URLFilePath)
	metricsSvc.SetExtra("access_file", conf.Storage.LogFilePath)
	// 预注册一些基础计数器（更方便之后看）。
	metricsSvc.Registry().GetOrCreateCounter("requests_total", "").Inc()
	metricsSvc.Registry().GetOrCreateGauge("info", "version="+Version).Set(1)

	// 构建管理服务（注册 flush/sync/close）。
	adminSvc := admin.New(nil)
	adminSvc.RegisterFlusher("url_store", urlStore)
	adminSvc.RegisterSyncer("access_log", logStore)
	adminSvc.RegisterCloser("url_store", urlStore)
	adminSvc.RegisterCloser("access_log", logStore)
	adminSvc.SetMeta("version", Version)
	adminSvc.SetMeta("commit", CommitID)
	adminSvc.SetMeta("resolver_stats", func() any { return rsv.Stats() })
	adminSvc.SetMeta("rate_stats", limiter.Stats())

	// 构建 handlers。
	urlH, err := handler.NewURLHandler(urlSvc)
	if err != nil {
		logger.Fatal("build url handler failed", logger.Fields{"err": err.Error()})
	}
	rdH, err := handler.NewRedirectHandler(rdSvc)
	if err != nil {
		logger.Fatal("build redirect handler failed", logger.Fields{"err": err.Error()})
	}
	stH, err := handler.NewStatsHandler(stSvc)
	if err != nil {
		logger.Fatal("build stats handler failed", logger.Fields{"err": err.Error()})
	}
	hh, err := handler.NewHealthHandler(healthSvc)
	if err != nil {
		logger.Fatal("build health handler failed", logger.Fields{"err": err.Error()})
	}
	adminH := handler.NewAdminHandler(adminSvc)
	metricsH := handler.NewMetricsHandler(metricsSvc)

	// 构建 mux 与路由。
	mux := http.NewServeMux()
	registerStatic(mux)
	hh.Register(mux)
	urlH.Register(mux)
	stH.Register(mux)
	rdH.Register(mux)
	adminH.RegisterRoutes(mux, "/api/admin")
	metricsH.RegisterRoutes(mux, "/api")
	// 根路径处理：短码重定向 + 首页。
	mux.HandleFunc("/", makeRootHandler(rdH))

	// 构建中间件栈（顺序相反：先声明的后执行）。
	var h http.Handler = mux
	// 1. 限流（全局 + 每 IP）—— 挂在中间件最外层（路由前）。
	h = rateLimitMiddleware(limiter)(h)
	// 2. 简单 CORS。
	h = handler.CORSMiddleware([]string{"*"})(h)
	// 3. 增强版 CORS（同一层级只会生效一种，这里保留配置能力，实际走上面）。
	_ = handler.NewAdvancedCORS(handler.DefaultAdvancedCORS())
	// 4. 请求体大小限制。
	h = handler.BodyLimitMiddleware(conf.Server.MaxBodyBytes)(h)
	// 5. 基础日志 + request-id。
	h = handler.LoggingMiddleware(h)
	h = handler.RequestIDMiddleware(h)
	// 6. panic 恢复。
	h = handler.RecoveryMiddleware(h)
	// 7. URL 规范化（去掉末尾 /）。
	h = handler.TrimLastSlashMiddleware(h)

	srv := &http.Server{
		Addr:         conf.Server.Addr,
		Handler:      h,
		ReadTimeout:  conf.Server.ReadTimeout,
		WriteTimeout: conf.Server.WriteTimeout,
		IdleTimeout:  conf.Server.IdleTimeout,
	}

	// 启动 HTTP 服务器（后台）。
	var wg sync.WaitGroup
	var srvErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("http server listening", logger.Fields{"addr": srv.Addr})
		healthSvc.MarkStarted()
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr = err
			logger.Error("http server error", logger.Fields{"err": err.Error()})
			cancel() // 通知主流程退出。
		}
	}()

	// 等待信号或错误。
	<-ctx.Done()
	logger.Info("shutdown signal received, begin graceful shutdown")
	shutdownTimeout := conf.Server.ShutdownTimeout
	if shutdownTimeout <= 0 {
		shutdownTimeout = 10 * time.Second
	}
	shutCtx, shutCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutCancel()

	// 1. 关闭 HTTP 服务器。
	if err := srv.Shutdown(shutCtx); err != nil {
		logger.Warn("http server shutdown error", logger.Fields{"err": err.Error()})
	}
	logger.Info("http server stopped")

	// 2. 停止巡检任务。
	if err := janitor.Shutdown(shutCtx); err != nil {
		logger.Warn("janitor shutdown error", logger.Fields{"err": err.Error()})
	}

	// 3. 管理服务：强制 flush + close（倒序）。
	if err := adminSvc.FlushAll(); err != nil {
		logger.Warn("admin flush-all error", logger.Fields{"err": err.Error()})
	}
	if err := adminSvc.CloseAll(); err != nil {
		logger.Warn("admin close-all error", logger.Fields{"err": err.Error()})
	}

	// 4. 等待所有 goroutine 退出。
	wg.Wait()
	if srvErr != nil {
		logger.Fatal("shutdown finished with server error", logger.Fields{"err": srvErr.Error()})
	}
	logger.Info("shutdown finished successfully")
}

// rateLimitMiddleware 把 rate.RateLimiter 封装为中间件。
// 命中限流时返回 429 + 简洁错误 JSON。
func rateLimitMiddleware(l *rate.RateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ok, reason := l.Allow(r)
			if !ok {
				w.Header().Set("X-RateLimit-Reason", reason)
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":"rate limit exceeded","reason":"` + reason + `"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// registerStatic 注册前端静态资源路由（从 web 包嵌入）。
func registerStatic(mux *http.ServeMux) {
	sub, err := fs.Sub(web.Static, "static")
	if err != nil {
		logger.Fatal("failed to sub static fs", logger.Fields{"err": err.Error()})
	}
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))
	// 另暴露 /favicon.ico
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
}

// makeRootHandler 构造根路径处理器：
//   - 若路径为 / 或 /index.html，返回前端首页。
//   - 否则交给 RedirectHandler 按短码处理。
func makeRootHandler(rdH *handler.RedirectHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" || path == "index.html" {
			data, err := fs.ReadFile(web.Static, "static/index.html")
			if err != nil {
				http.Error(w, "failed to read index.html: "+err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(data)
			return
		}
		if handled := rdH.RedirectRoot(w, r, path); handled {
			return
		}
		// 未处理 => 404。
		http.NotFound(w, r)
	}
}

// 确保 fmt 等导入不被未使用（某些情况下用不到时也能通过编译）。
var _ = fmt.Sprintf
