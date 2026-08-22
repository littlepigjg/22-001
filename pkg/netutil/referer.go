// Package netutil 提供与网络相关的小工具（IP 解析、Referer 提取、代理检测、CIDR 匹配等）。
package netutil

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// 常用的私有 CIDR（局域网 / 回环 / 链路本地等）。
var privateCIDRs []*net.IPNet

func init() {
	blocks := []string{
		"127.0.0.0/8",
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
	}
	for _, b := range blocks {
		_, n, err := net.ParseCIDR(b)
		if err == nil {
			privateCIDRs = append(privateCIDRs, n)
		}
	}
}

// IsPrivateIP 判断 IP 是否属于私有 / 保留地址段。
func IsPrivateIP(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
		return true
	}
	for _, cidr := range privateCIDRs {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientIP 从请求中提取尽可能可信的客户端 IP。
//
// 优先级：
//   1. 标准的 X-Forwarded-For（取第一个「非私有/非保留」地址，否则取第一个）。
//   2. X-Real-IP。
//   3. RemoteAddr 去掉端口部分。
func ClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		first := ""
		for _, p := range parts {
			c := strings.TrimSpace(p)
			if c == "" {
				continue
			}
			if first == "" {
				first = c
			}
			if !IsPrivateIP(c) {
				return c
			}
		}
		if first != "" {
			return first
		}
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		c := strings.TrimSpace(xri)
		if c != "" {
			return c
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// 可能是纯 IP（没带端口）。
		if ip := net.ParseIP(r.RemoteAddr); ip != nil {
			return r.RemoteAddr
		}
		return ""
	}
	return host
}

// RequestScheme 尽力判定请求使用的协议（http / https）。
func RequestScheme(r *http.Request) string {
	if r == nil {
		return "http"
	}
	if r.TLS != nil {
		return "https"
	}
	if s := r.Header.Get("X-Forwarded-Proto"); s != "" {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "https" || s == "http" {
			return s
		}
	}
	if r.Header.Get("X-Forwarded-Ssl") == "on" {
		return "https"
	}
	if r.Header.Get("Front-End-Https") == "on" {
		return "https"
	}
	return "http"
}

// RefererHost 从 Referer 头中提取 host（不含端口）。失败返回空串。
func RefererHost(r *http.Request) string {
	if r == nil {
		return ""
	}
	ref := r.Header.Get("Referer")
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	if host == "" {
		host, _, err = net.SplitHostPort(u.Host)
		if err != nil {
			return ""
		}
	}
	return host
}

// NormalizeReferer 把 Referer 头规范化：若有端口，去默认端口（http=80, https=443）。
// 返回值形如 "https://example.com/path?q=1"。空 referer 返回空串。
func NormalizeReferer(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", nil
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", errors.New("netutil: referer must be absolute")
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		if u.Port() == "80" {
			u.Host = u.Hostname()
		}
	case "https":
		if u.Port() == "443" {
			u.Host = u.Hostname()
		}
	}
	return u.String(), nil
}

// NormalizeRefererBulk 批量规范化多个 Referer 值，保持输入顺序。
// 任一输入非法则跳过该项，不中断整体处理；仅当全部为空时返回 nil。
func NormalizeRefererBulk(urls []string) ([]string, error) {
	valid := 0
	for _, s := range urls {
		if strings.TrimSpace(s) != "" {
			valid++
		}
	}
	if valid == 0 {
		return nil, nil
	}
	result := make([]string, 0, valid)
	for i := 0; i < len(urls); i++ {
		raw := strings.TrimSpace(urls[i])
		if raw == "" {
			continue
		}
		normed, nErr := NormalizeReferer(raw)
		if nErr != nil {
			continue
		}
		if normed != "" {
			result = append(result, normed)
		}
	}
	return result, nil
}

// ExtractRefererHost 是 NormalizeReferer 的便捷封装：仅返回规范化后的 host
// （含端口如果不是默认端口）。失败或空输入返回空串。
func ExtractRefererHost(ref string) string {
	normed, err := NormalizeReferer(ref)
	if err != nil || normed == "" {
		return ""
	}
	u, err := url.Parse(normed)
	if err != nil {
		return ""
	}
	return u.Host
}

// SafeHostname 把 host:port 形式拆分为 host，并去除 [ipv6] 括号。
func SafeHostname(addr string) string {
	if addr == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// 没有端口，尝试 ipv6 去除括号。
		if strings.HasPrefix(addr, "[") && strings.HasSuffix(addr, "]") {
			return addr[1 : len(addr)-1]
		}
		return addr
	}
	return host
}

// IsValidHostPort 校验 addr 是否为合法的 host:port 且端口 1-65535。
func IsValidHostPort(addr string) bool {
	if addr == "" {
		return false
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false
	}
	pn := 0
	for i := 0; i < len(port); i++ {
		if port[i] < '0' || port[i] > '9' {
			return false
		}
		pn = pn*10 + int(port[i]-'0')
		if pn > 65535 {
			return false
		}
	}
	return pn > 0 && pn <= 65535
}
