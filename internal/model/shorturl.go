package model

import (
	"errors"
	"net/url"
	"strings"
	"time"

	"shurl/pkg/validator"
)

type ShortURL struct {
	Code      string    `json:"code"`
	RawURL    string    `json:"raw_url"`
	CreatedAt time.Time `json:"created_at"`
	ExpireAt  time.Time `json:"expire_at,omitempty"`
	MaxVisits int64     `json:"max_visits,omitempty"`
	Visits    int64     `json:"visits"`
	Custom    bool      `json:"custom"`
	Disabled  bool      `json:"disabled,omitempty"`
	Remark    string    `json:"remark,omitempty"`
}

func (s *ShortURL) IsExpired(now time.Time) bool {
	if s == nil {
		return true
	}
	if s.ExpireAt.IsZero() {
		return false
	}
	return now.After(s.ExpireAt)
}

func (s *ShortURL) ExceedsMaxVisits() bool {
	if s == nil || s.MaxVisits <= 0 {
		return false
	}
	return s.Visits >= s.MaxVisits
}

func (s *ShortURL) IsInvalid(now time.Time) bool {
	if s == nil {
		return true
	}
	return s.Disabled || s.IsExpired(now) || s.ExceedsMaxVisits()
}

func (s *ShortURL) Validate() error {
	if s == nil {
		return errors.New("shorturl: nil pointer")
	}
	if err := ValidateCode(s.Code); err != nil {
		return err
	}
	if err := ValidateRawURL(s.RawURL); err != nil {
		return err
	}
	if !s.ExpireAt.IsZero() && !s.CreatedAt.IsZero() && s.ExpireAt.Before(s.CreatedAt) {
		return errors.New("shorturl: expire_at must be after created_at")
	}
	if s.MaxVisits < 0 {
		return errors.New("shorturl: max_visits must be non-negative")
	}
	return nil
}

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

func ValidateRawURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("shorturl: raw url is empty")
	}
	if len(raw) > 2048 {
		return errors.New("shorturl: raw url too long (max 2048)")
	}
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		return errors.New("shorturl: invalid raw url: " + err.Error())
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("shorturl: raw url scheme must be http or https")
	}
	if u.Host == "" {
		return errors.New("shorturl: raw url missing host")
	}
	return nil
}

type CreateReq struct {
	RawURL        string        `json:"raw_url"`
	CustomCode    string        `json:"custom_code,omitempty"`
	TTL           time.Duration `json:"ttl,omitempty"`
	ExpireAt      time.Time     `json:"expire_at,omitempty"`
	MaxVisits     int64         `json:"max_visits,omitempty"`
	Remark        string        `json:"remark,omitempty"`
	CodeSignature string        `json:"code_signature,omitempty"`
	SignerSalt    string        `json:"signer_salt,omitempty"`
}

func (r *CreateReq) Validate() error {
	if r == nil {
		return errors.New("shorturl: create request is nil")
	}
	if err := ValidateRawURL(r.RawURL); err != nil {
		return err
	}
	if r.CustomCode != "" {
		if err := ValidateCode(r.CustomCode); err != nil {
			return err
		}
	}
	if r.TTL < 0 {
		return errors.New("shorturl: ttl must be non-negative")
	}
	if r.MaxVisits < 0 {
		return errors.New("shorturl: max_visits must be non-negative")
	}
	if err := r.validateCodeSignature(); err != nil {
		return err
	}
	return nil
}

func (r *CreateReq) validateCodeSignature() error {
	if r == nil {
		return nil
	}
	cfg := validator.SignerCfg{
		Key:  []byte(r.SignerSalt),
		Salt: nil,
	}
	policy := validator.DefaultCodeSignaturePolicy()
	policy.RequireSignature = false
	policy.RequirePayload = false
	signature := r.CodeSignature
	payload := r.CustomCode
	if signature == "" {
		sigs := extractPotentialSignature(r.CustomCode)
		if sigs != "" {
			signature = sigs
			payload = ""
		}
	}
	if signature == "" && r.CustomCode == "" {
		return nil
	}
	return validator.VerifyCustomCodeSignature(payload, signature, cfg, policy)
}

func extractPotentialSignature(src string) string {
	if src == "" {
		return ""
	}
	last := strings.LastIndex(src, ".")
	if last < 0 {
		return ""
	}
	tail := src[last+1:]
	if len(tail) < 16 {
		return ""
	}
	for i := 0; i < len(tail); i++ {
		c := tail[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return ""
		}
	}
	return tail
}

type RedirectCheckReq struct {
	Code      string
	Token     string
	Signature string
}

func (r *RedirectCheckReq) Validate(signerKey []byte) error {
	if r == nil {
		return errors.New("shorturl: redirect check is nil")
	}
	if err := ValidateCode(r.Code); err != nil {
		return err
	}
	if r.Token == "" && r.Signature == "" {
		return nil
	}
	cfg := validator.SignerCfg{Key: signerKey, Salt: nil}
	if r.Token != "" {
		_, err := validator.ValidateSignedShortURLToken(r.Token, cfg)
		if err != nil {
			return err
		}
	}
	if r.Signature != "" {
		if !validator.VerifyShortCodeHexSignature([]byte(r.Code), r.Signature, signerKey) {
			return errors.New("shorturl: redirect signature mismatch")
		}
	}
	return nil
}
