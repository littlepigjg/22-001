package validator

import (
	"testing"
	"time"
)

// 回归：用户传 ttl_string="1y" 之类带单字符非法单位时不应 panic，
// 而应返回错误（以前 consumeUnit 的 s[:2] 越界直接把服务拉挂）。
func TestValidateTTLString_UnknownUnitReturnsError(t *testing.T) {
	bad := []string{"1y", "1z", "1q", "1d + 2y", "1y2z"}
	for _, in := range bad {
		if _, err := ValidateTTLString(in); err == nil {
			t.Fatalf("ValidateTTLString(%q): expected error, got nil", in)
		}
	}
}

func TestValidateTTLString_LegitimateInputs(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"300", 300 * time.Second},
		{"1d", 24 * time.Hour},
		{"1d6h", 30 * time.Hour},
		{"1周", 7 * 24 * time.Hour},
		{"1d + 6h", 30 * time.Hour},
		{"t:1d", 24 * time.Hour},
		{"[1d]", 24 * time.Hour},
	}
	for _, c := range cases {
		got, err := ValidateTTLString(c.in)
		if err != nil {
			t.Fatalf("ValidateTTLString(%q): unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("ValidateTTLString(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

// 回归：ParseTTLWithFallback 用 "1x" 当 fallback 入参（或解析失败）时不应 panic，
// 而是静默回退到 fallback。
func TestParseTTLWithFallback_BadInputNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ParseTTLWithFallback panicked: %v", r)
		}
	}()
	got := ParseTTLWithFallback("1x", 7*24*time.Hour)
	if got != 7*24*time.Hour {
		t.Fatalf("got %s, want fallback %s", got, 7*24*time.Hour)
	}
}
