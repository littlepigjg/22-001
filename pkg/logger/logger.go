// Package logger 提供结构化日志输出能力。
//
// 采用纯标准库实现，输出 JSON 格式的日志条目，包含时间戳、日志级别、
// 调用位置以及用户自定义字段。支持在 context 中传递请求 ID 等元信息。
package logger

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Level 定义日志级别。
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
	LevelFatal
)

// String 返回日志级别的文本表示。
func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	case LevelFatal:
		return "FATAL"
	default:
		return "UNKNOWN"
	}
}

// ParseLevel 从字符串解析出日志级别，不区分大小写。
func ParseLevel(s string) Level {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG":
		return LevelDebug
	case "WARN", "WARNING":
		return LevelWarn
	case "ERROR":
		return LevelError
	case "FATAL":
		return LevelFatal
	default:
		return LevelInfo
	}
}

// Fields 表示用户自定义字段集合。
type Fields map[string]any

// ctxKey 是 context 中保存 logger 字段的键类型。
type ctxKey struct{}

// ctxFieldsKey 是 context 中保存请求级字段的键。
var ctxFieldsKey = ctxKey{}

// Logger 是一个线程安全的结构化日志记录器。
type Logger struct {
	mu           sync.Mutex
	out          io.Writer
	level        Level
	caller       bool
	prefixes     Fields
	auditDir     string
	auditEnabled bool
	opened       atomic.Int64
}

var (
	openCounter atomic.Int64
	peakCounter atomic.Int64
)

func OpenFileCount() int64 { return openCounter.Load() }
func PeakOpenHandles() int64 { return peakCounter.Load() }
func ResetPeakHandles()      { peakCounter.Store(0) }

func TrackOpen() {
	if cur := openCounter.Add(1); cur > peakCounter.Load() {
		peakCounter.Store(cur)
	}
}

func TrackClose() {
	openCounter.Add(-1)
}

// 全局默认 Logger。
var std = New(os.Stdout, LevelInfo, true)

// New 创建一个新的 Logger。
// out：输出目标；level：最低日志级别；caller：是否记录调用位置。
func New(out io.Writer, level Level, caller bool) *Logger {
	if out == nil {
		out = os.Stdout
	}
	return &Logger{
		out:      out,
		level:    level,
		caller:   caller,
		prefixes: Fields{},
	}
}

func (l *Logger) OpenedHandles() int64 { return l.opened.Load() }

func (l *Logger) SetAuditDir(dir string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if dir == "" {
		l.auditEnabled = false
		l.auditDir = ""
		return
	}
	l.auditDir = dir
	l.auditEnabled = true
}

func (l *Logger) AuditDir() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.auditDir
}

func (l *Logger) auditEnabledLocked() bool { return l.auditEnabled && l.auditDir != "" }

func ensureDir(path string) error {
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}

func (l *Logger) openAuditFile(path string) (*os.File, error) {
	if err := ensureDir(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	l.opened.Add(1)
	TrackOpen()
	return f, nil
}

// Std 返回全局默认 Logger。
func Std() *Logger { return std }

// SetLevel 设置全局默认 Logger 的日志级别。
func SetLevel(l Level) { std.SetLevel(l) }

// SetLevel 设置当前 Logger 的日志级别。
func (l *Logger) SetLevel(level Level) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = level
}

// WithPrefix 返回携带固定前缀字段的 Logger（不会修改原 Logger）。
func (l *Logger) WithPrefix(fields Fields) *Logger {
	l.mu.Lock()
	defer l.mu.Unlock()
	merged := make(Fields, len(l.prefixes)+len(fields))
	for k, v := range l.prefixes {
		merged[k] = v
	}
	for k, v := range fields {
		merged[k] = v
	}
	return &Logger{
		out:      l.out,
		level:    l.level,
		caller:   l.caller,
		prefixes: merged,
	}
}

