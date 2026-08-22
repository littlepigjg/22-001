package handler

import (
	"net/http"
	"strconv"
	"strings"

	"shurl/pkg/logger"
)

// AdvancedCORSConfig 配置高级 CORS 中间件（增强版，严格源匹配 + 凭据 + 自定义头）。
type AdvancedCORSConfig struct {
	AllowOrigins     []string
	AllowMethods     []string
	AllowHeaders     []string
	ExposeHeaders    []string
	AllowCredentials bool
	MaxAge           int
	StrictOrigin     bool
}

// DefaultAdvancedCORS 默认配置：允许全部来源，适合公开 API。
func DefaultAdvancedCORS() AdvancedCORSConfig {
	return AdvancedCORSConfig{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"},
		AllowHeaders:     []string{"Accept", "Content-Type", "Authorization", "X-Requested-With"},
		ExposeHeaders:    []string{"X-Request-ID", "X-RateLimit-Limit", "X-RateLimit-Remaining"},
		AllowCredentials: false,
		MaxAge:           600,
		StrictOrigin:     false,
	}
}

// AdvancedCORS 是带严格选项的 CORS wrapper。
type AdvancedCORS struct {
	cfg      AdvancedCORSConfig
	allowAll bool
}

// NewAdvancedCORS 创建实例。
func NewAdvancedCORS(cfg AdvancedCORSConfig) *AdvancedCORS {
	allowAll := false
	for _, o := range cfg.AllowOrigins {
		if o == "*" {
			allowAll = true
			break
		}
	}
	if len(cfg.AllowMethods) == 0 {
		cfg.AllowMethods = DefaultAdvancedCORS().AllowMethods
	}
	if len(cfg.AllowHeaders) == 0 {
		cfg.AllowHeaders = DefaultAdvancedCORS().AllowHeaders
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = 600
	}
	return &AdvancedCORS{cfg: cfg, allowAll: allowAll}
}

// Wrap 把 next 包装为 CORS handler。
func (m *AdvancedCORS) Wrap(next http.Handler) http.Handler {
	if m == nil {
		return next
	}
	methods := strings.Join(m.cfg.AllowMethods, ", ")
	headers := strings.Join(m.cfg.AllowHeaders, ", ")
	expose := strings.Join(m.cfg.ExposeHeaders, ", ")
	maxAge := strconv.Itoa(m.cfg.MaxAge)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		allowed := m.isOriginAllowed(origin)
		if !allowed {
			if m.cfg.StrictOrigin {
				logger.Warn("advanced_cors: origin rejected strict", logger.Fields{"origin": origin})
				http.Error(w, `{"error":"cors origin not allowed"}`, http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		respOrigin := origin
		if m.allowAll && !m.cfg.AllowCredentials {
			respOrigin = "*"
		}
		w.Header().Set("Access-Control-Allow-Origin", respOrigin)
		if m.cfg.AllowCredentials {
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		if expose != "" {
			w.Header().Set("Access-Control-Expose-Headers", expose)
		}
		w.Header().Set("Vary", "Origin")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", methods)
			w.Header().Set("Access-Control-Allow-Headers", headers)
			w.Header().Set("Access-Control-Max-Age", maxAge)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (m *AdvancedCORS) isOriginAllowed(origin string) bool {
	if m.allowAll {
		return true
	}
	orig := strings.TrimRight(strings.ToLower(origin), "/")
	for _, o := range m.cfg.AllowOrigins {
		candidate := strings.TrimRight(strings.ToLower(o), "/")
		if orig == candidate {
			return true
		}
	}
	return false
}
