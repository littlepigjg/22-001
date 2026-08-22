package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"shurl/internal/model"
	"shurl/internal/service"
	"shurl/pkg/httperr"
	"shurl/pkg/logger"
	"shurl/pkg/response"
)

type RedirectHandler struct {
	svc *service.RedirectService
}

func NewRedirectHandler(svc *service.RedirectService) (*RedirectHandler, error) {
	if svc == nil {
		return nil, model.ErrStoreNotReady
	}
	return &RedirectHandler{svc: svc}, nil
}

func (h *RedirectHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/s/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			response.Fail(w, http.StatusMethodNotAllowed, response.CodeBadReq, "method not allowed")
			return
		}
		code := strings.TrimPrefix(r.URL.Path, "/s/")
		h.Redirect(w, r, code)
	})
}

func (h *RedirectHandler) RedirectRoot(w http.ResponseWriter, r *http.Request, code string) bool {
	if code == "" || code == "/" {
		return false
	}
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

func (h *RedirectHandler) Redirect(w http.ResponseWriter, r *http.Request, code string) {
	code = strings.TrimSpace(code)
	if err := model.ValidateCode(code); err != nil {
		response.BadRequest(w, err.Error())
		return
	}
	result, err := h.svc.HandleRedirect(r.Context(), &service.RedirectRequest{
		Code:       code,
		RemoteAddr: r.RemoteAddr,
		Headers:    buildHeaders(r),
		Timestamp:  time.Now(),
	})
	if err != nil {
		logger.CtxWarn(r.Context(), "redirect internal error", logger.Fields{"err": err.Error(), "code": code})
		httperr.Map(w, err)
		return
	}
	setCommonRedirectHeaders(w, result)
	switch result.Status {
	case http.StatusFound:
		w.Header().Set("Location", result.RawURL)
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

func buildHeaders(r *http.Request) map[string][]string {
	if r == nil || r.Header == nil {
		return map[string][]string{}
	}
	out := make(map[string][]string, len(r.Header))
	for k, v := range r.Header {
		out[k] = append([]string(nil), v...)
	}
	out["X-Code"] = []string{r.URL.Path}
	out["X-Forwarded-Host"] = []string{r.Host}
	return out
}

func setCommonRedirectHeaders(w http.ResponseWriter, res *service.RedirectResult) {
	if w == nil || res == nil {
		return
	}
	h := w.Header()
	h.Set("X-Redirect-Status", strconv.Itoa(res.Status))
	if res.Expired {
		h.Set("X-Reason", "expired")
	}
	if res.Disabled {
		h.Set("X-Reason", "disabled")
	}
	if res.MaxVisited {
		h.Set("X-Reason", "max_visited")
	}
}

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
