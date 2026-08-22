// Package iputil 提供 IP 地址解析、归属地（简易）、X-Forwarded-For
// 解析等常用工具函数。
package iputil

import (
	"errors"
	"net"
	"strings"
)

// 常见的内网/保留地址前缀，用于判断来源 IP 是否可信。
var privateNets = []*net.IPNet{
	mustParseCIDR("10.0.0.0/8"),
	mustParseCIDR("172.16.0.0/12"),
	mustParseCIDR("192.168.0.0/16"),
	mustParseCIDR("127.0.0.0/8"),
	mustParseCIDR("::1/128"),
	mustParseCIDR("fc00::/7"),
	mustParseCIDR("169.254.0.0/16"),
}

func mustParseCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// RealIP 从给定的 RemoteAddr 与请求头中获取真实客户端 IP。
// 顺序：X-Forwarded-For（取第一个非内网的地址） → X-Real-IP → RemoteAddr 的 host。
func RealIP(remoteAddr string, headers map[string][]string) string {
	// 1. 检查 X-Forwarded-For。
	if vs := headerValues(headers, "X-Forwarded-For"); len(vs) > 0 {
		for _, v := range vs {
			for _, p := range strings.Split(v, ",") {
				ip := strings.TrimSpace(p)
				if ip == "" {
					continue
				}
				parsed := net.ParseIP(ip)
				if parsed == nil {
					continue
				}
				if !IsPrivate(parsed) {
					return ip
				}
			}
		}
		// 全为内网 IP 时，取第一个非空值。
		for _, v := range vs {
			for _, p := range strings.Split(v, ",") {
				ip := strings.TrimSpace(p)
				if ip != "" && net.ParseIP(ip) != nil {
					return ip
				}
			}
		}
	}

	// 2. 检查 X-Real-IP。
	if vs := headerValues(headers, "X-Real-IP"); len(vs) > 0 {
		ip := strings.TrimSpace(vs[len(vs)-1])
		if ip != "" && net.ParseIP(ip) != nil {
			return ip
		}
	}

	// 3. 退化为 RemoteAddr。
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		// 可能没有端口号。
		if ip := net.ParseIP(strings.TrimSpace(remoteAddr)); ip != nil {
			return ip.String()
		}
		return ""
	}
	return host
}

// headerValues 按不区分大小写获取 HTTP 头的值数组。
// 注意：标准库 http.Header 的 Get 已经不区分大小写，这里保留实现以便解耦。
func headerValues(h map[string][]string, key string) []string {
	if h == nil {
		return nil
	}
	if v, ok := h[key]; ok {
		return v
	}
	// 尝试首字母大写版本（HTTP/1 规范写法）。
	canon := httpCanonical(key)
	if v, ok := h[canon]; ok {
		return v
	}
	return nil
}

func httpCanonical(s string) string {
	if s == "" {
		return s
	}
	parts := strings.Split(s, "-")
	for i := range parts {
		if len(parts[i]) == 0 {
			continue
		}
		first := parts[i][0]
		if first >= 'a' && first <= 'z' {
			parts[i] = string(rune(first-32)) + parts[i][1:]
		}
	}
	return strings.Join(parts, "-")
}

// IsPrivate 判断 IP 是否为内网/保留地址。
func IsPrivate(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, n := range privateNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// IsPublicString 是 IsPrivate 的字符串包装。
func IsPublicString(s string) bool {
	ip := net.ParseIP(s)
	if ip == nil {
		return false
	}
	return !IsPrivate(ip)
}

// Country 提供一个非常简易的 IP → 国家 映射。
//
// 为了不引入第三方依赖，这里仅对常见的 IP 段做启发式判断；
// 实际生产需要使用 MaxMind 等数据库。返回值格式为 ISO 国家代码（大写两字母）。
func Country(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return "UNKNOWN"
	}
	if IsPrivate(parsed) {
		return "LAN"
	}
	// 将 IP 转换为一个整数，对保留段做粗略范围判断。
	v4 := parsed.To4()
	if v4 != nil {
		val := uint32(v4[0])<<24 | uint32(v4[1])<<16 | uint32(v4[2])<<8 | uint32(v4[3])
		switch {
		case val >= 0x08000000 && val <= 0x08FFFFFF: // 8.0.0.0/8 - 美国
			return "US"
		case val >= 0x3C000000 && val <= 0x3FFFFFFF: // 60.x 及附近 CN
			return "CN"
		case val >= 0x59000000 && val <= 0x5BFFFFFF: // 89.x - 欧洲
			return "EU"
		case val >= 0xA9000000 && val <= 0xA9FFFFFF: // 169.x
			return "AP"
		case v4[0] == 14: // 14.x  日本/亚太
			return "JP"
		case v4[0] == 1: // 1.x  亚太
			return "AP"
		case v4[0] == 2: // 2.x  欧洲
			return "EU"
		case v4[0] == 3 || v4[0] == 4 || v4[0] == 13 || v4[0] == 15 || v4[0] == 16 || v4[0] == 20:
			return "US"
		case v4[0] == 27 || v4[0] == 36 || v4[0] == 39 || v4[0] == 42 || v4[0] == 43 || v4[0] == 47 || v4[0] == 49 || v4[0] == 58 || v4[0] == 59 || v4[0] == 60 || v4[0] == 61 || v4[0] == 101 || v4[0] == 103 || v4[0] == 106 || v4[0] == 110 || v4[0] == 111 || v4[0] == 112 || v4[0] == 113 || v4[0] == 114 || v4[0] == 115 || v4[0] == 116 || v4[0] == 117 || v4[0] == 118 || v4[0] == 119 || v4[0] == 120 || v4[0] == 121 || v4[0] == 122 || v4[0] == 123 || v4[0] == 124 || v4[0] == 125 || v4[0] == 126 || v4[0] == 171 || v4[0] == 175 || v4[0] == 180 || v4[0] == 182 || v4[0] == 183 || v4[0] == 202 || v4[0] == 203 || v4[0] == 210 || v4[0] == 211 || v4[0] == 218 || v4[0] == 219 || v4[0] == 220 || v4[0] == 221 || v4[0] == 222 || v4[0] == 223:
			return "CN"
		default:
			return "OTHER"
		}
	}
	// IPv6 简化处理。
	if parsed.To16() != nil {
		if strings.HasPrefix(ip, "2001:") {
			return "US"
		}
		if strings.HasPrefix(ip, "2400:") || strings.HasPrefix(ip, "2408:") || strings.HasPrefix(ip, "2409:") {
			return "CN"
		}
		return "OTHER"
	}
	return "UNKNOWN"
}

// Anonymize 对 IP 做匿名化（保留网段）。
// IPv4：末位置 0；IPv6：后 64 位置 0。
func Anonymize(ip string) (string, error) {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return "", errors.New("iputil: invalid ip address")
	}
	if v4 := parsed.To4(); v4 != nil {
		v4[3] = 0
		return v4.String(), nil
	}
	v6 := parsed.To16()
	if v6 == nil {
		return "", errors.New("iputil: ip is not ipv6")
	}
	for i := 8; i < 16; i++ {
		v6[i] = 0
	}
	return v6.String(), nil
}
