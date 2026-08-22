package handler

import (
	"net/http"
	"strings"
	"time"

	"shurl/internal/model"
	"shurl/internal/service"
	"shurl/pkg/httperr"
	"shurl/pkg/response"
)

// URLHandler 处理短链接的 CRUD HTTP 请求。
type URLHandler struct {
	svc *service.URLService
}

// NewURLHandler 构造 URLHandler。
func NewURLHandler(svc *service.URLService) (*URLHandler, error) {
	if svc == nil {
		return nil, model.ErrStoreNotReady
	}
	return &URLHandler{svc: svc}, nil
}

// Register 在给定的 mux 上注册短链接相关的路由。
// 路由前缀固定为 /api/urls。
func (h *URLHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/urls", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			h.Create(w, r)
		default:
			response.Fail(w, http.StatusMethodNotAllowed, response.CodeBadReq, "method not allowed")
		}
	})
	mux.HandleFunc("/api/urls/", func(w http.ResponseWriter, r *http.Request) {
		code := strings.TrimPrefix(r.URL.Path, "/api/urls/")
		code = strings.TrimSpace(code)
		switch r.Method {
		case http.MethodGet:
			h.Get(w, r, code)
		case http.MethodDelete:
			h.Delete(w, r, code)
		case http.MethodPatch:
			h.Patch(w, r, code)
		default:
			response.Fail(w, http.StatusMethodNotAllowed, response.CodeBadReq, "method not allowed")
		}
	})
}

// Create 处理创建短链接。
//
//	POST /api/urls
//	Request:  { raw_url, custom_code?, ttl_seconds?, expire_at?, max_visits?, remark? }
//	Response: { code, raw_url, created_at, expire_at, max_visits, visits, custom, disabled, remark }
func (h *URLHandler) Create(w http.ResponseWriter, r *http.Request) {
	type createBody struct {
		RawURL     string `json:"raw_url"`
		CustomCode string `json:"custom_code,omitempty"`
		TTLSeconds int64  `json:"ttl_seconds,omitempty"`
		ExpireAt   string `json:"expire_at,omitempty"` // RFC3339
		MaxVisits  int64  `json:"max_visits,omitempty"`
		Remark     string `json:"remark,omitempty"`
	}
	var body createBody
	if err := DecodeJSON(r, &body); err != nil {
		response.BadRequest(w, err.Error())
		return
	}
	req := &model.CreateReq{
		RawURL:     body.RawURL,
		CustomCode: body.CustomCode,
		MaxVisits:  body.MaxVisits,
		Remark:     body.Remark,
	}
	if body.TTLSeconds > 0 {
		req.TTL = time.Duration(body.TTLSeconds) * time.Second
	}
	if body.ExpireAt != "" {
		if t, err := time.Parse(time.RFC3339, body.ExpireAt); err == nil {
			req.ExpireAt = t
		} else {
			response.BadRequest(w, "invalid expire_at, must be RFC3339, e.g. 2026-01-01T00:00:00Z")
			return
		}
	}
	res, err := h.svc.Create(r.Context(), req)
	if err != nil {
		httperr.Map(w, err)
		return
	}
	response.Created(w, res)
}

// Get 处理查询短链接详情。
//
//	GET /api/urls/{code}
func (h *URLHandler) Get(w http.ResponseWriter, r *http.Request, code string) {
	if err := model.ValidateCode(code); err != nil {
		response.BadRequest(w, err.Error())
		return
	}
	res, err := h.svc.Get(r.Context(), code)
	if err != nil {
		httperr.Map(w, err)
		return
	}
	response.OK(w, res)
}

// Delete 处理删除短链接。
//
//	DELETE /api/urls/{code}
func (h *URLHandler) Delete(w http.ResponseWriter, r *http.Request, code string) {
	if err := model.ValidateCode(code); err != nil {
		response.BadRequest(w, err.Error())
		return
	}
	if err := h.svc.Delete(r.Context(), code); err != nil {
		httperr.Map(w, err)
		return
	}
	response.OK(w, map[string]string{"code": code, "status": "deleted"})
}

// Patch 处理部分更新（目前仅支持禁用与修改备注）。
//
//	PATCH /api/urls/{code}
//	Request:  { disabled?: bool, remark?: string }
func (h *URLHandler) Patch(w http.ResponseWriter, r *http.Request, code string) {
	if err := model.ValidateCode(code); err != nil {
		response.BadRequest(w, err.Error())
		return
	}
	type patchBody struct {
		Disabled *bool   `json:"disabled,omitempty"`
		Remark   *string `json:"remark,omitempty"`
	}
	var body patchBody
	if err := DecodeJSON(r, &body); err != nil {
		response.BadRequest(w, err.Error())
		return
	}
	if body.Disabled == nil && body.Remark == nil {
		response.BadRequest(w, "no field provided (disabled / remark)")
		return
	}
	if body.Disabled != nil && *body.Disabled {
		if err := h.svc.Disable(r.Context(), code); err != nil {
			httperr.Map(w, err)
			return
		}
	}
	if body.Remark != nil {
		latest, err := h.svc.UpdateRemark(r.Context(), code, *body.Remark)
		if err != nil {
			httperr.Map(w, err)
			return
		}
		response.OK(w, latest)
		return
	}
	latest, err := h.svc.Get(r.Context(), code)
	if err != nil {
		httperr.Map(w, err)
		return
	}
	response.OK(w, latest)
}
