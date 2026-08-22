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
	"shurl/pkg/stopctrl"
	"shurl/web"
)

var Version = "dev"
var BuildTime = ""
var CommitID = ""

func runServer() error {
	conf := config.Load()

	flag.StringVar(&conf.Server.Addr, "addr", conf.Server.Addr, "listen address")
	flag.StringVar(&conf.Log.Level, "log-level", conf.Log.Level, "log level (DEBUG/INFO/WARN/ERROR/FATAL)")
	flag.BoolVar(&conf.Storage.FlushOnWrite, "flush", conf.Storage.FlushOnWrite, "flush storage on every write")
	flag.Parse()

	logger.SetLevel(logger.ParseLevel(conf.Log.Level))
	logger.Info("shurl starting", logger.Fields{
		"addr":      conf.Server.Addr,
		"logLevel":  conf.Log.Level,
		"version":   Version,
		"buildTime": BuildTime,
		"commit":    CommitID,
	})

	rootCtx, rootCancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer rootCancel()

	stopGroup := stopctrl.New(rootCtx)

	urlStore, err := store.NewURLStore(conf)
	if err != nil {
		return fmt.Errorf("create url store failed: %w", err)
	}
	if err := urlStore.Load(rootCtx); err != nil {
		return fmt.Errorf("load url store failed: %w", err)
	}
	logStore, err := store.NewAccessLogStore(conf)
	if err != nil {
		return fmt.Errorf("create log store failed: %w", err)
	}
	if err := logStore.Open(rootCtx); err != nil {
		return fmt.Errorf("open log store failed: %w", err)
	}

	rsv, err := resolver.New(resolver.Config{
		Store:      urlStore,
		CacheCap:   4096,
		CacheTTL:   10 * time.Minute,
		WarmOnBoot: true,
	})
	if err != nil {
		return fmt.Errorf("create resolver failed: %w", err)
	}

	urlSvc, err := service.NewURLService(conf, urlStore)
	if err != nil {
		return fmt.Errorf("create url service failed: %w", err)
	}
	rdSvc, err := service.NewRedirectService(urlStore, logStore)
	if err != nil {
		return fmt.Errorf("create redirect service failed: %w", err)
	}
	stSvc, err := service.NewStatsService(conf, urlStore, logStore)
	if err != nil {
		return fmt.Errorf("create stats service failed: %w", err)
	}
	healthSvc, err := service.NewHealthService(urlStore, logStore)
	if err != nil {
		return fmt.Errorf("create health service failed: %w", err)
	}
	janitor, err := service.NewJanitorService(conf, urlStore)
	if err != nil {
		return fmt.Errorf("create janitor service failed: %w", err)
	}
	if err := janitor.Start(rootCtx); err != nil {
		return fmt.Errorf("start janitor failed: %w", err)
	}

	rlCfg := rate.DefaultConfig()
	limiter, err := rate.New(rlCfg, nil)
	if err != nil {
		return fmt.Errorf("create rate limiter failed: %w", err)
	}

	metricsSvc := metrics.New(nil, nil)
	metricsSvc.SetExtra("version", Version)
	metricsSvc.SetExtra("build_time", BuildTime)
	metricsSvc.SetExtra("commit_id", CommitID)
	metricsSvc.SetExtra("listen_addr", conf.Server.Addr)
	metricsSvc.SetExtra("url_file", conf.Storage.URLFilePath)
	metricsSvc.SetExtra("access_file", conf.Storage.LogFilePath)
	metricsSvc.Registry().GetOrCreateCounter("requests_total", "").Inc()
	metricsSvc.Registry().GetOrCreateGauge("info", "version="+Version).Set(1)

	adminSvc := admin.New(nil)
	adminSvc.RegisterFlusher("url_store", urlStore)
	adminSvc.RegisterSyncer("access_log", logStore)
	adminSvc.RegisterCloser("url_store", urlStore)
	adminSvc.RegisterCloser("access_log", logStore)
	adminSvc.SetMeta("version", Version)
	adminSvc.SetMeta("commit", CommitID)
	adminSvc.SetMeta("resolver_stats", func() any { return rsv.Stats() })
	adminSvc.SetMeta("rate_stats", limiter.Stats())

	urlH, err := handler.NewURLHandler(urlSvc)
	if err != nil {
		return fmt.Errorf("build url handler failed: %w", err)
	}
	rdH, err := handler.NewRedirectHandler(rdSvc)
	if err != nil {
		return fmt.Errorf("build redirect handler failed: %w", err)
	}
	stH, err := handler.NewStatsHandler(stSvc)
	if err != nil {
		return fmt.Errorf("build stats handler failed: %w", err)
	}
	hh, err := handler.NewHealthHandler(healthSvc)
	if err != nil {
		return fmt.Errorf("build health handler failed: %w", err)
	}
	adminH := handler.NewAdminHandler(adminSvc)
	metricsH := handler.NewMetricsHandler(metricsSvc)

	mux := http.NewServeMux()
	registerStatic(mux)
	hh.Register(mux)
	urlH.Register(mux)
	stH.Register(mux)
	rdH.Register(mux)
	adminH.RegisterRoutes(mux, "/api/admin")
	metricsH.RegisterRoutes(mux, "/api")
	mux.HandleFunc("/", makeRootHandler(rdH))

	var h http.Handler = mux
	h = rateLimitMiddleware(limiter)(h)
	h = handler.CORSMiddleware([]string{"*"})(h)
	_ = handler.NewAdvancedCORS(handler.DefaultAdvancedCORS())
	h = handler.BodyLimitMiddleware(conf.Server.MaxBodyBytes)(h)
	h = handler.LoggingMiddleware(h)
	h = handler.RequestIDMiddleware(h)
	h = handler.RecoveryMiddleware(h)
	h = handler.TrimLastSlashMiddleware(h)

	srv := &http.Server{
		Addr:         conf.Server.Addr,
		Handler:      h,
		ReadTimeout:  conf.Server.ReadTimeout,
		WriteTimeout: conf.Server.WriteTimeout,
		IdleTimeout:  conf.Server.IdleTimeout,
	}

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
			rootCancel()
		}
	}()

	<-rootCtx.Done()
	logger.Info("shutdown signal received, begin graceful shutdown")
	shutdownTimeout := conf.Server.ShutdownTimeout
	if shutdownTimeout <= 0 {
		shutdownTimeout = 10 * time.Second
	}
	shutCtx, shutCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutCancel()

	if err := srv.Shutdown(shutCtx); err != nil {
		logger.Warn("http server shutdown error", logger.Fields{"err": err.Error()})
	}
	logger.Info("http server stopped")

	if err := janitor.Shutdown(shutCtx); err != nil {
		logger.Warn("janitor shutdown error", logger.Fields{"err": err.Error()})
	}

	stopGroup.OnStop("admin_flush_all", func() error {
		return adminSvc.FlushAll()
	})
	stopGroup.OnStop("admin_close_all", func() error {
		return adminSvc.CloseAll()
	})
	stopGroup.OnStop("admin_shutdown_group", func() error {
		return adminSvc.ShutdownAll(0)
	})

	if stopErr := stopGroup.Stop(shutdownTimeout); stopErr != nil {
		logger.Warn("stopctrl group stop error", logger.Fields{"err": stopErr.Error()})
	}

	wg.Wait()
	if srvErr != nil {
		return fmt.Errorf("shutdown finished with server error: %w", srvErr)
	}
	logger.Info("shutdown finished successfully")
	return nil
}

func main() {
	if err := runServer(); err != nil {
		logger.Fatal("server exited with error", logger.Fields{"err": err.Error()})
	}
}

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

func registerStatic(mux *http.ServeMux) {
	sub, err := fs.Sub(web.Static, "static")
	if err != nil {
		logger.Fatal("failed to sub static fs", logger.Fields{"err": err.Error()})
	}
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
}

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
		http.NotFound(w, r)
	}
}

var _ = fmt.Sprintf
