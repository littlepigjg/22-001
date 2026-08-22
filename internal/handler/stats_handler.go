package handler

import (
	"context"
	"net/http"
	"strings"
	"time"

	"shurl/internal/model"
	"shurl/internal/service"
	"shurl/pkg/httperr"
	"shurl/pkg/response"
)

// StatsHandler 提供短码统计相关的 API。
type StatsHandler struct {
	svc *service.StatsService
}

// NewStatsHandler 构造 StatsHandler。
func NewStatsHandler(svc *service.StatsService) (*StatsHandler, error) {
	if svc == nil {
		return nil, model.ErrStoreNotReady
	}
	return &StatsHandler{svc: svc}, nil
}

// Register 在 mux 上注册 /api/stats/ 相关路由。
func (h *StatsHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/stats/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			response.Fail(w, http.StatusMethodNotAllowed, response.CodeBadReq, "method not allowed")
			return
		}
		code := strings.TrimPrefix(r.URL.Path, "/api/stats/")
		code = strings.TrimSpace(code)
		h.Overall(w, r, code)
	})
}

// Overall 处理总体统计请求。
//
//	GET /api/stats/{code}?days=7
func (h *StatsHandler) Overall(w http.ResponseWriter, r *http.Request, code string) {
	if err := model.ValidateCode(code); err != nil {
		response.BadRequest(w, err.Error())
		return
	}
	days := ParseIntQuery(r, "days", 7)
	if days <= 0 {
		days = 7
	}
	if days > 90 {
		days = 90 // 限制最多 90 天，避免扫表过久。
	}
	// 统计接口可能要全表扫描访问日志，给一个有限截止时间（默认 200ms）。
	// aggregate 内部会在每条记录之间检查 ctx.Done()，超时即可及时打断，
	// 而不是任由 goroutine 把整张表扫完。
	statsTimeout := 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(r.Context(), statsTimeout)
	defer cancel()
	res, err := h.svc.Overall(ctx, code, days)
	if err != nil {
		httperr.Map(w, err)
		return
	}
	response.OK(w, res)
}
