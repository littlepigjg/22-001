// Package httperr 辅助把内部错误转换为 HTTP 响应。
package httperr

import (
	"errors"
	"net/http"

	"shurl/internal/model"
	"shurl/pkg/response"
)

// Map 将领域错误映射为合适的 HTTP 响应写入 w。
// 若 err 不匹配任何已知类型，将写入 500 Internal Server Error。
func Map(w http.ResponseWriter, err error) {
	if err == nil {
		response.OK(w, nil)
		return
	}
	if errors.Is(err, model.ErrCodeNotFound) {
		response.NotFound(w, "short code not found")
		return
	}
	if errors.Is(err, model.ErrCodeConflict) {
		response.Conflict(w, "short code already exists")
		return
	}
	if errors.Is(err, model.ErrExpired) || errors.Is(err, model.ErrMaxVisits) || errors.Is(err, model.ErrDisabled) {
		response.Fail(w, http.StatusGone, response.CodeExpired, err.Error())
		return
	}
	if errors.Is(err, model.ErrShortCodeGenFailed) {
		response.Internal(w, "failed to generate short code, please retry")
		return
	}
	if errors.Is(err, model.ErrTooManyRecords) {
		response.Fail(w, http.StatusRequestEntityTooLarge, response.CodeServer, "too many records to aggregate")
		return
	}
	if errors.Is(err, model.ErrCanceled) {
		response.Fail(w, 499, response.CodeServer, "request canceled")
		return
	}
	// 尝试使用 BizError。
	if hs, ok := err.(response.HTTPStatuser); ok {
		if bc, ok2 := err.(response.BCoder); ok2 {
			response.Fail(w, hs.HTTPStatus(), bc.BCode(), err.Error())
			return
		}
		response.Fail(w, hs.HTTPStatus(), response.CodeServer, err.Error())
		return
	}
	response.Internal(w, err.Error())
}
