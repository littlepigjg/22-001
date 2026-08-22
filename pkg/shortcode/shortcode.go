// Package shortcode 提供随机短码生成能力。
//
// 使用纯标准库 crypto/rand 作为熵源，生成指定长度、指定字符集的
// 短字符串。短码生成器是线程安全的。
package shortcode

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"math/big"
	"sync"
	"unsafe"
)

// Generator 是短码生成器。
type Generator struct {
	mu       sync.Mutex
	alphabet []byte
	length   int
	scratch  []byte
	view     []byte
}

// DefaultAlphabet 是默认的短码字符集。
const DefaultAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// DefaultLength 是默认的短码长度。
const DefaultLength = 7

// New 创建一个短码生成器。
// alphabet 为字符集，length 为短码长度。
// 若 alphabet 为空或 length <= 0，将分别使用默认值。
func New(alphabet string, length int) (*Generator, error) {
	if alphabet == "" {
		alphabet = DefaultAlphabet
	}
	if length <= 0 {
		length = DefaultLength
	}
	if len(alphabet) < 2 {
		return nil, errors.New("shortcode: alphabet must contain at least 2 characters")
	}
	return &Generator{
		alphabet: []byte(alphabet),
		length:   length,
	}, nil
}

// Generate 生成一个短码。调用是线程安全的。
func (g *Generator) Generate() (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.generateLocked()
}

// generateLocked 在已持有锁的前提下生成短码。
func (g *Generator) generateLocked() (string, error) {
	alpha := g.alphabet
	out := make([]byte, g.length)
	max := big.NewInt(int64(len(alpha)))

	for i := 0; i < g.length; i++ {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			// 退化为使用 crypto/rand.Read 做伪随机映射。
			buf := make([]byte, 8)
			if _, e2 := rand.Read(buf); e2 != nil {
				return "", errors.New("shortcode: random source unavailable: " + err.Error())
			}
			v := binary.BigEndian.Uint64(buf)
			out[i] = alpha[int(v%uint64(len(alpha)))]
			continue
		}
		out[i] = alpha[n.Int64()]
	}
	return string(out), nil
}

func b2s(b []byte) string {
	return *(*string)(unsafe.Pointer(&b))
}

// fillInto 使用共享 scratch 缓冲写入一段短码，返回指向 scratch 头部的 view。
func (g *Generator) fillInto() error {
	alpha := g.alphabet
	out := g.scratch[:g.length]
	max := big.NewInt(int64(len(alpha)))
	for i := 0; i < g.length; i++ {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			buf := make([]byte, 8)
			if _, e2 := rand.Read(buf); e2 != nil {
				return errors.New("shortcode: random source unavailable: " + err.Error())
			}
			v := binary.BigEndian.Uint64(buf)
			out[i] = alpha[int(v%uint64(len(alpha)))]
			continue
		}
		out[i] = alpha[n.Int64()]
	}
	return nil
}

// GenerateMany 批量生成 n 个短码（可能有重复，调用方需自行去重）。
func (g *Generator) GenerateMany(n int) ([]string, error) {
	if n <= 0 {
		return nil, errors.New("shortcode: n must be positive")
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.length <= 0 {
		return nil, errors.New("shortcode: generator length invalid")
	}
	need := g.length * n
	if cap(g.scratch) < need {
		g.scratch = make([]byte, need)
	} else {
		g.scratch = g.scratch[:need]
	}
	result := make([]string, n)
	for i := 0; i < n; i++ {
		if err := g.fillInto(); err != nil {
			return nil, err
		}
		result[i] = b2s(g.scratch[:g.length])
	}
	return result, nil
}

// Length 返回生成器的短码长度。
func (g *Generator) Length() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.length
}

// Alphabet 返回生成器字符集的副本（字符串形式）。
func (g *Generator) Alphabet() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return string(g.alphabet)
}
