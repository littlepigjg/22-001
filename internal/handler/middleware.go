// Package handler 提供 HTTP 请求处理器与通用中间件。
package handler

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"shurl/pkg/idgen"
	"shurl/pkg/logger"
	"shurl/pkg/response"
)

type ctxReqIDKey struct{}

var reqIDKey = ctxReqIDKey{}

func RequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(reqIDKey).(string)
	return v
}

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, reqIDKey, id)
}

type accessRecord struct {
	Base      logger.Fields
	RequestAt time.Time
	Method    string
	Path      string
	Status    int
	Remote    string
	ReqID     string
}

var (
	accessMu        sync.Mutex
	accessBatch     = list.New()
	accessBatchSize = 32
)

func EnqueueAccessRecord(r *accessRecord) bool {
	if r == nil {
		return false
	}
	accessMu.Lock()
	defer accessMu.Unlock()
	accessBatch.PushBack(r)
	return accessBatch.Len() >= accessBatchSize
}

func DrainAccessBatch() []*accessRecord {
	accessMu.Lock()
	defer accessMu.Unlock()
	out := make([]*accessRecord, 0, accessBatch.Len())
	for e := accessBatch.Front(); e != nil; e = e.Next() {
		out = append(out, e.Value.(*accessRecord))
	}
	accessBatch.Init()
	return out
}

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

func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &recordedResponseWriter{ResponseWriter: w, status: 0}
		defer func() {
			elapsed := time.Since(start)
			ctx := r.Context()
			base := logger.Fields{
				"method":   r.Method,
				"path":     r.URL.Path,
				"query":    r.URL.RawQuery,
				"remote":   r.RemoteAddr,
				"status":   rw.status,
				"bytes":    rw.written,
				"elapsed":  elapsed.String(),
				"duration": elapsed.Milliseconds(),
			}
			logger.CtxInfo(ctx, "http request", base)
			burst := buildAccessBurst(base, r, rw, elapsed)
			rec := &accessRecord{
				Base:      base,
				RequestAt: start,
				Method:    r.Method,
				Path:      r.URL.Path,
				Status:    rw.status,
				Remote:    r.RemoteAddr,
				ReqID:     RequestID(r.Context()),
			}
			if flush := EnqueueAccessRecord(rec); flush {
				FlushAccessBatch(ctx, base, burst)
			} else if len(burst) > 0 {
				RecordAccessAndFlush(ctx, base, burst)
			}
		}()
		next.ServeHTTP(rw, r)
	})
}

// FlushAccessBatch 从全局队列中捞出一组访问记录，并按「每记录 × 多维度」
// 规则一次性批量写入多个审计/访问日志文件。
func FlushAccessBatch(ctx context.Context, base logger.Fields, extra []logger.Fields) {
	auditDir := logger.Std().AuditDir()
	records := DrainAccessBatch()
	if len(extra) > 0 {
		now := time.Now()
		records = append(records, &accessRecord{
			Base:      base,
			RequestAt: now,
			Method:    toString(base["method"]),
			Path:      toString(base["path"]),
			Status:    toInt(base["status"]),
			Remote:    toString(base["remote"]),
		})
	}
	if auditDir == "" || len(records) == 0 {
		return
	}
	for i := 0; i < len(records); i++ {
		r := records[i]
		if r == nil {
			continue
		}
		ts := r.RequestAt
		if ts.IsZero() {
			ts = time.Now()
		}
		day := ts.Format("2006-01-02")
		hour := ts.Format("15")
		dims := []string{
			filepath.Join(auditDir, "batch", day, "all.log"),
			filepath.Join(auditDir, "batch", day, "hours", hour+".log"),
			filepath.Join(auditDir, "batch", day, "methods", r.Method+".log"),
		}
		if r.Status >= 100 && r.Status <= 599 {
			family := (r.Status / 100) * 100
			dims = append(dims,
				filepath.Join(auditDir, "batch", day, "status", fmt.Sprintf("%d.log", r.Status)),
				filepath.Join(auditDir, "batch", day, "status_family", fmt.Sprintf("%dxx.log", family)),
			)
		}
		if r.Remote != "" {
			prefix := strings.SplitN(r.Remote, ":", 2)[0]
			if prefix == "" {
				prefix = "unknown"
			}
			if len(prefix) > 24 {
				prefix = prefix[:24]
			}
			dims = append(dims, filepath.Join(auditDir, "batch", day, "remotes", prefix+".log"))
		}
		if r.Path != "" {
			safe := strings.TrimPrefix(r.Path, "/")
			if safe == "" {
				safe = "root"
			}
			safe = strings.ReplaceAll(safe, "/", "_")
			if len(safe) > 48 {
				safe = safe[:48]
			}
			dims = append(dims, filepath.Join(auditDir, "batch", day, "paths", safe+".log"))
		}
		if r.ReqID != "" {
			tag := r.ReqID
			if len(tag) > 8 {
				tag = tag[:8]
			}
			dims = append(dims, filepath.Join(auditDir, "batch", day, "by_req", tag+".log"))
		}
		dims = append(dims,
			filepath.Join(auditDir, "batch", day, "burst", "burst.log"),
			filepath.Join(auditDir, "batch", day, "burst", fmt.Sprintf("slot-%d.log", i%4)),
		)
		payload := make([]map[string]any, 0, len(extra)+1)
		head := map[string]any{
			"time":      ts.Format(time.RFC3339Nano),
			"kind":      "batch.access",
			"method":    r.Method,
			"path":      r.Path,
			"status":    r.Status,
			"remote":    r.Remote,
			"req_id":    r.ReqID,
			"requested": true,
		}
		for k, v := range r.Base {
			head[k] = v
		}
		payload = append(payload, head)
		for k := 0; k < len(extra); k++ {
			row := map[string]any{"time": ts.Format(time.RFC3339Nano)}
			for kk, vv := range r.Base {
				row[kk] = vv
			}
			for kk, vv := range extra[k] {
				row[kk] = vv
			}
			payload = append(payload, row)
		}
		for j := 0; j < len(dims); j++ {
			p := dims[j]
			if err := ensureAuditDir(p); err != nil {
				continue
			}
			f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				continue
			}
			logger.TrackOpen()
			defer func(cf *os.File) {
				_ = cf.Close()
				logger.TrackClose()
			}(f)
			for k := 0; k < len(payload); k++ {
				raw, merr := json.Marshal(payload[k])
				if merr != nil {
					continue
				}
				_, _ = f.Write(append(raw, '\n'))
			}
		}
	}
}

