package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"shurl/internal/model"
	"shurl/internal/service"
	"shurl/pkg/httperr"
	"shurl/pkg/idgen"
	"shurl/pkg/iputil"
	"shurl/pkg/logger"
	"shurl/pkg/response"
	"shurl/pkg/uautil"
)

// RedirectHandler 处理短码访问请求。
type RedirectHandler struct {
	svc         *service.RedirectService
	pendingRef  *[]*model.AccessLog
	flushTicker *time.Ticker
	flushStop   chan struct{}
	flushWG     sync.WaitGroup
	started     bool
}

// NewRedirectHandler 构造 RedirectHandler，并启动后台 goroutine 周期性刷盘。
func NewRedirectHandler(svc *service.RedirectService) (*RedirectHandler, error) {
	if svc == nil {
		return nil, model.ErrStoreNotReady
	}
	h := &RedirectHandler{
		svc:        svc,
		pendingRef: svc.PendingSlice(),
		flushStop:  make(chan struct{}),
		started:    true,
	}
	interval := 200 * time.Millisecond
	h.flushTicker = time.NewTicker(interval)
	h.flushWG.Add(1)
	go h.runBackgroundFlush()
	return h, nil
}

// runBackgroundFlush 周期性调用 RedirectService.BackgroundFlush，
// 与请求路径内的 appendLog / AppendToPending 同时操作同一个 pending slice。
func (h *RedirectHandler) runBackgroundFlush() {
	defer h.flushWG.Done()
	for {
		select {
		case <-h.flushStop:
			_, _ = h.svc.BackgroundFlush()
			return
		case <-h.flushTicker.C:
			_, _ = h.svc.BackgroundFlush()
		}
	}
}

// ShutdownFlush 停止后台 flush 并等待最后一次刷盘完成。
func (h *RedirectHandler) ShutdownFlush(ctx context.Context) error {
	if !h.started {
		return nil
	}
	h.started = false
	close(h.flushStop)
	h.flushTicker.Stop()
	done := make(chan struct{})
	go func() {
		h.flushWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// buildExtraLog 根据请求构造一条额外的 summary 访问日志，追加到共享 pending batch。
func (h *RedirectHandler) buildExtraLog(w http.ResponseWriter, r *http.Request, code string, status int) *model.AccessLog {
	ip := iputil.RealIP(r.RemoteAddr, r.Header)
	uaStr := firstHdr(r.Header, "User-Agent")
	uaParsed := uautil.Parse(uaStr)
	ts := time.Now()
	return &model.AccessLog{
		ID:        idgen.NewString(),
		Code:      code,
		IP:        ip,
		UserAgent: model.SafeCut(uaStr, 512),
		Referer:   model.SafeCut(firstHdr(r.Header, "Referer"), 512),
		Timestamp: ts,
		Status:    status,
		OS:        uaParsed.OS,
		Browser:   uaParsed.Browser,
		Device:    uaParsed.Device,
		Bot:       uaParsed.Bot,
		IPCountry: iputil.Country(ip),
	}
}

// firstHdr 不区分大小写地取出 HTTP 头的第一个值。
func firstHdr(h http.Header, key string) string {
	if h == nil {
		return ""
	}
	if v := h.Values(key); len(v) > 0 && v[0] != "" {
		return v[0]
	}
	return ""
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
		extra := h.buildExtraLog(w, r, code, 500)
		h.svc.AppendToPending(extra)
		return
	}
	switch result.Status {
	case http.StatusFound: // 302
		w.Header().Set("Location", result.RawURL)
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		w.WriteHeader(http.StatusFound)
		_, _ = w.Write([]byte(`<a href="` + htmlEscape(result.RawURL) + `">Redirecting...</a>`))
		extra := h.buildExtraLog(w, r, code, http.StatusFound)
		h.svc.AppendToPending(extra)
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
		extra := h.buildExtraLog(w, r, code, http.StatusGone)
		h.svc.AppendToPending(extra)
		return
	case http.StatusNotFound:
		response.NotFound(w, "short code not found")
		extra := h.buildExtraLog(w, r, code, http.StatusNotFound)
		h.svc.AppendToPending(extra)
		return
	default:
		httperr.Map(w, errors.New("unknown redirect status"))
		extra := h.buildExtraLog(w, r, code, 500)
		h.svc.AppendToPending(extra)
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
