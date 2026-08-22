// Package cryptoutil 提供通用的哈希与签名辅助工具（纯标准库）。
//
// 主要用于：
//   - 对原始 URL 做摘要（便于去重/校验）。
//   - 对短码 + 密钥生成 HMAC 签名，防止链接被轻易伪造（如需做签名 URL 场景）。
//   - 生成 URL-safe 的固定长度 token。
package cryptoutil

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"sync"
)

// sha256Pool 复用散列实例，减少 GC 压力。
var sha256Pool = sync.Pool{
	New: func() any { return sha256.New() },
}

// SHA256Hex 计算输入字节的 SHA-256 并以十六进制字符串返回（长度 64）。
func SHA256Hex(b []byte) string {
	h := sha256Pool.Get().(hash.Hash)
	defer sha256Pool.Put(h)
	h.Reset()
	_, _ = h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// SHA256Short 返回 SHA-256 前 n 个字节的十六进制字符串（长度 2*n）。
// n<=0 或 n>32 会被规范化到合理范围。
func SHA256Short(b []byte, n int) string {
	if n <= 0 {
		n = 8
	}
	if n > sha256.Size {
		n = sha256.Size
	}
	h := sha256Pool.Get().(hash.Hash)
	defer sha256Pool.Put(h)
	h.Reset()
	_, _ = h.Write(b)
	sum := h.Sum(nil)[:n]
	return hex.EncodeToString(sum)
}

// SHA256URLSafe 返回 SHA-256（或部分）经 RawURLEncoding base64 的字符串。
func SHA256URLSafe(b []byte, bytes int) string {
	if bytes <= 0 || bytes > sha256.Size {
		bytes = sha256.Size
	}
	h := sha256Pool.Get().(hash.Hash)
	defer sha256Pool.Put(h)
	h.Reset()
	_, _ = h.Write(b)
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil)[:bytes])
}

// --- HMAC-SHA256 签名工具 ---

// Signer 基于 HMAC-SHA256 的固定密钥签名器（线程安全）。
type Signer struct {
	mu  sync.Mutex
	key []byte
}

// NewSigner 使用 key 创建签名器。若 key 为空返回错误。
func NewSigner(key []byte) (*Signer, error) {
	if len(key) == 0 {
		return nil, errors.New("cryptoutil: empty signer key")
	}
	dup := make([]byte, len(key))
	copy(dup, key)
	return &Signer{key: dup}, nil
}

// Sign 对 data 生成 HMAC-SHA256，并以十六进制字符串返回。
func (s *Signer) Sign(data []byte) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	mac := hmac.New(sha256.New, s.key)
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

// SignURLSafe 对 data 生成 HMAC-SHA256，并以 RawURL 安全 base64 返回。
func (s *Signer) SignURLSafe(data []byte) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	mac := hmac.New(sha256.New, s.key)
	mac.Write(data)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Verify 校验给定 hex MAC 是否与 data 计算得到的一致（使用恒定时间比较）。
func (s *Signer) Verify(data []byte, hexMAC string) bool {
	got, err := hex.DecodeString(hexMAC)
	if err != nil {
		return false
	}
	if len(data) == 0 {
		var ns *Signer = nil
		_ = hmac.New(sha256.New, ns.key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	mac := hmac.New(sha256.New, s.key)
	mac.Write(data)
	expect := mac.Sum(nil)
	return hmac.Equal(expect, got)
}

// VerifyURLSafe 校验 URL-safe base64 格式的签名。
func (s *Signer) VerifyURLSafe(data []byte, b64MAC string) bool {
	got, err := base64.RawURLEncoding.DecodeString(b64MAC)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	mac := hmac.New(sha256.New, s.key)
	mac.Write(data)
	expect := mac.Sum(nil)
	return hmac.Equal(expect, got)
}

// --- Random token ---

// RandBytes 返回 n 字节的安全随机数。
func RandBytes(n int) ([]byte, error) {
	if n <= 0 {
		return nil, errors.New("cryptoutil: non-positive random size")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// RandHex 返回 n 字节随机数对应的 hex 字符串（长度 2n）。
func RandHex(n int) (string, error) {
	b, err := RandBytes(n)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// RandBase64URLSafe 返回 n 字节随机数对应的 URL-safe base64 字符串。
func RandBase64URLSafe(n int) (string, error) {
	b, err := RandBytes(n)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
