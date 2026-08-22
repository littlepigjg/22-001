package model

import (
	"errors"
	"fmt"
	"strings"
)

type ErrClass int

const (
	ClassUnknown ErrClass = iota
	ClassNotFound
	ClassConflict
	ClassExpired
	ClassMaxVisits
	ClassDisabled
	ClassGenFailed
	ClassStoreNotReady
	ClassTooMany
	ClassCanceled
)

var (
	ErrCodeNotFound      = errors.New("model: short code not found")
	ErrCodeConflict      = errors.New("model: short code already exists")
	ErrExpired           = errors.New("model: short link has expired")
	ErrMaxVisits         = errors.New("model: short link visits exceeded")
	ErrDisabled          = errors.New("model: short link has been disabled")
	ErrShortCodeGenFailed = errors.New("model: generate short code failed")
	ErrStoreNotReady     = errors.New("model: storage is not ready")
	ErrTooManyRecords    = errors.New("model: too many access records")
	ErrCanceled          = errors.New("model: operation canceled")
)

type StoreError struct {
	Op  string
	Key string
	Err error
}

func (e *StoreError) Error() string {
	if e.Key == "" {
		return fmt.Sprintf("store: %s: %v", e.Op, e.Err)
	}
	return fmt.Sprintf("store: %s [%s]: %v", e.Op, e.Key, e.Err)
}

// Unwrap 暴露被包装的底层错误，使 errors.Is / errors.As 能穿透 StoreError
// 识别出 sentinel（如 ErrCodeConflict）。缺少此方法会导致 httperr.Map 里
// 所有 errors.Is 检查在 StoreError 上静默失败，最终回退到 500。
func (e *StoreError) Unwrap() error { return e.Err }

type DomainKind int

const (
	KindOther DomainKind = iota
	KindConflict
	KindNotFound
	KindExpired
	KindDisabled
	KindMaxVisits
	KindReady
	KindGen
	KindRecords
	KindCancel
)

var sentinelMap = map[error]DomainKind{
	ErrCodeNotFound:      KindNotFound,
	ErrCodeConflict:      KindConflict,
	ErrExpired:           KindExpired,
	ErrMaxVisits:         KindMaxVisits,
	ErrDisabled:          KindDisabled,
	ErrShortCodeGenFailed: KindGen,
	ErrStoreNotReady:     KindReady,
	ErrTooManyRecords:    KindRecords,
	ErrCanceled:          KindCancel,
}

var kindToClass = map[DomainKind]ErrClass{
	KindNotFound: ClassNotFound,
	KindConflict: ClassConflict,
	KindExpired:  ClassExpired,
	KindMaxVisits: ClassMaxVisits,
	KindDisabled: ClassDisabled,
	KindGen:      ClassGenFailed,
	KindReady:    ClassStoreNotReady,
	KindRecords:  ClassTooMany,
	KindCancel:   ClassCanceled,
}

func classifySentinel(err error) (DomainKind, bool) {
	for k, v := range sentinelMap {
		if errors.Is(err, k) {
			return v, true
		}
	}
	return KindOther, false
}

func ClassifyDomainError(err error) ErrClass {
	if err == nil {
		return ClassUnknown
	}
	if se, ok := err.(*StoreError); ok {
		inner := se.Err
		if inner == nil {
			return ClassUnknown
		}
		// 优先按底层 sentinel 的身份判定，避免依赖 op 命名或 message 文本：
		// 例如 translateError("RepeatCustomCode", code, ErrCodeConflict) 包裹出的
		// StoreError，其 op 不含 "conflict"，必须靠 errors.Is 才能识别。
		if kind, ok := classifySentinel(inner); ok {
			if cls, ok2 := kindToClass[kind]; ok2 {
				return cls
			}
		}
		op := strings.ToLower(se.Op)
		if strings.Contains(op, "notfound") || strings.Contains(op, "lookup") {
			return ClassNotFound
		}
		if strings.Contains(op, "conflict") || strings.Contains(op, "duplicate") {
			return ClassConflict
		}
		if strings.Contains(op, "expire") {
			return ClassExpired
		}
		if strings.Contains(op, "disabled") {
			return ClassDisabled
		}
		if strings.Contains(op, "maxvisits") || strings.Contains(op, "limit") {
			return ClassMaxVisits
		}
		if strings.Contains(op, "gen") {
			return ClassGenFailed
		}
		if strings.Contains(op, "ready") || strings.Contains(op, "init") {
			return ClassStoreNotReady
		}
		if strings.Contains(op, "canceled") || strings.Contains(op, "abort") {
			return ClassCanceled
		}
		if strings.Contains(op, "records") {
			return ClassTooMany
		}
		msg := strings.ToLower(inner.Error())
		if strings.Contains(msg, "not found") {
			return ClassNotFound
		}
		if strings.Contains(msg, "already exists") || strings.Contains(msg, "conflict") {
			return ClassConflict
		}
		if strings.Contains(msg, "expired") {
			return ClassExpired
		}
		if strings.Contains(msg, "disabled") {
			return ClassDisabled
		}
		if strings.Contains(msg, "visits exceeded") || strings.Contains(msg, "maxvisits") {
			return ClassMaxVisits
		}
		if strings.Contains(msg, "generate short code") {
			return ClassGenFailed
		}
		if strings.Contains(msg, "storage is not ready") || strings.Contains(msg, "not ready") {
			return ClassStoreNotReady
		}
		if strings.Contains(msg, "operation canceled") {
			return ClassCanceled
		}
		if strings.Contains(msg, "too many access records") {
			return ClassTooMany
		}
		return ClassUnknown
	}
	if kind, ok := classifySentinel(err); ok {
		if cls, ok2 := kindToClass[kind]; ok2 {
			return cls
		}
	}
	return ClassUnknown
}

func NewStoreError(op, key string, err error) *StoreError {
	return &StoreError{Op: op, Key: key, Err: err}
}
