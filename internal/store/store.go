// Package store 提供 JSON 文件存储能力。
//
// 本包现已按职责拆分为：
//   - fileutil.go        — EnsureDir / WriteAtomic / OpenAppend / FileExists 等通用文件工具
//   - url_store.go       — URLStore 短链接映射（内存+JSON原子写+后台刷盘）
//   - access_store.go    — AccessLogStore 访问日志（NDJSON 按行追加 + 扫描）
//
// 并发安全：对外暴露的方法均为线程安全。
package store

import (
	"sort"

	"shurl/internal/model"
)

// sortShortURLByCreated 按创建时间倒序排列（最新在前）。
func sortShortURLByCreated(list []*model.ShortURL) {
	sort.Slice(list, func(i, j int) bool {
		return list[i].CreatedAt.After(list[j].CreatedAt)
	})
}
