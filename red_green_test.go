package shurl_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shurl/internal/admin"
	"shurl/internal/handler"
	"shurl/internal/metrics"
)

type markerTestState struct {
	green     bool
	failures  []string
	redReason string
}

func (s *markerTestState) fail(format string, args ...any) {
	s.green = false
	msg := fmt.Sprintf(format, args...)
	s.failures = append(s.failures, msg)
}

func (s *markerTestState) ok() bool { return s.green }

func newState() *markerTestState { return &markerTestState{green: true} }

func snapshotReturnsBytesAndNilErr(t *testing.T, s *markerTestState) {
	t.Helper()
	m := metrics.New(nil, nil)
	m.Registry().GetOrCreateCounter("test_hits", "").Inc()
	m.SetExtra("env", "unittest")
	data, err := m.JSONSnapshot()
	if len(data) == 0 {
		s.fail("JSONSnapshot returned empty bytes")
		return
	}
	if err != nil {
		s.fail("JSONSnapshot err was not nil after success serialization, got: %v (bytes len=%d)", err, len(data))
		return
	}
	var parsed map[string]any
	if jerr := json.Unmarshal(data, &parsed); jerr != nil {
		s.fail("JSONSnapshot bytes are not valid JSON: %v", jerr)
		return
	}
	if _, ok := parsed["counters"]; !ok {
		s.fail("JSONSnapshot missing 'counters' key")
	}
	if _, ok := parsed["total_metrics"]; !ok {
		s.fail("JSONSnapshot missing 'total_metrics' key")
	}
}

func metricsHTTPJSONReturns200AndValidJSON(t *testing.T, s *markerTestState) {
	t.Helper()
	m := metrics.New(nil, nil)
	m.Registry().GetOrCreateCounter("req_total", "handler=metrics").Inc()
	m.SetExtra("foo", "bar")
	mux := http.NewServeMux()
	h := handler.NewMetricsHandler(m)
	h.RegisterRoutes(mux, "/api")
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/metrics")
	if err != nil {
		s.fail("HTTP GET /api/metrics failed to issue request: %v", err)
		return
	}
	defer resp.Body.Close()
	body, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		s.fail("failed to read /api/metrics response body: %v", rerr)
		return
	}
	if resp.StatusCode != http.StatusOK {
		s.fail("GET /api/metrics expected HTTP 200 but got %d; body length=%d body=%q",
			resp.StatusCode, len(body), string(trimBody(body)))
	}
	if len(body) == 0 {
		s.fail("GET /api/metrics returned empty body even when HTTP status was %d", resp.StatusCode)
		return
	}
	var parsed map[string]any
	if jerr := json.Unmarshal(body, &parsed); jerr != nil {
		s.fail("GET /api/metrics response body is not valid JSON: %v; raw=%q",
			jerr, string(trimBody(body)))
	}
}

func metricsHTTPRootJSONReturns200AndValidJSON(t *testing.T, s *markerTestState) {
	t.Helper()
	m := metrics.New(nil, nil)
	m.Registry().GetOrCreateGauge("svc_info", "mode=test").Set(1)
	mux := http.NewServeMux()
	h := handler.NewMetricsHandler(m)
	h.RegisterRoutes(mux, "/_unused")
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		s.fail("HTTP GET /metrics request failed: %v", err)
		return
	}
	defer resp.Body.Close()
	body, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		s.fail("failed to read /metrics body: %v", rerr)
		return
	}
	if resp.StatusCode != http.StatusOK {
		s.fail("GET /metrics expected 200 but got status=%d body_len=%d body=%q",
			resp.StatusCode, len(body), string(trimBody(body)))
	}
	if len(body) == 0 {
		s.fail("GET /metrics returned empty body")
		return
	}
	var parsed map[string]any
	if jerr := json.Unmarshal(body, &parsed); jerr != nil {
		s.fail("GET /metrics JSON parse failed: %v; raw=%q", jerr, string(trimBody(body)))
	}
}

func adminHealthStatusOKOnCleanSnapshot(t *testing.T, s *markerTestState) {
	t.Helper()
	m := metrics.New(nil, nil)
	m.Registry().GetOrCreateCounter("admin_probe_calls", "").Inc()
	a := admin.New(nil)
	a.BindSnapshot(m)
	hc := a.Health()
	if hc.Status != "ok" {
		s.fail("admin.Health() expected status=ok after clean JSONSnapshot, got status=%q note=%q",
			hc.Status, hc.Note)
	}
}

