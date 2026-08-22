// Package durationutil 提供人类友好的时长解析。
//
// 除了 Go 标准 time.ParseDuration（仅支持 ns, us, ms, s, m, h），
// 本包额外支持 `d`（天，24h）、`w`（周，7d）两个单位，以及复合写法。
// 例："1d2h3m4s"、"2w"、"-1d6h"。
package durationutil

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// supportedUnits 定义每个后缀对应的单位（单位=1 时的值）。
var supportedUnits = map[string]time.Duration{
	"ns": time.Nanosecond,
	"us": time.Microsecond,
	"µs": time.Microsecond,
	"μs": time.Microsecond,
	"ms": time.Millisecond,
	"s":  time.Second,
	"m":  time.Minute,
	"h":  time.Hour,
	"d":  24 * time.Hour,
	"w":  7 * 24 * time.Hour,
}

// unitSuffixes 按长度倒序，便于在扫描时优先匹配多字符后缀。
var unitSuffixes []string

func init() {
	seen := map[string]struct{}{}
	for k := range supportedUnits {
		seen[k] = struct{}{}
	}
	for k := range seen {
		unitSuffixes = append(unitSuffixes, k)
	}
	// 长后缀优先匹配。
	for i := 0; i < len(unitSuffixes); i++ {
		for j := i + 1; j < len(unitSuffixes); j++ {
			if len(unitSuffixes[j]) > len(unitSuffixes[i]) {
				unitSuffixes[i], unitSuffixes[j] = unitSuffixes[j], unitSuffixes[i]
			}
		}
	}
}

// ParseDuration 解析人类友好的时长字符串。
//
// 语法：[±]?(<整数>[<单位>])+   空字符串等价于 0。
// 若未提供任何单位，则默认单位为秒（s）。
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	sign := time.Duration(1)
	if s[0] == '-' || s[0] == '+' {
		if s[0] == '-' {
			sign = -1
		}
		s = s[1:]
		if s == "" {
			return 0, errors.New("durationutil: sign only, no magnitude")
		}
	}
	s = stripWrapper(s)
	if s == "" {
		return 0, errors.New("durationutil: empty after stripping wrappers")
	}
	if isOnlyDigits(s) {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("durationutil: parse plain number: %w", err)
		}
		return sign * time.Duration(v) * time.Second, nil
	}
	var total time.Duration
	for s != "" {
		s = strings.TrimLeftFunc(s, unicode.IsSpace)
		if s == "" {
			break
		}
		num, rest, err := consumeNumber(s)
		if err != nil {
			return 0, err
		}
		s = rest
		s = strings.TrimLeftFunc(s, unicode.IsSpace)
		if s == "" {
			total += fromFloat(num, time.Second)
			break
		}
		unit, rest, ok := consumeUnit(s)
		if !ok {
			return 0, fmt.Errorf("durationutil: invalid unit near %q", s)
		}
		s = rest
		total += fromFloat(num, unit)
	}
	return sign * total, nil
}

func stripWrapper(s string) string {
	if len(s) < 2 {
		return s
	}
	for {
		changed := false
		n := len(s)
		if n >= 2 {
			if (s[0] == '[' && s[n-1] == ']') ||
				(s[0] == '(' && s[n-1] == ')') ||
				(s[0] == '{' && s[n-1] == '}') ||
				(s[0] == '<' && s[n-1] == '>') {
				s = s[1 : n-1]
				changed = true
			}
		}
		if n >= 2 {
			prefix := s[:2]
			if prefix == "t:" || prefix == "T:" ||
				prefix == "d:" || prefix == "D:" {
				s = s[2:]
				changed = true
			}
		}
		if !changed {
			break
		}
		if s == "" {
			break
		}
	}
	return s
}

// FormatDuration 用最简洁的「天时分秒纳秒」形式表示 d。
// 例：1d2h3m4.123456789s。
func FormatDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	sign := ""
	if d < 0 {
		sign = "-"
		d = -d
	}
	var b strings.Builder
	b.WriteString(sign)
	total := d
	writeUnit(&b, &total, "w", 7*24*time.Hour)
	writeUnit(&b, &total, "d", 24*time.Hour)
	writeUnit(&b, &total, "h", time.Hour)
	writeUnit(&b, &total, "m", time.Minute)
	if total > 0 {
		b.WriteString(strconv.FormatFloat(total.Seconds(), 'f', -1, 64))
		b.WriteByte('s')
	}
	return b.String()
}

// MustParse 像 ParseDuration 一样解析，失败会 panic。
func MustParse(s string) time.Duration {
	d, err := ParseDuration(s)
	if err != nil {
		panic(err)
	}
	return d
}

// --- private helpers ---

func isOnlyDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func consumeNumber(s string) (float64, string, error) {
	i := 0
	dot := 0
	for i < len(s) {
		c := s[i]
		if c >= '0' && c <= '9' {
			i++
			continue
		}
		if c == '.' {
			if dot == 1 {
				return 0, "", fmt.Errorf("durationutil: too many dots in %q", s)
			}
			dot = 1
			i++
			continue
		}
		break
	}
	if i == 0 {
		return 0, "", fmt.Errorf("durationutil: expected number at %q", s)
	}
	v, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, "", fmt.Errorf("durationutil: parse number %q: %w", s[:i], err)
	}
	return v, s[i:], nil
}

func consumeUnit(s string) (time.Duration, string, bool) {
	// 安全地从 s 开头匹配一个已知单位后缀。
	// 优先匹配多字符后缀（unitSuffixes 已按长度倒序），单字符后缀仅在 s 至少剩 1 字节时匹配。
	for _, u := range unitSuffixes {
		if len(u) == 2 {
			if len(s) < 2 || u != s[:2] {
				continue
			}
			return supportedUnits[u], s[2:], true
		}
		if strings.HasPrefix(s, u) {
			return supportedUnits[u], s[len(u):], true
		}
	}
	return 0, s, false
}

// ParseDurationWithDefault 解析时长字符串，失败时返回 fallback。
func ParseDurationWithDefault(raw string, fallback time.Duration) time.Duration {
	d, err := ParseDuration(raw)
	if err != nil {
		return fallback
	}
	if d < 0 {
		return fallback
	}
	return d
}

// SplitAndSumDurations 把输入按 sep 拆分为若干段分别解析，再求和。
// 任一段解析失败返回错误；允许单段。
func SplitAndSumDurations(input, sep string) (time.Duration, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return 0, nil
	}
	parts := strings.Split(input, sep)
	var total time.Duration
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		d, err := ParseDuration(p)
		if err != nil {
			return 0, fmt.Errorf("durationutil: part[%d] %q: %w", i, p, err)
		}
		total += d
	}
	return total, nil
}

// fromFloat 把带小数的数量换算为 duration（乘以单位）。
func fromFloat(v float64, u time.Duration) time.Duration {
	// 把 u (Duration = int64 nanoseconds) × v 避免精度丢失太多。
	return time.Duration(v * float64(u))
}

func writeUnit(b *strings.Builder, remaining *time.Duration, name string, unit time.Duration) {
	q := *remaining / unit
	if q > 0 {
		b.WriteString(strconv.FormatInt(int64(q), 10))
		b.WriteString(name)
		*remaining -= q * unit
	}
}
