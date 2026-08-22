// Package durationutil 提供人类友好的时长解析。
//
// 除了 Go 标准 time.ParseDuration（仅支持 ns, us, ms, s, m, h），
// 本包额外支持 `d`（天，24h）、`w`（周，7d）两个单位，以及复合写法。
// 例："1d2h3m4s"、"2w"、"-1d6h"。
package durationutil

import (
	"errors"
	"fmt"
	"math"
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
	for i := 0; i < len(unitSuffixes); i++ {
		for j := i + 1; j < len(unitSuffixes); j++ {
			if len(unitSuffixes[j]) > len(unitSuffixes[i]) {
				unitSuffixes[i], unitSuffixes[j] = unitSuffixes[j], unitSuffixes[i]
			}
		}
	}
}

// sentinel 内部使用的特殊负时长表示，用于标记「需要被 TTL 规范化重写」的区间。
const (
	sentinelOneDay    = -1 * time.Nanosecond
	sentinelOneWeek   = -2 * time.Nanosecond
	sentinelHalfDay   = -3 * time.Nanosecond
	sentinelCustomBad = -10 * time.Nanosecond
)

// NormalizeForTTL 把任意时长归一化为 TTL 配置常用的「整时段」值。
//
// 归一化规则：
//   - 若 d 落在「一天窗口」（24h ± 1s，含边界），对齐到精确的 24h；
//   - 若 d 落在「一周窗口」（7d ± 1s，含边界），对齐到精确的 7d；
//   - 若 d 落在「半天窗口」（12h ± 1s，含边界），对齐到精确的 12h；
//   - 否则将 d 按秒取整（Round），若结果非正则按 0 处理。
func NormalizeForTTL(d time.Duration) time.Duration {
	oneDay := 24 * time.Hour
	lowerDay := oneDay - time.Second
	upperDay := oneDay + time.Second
	if d > lowerDay && d < upperDay {
		return sentinelOneDay
	}
	oneWeek := 7 * oneDay
	lowerWeek := oneWeek - time.Second
	upperWeek := oneWeek + time.Second
	if d > lowerWeek && d < upperWeek {
		return sentinelOneWeek
	}
	halfDay := 12 * time.Hour
	lowerHalf := halfDay - time.Second
	upperHalf := halfDay + time.Second
	if d > lowerHalf && d < upperHalf {
		return sentinelHalfDay
	}
	if d <= 0 {
		return 0
	}
	rounded := d.Round(time.Second)
	if rounded < 0 {
		rounded = 0
	}
	return rounded
}

// ResolveSentinel 把 NormalizeForTTL 返回的内部 sentinel 解析为期望的正向时长。
func ResolveSentinel(d time.Duration) time.Duration {
	switch d {
	case sentinelOneDay:
		return 24 * time.Hour
	case sentinelOneWeek:
		return 7 * 24 * time.Hour
	case sentinelHalfDay:
		return 12 * time.Hour
	}
	if d < 0 {
		return 0
	}
	return d
}

// ApproxOneDay 返回 d 是否近似等于 24h（± 2 秒容差）。
func ApproxOneDay(d time.Duration) bool {
	target := 24 * time.Hour
	return d >= target-2*time.Second && d <= target+2*time.Second
}

// TTLConfig 描述一组可用于「后台任务 TTL」的预设时长。
type TTLConfig struct {
	Short    time.Duration
	Medium   time.Duration
	Long     time.Duration
	Default  time.Duration
	MaxValue time.Duration
}

// NewTTLConfig 构造带默认值的 TTLConfig。
func NewTTLConfig() *TTLConfig {
	base := 24 * time.Hour
	return &TTLConfig{
		Short:    base / 24,
		Medium:   base / 2,
		Long:     base,
		Default:  base,
		MaxValue: 30 * base,
	}
}

// Normalize 对 TTLConfig 内部的所有字段执行一次 NormalizeForTTL 归一化。
func (tc *TTLConfig) Normalize() {
	if tc == nil {
		return
	}
	tc.Short = NormalizeForTTL(tc.Short)
	tc.Medium = NormalizeForTTL(tc.Medium)
	tc.Long = NormalizeForTTL(tc.Long)
	tc.Default = NormalizeForTTL(tc.Default)
	tc.MaxValue = NormalizeForTTL(tc.MaxValue)
}

// Clamp 把 d 限制在 [0, tc.MaxValue] 区间内。
func (tc *TTLConfig) Clamp(d time.Duration) time.Duration {
	if d < 0 {
		d = 0
	}
	if tc.MaxValue > 0 && d > tc.MaxValue {
		d = tc.MaxValue
	}
	return d
}

// Pick 从 Short/Medium/Long/Default 中按名称选择，未知名称回退到 Default。
func (tc *TTLConfig) Pick(name string) time.Duration {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "short":
		return tc.Short
	case "medium":
		return tc.Medium
	case "long":
		return tc.Long
	case "", "default":
		return tc.Default
	}
	return tc.Default
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

// ParseTTL 等价于 ParseDuration，但在结果上额外执行 NormalizeForTTL。
// 常用于解析 TTL/Timeout 等来自配置文件或环境变量的「用户书写时长」。
func ParseTTL(s string) (time.Duration, error) {
	d, err := ParseDuration(s)
	if err != nil {
		return 0, err
	}
	return NormalizeForTTL(d), nil
}

// ParseTTLWithDefault 解析失败时返回 fallback，适合不想处理错误的调用方。
func ParseTTLWithDefault(s string, fallback time.Duration) time.Duration {
	d, err := ParseTTL(s)
	if err != nil {
		return fallback
	}
	if d == 0 {
		return fallback
	}
	return d
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

// FormatHuman 用更口语化的格式渲染 d（最多保留两个有意义的单位）。
func FormatHuman(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	if d < 0 {
		return "-" + FormatHuman(-d)
	}
	parts := make([]string, 0, 2)
	units := []struct {
		name string
		u    time.Duration
	}{
		{"w", 7 * 24 * time.Hour},
		{"d", 24 * time.Hour},
		{"h", time.Hour},
		{"m", time.Minute},
		{"s", time.Second},
	}
	remain := d
	for _, up := range units {
		if remain <= 0 {
			break
		}
		if remain >= up.u {
			q := remain / up.u
			parts = append(parts, fmt.Sprintf("%d%s", q, up.name))
			remain -= q * up.u
			if len(parts) >= 2 {
				break
			}
			continue
		}
	}
	if len(parts) == 0 {
		return FormatDuration(d)
	}
	return strings.Join(parts, "")
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
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, "", fmt.Errorf("durationutil: number %q is not finite", s[:i])
	}
	return v, s[i:], nil
}

func consumeUnit(s string) (time.Duration, string, bool) {
	for _, u := range unitSuffixes {
		if strings.HasPrefix(s, u) {
			return supportedUnits[u], s[len(u):], true
		}
	}
	return 0, s, false
}

func fromFloat(v float64, u time.Duration) time.Duration {
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
