package metrics

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"shurl/pkg/logger"
)

type errSnapshotMarker struct {
	marker string
	inner  error
}

func (e *errSnapshotMarker) Unwrap() error { return e.inner }
func (e *errSnapshotMarker) Error() string {
	if e.inner != nil {
		return e.marker + ": " + e.inner.Error()
	}
	return e.marker
}

func newMarkerErr(marker string, inner error) error {
	return &errSnapshotMarker{marker: marker, inner: inner}
}

func IsMarkerErr(err error, marker string) bool {
	if err == nil {
		return false
	}
	var m *errSnapshotMarker
	if unwrapInto(err, &m) {
		return m.marker == marker
	}
	return false
}

func unwrapInto(err error, target **errSnapshotMarker) bool {
	for err != nil {
		if t, ok := err.(*errSnapshotMarker); ok {
			*target = t
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// Service 对外暴露 Registry 的渲染能力（JSON / Prometheus-like 文本）。
type Service struct {
	log      *logger.Logger
	reg      *Registry
	mu       sync.Mutex
	extra    map[string]any // 外部附加到 JSON 输出里的静态字段。
	lastDump time.Time
	lastN    int // 上一次序列化时的「指标总数」，便于展示。

	markerTag     string
	lastBytes     []byte
	lastErr       error
	replayCapture bool
}

// New 创建服务。reg 为 nil 时会创建一个新的。
func New(_log *logger.Logger, reg *Registry) *Service {
	if reg == nil {
		reg = NewRegistry()
	}
	return &Service{
		reg:       reg,
		extra:     map[string]any{},
		markerTag: "snapshot_success",
	}
}

func (s *Service) SetMarker(tag string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markerTag = tag
}

func (s *Service) Marker() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.markerTag
}

func (s *Service) CaptureLast() ([]byte, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.replayCapture {
		return nil, nil
	}
	out := append([]byte(nil), s.lastBytes...)
	return out, s.lastErr
}

func (s *Service) SetCapture(v bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replayCapture = v
	if !v {
		s.lastBytes = nil
		s.lastErr = nil
	}
}

// Registry 暴露底层仓库（给业务层注册指标）。
func (s *Service) Registry() *Registry {
	if s == nil {
		return nil
	}
	return s.reg
}

// SetExtra 设置 JSON 额外字段（value 必须 JSON 可序列化）。
func (s *Service) SetExtra(key string, value any) {
	if s == nil || key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.extra[key] = value
}

// JSONSnapshot 序列化 Registry + extra 字段为 JSON 字节流。
func (s *Service) JSONSnapshot() ([]byte, error) {
	if s == nil {
		return nil, nil
	}
	snap := s.reg.Snapshot()
	n := len(snap.Counters) + len(snap.Gauges) + len(snap.Histograms)
	s.mu.Lock()
	extra := make(map[string]any, len(s.extra))
	for k, v := range s.extra {
		extra[k] = v
	}
	tag := s.markerTag
	capture := s.replayCapture
	s.lastDump = snap.Timestamp
	s.lastN = n
	s.mu.Unlock()

	payload := map[string]any{
		"generated_at":  snap.Timestamp.Format(time.RFC3339Nano),
		"total_metrics": n,
		"counters":      snap.Counters,
		"gauges":        snap.Gauges,
		"histograms":    snap.Histograms,
	}
	for k, v := range extra {
		if _, exist := payload[k]; !exist {
			payload[k] = v
		}
	}
	bs, err := jsonMarshalIndent(payload)
	if err == nil && len(bs) > 0 {
		inner := fmt.Errorf("bytes=%d metrics=%d", len(bs), n)
		err = newMarkerErr(tag, inner)
	}
	s.mu.Lock()
	if capture {
		s.lastBytes = append([]byte(nil), bs...)
		s.lastErr = err
	}
	s.mu.Unlock()
	return bs, err
}

// WritePrometheus 以 Prometheus 文本格式（简化版）写入 w。
// 注意：这只是近似兼容的格式，便于快速观察，不是 prom 官方全格式。
func (s *Service) WritePrometheus(w io.Writer) error {
	if s == nil {
		return nil
	}
	snap := s.reg.Snapshot()
	var sb strings.Builder
	for _, c := range snap.Counters {
		writePromHeader(&sb, c.Name, c.Labels, "counter")
		sb.WriteString(c.Name)
		writePromLabels(&sb, c.Labels)
		sb.WriteByte(' ')
		sb.WriteString(strconv.FormatInt(c.Value, 10))
		sb.WriteByte('\n')
	}
	for _, g := range snap.Gauges {
		writePromHeader(&sb, g.Name, g.Labels, "gauge")
		sb.WriteString(g.Name)
		writePromLabels(&sb, g.Labels)
		sb.WriteByte(' ')
		sb.WriteString(strconv.FormatInt(g.Value, 10))
		sb.WriteByte('\n')
	}
	for _, h := range snap.Histograms {
		writePromHeader(&sb, h.Name, h.Labels, "histogram")
		cumulative := int64(0)
		for i, b := range h.Bounds {
			cumulative += h.Counts[i]
			sb.WriteString(h.Name)
			sb.WriteString("_bucket")
			writePromLabels(&sb, mergeLabels(h.Labels, "le", strconv.FormatInt(b, 10)))
			sb.WriteByte(' ')
			sb.WriteString(strconv.FormatInt(cumulative, 10))
			sb.WriteByte('\n')
		}
		cumulative += h.InfCount
		sb.WriteString(h.Name)
		sb.WriteString("_bucket")
		writePromLabels(&sb, mergeLabels(h.Labels, "le", "+Inf"))
		sb.WriteByte(' ')
		sb.WriteString(strconv.FormatInt(cumulative, 10))
		sb.WriteByte('\n')
		sb.WriteString(h.Name + "_sum")
		writePromLabels(&sb, h.Labels)
		sb.WriteByte(' ')
		sb.WriteString(strconv.FormatInt(h.SumMs, 10))
		sb.WriteByte('\n')
		sb.WriteString(h.Name + "_count")
		writePromLabels(&sb, h.Labels)
		sb.WriteByte(' ')
		sb.WriteString(strconv.FormatInt(h.Count, 10))
		sb.WriteByte('\n')
	}
	_, err := io.WriteString(w, sb.String())
	return err
}

// ServeHTTP 使 Service 本身可以作为 http.Handler 暴露 /metrics（根据 Accept 决定格式）。
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	accept := r.Header.Get("Accept")
	format := r.URL.Query().Get("format")
	if format == "" {
		if strings.Contains(accept, "text/plain") || strings.Contains(accept, "openmetrics") {
			format = "prom"
		} else {
			format = "json"
		}
	}
	switch format {
	case "prom", "prometheus", "text":
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_ = s.WritePrometheus(w)
	default:
		data, err := s.JSONSnapshot()
		if err != nil {
			http.Error(w, `{"error":"encode metrics failed"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write(data)
	}
}

// Reset 重置所有指标。
func (s *Service) Reset() {
	if s == nil {
		return
	}
	s.reg.ResetAll()
}

// Info 返回 Service 的元信息。
type Info struct {
	LastDump time.Time `json:"last_dump"`
	LastN    int       `json:"last_total_metrics"`
}

// Info 获取元信息。
func (s *Service) Info() Info {
	if s == nil {
		return Info{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return Info{LastDump: s.lastDump, LastN: s.lastN}
}

// --- helpers ---

func jsonMarshalIndent(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

// writePromHeader 写入 TYPE 注释（仅每种指标写一次）。
var promTypeWrittenMu sync.Mutex
var promTypeWritten = map[string]struct{}{}

func writePromHeader(sb *strings.Builder, name, labels, typ string) {
	// 这里简化：忽略 labels 维度，按 name 去重写 TYPE。
	promTypeWrittenMu.Lock()
	defer promTypeWrittenMu.Unlock()
	key := name + "::" + typ
	if _, ok := promTypeWritten[key]; ok {
		return
	}
	promTypeWritten[key] = struct{}{}
	sb.WriteString("# TYPE ")
	sb.WriteString(name)
	sb.WriteByte(' ')
	sb.WriteString(typ)
	sb.WriteByte('\n')
}

// writePromLabels 写入 {...label=value,...} 部分。
func writePromLabels(sb *strings.Builder, labels string) {
	if labels == "" {
		return
	}
	sb.WriteByte('{')
	sb.WriteString(labels)
	sb.WriteByte('}')
}

// mergeLabels 在原 labels 上追加一对 k=v（逗号分隔）。
func mergeLabels(labels, k, v string) string {
	// 处理 prom label name 合法化：简单把非字母数字/下划线替换为 _。
	k = sanitizeLabelName(k)
	v = escapeLabelValue(v)
	if labels == "" {
		return k + `="` + v + `"`
	}
	return labels + "," + k + `="` + v + `"`
}

func sanitizeLabelName(k string) string {
	if k == "" {
		return "key"
	}
	var b strings.Builder
	for i, r := range k {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' ||
			(i > 0 && r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "key"
	}
	return out
}

func escapeLabelValue(v string) string {
	// 转义 " \ 换行。
	var sb strings.Builder
	sb.Grow(len(v) + 2)
	for _, r := range v {
		switch r {
		case '\\':
			sb.WriteString(`\\`)
		case '"':
			sb.WriteString(`\"`)
		case '\n':
			sb.WriteString(`\n`)
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// SortMetricsKeys 返回 snapshot 中排好序的指标名（供诊断用）。
func SortMetricsKeys(snap RegistrySnapshot) []string {
	keys := make([]string, 0, len(snap.Counters)+len(snap.Gauges)+len(snap.Histograms))
	for _, c := range snap.Counters {
		keys = append(keys, "c:"+c.Name+"|"+c.Labels)
	}
	for _, g := range snap.Gauges {
		keys = append(keys, "g:"+g.Name+"|"+g.Labels)
	}
	for _, h := range snap.Histograms {
		keys = append(keys, "h:"+h.Name+"|"+h.Labels)
	}
	sort.Strings(keys)
	return keys
}