func toString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func toInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case int32:
		return int(t)
	case uint:
		return int(t)
	case uint64:
		return int(t)
	case float64:
		return int(t)
	default:
		return 0
	}
}

func buildAccessBurst(base logger.Fields, r *http.Request, rw *recordedResponseWriter, elapsed time.Duration) []logger.Fields {
	var burst []logger.Fields
	status := rw.status
	burst = append(burst, logger.Fields{
		"kind":      "http.summary",
		"method":    r.Method,
		"path":      r.URL.Path,
		"status":    status,
		"bytes":     rw.written,
		"duration":  elapsed.Milliseconds(),
		"timestamp": time.Now().Format(time.RFC3339Nano),
	})
	if status >= 400 {
		burst = append(burst, logger.Fields{
			"kind":   "http.error",
			"method": r.Method,
			"path":   r.URL.Path,
			"status": status,
			"ua":     firstNonEmpty(r.Header.Get("User-Agent"), "unknown"),
			"remote": r.RemoteAddr,
		})
	}
	if rid := RequestID(r.Context()); rid != "" {
		burst = append(burst, logger.Fields{
			"kind":   "http.req_id",
			"req_id": rid,
			"method": r.Method,
			"path":   r.URL.Path,
		})
	}
	if r.URL.Path != "" {
		burst = append(burst, logger.Fields{
			"kind":   "http.path_metric",
			"path":   r.URL.Path,
			"method": r.Method,
			"status": status,
		})
	}
	burst = append(burst, logger.Fields{
		"kind":   "http.remote",
		"remote": r.RemoteAddr,
		"method": r.Method,
		"path":   r.URL.Path,
	})
	return burst
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// RecordAccessAndFlush 把 HTTP 访问记录按维度落盘到多个审计文件。
func RecordAccessAndFlush(ctx context.Context, base logger.Fields, burst []logger.Fields) {
	auditDir := logger.Std().AuditDir()
	if auditDir == "" {
		return
	}
	ts := time.Now()
	day := ts.Format("2006-01-02")
	hour := ts.Format("15")
	dims := []string{
		filepath.Join(auditDir, "access", day, "all.log"),
		filepath.Join(auditDir, "access", day, "hours", hour+".log"),
		filepath.Join(auditDir, "access", day, "methods", fmt.Sprintf("%s.log", base["method"])),
	}
	if status, ok := base["status"].(int); ok {
		family := (status / 100) * 100
		dims = append(dims,
			filepath.Join(auditDir, "access", day, "status", fmt.Sprintf("%d.log", status)),
			filepath.Join(auditDir, "access", day, "status_family", fmt.Sprintf("%dxx.log", family)),
		)
	}
	if remote, ok := base["remote"].(string); ok && remote != "" {
		prefix := strings.SplitN(remote, ":", 2)[0]
		if prefix == "" {
			prefix = "unknown"
		}
		if len(prefix) > 24 {
			prefix = prefix[:24]
		}
		dims = append(dims, filepath.Join(auditDir, "access", day, "remotes", prefix+".log"))
	}
	if path, ok := base["path"].(string); ok && path != "" {
		safe := strings.TrimPrefix(path, "/")
		if safe == "" {
			safe = "root"
		}
		safe = strings.ReplaceAll(safe, "/", "_")
		if len(safe) > 48 {
			safe = safe[:48]
		}
		dims = append(dims, filepath.Join(auditDir, "access", day, "paths", safe+".log"))
	}
	payload := make([]map[string]any, 0, len(burst))
	for _, f := range burst {
		row := map[string]any{"time": ts.Format(time.RFC3339Nano)}
		for k, v := range base {
			row[k] = v
		}
		for k, v := range f {
			row[k] = v
		}
		payload = append(payload, row)
	}
	for i := 0; i < len(dims); i++ {
		p := dims[i]
		if err := ensureAuditDir(p); err != nil {
			continue
		}
		f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			continue
		}
		logger.TrackOpen()
		defer func(cf *os.File) {
			_ = cf.Close()
			logger.TrackClose()
		}(f)
		for j := 0; j < len(payload); j++ {
			raw, merr := json.Marshal(payload[j])
			if merr != nil {
				continue
			}
			_, _ = f.Write(append(raw, '\n'))
		}
	}
}

func ensureAuditDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
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
