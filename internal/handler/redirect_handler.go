package handler

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"shurl/internal/model"
	"shurl/internal/service"
	"shurl/pkg/httperr"
	"shurl/pkg/logger"
	"shurl/pkg/response"
)

// RedirectHandler 处理短码访问请求。
type RedirectHandler struct {
	svc *service.RedirectService
}

// NewRedirectHandler 构造 RedirectHandler。
func NewRedirectHandler(svc *service.RedirectService) (*RedirectHandler, error) {
	if svc == nil {
		return nil, model.ErrStoreNotReady
	}
	return &RedirectHandler{svc: svc}, nil
}

// Register 在 mux 上注册根路径下的短码路由（使用根路径 /{code}）。
// 同时保留 /s/{code} 的兼容路径。
func (h *RedirectHandler) Register(mux *http.ServeMux) {
	// /s/{code}
	mux.HandleFunc("/s/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			response.Fail(w, http.StatusMethodNotAllowed, response.CodeBadReq, "method not allowed")
			return
		}
		code := strings.TrimPrefix(r.URL.Path, "/s/")
		h.Redirect(w, r, code)
	})
}

// RedirectRoot 用于根路径处理（在 main 中使用）。
// 若 code 为空，返回 false 让调用方继续处理（例如返回首页）。
func (h *RedirectHandler) RedirectRoot(w http.ResponseWriter, r *http.Request, code string) bool {
	if code == "" || code == "/" {
		return false
	}
	// 排除以 /api /static /health /ready /favicon 开头的路径。
	if strings.HasPrefix(code, "api/") ||
		strings.HasPrefix(code, "static/") ||
		strings.HasPrefix(code, "health") ||
		strings.HasPrefix(code, "ready") ||
		strings.HasPrefix(code, "favicon.ico") {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		response.Fail(w, http.StatusMethodNotAllowed, response.CodeBadReq, "method not allowed")
		return true
	}
	h.Redirect(w, r, code)
	return true
}

// Redirect 实际执行重定向处理。
func (h *RedirectHandler) Redirect(w http.ResponseWriter, r *http.Request, code string) {
	code = strings.TrimSpace(code)
	if err := model.ValidateCode(code); err != nil {
		response.BadRequest(w, err.Error())
		return
	}
	result, err := h.svc.HandleRedirect(r.Context(), &service.RedirectRequest{
		Code:       code,
		RemoteAddr: r.RemoteAddr,
		Headers:    r.Header,
		Timestamp:  time.Now(),
	})
	if err != nil {
		logger.CtxWarn(r.Context(), "redirect internal error", logger.Fields{"err": err.Error(), "code": code})
		httperr.Map(w, err)
		return
	}
	switch result.Status {
	case http.StatusFound: // 302
		w.Header().Set("Location", result.RawURL)
		// 防缓存：过期链接可能失效。
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		w.WriteHeader(http.StatusFound)
		_, _ = w.Write([]byte(`<a href="` + htmlEscape(result.RawURL) + `">Redirecting...</a>`))
		return
	case http.StatusGone:
		msg := "short link has expired or disabled"
		switch {
		case result.Disabled:
			msg = "short link has been disabled"
		case result.MaxVisited:
			msg = "short link visits exceeded"
		case result.Expired:
			msg = "short link has expired"
		}
		response.Fail(w, http.StatusGone, response.CodeExpired, msg)
		return
	case http.StatusNotFound:
		response.NotFound(w, "short code not found")
		return
	default:
		httperr.Map(w, errors.New("unknown redirect status"))
	}
}

// htmlEscape 用于在 HTML 中安全展示 URL。
func htmlEscape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString("&quot;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '&':
			b.WriteString("&amp;")
		case '\'':
			b.WriteString("&#39;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
