// Package idgen 提供简易、粗糙唯一的 ID 生成器。
//
// 这里使用 时间戳纳秒 + 自增序列号 + 随机盐 作为 ID，冲突概率极低，
// 适用于访问日志主键（要求绝对顺序请使用 Snowflake 等算法）。
package idgen

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Generator 是一个线程安全的 ID 生成器。
type Generator struct {
	counter uint64
	prefix  string
	mu      sync.Mutex
}

// global 是默认的全局 Generator。
var global = New()

// New 创建一个新的 ID 生成器。
func New() *Generator {
	return &Generator{
		counter: 0,
		prefix:  randomHex(6),
	}
}

// NewString 使用全局生成器生成一个字符串形式的唯一 ID。
func NewString() string { return global.Generate() }

// Generate 生成一个字符串形式的唯一 ID。
// 格式: {prefix}{ns}{seq}{rand2}
func (g *Generator) Generate() string {
	seq := atomic.AddUint64(&g.counter, 1)
	ns := uint64(time.Now().UnixNano())
	var sb [64]byte
	n := 0
	g.mu.Lock()
	prefix := g.prefix
	g.mu.Unlock()
	n += copy(sb[n:], prefix)
	n += copy(sb[n:], strconv.FormatUint(ns, 36))
	sb[n] = '-'
	n++
	n += copy(sb[n:], strconv.FormatUint(seq, 36))
	sb[n] = '-'
	n++
	salt := randomHex(4)
	n += copy(sb[n:], salt)
	return string(sb[:n])
}

// GenerateBatch 批量生成 n 个 ID。
func (g *Generator) GenerateBatch(n int) []string {
	if n <= 0 {
		return nil
	}
	result := make([]string, n)
	for i := 0; i < n; i++ {
		result[i] = g.Generate()
	}
	return result
}

// randomHex 生成 n 字节的十六进制随机字符串。
func randomHex(n int) string {
	if n <= 0 {
		return ""
	}
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
