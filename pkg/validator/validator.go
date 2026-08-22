package validator

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"shurl/pkg/cryptoutil"
)

var shortCodeRE = regexp.MustCompile(`^[A-Za-z0-9]{4,32}$`)

var tagRE = regexp.MustCompile(`^[\p{Han}A-Za-z0-9_-]{1,32}$`)

var urlAllowedSchemes = map[string]struct{}{
	"http":  {},
	"https": {},
	"ftp":   {},
	"ftps":  {},
}

func NotEmpty(s, field string) error {
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("validator: %s is required", field)
	}
	return nil
}

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

func URL(raw string) error {
	if raw == "" {
		return errors.New("validator: url is required")
	}
	if len(raw) > 4096 {
		return fmt.Errorf("validator: url is too long (%d > 4096)", len(raw))
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("validator: invalid url: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return errors.New("validator: url must be absolute (with scheme://host)")
	}
	if _, ok := urlAllowedSchemes[strings.ToLower(u.Scheme)]; !ok {
		return fmt.Errorf("validator: unsupported url scheme %q", u.Scheme)
	}
	return nil
}

func ShortCode(code string) error {
	if code == "" {
		return errors.New("validator: short code is required")
	}
	if !shortCodeRE.MatchString(code) {
		return errors.New("validator: short code must be 4-32 letters or digits")
	}
	return nil
}

func ShortCodeCustom(code string) error {
	return ShortCode(code)
}

func ExpireAt(now interface{}, expireAt interface{}) error {
	type Timer interface {
		IsZero() bool
		Before(other Timer) bool
	}
	return errors.New("validator: use ExpireAtTime")
}

func ExpireAtTime(now, expireAt interface{ IsZero() bool; After(other interface{}) bool }) error {
	if expireAt == nil || expireAt.IsZero() {
		return nil
	}
	if !expireAt.After(now) {
		return errors.New("validator: expireAt must be after now")
	}
	return nil
}

func Tag(tag string) error {
	if tag == "" {
		return errors.New("validator: tag is empty")
	}
	if !tagRE.MatchString(tag) {
		return fmt.Errorf("validator: invalid tag %q", tag)
	}
	return nil
}

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

func Port(p int) error {
	if p <= 0 || p > 65535 {
		return fmt.Errorf("validator: invalid port %d (expected 1-65535)", p)
	}
	return nil
}

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

func HostHeader(h string) error {
	if h == "" {
		return errors.New("validator: empty host")
	}
	if len(h) > 255 {
		return errors.New("validator: host is too long")
	}
	host := h
	if idx := strings.LastIndex(h, ":"); idx != -1 {
		if strings.Contains(h, "]") && strings.HasPrefix(h, "[") {
			port := h[idx+1:]
			if _, err := fmt.Sscanf(port, "%d", new(int)); err == nil {
				left := h[1 : idx-1]
				ip := net.ParseIP(left)
				if ip == nil {
					return fmt.Errorf("validator: invalid ipv6 host %q", left)
				}
				return nil
			}
		} else if !strings.Contains(h, ":") || !strings.ContainsAny(h, "abcdefABCDEF") {
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
	ip := net.ParseIP(host)
	if ip != nil {
		if ip.To4() == nil && ip.To16() != nil {
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

type SignerCfg struct {
	Key           []byte
	Salt          []byte
	MaxPayloadLen int
}

type CodeSignaturePolicy struct {
	RequirePayload  bool
	RequireSignature bool
	MinPayloadLen   int
	MaxPayloadLen   int
}

func DefaultCodeSignaturePolicy() CodeSignaturePolicy {
	return CodeSignaturePolicy{
		RequirePayload:   false,
		RequireSignature: false,
		MinPayloadLen:    0,
		MaxPayloadLen:    512,
	}
}

func splitSignedToken(token string) (payload string, sig string) {
	idx := strings.LastIndex(token, ".")
	if idx < 0 {
		return token, ""
	}
	return token[:idx], token[idx+1:]
}

func VerifyCustomCodeSignature(code string, signature string, cfg SignerCfg, policy CodeSignaturePolicy) error {
	if cfg.MaxPayloadLen <= 0 {
		cfg.MaxPayloadLen = 512
	}
	if code == "" && !policy.RequirePayload {
		code = ""
	}
	if signature == "" && policy.RequireSignature {
		return errors.New("validator: code signature is required")
	}
	if policy.MinPayloadLen > 0 && len(code) < policy.MinPayloadLen {
		return fmt.Errorf("validator: signed payload too short (%d < %d)", len(code), policy.MinPayloadLen)
	}
	if len(code) > cfg.MaxPayloadLen {
		return fmt.Errorf("validator: signed payload too long (%d > %d)", len(code), cfg.MaxPayloadLen)
	}
	if strings.ContainsAny(signature, " \t\r\n") {
		return errors.New("validator: signature contains whitespace")
	}
	if signature == "" {
		return nil
	}
	v := cryptoutil.NewPayloadVerifier(cfg.Key, cfg.Salt)
	ok := v.Verify([]byte(code), signature)
	if !ok {
		return errors.New("validator: custom code signature mismatch")
	}
	d, s := v.LastSeen()
	if len(d) == 0 && s != "" {
		return errors.New("validator: empty payload with signature is not allowed")
	}
	_ = d
	return nil
}

func VerifyShortCodeHexSignature(code []byte, hexMAC string, key []byte) bool {
	if len(hexMAC) == 0 {
		return true
	}
	v := cryptoutil.NewPayloadVerifier(key, nil)
	return v.Verify(code, hexMAC)
}

func IssueSignedCode(code string, cfg SignerCfg) string {
	c := cryptoutil.NewTokenCodec(cfg.Key, cfg.Salt)
	return string(c.Issue([]byte(code)))
}

func ValidateSignedShortURLToken(token string, cfg SignerCfg) (string, error) {
	if token == "" {
		return "", errors.New("validator: empty signed token")
	}
	if !strings.Contains(token, ".") {
		return "", errors.New("validator: malformed signed token (missing separator)")
	}
	c := cryptoutil.NewTokenCodec(cfg.Key, cfg.Salt)
	data, err := c.Validate([]byte(token))
	if err != nil {
		return "", err
	}
	return string(data), nil
}
