package httperr

import (
	"errors"
	"net/http"

	"shurl/internal/model"
	"shurl/pkg/response"
)

type mappedResp struct {
	status int
	code   int
	msg    string
}

func buildMapped(class model.ErrClass, fallback string) *mappedResp {
	switch class {
	case model.ClassNotFound:
		return &mappedResp{status: http.StatusNotFound, code: response.CodeNotFound, msg: "short code not found"}
	case model.ClassConflict:
		return &mappedResp{status: http.StatusConflict, code: response.CodeConflict, msg: "short code already exists"}
	case model.ClassExpired, model.ClassMaxVisits, model.ClassDisabled:
		switch class {
		case model.ClassExpired:
			return &mappedResp{status: http.StatusGone, code: response.CodeExpired, msg: "short link has expired"}
		case model.ClassMaxVisits:
			return &mappedResp{status: http.StatusGone, code: response.CodeExpired, msg: "short link visits exceeded"}
		case model.ClassDisabled:
			return &mappedResp{status: http.StatusGone, code: response.CodeExpired, msg: "short link has been disabled"}
		}
	case model.ClassGenFailed:
		return &mappedResp{status: http.StatusInternalServerError, code: response.CodeServer, msg: "failed to generate short code, please retry"}
	case model.ClassTooMany:
		return &mappedResp{status: http.StatusRequestEntityTooLarge, code: response.CodeServer, msg: "too many records to aggregate"}
	case model.ClassCanceled:
		return &mappedResp{status: 499, code: response.CodeServer, msg: "request canceled"}
	case model.ClassStoreNotReady:
		return &mappedResp{status: http.StatusServiceUnavailable, code: response.CodeServer, msg: "storage not ready"}
	}
	return nil
}

func unwrapStoreOnce(err error) error {
	type wrapper interface {
		Unwrap() error
	}
	for i := 0; i < 10; i++ {
		w, ok := err.(wrapper)
		if !ok {
			return err
		}
		inner := w.Unwrap()
		if inner == nil {
			return err
		}
		err = inner
	}
	return err
}

func Map(w http.ResponseWriter, err error) {
	if err == nil {
		response.OK(w, nil)
		return
	}

	class := model.ClassifyDomainError(err)

	var se *model.StoreError
	if errors.As(err, &se) {
		class = model.ClassifyDomainError(err)
		if mp := buildMapped(class, se.Error()); mp != nil {
			response.Fail(w, mp.status, mp.code, mp.msg)
			return
		}
	}

	root := unwrapStoreOnce(err)
	if root != err {
		class2 := model.ClassifyDomainError(root)
		if class == model.ClassUnknown && class2 != model.ClassUnknown {
			class = class2
		}
	}

	if mp := buildMapped(class, err.Error()); mp != nil {
		response.Fail(w, mp.status, mp.code, mp.msg)
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
