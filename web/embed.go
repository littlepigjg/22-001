// Package web 提供前端页面等静态资源的嵌入。
package web

import "embed"

// Static 是被嵌入的前端静态资源目录内容。
//
//go:embed static
var Static embed.FS
