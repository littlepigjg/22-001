package shurl_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/handler"
	"shurl/internal/service"
	"shurl/internal/store"
)

type apiResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func makeCfg(tmp string) *config.Config {
	cfg := config.Default()
	cfg.Storage.URLFilePath = filepath.Join(tmp, "urls.json")
	cfg.Storage.LogFilePath = filepath.Join(tmp, "access.log")
	cfg.Storage.SyncInterval = 0
	cfg.Storage.FlushOnWrite = false
	cfg.Janitor.Enabled = false
	return cfg
}

func bootServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	tmp, err := os.MkdirTemp("", "shurl-defect-*")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	cfg := makeCfg(tmp)
	us, err := store.NewURLStore(cfg)
	if err != nil {
		t.Fatalf("NewURLStore: %v", err)
	}
	if err := us.Load(context.Background()); err != nil {
		t.Fatalf("Load URLStore: %v", err)
	}
	ls, err := store.NewAccessLogStore(cfg)
	if err != nil {
		t.Fatalf("NewAccessLogStore: %v", err)
	}
	if err := ls.Open(context.Background()); err != nil {
		t.Fatalf("Open AccessLogStore: %v", err)
	}
	svc, err := service.NewURLService(cfg, us)
	if err != nil {
		t.Fatalf("NewURLService: %v", err)
	}
	rs, err := service.NewRedirectService(us, ls)
	if err != nil {
		t.Fatalf("NewRedirectService: %v", err)
	}
	uh, err := handler.NewURLHandler(svc)
	if err != nil {
		t.Fatalf("NewURLHandler: %v", err)
	}
	rh, err := handler.NewRedirectHandler(rs)
	if err != nil {
		t.Fatalf("NewRedirectHandler: %v", err)
	}
	mux := http.NewServeMux()
	uh.Register(mux)
	rh.Register(mux)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	cleanup := func() {
		ts.Close()
		_ = ls.Close()
		_ = us.Close()
		_ = os.RemoveAll(tmp)
	}
	return ts, cleanup
}

func postJSON(url string, body any) (*http.Response, []byte, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	return resp, out.Bytes(), nil
}

func TestRedGreen(t *testing.T) {
	ts, cleanup := bootServer(t)
	defer cleanup()

	custom := "abcde12345"
	body := map[string]any{
		"raw_url":     "https://example.com/homepage?ref=test",
		"custom_code": custom,
	}

	r1, b1, err := postJSON(ts.URL+"/api/urls", body)
	if err != nil {
		t.Fatalf("first POST failed: %v", err)
	}
	if r1.StatusCode != http.StatusCreated {
		t.Fatalf("first POST expected 201, got %d body=%s", r1.StatusCode, string(b1))
	}

	r2, b2, err := postJSON(ts.URL+"/api/urls", body)
	if err != nil {
		t.Fatalf("second POST failed: %v", err)
	}

	var parsed apiResp
	if jerr := json.Unmarshal(b2, &parsed); jerr != nil {
		t.Fatalf("second POST body not JSON: %v body=%s", jerr, string(b2))
	}

	msgHasAlready := bytes.Contains(bytes.ToLower(b2), []byte("already exists"))

	fmt.Printf("\n========== RESULT ==========\n")
	fmt.Printf("First POST status:  %d\n", r1.StatusCode)
	fmt.Printf("Second POST status: %d (expected 409)\n", r2.StatusCode)
	fmt.Printf("Second POST body:   %s\n", string(b2))
	fmt.Printf("message contains 'already exists': %v\n", msgHasAlready)
	fmt.Printf("============================\n\n")

	defectPresent := false
	reason := ""
	if r2.StatusCode != http.StatusConflict {
		defectPresent = true
		reason = fmt.Sprintf("second POST status=%d, expected 409 Conflict", r2.StatusCode)
		if r2.StatusCode == http.StatusInternalServerError && msgHasAlready {
			reason += " (got 500 with 'already exists' in message — classic broken error-chain symptom)"
		}
	}

	if defectPresent {
		fmt.Println("RED（红灯，缺陷未修复）")
		t.Errorf("RED（红灯，缺陷未修复） — %s", reason)
		return
	}
	fmt.Println("GREEN（绿灯，缺陷已修复）")
}
