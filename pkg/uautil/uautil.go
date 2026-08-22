// Package uautil 提供 User-Agent 字符串的简易解析能力。
//
// 目标是不引入任何第三方依赖，对常见的浏览器、操作系统、设备类型
// 做出合理判断，满足一般统计需求。
package uautil

import (
	"strings"
)

// Result 是 UA 解析结果。
type Result struct {
	OS      string // 操作系统：Windows / macOS / Linux / Android / iOS / Other
	Browser string // 浏览器：Chrome / Safari / Edge / Firefox / Opera / Other
	Device  string // 设备：pc / mobile / tablet / bot / other
	Bot     bool   // 是否为爬虫/机器人
}

// Parse 解析 User-Agent 字符串并返回解析结果。
func Parse(ua string) Result {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return Result{OS: "Other", Browser: "Other", Device: "other", Bot: false}
	}
	lower := strings.ToLower(ua)

	res := Result{}
	res.Bot = detectBot(lower)
	res.OS = detectOS(lower)
	res.Browser = detectBrowser(lower)
	res.Device = detectDevice(lower)
	if res.Bot {
		res.Device = "bot"
	}
	return res
}

// detectBot 启发式判断是否为爬虫。
func detectBot(lower string) bool {
	keywords := []string{
		"bot", "spider", "crawl", "crawler", "slurp", "bingpreview",
		"mediapartners", "googlebot", "bingbot", "yandexbot", "duckduckbot",
		"baiduspider", "sogou", "curl", "wget", "httpclient", "python-requests",
		"go-http-client", "postmanruntime", "pingdom", "uptimerobot",
		"facebookexternalhit", "twitterbot", "whatsapp", "telegrambot",
	}
	for _, k := range keywords {
		if strings.Contains(lower, k) {
			return true
		}
	}
	return false
}

// detectOS 判断操作系统。
func detectOS(lower string) string {
	switch {
	case strings.Contains(lower, "windows phone"):
		return "WindowsPhone"
	case strings.Contains(lower, "windows"):
		return "Windows"
	case strings.Contains(lower, "iphone") || strings.Contains(lower, "ipad") || strings.Contains(lower, "ipod"):
		return "iOS"
	case strings.Contains(lower, "mac os") || strings.Contains(lower, "macos") || strings.Contains(lower, "macintosh"):
		return "macOS"
	case strings.Contains(lower, "android"):
		return "Android"
	case strings.Contains(lower, "linux"):
		return "Linux"
	case strings.Contains(lower, "bsd"):
		return "BSD"
	case strings.Contains(lower, "chromeos") || strings.Contains(lower, "cros"):
		return "ChromeOS"
	default:
		return "Other"
	}
}

// detectBrowser 判断浏览器类型。
func detectBrowser(lower string) string {
	switch {
	case strings.Contains(lower, "edg/") || strings.Contains(lower, "edge"):
		return "Edge"
	case strings.Contains(lower, "opr/") || strings.Contains(lower, "opera"):
		return "Opera"
	case strings.Contains(lower, "firefox") || strings.Contains(lower, "fxios"):
		return "Firefox"
	case strings.Contains(lower, "msie") || strings.Contains(lower, "trident/"):
		return "IE"
	case strings.Contains(lower, "chrome") || strings.Contains(lower, "crios"):
		// 注意：Edge / Opera UA 也会包含 chrome 字样，因此判断顺序必须在其后。
		return "Chrome"
	case strings.Contains(lower, "safari"):
		return "Safari"
	case strings.Contains(lower, "ucbrowser"):
		return "UCBrowser"
	case strings.Contains(lower, "qqbrowser"):
		return "QQBrowser"
	case strings.Contains(lower, "miuibrowser"):
		return "MiBrowser"
	default:
		return "Other"
	}
}

// detectDevice 判断设备类型。
func detectDevice(lower string) string {
	// 先判断平板电脑。
	if strings.Contains(lower, "ipad") ||
		(strings.Contains(lower, "android") && !strings.Contains(lower, "mobile")) ||
		strings.Contains(lower, "tablet") ||
		strings.Contains(lower, "kindle") ||
		strings.Contains(lower, "silk/") ||
		strings.Contains(lower, "playbook") {
		return "tablet"
	}
	// 再判断移动设备。
	mobileKeywords := []string{
		"mobile", "phone", "iphone", "ipod", "android", "blackberry",
		"windows phone", "iemobile", "opera mini", "opera mobi", "fennec",
		"webos", "bada", "tizen", "meego", "maemo", "midp", "symbian",
	}
	for _, k := range mobileKeywords {
		if strings.Contains(lower, k) {
			return "mobile"
		}
	}
	return "pc"
}