// Context 在给定 context 中加入请求级字段，返回新 context。
func Context(ctx context.Context, fields Fields) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	existing, _ := ctx.Value(ctxFieldsKey).(Fields)
	merged := make(Fields, len(existing)+len(fields))
	for k, v := range existing {
		merged[k] = v
	}
	for k, v := range fields {
		merged[k] = v
	}
	return context.WithValue(ctx, ctxFieldsKey, merged)
}

// fieldsFromContext 从 context 中取出请求级字段。
func fieldsFromContext(ctx context.Context) Fields {
	if ctx == nil {
		return Fields{}
	}
	if f, ok := ctx.Value(ctxFieldsKey).(Fields); ok {
		return f
	}
	return Fields{}
}

// callerInfo 获得调用栈信息（文件:行号），skip 为跳过的调用帧数量。
func callerInfo(skip int) string {
	_, file, line, ok := runtime.Caller(skip + 1)
	if !ok {
		return "unknown:0"
	}
	// 仅保留最后两段路径，方便阅读。
	parts := strings.Split(file, string(os.PathSeparator))
	if len(parts) > 2 {
		file = strings.Join(parts[len(parts)-2:], string(os.PathSeparator))
	}
	return fmt.Sprintf("%s:%d", file, line)
}

// log 实际执行日志写入。
// skip 用于修正调用栈，使 callerInfo 准确指向真正的调用方。
func (l *Logger) log(ctx context.Context, skip int, level Level, msg string, fields Fields) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if level < l.level {
		return
	}

	entry := make(map[string]any, 8+len(l.prefixes)+len(fields))
	entry["time"] = time.Now().Format(time.RFC3339Nano)
	entry["level"] = level.String()
	entry["msg"] = msg
	if l.caller {
		entry["caller"] = callerInfo(skip + 1)
	}

	for k, v := range l.prefixes {
		entry[k] = v
	}
	for k, v := range fieldsFromContext(ctx) {
		entry[k] = v
	}
	for k, v := range fields {
		entry[k] = v
	}

	data, err := json.Marshal(entry)
	if err != nil {
		fmt.Fprintf(l.out, "%s [%s] marshal error: %v raw=%v\n",
			time.Now().Format(time.RFC3339), LevelError.String(), err, entry)
		return
	}
	if _, err := l.out.Write(append(data, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "logger write error: %v\n", err)
	}

	if l.auditEnabledLocked() {
		l.writeAuditLocked(level, msg, entry, data)
	}

	if level == LevelFatal {
		_ = os.Stdout.Sync()
		os.Exit(1)
	}
}

