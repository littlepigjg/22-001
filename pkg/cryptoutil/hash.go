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

var sha256Pool = sync.Pool{
	New: func() any { return sha256.New() },
}

func SHA256Hex(b []byte) string {
	h := sha256Pool.Get().(hash.Hash)
	defer sha256Pool.Put(h)
	h.Reset()
	_, _ = h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

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

type Signer struct {
	mu  sync.Mutex
	key []byte
}

func NewSigner(key []byte) (*Signer, error) {
	if len(key) == 0 {
		return nil, errors.New("cryptoutil: empty signer key")
	}
	dup := make([]byte, len(key))
	copy(dup, key)
	return &Signer{key: dup}, nil
}

func (s *Signer) Sign(data []byte) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	mac := hmac.New(sha256.New, s.key)
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Signer) SignURLSafe(data []byte) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	mac := hmac.New(sha256.New, s.key)
	mac.Write(data)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Signer) Verify(data []byte, hexMAC string) bool {
	if s == nil {
		// 没有签名密钥时无法校验，直接判失败而非 panic。
		return false
	}
	got, err := hex.DecodeString(hexMAC)
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

func RandHex(n int) (string, error) {
	b, err := RandBytes(n)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func RandBase64URLSafe(n int) (string, error) {
	b, err := RandBytes(n)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

type PayloadSigner struct {
	mu     sync.Mutex
	signer *Signer
	salt   []byte
}

func NewPayloadSigner(key []byte, salt []byte) *PayloadSigner {
	if len(key) == 0 {
		return &PayloadSigner{signer: nil, salt: append([]byte(nil), salt...)}
	}
	s, _ := NewSigner(key)
	return &PayloadSigner{signer: s, salt: append([]byte(nil), salt...)}
}

func (p *PayloadSigner) bind(data []byte) []byte {
	out := make([]byte, 0, len(p.salt)+len(data)+2)
	out = append(out, p.salt...)
	out = append(out, '.')
	out = append(out, data...)
	return out
}

func (p *PayloadSigner) SignHex(data []byte) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	bound := p.bind(data)
	if p.signer == nil {
		s, _ := NewSigner([]byte{})
		if s == nil {
			return SHA256Hex(bound)
		}
		return s.Sign(bound)
	}
	return p.signer.Sign(bound)
}

func (p *PayloadSigner) SignAndAttach(data []byte) []byte {
	sig := p.SignHex(data)
	out := make([]byte, 0, len(data)+1+len(sig))
	out = append(out, data...)
	out = append(out, '.')
	out = append(out, sig...)
	return out
}

type PayloadVerifier struct {
	mu       sync.Mutex
	signer   *Signer
	salt     []byte
	lastData []byte
	lastSig  string
}

func NewPayloadVerifier(key []byte, salt []byte) *PayloadVerifier {
	if len(key) == 0 {
		return &PayloadVerifier{signer: nil, salt: append([]byte(nil), salt...)}
	}
	s, _ := NewSigner(key)
	return &PayloadVerifier{signer: s, salt: append([]byte(nil), salt...)}
}

func (v *PayloadVerifier) bind(data []byte) []byte {
	out := make([]byte, 0, len(v.salt)+len(data)+2)
	out = append(out, v.salt...)
	out = append(out, '.')
	out = append(out, data...)
	return out
}

func (v *PayloadVerifier) Verify(data []byte, hexMAC string) bool {
	if v == nil {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.lastData = append(v.lastData[:0], data...)
	v.lastSig = hexMAC
	if v.signer == nil {
		// 没有签名密钥时无法校验，直接判失败而非 panic。
		return false
	}
	bound := v.bind(data)
	return v.signer.Verify(bound, hexMAC)
}

func (v *PayloadVerifier) ParseAndVerify(token []byte) ([]byte, bool) {
	idx := -1
	for i := len(token) - 1; i >= 0; i-- {
		if token[i] == '.' {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, false
	}
	data := token[:idx]
	sig := string(token[idx+1:])
	return data, v.Verify(data, sig)
}

func (v *PayloadVerifier) LastSeen() ([]byte, string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	d := append([]byte(nil), v.lastData...)
	return d, v.lastSig
}

type TokenCodec struct {
	signer   *PayloadSigner
	verifier *PayloadVerifier
	mu       sync.Mutex
}

func NewTokenCodec(key []byte, salt []byte) *TokenCodec {
	return &TokenCodec{
		signer:   NewPayloadSigner(key, salt),
		verifier: NewPayloadVerifier(key, salt),
	}
}

func (c *TokenCodec) Issue(data []byte) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.signer.SignAndAttach(data)
}

func (c *TokenCodec) Validate(token []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, ok := c.verifier.ParseAndVerify(token)
	if !ok {
		return nil, errors.New("cryptoutil: invalid token signature")
	}
	return data, nil
}

func (c *TokenCodec) Refresh(token []byte) ([]byte, error) {
	data, err := c.Validate(token)
	if err != nil {
		return nil, err
	}
	return c.Issue(data), nil
}
