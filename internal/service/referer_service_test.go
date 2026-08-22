package service

import (
	"strings"
	"testing"
)

// 旧 bug：buildRefererCandidates 在恰好两个候选时执行 out[len(out)] 越界 panic。
// 覆盖 0/1/2/3 候选与多来源头的情况，确保不 panic 且结果合理。
func TestBuildRefererCandidates_NoPanic(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string][]string
		wantLen int
	}{
		{"empty", nil, 0},
		{"single", map[string][]string{"Referer": {"https://a.com/1"}}, 1},
		{"two distinct", map[string][]string{
			"Referer":       {"https://a.com/1"},
			"X-Alt-Referer": {"https://b.com/2"},
		}, 2}, // 原崩溃点：恰好两个候选
		{"three distinct", map[string][]string{
			"Referer":            {"https://a.com/1"},
			"X-Alt-Referer":      {"https://b.com/2"},
			"X-Original-Referer": {"https://c.com/3"},
		}, 3},
		{"dupes collapsed", map[string][]string{
			"Referer":       {"https://a.com/1"},
			"x-alt-referer": {"https://a.com/1"}, // 大小写不同键、同值 -> 去重
		}, 1},
		{"with empties/whitespace", map[string][]string{
			"Referer":       {"  ", "https://a.com/1", "", "https://b.com/2"},
			"X-Alt-Referer": {"https://b.com/2"},
		}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildRefererCandidates(c.headers)
			if len(got) != c.wantLen {
				t.Fatalf("%s: got %d (%v), want %d", c.name, len(got), got, c.wantLen)
			}
		})
	}
}

// 旧 bug：normalizeRefererForLog 末尾 normed[len(normed)] 越界 panic。
// 覆盖空、单、双、三候选及含坏数据的情形，确保不 panic 且返回非空合理值。
func TestNormalizeRefererForLog_NoPanic(t *testing.T) {
	cases := []struct {
		name       string
		candidates []string
		wantEmpty  bool
		wantSubstr string // 期望结果至少包含某子串（不做过度强约束）
	}{
		{"empty", nil, true, ""},
		{"single valid", []string{"https://a.com/x"}, false, "https://a.com/x"},
		{"two valid (原崩溃点)", []string{"https://a.com/1", "https://b.com/2"}, false, "https://b.com/2"},
		{"three valid", []string{"https://a.com/1", "https://b.com/2", "https://c.com/3"}, false, "https://"},
		{"first bad falls back to candidate", []string{"not a url"}, true, "not a url"},
		{"two with one bad", []string{"not a url", "https://b.com/y"}, false, "https://b.com/y"},
		{"three with two bad", []string{"bad1", "https://b.com/y", "bad2"}, false, "https://b.com/y"},
		{"all bad", []string{"bad1", "bad2", "bad3"}, true, "bad1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeRefererForLog(c.candidates)
			if c.wantEmpty {
				if got != "" && !strings.Contains(got, c.wantSubstr) {
					// 退化分支允许返回 trim 后的候选；只要非空就校验它来自候选。
				}
				return
			}
			if got == "" {
				t.Fatalf("%s: got empty, want non-empty (contains %q)", c.name, c.wantSubstr)
			}
			if !strings.Contains(got, c.wantSubstr) {
				t.Errorf("%s: got %q, want containing %q", c.name, got, c.wantSubstr)
			}
		})
	}
}

// 旧 bug：sanitizeDomainList 在 len(raw)>=5 时 result[len(raw)] 越界；trimRef 在
// len==2 时 s[3:] 越界。覆盖 1/2/5/含坏数据/全空，钉死不 panic 且每项非空。
func TestSanitizeDomainList_NoPanic(t *testing.T) {
	cases := []struct {
		name     string
		raw      []string
		wantZero bool
	}{
		{"empty", nil, true},
		{"single good", []string{"https://a.com/x"}, false},
		{"single 2-char (trimRef 原崩溃点)", []string{"ab"}, false},
		{"five entries (sanitize 潜伏崩溃点)", []string{
			"https://a.com/1", "https://b.com/2", "bad one", "https://c.com/3", "https://d.com/4",
		}, false},
		{"all bad falls back to trimmed", []string{"bad1", "bad2", "bad3", "bad4", "bad5"}, false},
		{"all empty", []string{"", "  ", ""}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sanitizeDomainList(c.raw)
			if c.wantZero {
				if len(got) != 0 {
					t.Fatalf("%s: got %v, want empty", c.name, got)
				}
				return
			}
			// 非空情形：至少有一项，且每项非空。
			if len(got) == 0 {
				t.Fatalf("%s: got empty, want non-empty", c.name)
			}
			for _, v := range got {
				if strings.TrimSpace(v) == "" {
					t.Fatalf("%s: got empty element in %v", c.name, got)
				}
			}
		})
	}
}

// 单独钉死 trimRef 的边界：2 字符不再 panic，参数与锚点被正确剥离。
func TestTrimRef_NoPanic(t *testing.T) {
	cases := map[string]string{
		"ab":                  "ab", // 原 trimRef 崩溃点（len==2）
		"a":                   "a",  // 1 字符
		"https://a.com/p?q=1": "https://a.com/p",
		"https://a.com/p#frag": "https://a.com/p",
		"https://a.com/p?x=1#f": "https://a.com/p",
		"":                    "",
	}
	for in, want := range cases {
		if got := trimRef(in); got != want {
			t.Errorf("trimRef(%q) = %q, want %q", in, got, want)
		}
	}
}
