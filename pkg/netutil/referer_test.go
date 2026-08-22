package netutil

import (
	"strings"
	"testing"
)

// 旧的致命 bug：路径恰好两个字符（如 "/x"）时，NormalizeReferer 会访问 u.RawPath[2]
// 而 RawPath 为空，触发 index out of range [2] with length 0。这里把这条链路上的各种
// 边界输入都钉死：要么返回规范化后的字符串，要么返回明确 error，绝不 panic。
func TestNormalizeReferer_ShortPaths(t *testing.T) {
	cases := map[string]string{
		"https://example.com/x":  "https://example.com/x",  // 路径 2 字符（原崩溃点）
		"https://example.com/a":  "https://example.com/a",  // 同样 2 字符
		"https://example.com":    "https://example.com",   // 无路径
		"https://example.com/":   "https://example.com/",  // 仅根
		"https://example.com/ab": "https://example.com/ab", // 路径 3 字符
		"https://example.com/p?q=1":      "https://example.com/p?q=1",
		"https://example.com/p#frag":      "https://example.com/p#frag",
		"https://example.com/path/to/r":  "https://example.com/path/to/r",
	}
	for in, want := range cases {
		got, err := NormalizeReferer(in)
		if err != nil {
			t.Errorf("NormalizeReferer(%q) unexpected err: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeReferer(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeReferer_StripsDefaultPorts(t *testing.T) {
	cases := map[string]string{
		"http://example.com:80/p":  "http://example.com/p",
		"https://example.com:443/p": "https://example.com/p",
	}
	for in, want := range cases {
		got, err := NormalizeReferer(in)
		if err != nil {
			t.Fatalf("NormalizeReferer(%q) err: %v", in, err)
		}
		if got != want {
			t.Errorf("NormalizeReferer(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeReferer_KeepsNonDefaultPorts(t *testing.T) {
	got, err := NormalizeReferer("https://example.com:8443/p")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if want := "https://example.com:8443/p"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNormalizeReferer_Invalid(t *testing.T) {
	// 空串 → 空串、无 error（按函数约定）。
	if got, err := NormalizeReferer(""); err != nil || got != "" {
		t.Errorf("empty = (%q,%v), want (\"\",nil)", got, err)
	}
	// 相对路径 / 无 host → 明确 error，不 panic。
	for _, in := range []string{"/just/path", "//no-scheme/x", "://bad", "not a url at all"} {
		if _, err := NormalizeReferer(in); err == nil {
			t.Errorf("NormalizeReferer(%q) want error, got nil", in)
		}
	}
}

// 批量为 3 且全部合法：旧代码 result[len(result)] 恒越界 panic。这里钉死顺序与数量。
func TestNormalizeRefererBulk_ThreeValid(t *testing.T) {
	in := []string{"https://a.com/1", "https://b.com/2", "https://c.com/3"}
	got, err := NormalizeRefererBulk(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if want := []string{"https://a.com/1", "https://b.com/2", "https://c.com/3"}; !sliceEq(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// 批量为 3 且含一条非法：跳过非法项、保持顺序、不 panic。
func TestNormalizeRefererBulk_ThreeWithBad(t *testing.T) {
	in := []string{"https://a.com/x", "not a url", "https://c.com/y"}
	got, err := NormalizeRefererBulk(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if want := []string{"https://a.com/x", "https://c.com/y"}; !sliceEq(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// 含默认端口也应在批量里被规范化。
func TestNormalizeRefererBulk_NormalizesPorts(t *testing.T) {
	in := []string{"https://a.com:443/x", "http://b.com:80/y"}
	got, err := NormalizeRefererBulk(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if want := []string{"https://a.com/x", "http://b.com/y"}; !sliceEq(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestNormalizeRefererBulk_AllEmpty(t *testing.T) {
	got, err := NormalizeRefererBulk([]string{"", "  ", "\t"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestNormalizeRefererBulk_Nil(t *testing.T) {
	got, err := NormalizeRefererBulk(nil)
	if err != nil || got != nil {
		t.Errorf("got (%v,%v), want (nil,nil)", got, err)
	}
}

// 重复 count 跑，防止偶发 / 缓存掩盖回归（用户报告 -count=3 次次红）。
func TestNormalizeRefererBulk_ThreeValidRepeated(t *testing.T) {
	in := []string{"https://a.com/1", "https://b.com/2", "https://c.com/3"}
	for i := 0; i < 5; i++ {
		got, err := NormalizeRefererBulk(in)
		if err != nil {
			t.Fatalf("iter %d err: %v", i, err)
		}
		if len(got) != 3 {
			t.Fatalf("iter %d len=%d", i, len(got))
		}
	}
}

func TestExtractRefererHost_Basic(t *testing.T) {
	if got := ExtractRefererHost("https://example.com:8443/p"); got != "example.com:8443" {
		t.Errorf("got %q, want example.com:8443", got)
	}
	if got := ExtractRefererHost("https://example.com:443/p"); got != "example.com" {
		t.Errorf("got %q, want example.com (default port stripped)", got)
	}
	if got := ExtractRefererHost("not a url"); got != "" {
		t.Errorf("got %q, want empty for invalid", got)
	}
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		// 忽略首尾空白差异，仅做字符串相等判断。
		if strings.TrimSpace(a[i]) != strings.TrimSpace(b[i]) {
			return false
		}
	}
	return true
}
