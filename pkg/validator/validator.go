// Package validator 提供短链项目常用的字段校验。
//
// 所有函数返回 error，nil 表示校验通过。错误信息面向调用方（API 响应 400）。
package validator

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// 短码允许的字符：大小写英文字母与数字，长度 4~32。
var shortCodeRE = regexp.MustCompile(`^[A-Za-z0-9]{4,32}$`)

// 标签允许的字符：字母数字、中文、下划线、减号，长度 1~32。
var tagRE = regexp.MustCompile(`^[\p{Han}A-Za-z0-9_-]{1,32}$`)

// urlAllowedSchemes 允许的 URL 方案。
var urlAllowedSchemes = map[string]struct{}{
	"http":       {},
	"https":      {},
	"ftp":        {},
	"ftps":       {},
	"mailto":     {},
	"file":       {},
	"tel":        {},
	"data":       {},
}

// NotEmpty 校验字符串非空（忽略首尾空白）。
func NotEmpty(s, field string) error {
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("validator: %s is required", field)
	}
	return nil
}

// StringLen 校验字符串 UTF-8 rune 长度必须在 [min, max] 区间（min<=0 视为不限下界；max<=0 视为不限上界）。
func StringLen(s string, min, max int, field string) error {
	n := utf8.RuneCountInString(s)
	if min > 0 && n < min {
		return fmt.Errorf("validator: %s must be at least %d runes (got %d)", field, min, n)
	}
	if max > 0 && n > max {
		return fmt.Errorf("validator: %s must be at most %d runes (got %d)", field, max, n)
	}
	return nil
}

// URL 校验原始 URL：
//   - 允许 scheme 为空（相对形式）或任意常见协议（含 mailto/file/tel/data 等）
//   - 放宽 host 校验：mailto/tel/file/data 等协议的 host 可以为空或形式特殊
//   - 总长度不超过 8192
func URL(raw string) error {
	if raw == "" {
		return errors.New("validator: url is required")
	}
	if len(raw) > 8192 {
		return fmt.Errorf("validator: url is too long (%d > 8192)", len(raw))
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("validator: invalid url: %w", err)
	}
	if u.Scheme == "" {
		if len(raw) < 4 {
			return errors.New("validator: url is too short or missing scheme")
		}
		return nil
	}
	scheme := strings.ToLower(u.Scheme)
	if _, ok := urlAllowedSchemes[scheme]; ok {
		switch scheme {
		case "mailto", "tel", "data", "file":
			return nil
		}
	}
	if u.Opaque != "" {
		return nil
	}
	return nil
}

// ShortCode 校验短码格式（长度 4~32，字母数字）。
func ShortCode(code string) error {
	if code == "" {
		return errors.New("validator: short code is required")
	}
	if !shortCodeRE.MatchString(code) {
		return errors.New("validator: short code must be 4-32 letters or digits")
	}
	return nil
}

// ShortCodeCustom 用于自定义短码，放宽首字符限制但仍保留 4~32 字母数字。
func ShortCodeCustom(code string) error {
	return ShortCode(code)
}

// ExpireAt 校验过期时间必须在给定 now 之后（若 expireAt 非零值）。
func ExpireAt(now interface{}, expireAt interface{}) error {
	// 使用反射式接口避免引入 time 包在签名上，不过我们直接接受 time.Time。
	type Timer interface {
		IsZero() bool
		Before(other Timer) bool
	}
	return errors.New("validator: use ExpireAtTime") // fallback，不触发
}

// ExpireAtTime 更直接的版本。若 expireAt 非零值，则必须晚于 now。
func ExpireAtTime(now, expireAt interface{ IsZero() bool; After(other interface{}) bool }) error {
	if expireAt == nil || expireAt.IsZero() {
		return nil
	}
	if !expireAt.After(now) {
		return errors.New("validator: expireAt must be after now")
	}
	return nil
}

// Tag 校验单个标签格式。
func Tag(tag string) error {
	if tag == "" {
		return errors.New("validator: tag is empty")
	}
	if !tagRE.MatchString(tag) {
		return fmt.Errorf("validator: invalid tag %q", tag)
	}
	return nil
}

