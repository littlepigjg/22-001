// Package handler 提供 HTTP 请求处理器与通用中间件。
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"shurl/pkg/idgen"
	"shurl/pkg/logger"
	"shurl/pkg/response"
)

// ctxReqIDKey 是 context 中保存 request_id 的键类型。
type ctxReqIDKey struct{}

var reqIDKey = ctxReqIDKey{}

// RequestID 从 context 中取出请求 ID。
func RequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(reqIDKey).(string)
	return v
}

// WithRequestID 将请求 ID 放入 context。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, reqIDKey, id)
}

// RequestIDMiddleware 为每个请求注入唯一的 request_id：
// 1. 写入 context（键 reqIDKey）
// 2. 写入响应头 X-Request-ID
// 3. 写入 logger 字段，方便追踪。
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = idgen.NewString()
		}
		w.Header().Set("X-Request-ID", id)
		ctx := WithRequestID(r.Context(), id)
		ctx = logger.Context(ctx, logger.Fields{"req_id": id})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// LoggingMiddleware 记录每个 HTTP 请求的基本信息（方法、路径、耗时、状态码等）。
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &recordedResponseWriter{ResponseWriter: w, status: 0}
		defer func() {
			// 保证记录到响应体大小与状态码。
			elapsed := time.Since(start)
			logger.CtxInfo(r.Context(), "http request", logger.Fields{
				"method":   r.Method,
				"path":     r.URL.Path,
				"query":    r.URL.RawQuery,
				"remote":   r.RemoteAddr,
				"status":   rw.status,
				"bytes":    rw.written,
				"elapsed":  elapsed.String(),
				"duration": elapsed.Milliseconds(),
			})
		}()
		next.ServeHTTP(rw, r)
	})
}

// RecoveryMiddleware 捕获后续 handler 中的 panic，记录日志并返回 500。
func RecoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				stack := string(debug.Stack())
				logger.CtxError(r.Context(), "panic recovered", logger.Fields{
					"panic": fmt.Sprintf("%v", rec),
					"stack": stack,
				})
				response.Internal(w, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// BodyLimitMiddleware 限制请求体大小，超过直接返回 413。
func BodyLimitMiddleware(maxBytes int64) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if maxBytes <= 0 {
				next.ServeHTTP(w, r)
				return
			}
			// 仅对可能有 body 的方法限制。
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch:
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CORSMiddleware 提供简单的跨域支持（用于开发环境与前端页面）。
func CORSMiddleware(allowOrigins []string) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			allow := ""
			if len(allowOrigins) == 0 {
				allow = "*"
			} else {
				for _, o := range allowOrigins {
					if o == "*" || o == origin {
						allow = o
						break
					}
				}
			}
			if allow != "" {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", allow)
				h.Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS,PATCH,HEAD")
				h.Set("Access-Control-Allow-Headers", "Content-Type,Authorization,X-Request-ID")
				h.Set("Access-Control-Expose-Headers", "X-Request-ID,Location")
				h.Set("Access-Control-Max-Age", "86400")
				if allow != "*" {
					h.Set("Vary", "Origin")
				}
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// recordedResponseWriter 包装 ResponseWriter，记录写入状态与字节数。
type recordedResponseWriter struct {
	http.ResponseWriter
	status  int
	written int64
	wrote   bool
}

// WriteHeader 记录状态码。
func (r *recordedResponseWriter) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
		r.ResponseWriter.WriteHeader(code)
	}
}

// Write 写入响应体并累加字节数；若之前未写状态码，默认写 200。
func (r *recordedResponseWriter) Write(p []byte) (int, error) {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(p)
	r.written += int64(n)
	return n, err
}

// DecodeJSON 将请求体 JSON 解码到 v。
// 限制最大读取量避免内存问题，并对 EOF / 语法错误等做统一处理。
func DecodeJSON(r *http.Request, v any) error {
	if r == nil || r.Body == nil {
		return errors.New("handler: empty request body")
	}
	defer func() { _ = r.Body.Close() }()
	// 先做一段限制，避免超大 body（若中间件已限制，这里是双保险）。
	body := io.LimitReader(r.Body, 2<<20)
	data, err := io.ReadAll(body)
	if err != nil {
		// MaxBytesReader 超过的错误。
		if merr, ok := err.(*http.MaxBytesError); ok {
			_ = merr
			return fmt.Errorf("request body too large (limit %d bytes)", merr.Limit)
		}
		return fmt.Errorf("invalid request body: %w", err)
	}
	if len(data) == 0 {
		return errors.New("empty request body")
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var syntax *json.SyntaxError
		var unmarshal *json.UnmarshalTypeError
		switch {
		case errors.As(err, &syntax):
			return fmt.Errorf("invalid JSON syntax (offset %d)", syntax.Offset)
		case errors.As(err, &unmarshal):
			return fmt.Errorf("invalid field %s: expected type %s", unmarshal.Field, unmarshal.Type.String())
		default:
			return fmt.Errorf("invalid JSON: %w", err)
		}
	}
	return nil
}

// ParseIntQuery 从 URL 查询参数中解析一个 int 值（可选）。
func ParseIntQuery(r *http.Request, key string, def int) int {
	if r == nil {
		return def
	}
	s := strings.TrimSpace(r.URL.Query().Get(key))
	if s == "" {
		return def
	}
	n := 0
	neg := false
	i := 0
	if s[0] == '-' {
		neg = true
		i = 1
	}
	for ; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	if neg {
		n = -n
	}
	return n
}

// TrimLastSlashMiddleware 将路径末尾的斜杠（除根路径外）去掉后继续路由。
func TrimLastSlashMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if len(p) > 1 && strings.HasSuffix(p, "/") {
			r2 := new(http.Request)
			*r2 = *r
			u2 := *r.URL
			u2.Path = strings.TrimRight(p, "/")
			if u2.Path == "" {
				u2.Path = "/"
			}
			r2.URL = &u2
			next.ServeHTTP(w, r2)
			return
		}
		next.ServeHTTP(w, r)
	})
}
