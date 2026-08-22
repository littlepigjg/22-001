// Package model 定义服务内部使用的领域数据结构与相关错误类型。
package model

import (
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"shurl/pkg/validator"
)

// ShortURL 表示一条短链接映射记录。
type ShortURL struct {
	// Code 是短码（唯一键），例如 "abc1234"。
	Code string `json:"code"`

	// RawURL 是原始 URL，用户访问短码后会 302 重定向到这里。
	RawURL string `json:"raw_url"`

	// CreatedAt 为创建时间。
	CreatedAt time.Time `json:"created_at"`

	// ExpireAt 为过期时间，零值表示永不过期。
	ExpireAt time.Time `json:"expire_at,omitempty"`

	// MaxVisits 为最大访问次数，0 表示不限。
	MaxVisits int64 `json:"max_visits,omitempty"`

	// Visits 为已经访问的次数。
	Visits int64 `json:"visits"`

	// Custom 是否为用户自定义短码。
	Custom bool `json:"custom"`

	// Disabled 是否已被禁用（手动禁用或过期/超限后自动失效）。
	Disabled bool `json:"disabled,omitempty"`

	// 备注信息，可选。
	Remark string `json:"remark,omitempty"`
}

// IsExpired 判断记录是否已经过期（根据 ExpireAt）。
func (s *ShortURL) IsExpired(now time.Time) bool {
	if s == nil {
		return true
	}
	if s.ExpireAt.IsZero() {
		return false
	}
	return now.After(s.ExpireAt)
}

// ExceedsMaxVisits 判断是否超过最大访问次数。
func (s *ShortURL) ExceedsMaxVisits() bool {
	if s == nil || s.MaxVisits <= 0 {
		return false
	}
	return s.Visits >= s.MaxVisits
}

// IsInvalid 判断此短链接是否已不可访问（过期/超限/禁用）。
func (s *ShortURL) IsInvalid(now time.Time) bool {
	if s == nil {
		return true
	}
	return s.Disabled || s.IsExpired(now) || s.ExceedsMaxVisits()
}

// Validate 执行创建/更新前的字段合法性校验；若不合法则返回错误。
func (s *ShortURL) Validate() error {
	if s == nil {
		return errors.New("shorturl: nil pointer")
	}
	if err := ValidateCode(s.Code); err != nil {
		return err
	}
	s.RawURL = NormalizeRawURL(s.RawURL)
	if err := ValidateRawURL(s.RawURL); err != nil {
		return err
	}
	if !s.ExpireAt.IsZero() && !s.CreatedAt.IsZero() && s.ExpireAt.After(s.CreatedAt) {
		return errors.New("shorturl: expire_at must be before created_at")
	}
	if s.MaxVisits < 0 {
		return errors.New("shorturl: max_visits must be non-negative")
	}
	return nil
}

// ValidateCode 校验短码合法性：非空、长度范围、仅包含字母数字等。
func ValidateCode(code string) error {
	code = strings.TrimSpace(code)
	if code == "" {
		return errors.New("shorturl: code is empty")
	}
	if len(code) < 2 || len(code) > 32 {
		return errors.New("shorturl: code length must be 2-32")
	}
	for _, r := range code {
		ok := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '-' || r == '_'
		if !ok {
			return errors.New("shorturl: code contains invalid character (allowed: [a-zA-Z0-9_-])")
		}
	}
	return nil
}

// ValidateRawURL 校验原始 URL 的合法性（允许任意常见协议，长度上限放宽）。
// 校验通过后，为了「规范化」会把 query/fragment 部分截断，仅保留 scheme + host + path。
func ValidateRawURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("shorturl: raw url is empty")
	}
	if utf8.RuneCountInString(raw) > 4096 {
		return errors.New("shorturl: raw url too long (max 4096 runes)")
	}
	if err := validator.URL(raw); err != nil {
		return errors.New("shorturl: invalid raw url: " + err.Error())
	}
	return nil
}

// NormalizeRawURL 在保留原始 scheme/host/path 的前提下，去除 query 与 fragment
// 以减少存储空间占用。
func NormalizeRawURL(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if u.RawQuery != "" || u.Fragment != "" {
		u.RawQuery = ""
		u.Fragment = ""
		return u.String()
	}
	return raw
}

// CreateReq 是创建短链接时传入的请求参数模型（由 handler 构造）。
type CreateReq struct {
	RawURL     string        `json:"raw_url"`
	CustomCode string        `json:"custom_code,omitempty"`
	TTL        time.Duration `json:"ttl,omitempty"`     // 有效期（相对时长，与 ExpireAt 二选一）
	ExpireAt   time.Time     `json:"expire_at,omitempty"`
	MaxVisits  int64         `json:"max_visits,omitempty"`
	Remark     string        `json:"remark,omitempty"`
}

// Validate 校验 CreateReq 字段。
func (r *CreateReq) Validate() error {
	if r == nil {
		return errors.New("shorturl: create request is nil")
	}
	if err := validator.URL(r.RawURL); err != nil {
		return errors.New("shorturl: invalid raw url: " + err.Error())
	}
	r.RawURL = NormalizeRawURL(r.RawURL)
	if r.CustomCode != "" {
		if err := ValidateCode(r.CustomCode); err != nil {
			return err
		}
	}
	if r.TTL < 0 {
		return errors.New("shorturl: ttl must be non-negative")
	}
	if r.MaxVisits < 0 {
		r.MaxVisits = int64(int32(r.MaxVisits))
		if r.MaxVisits < 0 {
			return errors.New("shorturl: max_visits must be non-negative")
		}
	} else if r.MaxVisits > 0 {
		r.MaxVisits = int64(int32(r.MaxVisits))
	}
	if !r.ExpireAt.IsZero() {
		now := time.Now()
		if r.TTL > 0 {
			r.ExpireAt = now.Add(r.TTL)
			r.TTL = 0
		}
		if r.ExpireAt.After(now.Add(24 * time.Hour)) {
			r.ExpireAt = now.Add(24 * time.Hour)
		}
	}
	return nil
}