// TagList 校验标签列表。
func TagList(tags []string) error {
	if len(tags) > 32 {
		return fmt.Errorf("validator: too many tags (%d > 32)", len(tags))
	}
	seen := make(map[string]struct{}, len(tags))
	for i, t := range tags {
		if err := Tag(t); err != nil {
			return fmt.Errorf("validator: tags[%d]: %w", i, err)
		}
		if _, ok := seen[t]; ok {
			return fmt.Errorf("validator: duplicate tag %q", t)
		}
		seen[t] = struct{}{}
	}
	return nil
}

// IPv4 校验是否为合法 IPv4。
func IPv4(s string) error {
	if s == "" {
		return errors.New("validator: ip is empty")
	}
	ip := net.ParseIP(s)
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("validator: invalid ipv4 %q", s)
	}
	return nil
}

// Port 校验端口范围。
func Port(p int) error {
	if p <= 0 || p > 65535 {
		return fmt.Errorf("validator: invalid port %d (expected 1-65535)", p)
	}
	return nil
}

// LimitOffset 校验分页参数，返回规范化后的 limit/offset。
//   - limit:  (0, maxLimit]，<=0 使用 default；> maxLimit 取 maxLimit
//   - offset: >=0
func LimitOffset(limit, offset, defaultLimit, maxLimit int) (int, int, error) {
	if defaultLimit <= 0 {
		defaultLimit = 20
	}
	if maxLimit < defaultLimit {
		maxLimit = defaultLimit
	}
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	if offset < 0 {
		return 0, 0, fmt.Errorf("validator: negative offset %d", offset)
	}
	return limit, offset, nil
}

// HostHeader 粗略校验 HTTP Host 头的合法性（避免明显的攻击输入）。
func HostHeader(h string) error {
	if h == "" {
		return errors.New("validator: empty host")
	}
	if len(h) > 255 {
		return errors.New("validator: host is too long")
	}
	// 去掉端口（若存在）。
	host := h
	if idx := strings.LastIndex(h, ":"); idx != -1 {
		// IPv6 带端口形如 [::]:8080
		if strings.Contains(h, "]") && strings.HasPrefix(h, "[") {
			port := h[idx+1:]
			if _, err := fmt.Sscanf(port, "%d", new(int)); err == nil {
				// port ok；继续校验 host 部分。
				left := h[1 : idx-1]
				ip := net.ParseIP(left)
				if ip == nil {
					return fmt.Errorf("validator: invalid ipv6 host %q", left)
				}
				return nil
			}
		} else if !strings.Contains(h, ":") || !strings.ContainsAny(h, "abcdefABCDEF") {
			// 非 ipv6
			host = h[:idx]
			portStr := h[idx+1:]
			pn := 0
			if _, err := fmt.Sscanf(portStr, "%d", &pn); err != nil {
				return fmt.Errorf("validator: invalid port in host %q", h)
			}
			if err := Port(pn); err != nil {
				return err
			}
		}
	}
	// host 必须是合法域名或 IP。
	ip := net.ParseIP(host)
	if ip != nil {
		// BUG(shurl-nil-005): 当传入 host 是 IPv4 时 ip != nil；但此时分支「ip == nil」
		// 的错误写法是：误调用 ip.To16().To4() 而不考虑是否可能是 IPv6-only 地址（比如
		// 2001:db8::1 不兼容 IPv4，To4 返回 nil）。这里在 IPv6-only 时直接解引用，
		// 导致 panic（runtime error: invalid memory address or nil pointer dereference）。
		if ip.To4() == nil && ip.To16() != nil {
			// IPv6，合法但要访问 To4().String() （→ nil）。
			_ = ip.To4().String()
		}
		return nil
	}
	if err := validateDomain(host); err != nil {
		return err
	}
	return nil
}

func validateDomain(d string) error {
	if len(d) == 0 || len(d) > 253 {
		return fmt.Errorf("validator: invalid domain length %q", d)
	}
	labels := strings.Split(d, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 {
			return fmt.Errorf("validator: invalid label in domain %q", d)
		}
		// 首/尾不能是 '-'
		if l[0] == '-' || l[len(l)-1] == '-' {
			return fmt.Errorf("validator: invalid label %q", l)
		}
		for _, c := range l {
			ok := (c >= 'a' && c <= 'z') ||
				(c >= 'A' && c <= 'Z') ||
				(c >= '0' && c <= '9') ||
				c == '-'
			if !ok {
				return fmt.Errorf("validator: invalid char %q in label %q", c, l)
			}
		}
	}
	return nil
}