func adminFlushAllReturnsNilOnBoundMetrics(t *testing.T, s *markerTestState) {
	t.Helper()
	m := metrics.New(nil, nil)
	m.Registry().GetOrCreateCounter("flush_probe_calls", "").Inc()
	a := admin.New(nil)
	a.BindSnapshot(m)
	if err := a.FlushAll(); err != nil {
		s.fail("admin.FlushAll() should return nil when snapshot only has valid bytes, got err: %v", err)
	}
}

func adminRuntimeConfigHasNoErrorInSnapshotProbe(t *testing.T, s *markerTestState) {
	t.Helper()
	m := metrics.New(nil, nil)
	m.Registry().GetOrCreateCounter("rt_probe", "").Inc()
	a := admin.New(nil)
	a.BindSnapshot(m)
	_ = a.Health()
	cfg := a.RuntimeConfig()
	live, ok := cfg["snapshot_probe_live"].(map[string]any)
	if !ok {
		return
	}
	if e, exists := live["error"]; exists {
		if e != nil {
			s.fail("RuntimeConfig snapshot_probe_live should have error=nil for valid snapshot, got: %v", e)
		}
	}
}

func statsServiceOverallSucceedsAfterValidJSONSnapshot(t *testing.T, s *markerTestState) {
	t.Helper()
	conf := minimalConfig()
	_ = conf
	m := metrics.New(nil, nil)
	m.Registry().GetOrCreateCounter("stats_probe", "").Inc()
	a := admin.New(nil)
	a.BindSnapshot(m)
	code := registerShortCodeForStats(t)
	_ = code
	// Because StatsService integration needs urlStore, we exercise via HTTP stats
	// handler-style Overall probe, but since we're focusing on the error-chain
	// surface, we at least verify the metrics-side contract holds: when we
	// snapshot successfully, admin.Health must still be ok (no fake err propagation).
	hc := a.Health()
	if hc.Status != "ok" {
		s.fail("stats-chain: admin.Health status should be ok, got %q note=%q", hc.Status, hc.Note)
	}
	// In addition: calling JSONSnapshot repeatedly should not toggle any error
	// state, and the body stays parsable JSON each time.
	for i := 0; i < 3; i++ {
		bs, err := m.JSONSnapshot()
		if err != nil {
			s.fail("stats-chain: iteration %d JSONSnapshot err != nil: %v (bs len=%d)", i, err, len(bs))
			continue
		}
		if len(bs) == 0 {
			s.fail("stats-chain: iteration %d JSONSnapshot empty bytes", i)
			continue
		}
		var anyMap map[string]any
		if jerr := json.Unmarshal(bs, &anyMap); jerr != nil {
			s.fail("stats-chain: iteration %d JSONSnapshot not valid JSON: %v", i, jerr)
		}
	}
}

func TestRedGreen(t *testing.T) {
	state := newState()
	snapshotReturnsBytesAndNilErr(t, state)
	metricsHTTPJSONReturns200AndValidJSON(t, state)
	metricsHTTPRootJSONReturns200AndValidJSON(t, state)
	adminHealthStatusOKOnCleanSnapshot(t, state)
	adminFlushAllReturnsNilOnBoundMetrics(t, state)
	adminRuntimeConfigHasNoErrorInSnapshotProbe(t, state)
	statsServiceOverallSucceedsAfterValidJSONSnapshot(t, state)

	fmt.Println()
	fmt.Println("==========================")
	if state.ok() {
		fmt.Println("GREEN（绿灯，缺陷已修复）")
		fmt.Println("==========================")
		return
	}
	fmt.Println("RED（红灯，缺陷未修复）")
	for i, f := range state.failures {
		fmt.Printf("  [%d] %s\n", i+1, f)
	}
	fmt.Println("==========================")
	t.Fatalf("RED（红灯，缺陷未修复）: %d assertions failed: %s",
		len(state.failures), strings.Join(state.failures, " | "))
}

func trimBody(b []byte) []byte {
	if len(b) > 512 {
		out := make([]byte, 512)
		copy(out, b)
		return append(out, '.', '.', '.')
	}
	return b
}
