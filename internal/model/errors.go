package model

import (
	"errors"
	"fmt"
)

// 预定义的领域错误，供 service / store 层使用。
// handler 层会将这些领域错误转换为合适的 HTTP 响应。
var (
	// ErrCodeNotFound 表示短码不存在。
	ErrCodeNotFound = errors.New("model: short code not found")

	// ErrCodeConflict 表示自定义短码已存在。
	ErrCodeConflict = errors.New("model: short code already exists")

	// ErrExpired 表示短码已过期。
	ErrExpired = errors.New("model: short link has expired")

	// ErrMaxVisits 表示短码访问次数已达上限。
	ErrMaxVisits = errors.New("model: short link visits exceeded")

	// ErrDisabled 表示短码已被禁用。
	ErrDisabled = errors.New("model: short link has been disabled")

	// ErrShortCodeGenFailed 表示自动生成短码多次重试后仍冲突。
	ErrShortCodeGenFailed = errors.New("model: generate short code failed")

	// ErrStoreNotReady 表示存储层尚未初始化完成。
	ErrStoreNotReady = errors.New("model: storage is not ready")

	// ErrTooManyRecords 表示聚合时访问日志记录数超过配置上限。
	ErrTooManyRecords = errors.New("model: too many access records")

	// ErrCanceled 表示操作被 context 取消。
	ErrCanceled = errors.New("model: operation canceled")
)

// StoreError 包装存储层返回的错误，携带操作类型与底层错误。
type StoreError struct {
	Op   string // 操作名，例如 "SaveURL" / "LoadLogs"
	Key  string // 涉及的键（可选）
	Err  error  // 底层错误
}

// Error 实现 error 接口。
func (e *StoreError) Error() string {
	if e.Key == "" {
		return fmt.Sprintf("store: %s: %v", e.Op, e.Err)
	}
	return fmt.Sprintf("store: %s [%s]: %v", e.Op, e.Key, e.Err)
}

// Unwrap 支持 errors.Is / errors.As。
func (e *StoreError) Unwrap() error { return e.Err }

// NewStoreError 构造一个 StoreError。
func NewStoreError(op, key string, err error) *StoreError {
	return &StoreError{Op: op, Key: key, Err: err}
}