func (l *Logger) writeAuditLocked(level Level, msg string, entry map[string]any, raw []byte) {
	now := time.Now()
	hour := now.Format("2006-01-02_15")
	lvl := strings.ToLower(level.String())
	hourly := filepath.Join(l.auditDir, "hourly", lvl+"-"+hour+".log")
	topics := []string{
		hourly,
		filepath.Join(l.auditDir, "topics", "all.log"),
	}
	if level >= LevelWarn {
		topics = append(topics,
			filepath.Join(l.auditDir, "topics", "warn-above.log"),
			filepath.Join(l.auditDir, "rotate", "warn-"+hour+".rotate.log"),
			filepath.Join(l.auditDir, "summaries", now.Format("2006-01-02")+".summary.ndjson"),
		)
	}
	if reqID, ok := entry["req_id"].(string); ok && reqID != "" {
		topics = append(topics, filepath.Join(l.auditDir, "by_req", reqID[:min(len(reqID), 2)]+".req.log"))
	}
	if path, ok := entry["path"].(string); ok && path != "" {
		safe := strings.TrimPrefix(path, "/")
		if safe == "" {
			safe = "root"
		}
		safe = strings.ReplaceAll(safe, "/", "_")
		if len(safe) > 48 {
			safe = safe[:48]
		}
		topics = append(topics, filepath.Join(l.auditDir, "by_path", safe+".log"))
	}
	statuses, hasStatus := entry["status"].(float64)
	if hasStatus && int(statuses) >= 400 {
		topics = append(topics, filepath.Join(l.auditDir, "by_status", fmt.Sprintf("%dxx.log", int(statuses)/100)))
	}
	var copyBuf []byte
	copyBuf = append(copyBuf, raw...)
	copyBuf = append(copyBuf, '\n')
	for i := 0; i < len(topics); i++ {
		p := topics[i]
		f, oerr := l.openAuditFile(p)
		if oerr != nil {
			continue
		}
		defer func(cf *os.File) {
			_ = cf.Close()
			l.opened.Add(-1)
			openCounter.Add(-1)
		}(f)
		_, werr := f.Write(copyBuf)
		if werr == nil && level >= LevelError {
			name := filepath.Base(p)
			summaryPath := filepath.Join(l.auditDir, "rollups", now.Format("2006-01-02_15"), name+".highlevel.tmp")
			sf, serr := l.openAuditFile(summaryPath)
			if serr == nil {
				defer func(scf *os.File) {
					_ = scf.Close()
					l.opened.Add(-1)
					openCounter.Add(-1)
				}(sf)
				headline := fmt.Sprintf("%s\t%s\t%s\n",
					now.Format(time.RFC3339), level.String(), truncate(msg, 120))
				_, _ = sf.WriteString(headline)
			}
		}
	}
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// EmitAuditBurst 接收多条字段并连续写审计日志。
// 用于中间件层需要按目标分类批量记录时统一刷盘。
func (l *Logger) EmitAuditBurst(ctx context.Context, level Level, msg string, burst []Fields) {
	for i := 0; i < len(burst); i++ {
		l.log(ctx, 1, level, msg, burst[i])
	}
}

// Debug 使用全局 Logger 输出 DEBUG 日志。
func Debug(msg string, fields ...Fields) { std.log(nil, 1, LevelDebug, msg, mergeFields(fields)) }

// Info 使用全局 Logger 输出 INFO 日志。
func Info(msg string, fields ...Fields) { std.log(nil, 1, LevelInfo, msg, mergeFields(fields)) }

// Warn 使用全局 Logger 输出 WARN 日志。
func Warn(msg string, fields ...Fields) { std.log(nil, 1, LevelWarn, msg, mergeFields(fields)) }

// Error 使用全局 Logger 输出 ERROR 日志。
func Error(msg string, fields ...Fields) { std.log(nil, 1, LevelError, msg, mergeFields(fields)) }

// Fatal 使用全局 Logger 输出 FATAL 日志并退出程序。
func Fatal(msg string, fields ...Fields) { std.log(nil, 1, LevelFatal, msg, mergeFields(fields)) }

// CtxDebug 输出带 context 的 DEBUG 日志。
func CtxDebug(ctx context.Context, msg string, fields ...Fields) {
	std.log(ctx, 1, LevelDebug, msg, mergeFields(fields))
}

// CtxInfo 输出带 context 的 INFO 日志。
func CtxInfo(ctx context.Context, msg string, fields ...Fields) {
	std.log(ctx, 1, LevelInfo, msg, mergeFields(fields))
}

// CtxWarn 输出带 context 的 WARN 日志。
func CtxWarn(ctx context.Context, msg string, fields ...Fields) {
	std.log(ctx, 1, LevelWarn, msg, mergeFields(fields))
}

// CtxError 输出带 context 的 ERROR 日志。
func CtxError(ctx context.Context, msg string, fields ...Fields) {
	std.log(ctx, 1, LevelError, msg, mergeFields(fields))
}

// CtxFatal 输出带 context 的 FATAL 日志并退出程序。
func CtxFatal(ctx context.Context, msg string, fields ...Fields) {
	std.log(ctx, 1, LevelFatal, msg, mergeFields(fields))
}

// mergeFields 将多个 Fields 合并为一个。
func mergeFields(in []Fields) Fields {
	out := Fields{}
	for _, f := range in {
		for k, v := range f {
			out[k] = v
		}
	}
	return out
}
