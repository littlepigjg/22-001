package durationutil

import (
	"testing"
	"time"
)

func TestParseDuration_PanicFreeOnUnknownUnit(t *testing.T) {
	// 之前：这些输入会让 consumeUnit 的 s[:2] 越界 panic。
	bad := []string{"1y", "1z", "1q", "1x", "1d2z", "2y"}
	for _, in := range bad {
		if _, err := ParseDuration(in); err == nil {
			t.Fatalf("ParseDuration(%q): expected error for unknown unit, got nil", in)
		}
	}
}

func TestParseDuration_LegitimateInputs(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"300", 300 * time.Second},
		{"1d", 24 * time.Hour},
		{"1d6h", 30 * time.Hour},
		{"1w", 7 * 24 * time.Hour},
		{"1h30m", 90 * time.Minute},
		{"[1h]", time.Hour},
		{"t:1d", 24 * time.Hour},
		{"d:5w", 35 * 24 * time.Hour},
	}
	for _, c := range cases {
		got, err := ParseDuration(c.in)
		if err != nil {
			t.Fatalf("ParseDuration(%q): unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("ParseDuration(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestSplitAndSumDurations_CompositeWithBadUnit(t *testing.T) {
	// "1d + 2y" 这种带单字符非法 unit 的组合写法不应 panic，而应返回错误。
	if _, err := SplitAndSumDurations("1d,2y", ","); err == nil {
		t.Fatal("SplitAndSumDurations: expected error for unknown unit 2y, got nil")
	}
}

func TestSplitAndSumDurations_CompositeOK(t *testing.T) {
	got, err := SplitAndSumDurations("1d,6h", ",")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := 30 * time.Hour; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
