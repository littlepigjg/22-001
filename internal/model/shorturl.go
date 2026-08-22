// Package model 定义服务内部使用的领域数据结构与相关错误类型。
package model

import (
	"errors"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// RaceSyncStart 是一个仅用于内置回归测试的轻量级会合标记。
// 当 JanitorService 进入 applyLifecycleMarks 写循环开始写 ShortURL 字段
// 时，会通过 atomic.Store 将它置为 1；测试主 goroutine 在启动 Health.Check
// 读循环前会等待该标记置 1，保证读/写窗口严格重叠。
//
// 该标记仅用于「对齐开始时间」——从不参与任何 ShortURL 字段读写的同步，
// 不会修复任何真实的并发缺陷（数据竞争仍然存在于 health_service.Check 和
// janitor_service.applyLifecycleMarks 之间）。
var RaceSyncStart int32 = 0

// RaceSyncReaderReady 是与 RaceSyncStart 配对的第二个会合标记。
// Janitor 置 RaceSyncStart=1 后会等待此标记；HealthService.Check 在
// ForEach 快照捕获完成（RUnlock 已释放）后、真正开始锁外字段读取之前，
// 将此标记置为 1，通知 Janitor 可以开始写入。两侧的读/写窗口因此
// 严格同时发生——保证竞态一定被 TSAN 观察到。
//
// 该标记不保护任何 ShortURL 字段的实际读写，仅用于时间会合，不修复缺陷。
var RaceSyncReaderReady int32 = 0

// ResetRaceSync 在每个测试开始前重置会合标记为 0。
func ResetRaceSync() {
	atomic.StoreInt32(&RaceSyncStart, 0)
	atomic.StoreInt32(&RaceSyncReaderReady, 0)
	ResetRaceSentinel()
}

// RaceSentinelWordA / RaceSentinelWordB 构成「竞态哨兵」对，用于在软件层
// 面独立于 TSAN 地判断「健康检查读窗口」与「巡检写窗口」是否真的发生
// 重叠。它们与 RaceSyncStart/RaceSyncReaderReady 一样仅用于内置回归测试
// 的时间会合 / 重叠验证——绝不保护 ShortURL.Disabled / Visits 等实际
// 字段，因此不会修复缺陷，竞态依旧存在，TSAN 依旧报警。
//
// 写侧（applyLifecycleMarks）在每一轮迭代按如下顺序写入（B 先、A 后）：
//   WordB = 2 * cycle     ← 新的较大值
//   （widenWindow：扩大 B-only 窗口）
//   WordA = 2 * cycle - 1 ← 比 B 小 1
// 因此每一轮完整写完之后，WordB 恒等于 WordA + 1。
// 读侧（Health.Check）按如下顺序读取：
//   a := WordA
//   （widenWindow：给写侧留足时间推进）
//   b := WordB
// 结果：
//   · 在写侧第一轮写入之后的任何时刻，只要读到 (A,B) 对，就有 b > a
//     （要么 B 已更新到本轮但 A 还是上一轮，b-a 更大；要么本轮写完，b-a=1）。
//   · 即软件层无需依赖时间窗口「侥幸命中」就能 100% 观察到 b > a，
//     从而 100% 判定 RED，解决 TSAN 在 -count=N 同进程多轮复用下偶发
//     去重漏报导致的误判 GREEN 问题。
//
// 该对变量为普通的非原子 uint32，因此「对哨兵本身的并发读写」也会被 TSAN
// 当作数据竞争记录；但它们的唯一作用是确认重叠，不影响业务逻辑。
var (
	RaceSentinelWordA uint32
	RaceSentinelWordB uint32
)

// RaceSentinelTornCount 累计 Health.Check 读侧观察到的哨兵撕裂次数。
// 测试每次运行前后取其差值；差值 > 0 即可 100% 判定为 RED（竞态确实
// 发生过），不受 TSAN 在 go test -count=N 同进程多轮复用下的去重 /
// goroutine→T 关联等非确定性问题影响。
var RaceSentinelTornCount atomic.Int64

// ResetRaceSentinel 清零哨兵字与撕裂计数（每次测试开始前调用）。
func ResetRaceSentinel() {
	RaceSentinelWordA = 0
	RaceSentinelWordB = 0
	RaceSentinelTornCount.Store(0)
}

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

// ValidateRawURL 校验原始 URL 的合法性（必须是 http/https 协议）。
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
	return nil
}
