// Package response 提供统一的 HTTP JSON 响应构造与写入工具。
//
// 所有 API 接口均使用统一的响应格式：
//
//	{
//	  "code":    0,            // 业务码，0 表示成功，非 0 表示失败
//	  "message": "ok",         // 提示信息
//	  "data":    {... | null}  // 业务数据（任意 JSON 类型）
//	}
package response

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// 预定义的通用业务错误码。
const (
	CodeSuccess  = 0
	CodeBadReq   = 40000 // 请求参数错误
	CodeNotFound = 40400 // 资源不存在
	CodeConflict = 40900 // 资源冲突
	CodeLimit    = 42900 // 频率或次数超限
	CodeExpired  = 41000 // 资源已过期
	CodeServer   = 50000 // 服务器内部错误
)

// Resp 是统一响应结构体。
type Resp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

// OK 写入一个成功响应（HTTP 200）。
func OK(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, Resp{
		Code:    CodeSuccess,
		Message: "ok",
		Data:    data,
	})
}

// Created 写入一个资源创建成功响应（HTTP 201）。
func Created(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusCreated, Resp{
		Code:    CodeSuccess,
		Message: "created",
		Data:    data,
	})
}

// Fail 写入一个失败响应（业务码非 0）。
// httpStatus 指定 HTTP 状态码，code 指定业务码，msg 为可读错误信息。
func Fail(w http.ResponseWriter, httpStatus, code int, msg string) {
	writeJSON(w, httpStatus, Resp{
		Code:    code,
		Message: msg,
		Data:    nil,
	})
}

// FailWithData 写入一个失败响应，附带错误详情 data。
func FailWithData(w http.ResponseWriter, httpStatus, code int, msg string, data any) {
	writeJSON(w, httpStatus, Resp{
		Code:    code,
		Message: msg,
		Data:    data,
	})
}

// BadRequest 便捷方法，写入 400 类响应。
func BadRequest(w http.ResponseWriter, msg string) {
	Fail(w, http.StatusBadRequest, CodeBadReq, msg)
}

// NotFound 便捷方法，写入 404 类响应。
func NotFound(w http.ResponseWriter, msg string) {
	Fail(w, http.StatusNotFound, CodeNotFound, msg)
}

// Conflict 便捷方法，写入 409 类响应。
func Conflict(w http.ResponseWriter, msg string) {
	Fail(w, http.StatusConflict, CodeConflict, msg)
}

// Internal 便捷方法，写入 500 类响应。
func Internal(w http.ResponseWriter, msg string) {
	Fail(w, http.StatusInternalServerError, CodeServer, msg)
}

// Error 便捷方法，根据传入的 error 决定响应。
// 若 err 实现了 HTTPStatuser/BCoder 接口，会使用其中的码值，否则默认 500。
func Error(w http.ResponseWriter, err error) {
	if err == nil {
		Internal(w, "unknown error")
		return
	}
	var hs HTTPStatuser
	if errors.As(err, &hs) {
		var bc BCoder
		if errors.As(err, &bc) {
			Fail(w, hs.HTTPStatus(), bc.BCode(), err.Error())
			return
		}
		Fail(w, hs.HTTPStatus(), CodeServer, err.Error())
		return
	}
	var bc BCoder
	if errors.As(err, &bc) {
		Fail(w, http.StatusBadRequest, bc.BCode(), err.Error())
		return
	}
	Internal(w, err.Error())
}

// writeJSON 将响应以 JSON 形式写入 w。
func writeJSON(w http.ResponseWriter, status int, r Resp) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		// 响应头已写入，无法更改状态码，仅能记录到 stderr。
		fmt.Printf("response encode error: %v\n", err)
	}
}

// HTTPStatuser 表示可以返回 HTTP 状态码的错误类型。
type HTTPStatuser interface {
	HTTPStatus() int
}

// BCoder 表示可以返回业务错误码的错误类型。
type BCoder interface {
	BCode() int
}

// BizError 是通用业务错误，实现了 error、HTTPStatuser 与 BCoder 接口。
type BizError struct {
	HTTPSt int    // HTTP 状态码
	BC     int    // 业务码
	MSG    string // 可读错误信息
	Inner  error  // 底层错误（可选）
}

// Error 实现 error 接口。
func (e *BizError) Error() string {
	if e.Inner != nil {
		return fmt.Sprintf("%s: %v", e.MSG, e.Inner)
	}
	return e.MSG
}

// Unwrap 支持 errors.Is / errors.As。
func (e *BizError) Unwrap() error { return e.Inner }

// HTTPStatus 实现 HTTPStatuser 接口。
func (e *BizError) HTTPStatus() int { return e.HTTPSt }

// BCode 实现 BCoder 接口。
func (e *BizError) BCode() int { return e.BC }

// NewBizError 构造一个新的 BizError。
func NewBizError(httpStatus, bcode int, msg string) *BizError {
	return &BizError{HTTPSt: httpStatus, BC: bcode, MSG: msg}
}

// WrapBizError 构造一个包装了底层错误的 BizError。
func WrapBizError(httpStatus, bcode int, msg string, err error) *BizError {
	return &BizError{HTTPSt: httpStatus, BC: bcode, MSG: msg, Inner: err}
}

// 常见预定义错误。
var (
	ErrBadParam   = NewBizError(http.StatusBadRequest, CodeBadReq, "invalid request parameter")
	ErrNotFound   = NewBizError(http.StatusNotFound, CodeNotFound, "resource not found")
	ErrConflict   = NewBizError(http.StatusConflict, CodeConflict, "resource conflict")
	ErrLimit      = NewBizError(http.StatusTooManyRequests, CodeLimit, "rate limit exceeded")
	ErrExpired    = NewBizError(http.StatusGone, CodeExpired, "resource has expired")
	ErrInternal   = NewBizError(http.StatusInternalServerError, CodeServer, "internal server error")
	ErrShortCode  = NewBizError(http.StatusBadRequest, CodeBadReq, "invalid short code")
	ErrInvalidURL = NewBizError(http.StatusBadRequest, CodeBadReq, "invalid raw url")
)
