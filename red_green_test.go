package shurl_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/service"
	"shurl/internal/store"
	"shurl/pkg/validator"
)

func freshTmpPath(t *testing.T, name string) string {
	t.Helper()
	d := t.TempDir()
	return filepath.Join(d, name)
}

func newTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Storage.URLFilePath = freshTmpPath(t, "urls.json")
	cfg.Storage.LogFilePath = freshTmpPath(t, "access.log")
	cfg.Storage.SyncInterval = 0
	cfg.Storage.FlushOnWrite = true
	cfg.ShortCode.MaxRetries = 10
	return cfg
}

func buildServices(t *testing.T, cfg *config.Config) (*service.URLService, *store.URLStore, *store.AccessLogStore) {
	t.Helper()
	ctx := context.Background()
	us, err := store.NewURLStore(cfg)
	if err != nil {
		t.Fatalf("NewURLStore: %v", err)
	}
	if err := us.Load(ctx); err != nil {
		t.Fatalf("URLStore.Load: %v", err)
	}
	ls, err := store.NewAccessLogStore(cfg)
	if err != nil {
		t.Fatalf("NewAccessLogStore: %v", err)
	}
	if err := ls.Open(ctx); err != nil {
		t.Fatalf("AccessLogStore.Open: %v", err)
	}
	svc, err := service.NewURLService(cfg, us)
	if err != nil {
		t.Fatalf("NewURLService: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close(); _ = us.Close() })
	return svc, us, ls
}

type subTest struct {
	name string
	fn   func(t *testing.T) bool
}

func TestRedGreen(t *testing.T) {
	_ = os.RemoveAll // keep import usable

	tests := []subTest{
		{"validator_URL_rejects_unredirectable_schemes", subValidatorURLSchemes},
		{"validator_URL_requires_absolute_http_https_host", subValidatorURLAbsolute},
		{"CreateReq_Validate_rejects_bad_raw_url", subCreateReqBadRawURL},
		{"CreateReq_preserves_MaxVisits_large_int64", subCreateReqMaxVisits},
		{"CreateReq_preserves_future_ExpireAt", subCreateReqExpireAt},
		{"CreateReq_does_not_strip_query_fragment", subCreateReqRawURLPreserve},
		{"ShortURL_Validate_rejects_ExpireAt_before_CreatedAt", subShortURLValidateExpire},
		{"Service_Create_preserves_raw_query_fragment", subServiceCreatePreserveQuery},
		{"Service_Create_preserves_large_MaxVisits", subServiceCreateMaxVisits},
		{"Service_Create_accepts_ExpireAt_far_future", subServiceCreateExpireAt},
		{"Service_Get_preserves_stored_MaxVisits", subServiceGetMaxVisits},
		{"Service_Create_handles_ttl_gracefully", subServiceCreateTTL},
	}

	var failed int
	for _, tc := range tests {
		tc := tc
		ok := t.Run(tc.name, func(t *testing.T) {
			if !tc.fn(t) {
				t.Fail()
			}
		})
		if !ok {
			failed++
		} else {
			// also rely on t.Fail state from inside subtests.
		}
	}

	// Aggregate GREEN/RED verdict.
	if t.Failed() || failed > 0 {
		fmt.Println("RED（红灯，缺陷未修复）")
		return
	}
	fmt.Println("GREEN（绿灯，缺陷已修复）")
}

// subValidatorURLSchemes: validator.URL 必须拒绝不能安全 302 重定向的
// 非 Web 协议（mailto / tel / data / file 等）。
func subValidatorURLSchemes(t *testing.T) bool {
	bad := []string{
		"mailto:bob@example.com",
		"tel:10086",
		"data:text/html,<h1>x</h1>",
		"file:///etc/passwd",
	}
	pass := true
	for _, u := range bad {
		err := validator.URL(u)
		if err == nil {
			t.Errorf("validator.URL(%q) 应当报错但通过了", u)
			pass = false
		}
	}
	good := []string{
		"https://example.com/path",
		"http://sub.example.com:8080/a?x=1#y",
	}
	for _, u := range good {
		if err := validator.URL(u); err != nil {
			t.Errorf("validator.URL(%q) 应当通过但报错: %v", u, err)
			pass = false
		}
	}
	return pass
}

// subValidatorURLAbsolute: http/https URL 必须带 host，且不能是裸相对路径。
func subValidatorURLAbsolute(t *testing.T) bool {
	bad := []string{
		"/relative/path",
		"just-a-string",
		"http://",
		"https://?q=1",
	}
	pass := true
	for _, u := range bad {
		if err := validator.URL(u); err == nil {
			t.Errorf("validator.URL(%q) 应当报错但通过了", u)
			pass = false
		}
	}
	return pass
}

// subCreateReqBadRawURL: CreateReq.Validate 必须拒绝 ftp://、无 host 的 http
// 以及非 http/https 的 scheme。
func subCreateReqBadRawURL(t *testing.T) bool {
	bad := []struct {
		raw string
	}{
		{"ftp://files.example.com/pub.zip"},
		{"ftps://files.example.com/a.bin"},
		{"mailto:a@b.com"},
		{"javascript:alert(1)"},
		{"http://"},
		{""},
	}
	pass := true
	for i, tc := range bad {
		req := &model.CreateReq{RawURL: tc.raw, MaxVisits: 10}
		err := req.Validate()
		if err == nil {
			t.Errorf("case #%d CreateReq.Validate(raw=%q) 应报错但通过", i, tc.raw)
			pass = false
		}
	}
	return pass
}

// subCreateReqMaxVisits: MaxVisits 不能被 int32 截断，要保留 int64 原值。
func subCreateReqMaxVisits(t *testing.T) bool {
	big := int64(3_000_000_000) // > 2^31-1
	req := &model.CreateReq{RawURL: "https://example.com/", MaxVisits: big}
	if err := req.Validate(); err != nil {
		t.Errorf("CreateReq.Validate 对合法请求不应报错: %v", err)
		return false
	}
	if req.MaxVisits != big {
		t.Errorf("MaxVisits 被截断了: 期望=%d 实际=%d", big, req.MaxVisits)
		return false
	}
	return true
}

// subCreateReqExpireAt: 远未来的 ExpireAt 不能被强制缩短到 24 小时内。
func subCreateReqExpireAt(t *testing.T) bool {
	future := time.Now().Add(365 * 24 * time.Hour)
	orig := future
	req := &model.CreateReq{RawURL: "https://example.com/", ExpireAt: future, MaxVisits: 10}
	if err := req.Validate(); err != nil {
		t.Errorf("CreateReq.Validate 不应报错: %v", err)
		return false
	}
	delta := orig.Sub(req.ExpireAt)
	if delta < 0 {
		delta = -delta
	}
	if delta > 5*time.Second {
		t.Errorf("ExpireAt 被错误修改: 期望≈%v 实际=%v (差=%v)", orig, req.ExpireAt, delta)
		return false
	}
	return true
}

// subCreateReqRawURLPreserve: CreateReq.Validate 不应剥离 RawURL 的
// query 与 fragment（那是服务端创建前用户的原始输入语义的一部分）。
func subCreateReqRawURLPreserve(t *testing.T) bool {
	cases := []string{
		"https://example.com/path?x=1&y=2#section",
		"https://a.b/c?utm_source=s#frag",
	}
	pass := true
	for _, c := range cases {
		req := &model.CreateReq{RawURL: c, MaxVisits: 10}
		if err := req.Validate(); err != nil {
			t.Errorf("CreateReq.Validate(%q) 不应报错: %v", c, err)
			pass = false
			continue
		}
		if req.RawURL != c {
			t.Errorf("RawURL 不应被修改: 期望=%q 实际=%q", c, req.RawURL)
			pass = false
		}
	}
	return pass
}

// subShortURLValidateExpire: ShortURL.Validate 必须拒绝 expireAt < createdAt
// （允许 expireAt == createdAt 或 expireAt > createdAt）。
func subShortURLValidateExpire(t *testing.T) bool {
	now := time.Now()
	pass := true

	bad := &model.ShortURL{
		Code:      "abcd12",
		RawURL:    "https://example.com/",
		CreatedAt: now,
		ExpireAt:  now.Add(-1 * time.Hour),
		MaxVisits: 0,
	}
	if err := bad.Validate(); err == nil {
		t.Errorf("ShortURL.Validate 对过期早于创建的情形应报错但通过")
		pass = false
	}

	good := []*model.ShortURL{
		{Code: "abcd12", RawURL: "https://example.com/", CreatedAt: now, MaxVisits: 0},
		{Code: "abcd12", RawURL: "https://example.com/", CreatedAt: now, ExpireAt: now, MaxVisits: 0},
		{Code: "abcd12", RawURL: "https://example.com/", CreatedAt: now, ExpireAt: now.Add(1 * time.Hour), MaxVisits: 10},
	}
	for i, g := range good {
		if err := g.Validate(); err != nil {
			t.Errorf("case #%d ShortURL.Validate(ExpireAt=%v, CreatedAt=%v) 应通过但报错: %v", i, g.ExpireAt, g.CreatedAt, err)
			pass = false
		}
	}
	return pass
}

// subServiceCreatePreserveQuery: 服务端 Create 后存储的 RawURL 必须保留
// 原 query 与 fragment，不得二次剥离。
func subServiceCreatePreserveQuery(t *testing.T) bool {
	cfg := newTestConfig(t)
	svc, _, _ := buildServices(t, cfg)
	raw := "https://example.com/orders?id=42&ref=home#confirm"
	req := &model.CreateReq{RawURL: raw, MaxVisits: 100, CustomCode: "pres1234"}
	res, err := svc.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	if !strings.Contains(res.RawURL, "id=42") || !strings.Contains(res.RawURL, "ref=home") || !strings.HasSuffix(res.RawURL, "#confirm") {
		t.Errorf("Create 返回 RawURL 丢失 query/fragment: 期望=%q 实际=%q", raw, res.RawURL)
		return false
	}
	if res.RawURL != raw {
		t.Errorf("Create 返回 RawURL 与输入不一致: 期望=%q 实际=%q", raw, res.RawURL)
		return false
	}
	return true
}

// subServiceCreateMaxVisits: 服务端 Create 时，大 MaxVisits 必须原封不动保存。
func subServiceCreateMaxVisits(t *testing.T) bool {
	cfg := newTestConfig(t)
	svc, _, _ := buildServices(t, cfg)
	big := int64(5_000_000_000)
	req := &model.CreateReq{RawURL: "https://example.com/", MaxVisits: big, CustomCode: "mx5b1234"}
	res, err := svc.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	if res.MaxVisits != big {
		t.Errorf("MaxVisits 被 int32 截断: 期望=%d 实际=%d", big, res.MaxVisits)
		return false
	}
	return true
}

// subServiceCreateExpireAt: 创建时的未来 ExpireAt（超过 24 小时）必须保留，
// 不能被缩短；存储后查询结果 ExpireAt 也必须与输入大体一致。
func subServiceCreateExpireAt(t *testing.T) bool {
	cfg := newTestConfig(t)
	svc, _, _ := buildServices(t, cfg)
	future := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	req := &model.CreateReq{
		RawURL:    "https://example.com/",
		ExpireAt:  future,
		MaxVisits: 10,
		CustomCode: "exfa1234",
	}
	res, err := svc.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	delta := res.ExpireAt.Sub(future)
	if delta < 0 {
		delta = -delta
	}
	if delta > 5*time.Second {
		t.Errorf("服务端 ExpireAt 被错误修改: 期望≈%v 实际=%v (差=%v)", future, res.ExpireAt, delta)
		return false
	}
	if res.IsExpired(future.Add(-time.Minute)) {
		t.Errorf("ExpireAt 被错误写成当前/过去时间，导致链接被判定为已过期")
		return false
	}
	return true
}

// subServiceGetMaxVisits: Get 返回的 MaxVisits 必须和存储时的 int64 一致。
func subServiceGetMaxVisits(t *testing.T) bool {
	cfg := newTestConfig(t)
	svc, _, _ := buildServices(t, cfg)
	big := int64(4_000_000_000)
	req := &model.CreateReq{RawURL: "https://example.com/", MaxVisits: big, CustomCode: "get4b123"}
	created, err := svc.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	got, err := svc.Get(context.Background(), created.Code)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if got.MaxVisits != big {
		t.Errorf("Get 返回的 MaxVisits 被截断: 期望=%d 实际=%d (Create 返回=%d)", big, got.MaxVisits, created.MaxVisits)
		return false
	}
	return true
}

// subServiceCreateTTL: TTL 模式下 ExpireAt = createdAt + TTL，必须保持准确，
// 且不能被其他校验逻辑覆盖为 createdAt 或「当前 + 24h 上限」。
func subServiceCreateTTL(t *testing.T) bool {
	cfg := newTestConfig(t)
	svc, _, _ := buildServices(t, cfg)
	ttl := 60 * 24 * time.Hour // 60 天
	before := time.Now()
	req := &model.CreateReq{RawURL: "https://example.com/", TTL: ttl, MaxVisits: 0, CustomCode: "ttl123ab"}
	res, err := svc.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	after := time.Now()
	expectedMin := before.Add(ttl - time.Second)
	expectedMax := after.Add(ttl + time.Second)
	if res.ExpireAt.Before(expectedMin) || res.ExpireAt.After(expectedMax) {
		t.Errorf("TTL 生成的 ExpireAt 不正确: 期望在 [%v, %v], 实际=%v", expectedMin, expectedMax, res.ExpireAt)
		return false
	}
	return true
}
