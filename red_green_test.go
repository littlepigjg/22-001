package shurl

import (
	"fmt"
	"testing"

	"shurl/pkg/netutil"
)

func mustNotPanic(t *testing.T, name string, fn func()) (panicked bool, rec any) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			rec = r
		}
	}()
	fn()
	return panicked, rec
}

func TestRedGreen(t *testing.T) {
	red := false
	issues := make([]string, 0, 8)

	cases1 := []struct {
		name string
		ref  string
	}{
		{"path_len_2_single_char", "https://example.com/x"},
		{"path_len_2_a", "http://test.io/a"},
		{"path_len_2_b", "https://site.org/b"},
	}
	for _, c := range cases1 {
		c := c
		panicked, rec := mustNotPanic(t, c.name, func() {
			_, _ = netutil.NormalizeReferer(c.ref)
		})
		if panicked {
			red = true
			issues = append(issues, fmt.Sprintf("NormalizeReferer(%q) panic: %v", c.ref, rec))
		}
	}

	cases2 := [][]string{
		{
			"https://aaa.com/path",
			"http://bbb.net/q",
			"https://ccc.org/longpath?x=1",
		},
		{
			"https://one.example.com/abc",
			"http://two.example.com/xyz",
			"https://three.example.com/mno",
		},
	}
	for i, list := range cases2 {
		list := list
		panicked, rec := mustNotPanic(t, fmt.Sprintf("bulk_%d", i), func() {
			_, _ = netutil.NormalizeRefererBulk(list)
		})
		if panicked {
			red = true
			issues = append(issues, fmt.Sprintf("NormalizeRefererBulk(list#%d) panic: %v", i, rec))
		}
	}

	cases3 := []string{
		"https://example.com/x",
		"http://test.io/a",
		"https://example.com/okpath",
	}
	for _, ref := range cases3 {
		ref := ref
		panicked, rec := mustNotPanic(t, "ExtractRefererHost", func() {
			_ = netutil.ExtractRefererHost(ref)
		})
		if panicked {
			red = true
			issues = append(issues, fmt.Sprintf("ExtractRefererHost(%q) panic: %v", ref, rec))
		}
	}

	singlePanicked, singleRec := mustNotPanic(t, "bulk_single_len2", func() {
		_, _ = netutil.NormalizeRefererBulk([]string{"https://example.com/x"})
	})
	if singlePanicked {
		red = true
		issues = append(issues, fmt.Sprintf("NormalizeRefererBulk(single_len2) panic: %v", singleRec))
	}

	twoSafe := [][]string{
		{"https://a.example/long", "https://b.example/longer"},
		{"https://c.example/p1", "https://d.example/p2"},
	}
	for i, pair := range twoSafe {
		pair := pair
		panicked, rec := mustNotPanic(t, fmt.Sprintf("bulk_two_safe_%d", i), func() {
			res, err := netutil.NormalizeRefererBulk(pair)
			if err != nil {
				return
			}
			if len(res) < 2 {
				return
			}
			for j := 0; j < len(res); j++ {
				_ = res[j]
			}
		})
		if panicked {
			red = true
			issues = append(issues, fmt.Sprintf("NormalizeRefererBulk(pair#%d) panic: %v", i, rec))
		}
	}

	if red {
		fmt.Println("RED（红灯，缺陷未修复）")
		for _, iss := range issues {
			fmt.Println("  -", iss)
		}
		t.Fatalf("RED（红灯，缺陷未修复）: %d issue(s)", len(issues))
	}

	fmt.Println("GREEN（绿灯，缺陷已修复）")
	t.Logf("GREEN（绿灯，缺陷已修复）")
}
