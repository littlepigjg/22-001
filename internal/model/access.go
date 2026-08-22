package model

import (
	"errors"
	"strings"
	"time"
)

// AccessLog 表示一次短链接访问记录。
type AccessLog struct {
	// ID 为访问记录唯一 ID（基于时间 + 随机生成，保证唯一性）。
	ID string `json:"id"`

	// Code 为被访问的短码。
	Code string `json:"code"`

	// IP 为访问者 IP 地址。
	IP string `json:"ip"`

	// UserAgent 为访问者 UA 字符串。
	UserAgent string `json:"user_agent,omitempty"`

	// Referer 为请求头中的 Referer 值。
	Referer string `json:"referer,omitempty"`

	// Timestamp 为访问时间戳。
	Timestamp time.Time `json:"timestamp"`

	// Status 为重定向的状态（成功 302、已过期 410、不存在 404 等）。
	Status int `json:"status"`

	// 以下字段由 UA 解析后填充。
	OS        string `json:"os,omitempty"`         // 操作系统，如 "Windows", "Android"
	Browser   string `json:"browser,omitempty"`    // 浏览器，如 "Chrome", "Safari"
	Device    string `json:"device,omitempty"`     // 设备类型：pc / mobile / tablet / bot / other
	Bot       bool   `json:"bot,omitempty"`        // 是否判定为爬虫
	IPCountry string `json:"ip_country,omitempty"` // IP 所属国家（简易解析）
	IPRegion  string `json:"ip_region,omitempty"`  // IP 所属区域（保留字段）
}

// Validate 校验 AccessLog 的必填字段。
func (a *AccessLog) Validate() error {
	if a == nil {
		return errors.New("accesslog: nil pointer")
	}
	if a.ID == "" {
		return errors.New("accesslog: id is empty")
	}
	if err := ValidateCode(a.Code); err != nil {
		return err
	}
	if a.Timestamp.IsZero() {
		return errors.New("accesslog: timestamp is zero")
	}
	if a.Status < 100 || a.Status > 599 {
		return errors.New("accesslog: invalid status code")
	}
	return nil
}

// DailyStat 表示某短码按天聚合的统计项。
type DailyStat struct {
	Date     string `json:"date"`      // YYYY-MM-DD
	PV       int64  `json:"pv"`        // 页面浏览次数
	UV       int64  `json:"uv"`        // 独立访客数（按 IP 去重）
	Redirect int64  `json:"redirect"`  // 成功重定向次数 (302)
	Expired  int64  `json:"expired"`   // 已过期/超限次数 (410)
	NotFound int64  `json:"not_found"` // 不存在次数 (404)
}

// SourceStat 表示来源域名分布统计。
type SourceStat struct {
	Domain string `json:"domain"`
	Count  int64  `json:"count"`
}

// DeviceStat 表示设备类型分布统计。
type DeviceStat struct {
	Device string `json:"device"`
	Count  int64  `json:"count"`
}

// BrowserStat 表示浏览器分布统计。
type BrowserStat struct {
	Browser string `json:"browser"`
	Count   int64  `json:"count"`
}

// OSStat 表示操作系统分布统计。
type OSStat struct {
	OS    string `json:"os"`
	Count int64  `json:"count"`
}

// OverallStats 表示总体统计结果。
type OverallStats struct {
	Code            string            `json:"code"`
	TotalPV         int64             `json:"total_pv"`
	TotalUV         int64             `json:"total_uv"`
	Daily           []DailyStat       `json:"daily,omitempty"`
	Sources         []SourceStat      `json:"sources,omitempty"`
	Devices         []DeviceStat      `json:"devices,omitempty"`
	Browsers        []BrowserStat     `json:"browsers,omitempty"`
	Systems         []OSStat          `json:"systems,omitempty"`
	GeneratedAt     time.Time         `json:"generated_at"`
	SampleSize      int64             `json:"sample_size"`
}

// SafeCut 安全截断字符串，用于避免日志中出现过长 UA。
func SafeCut(s string, n int) string {
	if n <= 0 {
		return ""
	}
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
