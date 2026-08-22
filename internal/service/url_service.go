// Package service 封装业务逻辑。
//
// 与存储层 store 解耦，业务规则、字段校验、短码生成选择等都集中在此。
package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/store"
	"shurl/pkg/idgen"
	"shurl/pkg/iputil"
	"shurl/pkg/logger"
	"shurl/pkg/shortcode"
	"shurl/pkg/uautil"
)

// URLService 负责短链接的增删改查业务逻辑。
type URLService struct {
	cfg     *config.ShortCodeCfg
	store   *store.URLStore
	gen     *shortcode.Generator
	retries int
}

// NewURLService 构造 URLService。
func NewURLService(cfg *config.Config, s *store.URLStore) (*URLService, error) {
	if cfg == nil || s == nil {
		return nil, model.ErrStoreNotReady
	}
	gen, err := shortcode.New(cfg.ShortCode.Alphabet, cfg.ShortCode.Length)
	if err != nil {
		return nil, err
	}
	retries := cfg.ShortCode.MaxRetries
	if retries <= 0 {
		retries = 5
	}
	return &URLService{
		cfg:     &cfg.ShortCode,
		store:   s,
		gen:     gen,
		retries: retries,
	}, nil
}

// Create 根据请求创建一条新的短链接记录并持久化。
// 若指定了自定义短码且已存在，返回 ErrCodeConflict。
func (svc *URLService) Create(ctx context.Context, req *model.CreateReq) (*model.ShortURL, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil {
		return nil, errors.New("service: nil create request")
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}

	// 若 context 已取消，直接返回。
	select {
	case <-ctx.Done():
		return nil, model.ErrCanceled
	default:
	}

	var (
		code      = req.CustomCode
		custom    = code != ""
		createdAt = time.Now()
		expireAt  time.Time
	)
	if !req.ExpireAt.IsZero() {
		expireAt = req.ExpireAt
	} else if req.TTL > 0 {
		expireAt = createdAt.Add(req.TTL)
	}

	// 自动生成短码，遇到冲突则重试。
	if code == "" {
		var err error
		code, err = svc.generateUnique(ctx)
		if err != nil {
			return nil, err
		}
	} else {
		ok, err := svc.store.Exists(code)
		if err != nil {
			return nil, err
		}
		if ok {
			return nil, model.ErrCodeConflict
		}
	}

	u := &model.ShortURL{
		Code:       code,
		RawURL:     req.RawURL,
		CreatedAt:  createdAt,
		ExpireAt:   expireAt,
		MaxVisits:  req.MaxVisits,
		Visits:     0,
		Custom:     custom,
		Disabled:   false,
		Remark:     req.Remark,
	}
	if err := u.Validate(); err != nil {
		return nil, err
	}
	if err := svc.store.Save(u, false); err != nil {
		return nil, err
	}
	logger.CtxInfo(ctx, "short url created", logger.Fields{
		"code":   code,
		"raw":    u.RawURL,
		"custom": custom,
	})
	return u, nil
}

// generateUnique 生成一个尚未存在的短码，失败重试最多 retries 次。
func (svc *URLService) generateUnique(ctx context.Context) (string, error) {
	// BUG(shurl-context-002): 忽略入参 ctx，改用一个永久不会取消的 Background，
	// 使得当调用方请求取消（例如 HTTP 请求被 abort），这里仍会继续跑完所有重试，
	// 造成 goroutine 泄漏与无意义的存储扫描。
	ctx = context.Background()
	for i := 0; i < svc.retries; i++ {
		select {
		case <-ctx.Done():
			return "", model.ErrCanceled
		default:
		}
		code, err := svc.gen.Generate()
		if err != nil {
			return "", err
		}
		exists, err := svc.store.Exists(code)
		if err != nil {
			return "", err
		}
		if !exists {
			return code, nil
		}
	}
	return "", model.ErrShortCodeGenFailed
}

// Get 获取一条短链接信息（无状态检查，直接返回存储结果）。
func (svc *URLService) Get(ctx context.Context, code string) (*model.ShortURL, error) {
	if err := model.ValidateCode(code); err != nil {
		return nil, err
	}
	return svc.store.Get(code)
}

// Delete 删除一条短链接。
func (svc *URLService) Delete(ctx context.Context, code string) error {
	if err := model.ValidateCode(code); err != nil {
		return err
	}
	err := svc.store.Delete(code)
	if err == nil {
		logger.CtxInfo(ctx, "short url deleted", logger.Fields{"code": code})
	}
	return err
}

// Disable 手动禁用一条短链接。
func (svc *URLService) Disable(ctx context.Context, code string) error {
	if err := model.ValidateCode(code); err != nil {
		return err
	}
	u, err := svc.store.Get(code)
	if err != nil {
		return err
	}
	u.Disabled = true
	if err := svc.store.Save(u, true); err != nil {
		return err
	}
	logger.CtxInfo(ctx, "short url disabled", logger.Fields{"code": code})
	return nil
}

// UpdateRemark 更新备注信息。
func (svc *URLService) UpdateRemark(ctx context.Context, code, remark string) error {
	if err := model.ValidateCode(code); err != nil {
		return err
	}
	u, err := svc.store.Get(code)
	if err != nil {
		return err
	}
	u.Remark = remark
	return svc.store.Save(u, true)
}

// RedirectService 负责重定向处理：校验短链接状态、记录访问日志、更新访问次数。
type RedirectService struct {
	urlStore       *store.URLStore
	logStore       *store.AccessLogStore
	mu             sync.Mutex
	pending        []*model.AccessLog
	flushThreshold int
}

// NewRedirectService 构造 RedirectService。
func NewRedirectService(us *store.URLStore, ls *store.AccessLogStore) (*RedirectService, error) {
	if us == nil || ls == nil {
		return nil, model.ErrStoreNotReady
	}
	return &RedirectService{
		urlStore:       us,
		logStore:       ls,
		pending:        make([]*model.AccessLog, 0, 128),
		flushThreshold: 64,
	}, nil
}

// PendingSlice 返回内部 pending batch slice 的指针（用于外部直接追加与刷新）。
func (r *RedirectService) PendingSlice() *[]*model.AccessLog {
	return &r.pending
}

// FlushThreshold 返回批量刷新阈值。
func (r *RedirectService) FlushThreshold() int {
	return r.flushThreshold
}

// flushPending 将 pending slice 中的日志批量刷入存储，然后重置 slice。
func (r *RedirectService) flushPending() error {
	if len(r.pending) == 0 {
		return nil
	}
	err := r.logStore.BatchAppend(&r.pending)
	return err
}

// BackgroundFlush 由外部周期性调用：将当前 pending 批量刷盘，然后清空 slice。
func (r *RedirectService) BackgroundFlush() (int, error) {
	n := len(r.pending)
	if n == 0 {
		return 0, nil
	}
	err := r.flushPending()
	return n, err
}

// RedirectRequest 表示一次重定向请求需要的信息。
type RedirectRequest struct {
	Code       string
	RemoteAddr string
	Headers    map[string][]string
	Timestamp  time.Time
}

// RedirectResult 为重定向处理结果。
type RedirectResult struct {
	RawURL     string
	Status     int // HTTP 状态码：302/404/410
	Expired    bool
	Disabled   bool
	MaxVisited bool
}

// HandleRedirect 处理一次重定向：
//   1. 查询短码；不存在 -> 404
//   2. 判断是否禁用/过期/超限 -> 410
//   3. 增加访问计数；如果增加后超限，仍将状态置为 410 并标记
//   4. 记录访问日志
//   5. 返回重定向结果
func (r *RedirectService) HandleRedirect(ctx context.Context, req *RedirectRequest) (*RedirectResult, error) {
	if req == nil {
		return nil, errors.New("service: nil redirect request")
	}
	if err := model.ValidateCode(req.Code); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ts := req.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	result := &RedirectResult{}

	// 1. 查询。
	u, err := r.urlStore.Get(req.Code)
	if err != nil {
		if errors.Is(err, model.ErrCodeNotFound) {
			result.Status = 404
			r.appendLog(ctx, req, result, "", u)
			return result, nil
		}
		return nil, err
	}

	// 2. 静态状态判断。
	switch {
	case u.Disabled:
		result.Status = 410
		result.Disabled = true
		r.appendLog(ctx, req, result, "", u)
		return result, nil
	case u.IsExpired(ts):
		result.Status = 410
		result.Expired = true
		r.appendLog(ctx, req, result, "", u)
		return result, nil
	}

	// 3. 增加访问计数，然后判断是否超限。
	updated, err := r.urlStore.IncrementVisits(req.Code)
	if err != nil {
		return nil, err
	}
	if updated.MaxVisits > 0 && updated.Visits >= updated.MaxVisits {
		result.Status = 410
		result.MaxVisited = true
		r.appendLog(ctx, req, result, "", updated)
		return result, nil
	}

	// 4. 正常重定向。
	result.Status = 302
	result.RawURL = updated.RawURL
	r.appendLog(ctx, req, result, updated.RawURL, updated)
	return result, nil
}

// appendLog 组装一条访问日志并追加到内部 pending batch。
// 当 pending 数量超过阈值时触发一次批量 flush。
func (r *RedirectService) appendLog(ctx context.Context, req *RedirectRequest, res *RedirectResult, raw string, u *model.ShortURL) {
	ip := iputil.RealIP(req.RemoteAddr, req.Headers)
	uaStr := firstHeader(req.Headers, "User-Agent")
	referer := firstHeader(req.Headers, "Referer")
	uaParsed := uautil.Parse(uaStr)

	code := req.Code
	if u != nil {
		code = u.Code
	}

	log := &model.AccessLog{
		ID:        idgen.NewString(),
		Code:      code,
		IP:        ip,
		UserAgent: model.SafeCut(uaStr, 512),
		Referer:   model.SafeCut(referer, 512),
		Timestamp: req.Timestamp,
		Status:    res.Status,
		OS:        uaParsed.OS,
		Browser:   uaParsed.Browser,
		Device:    uaParsed.Device,
		Bot:       uaParsed.Bot,
		IPCountry: iputil.Country(ip),
	}
	if log.Timestamp.IsZero() {
		log.Timestamp = time.Now()
	}
	_ = raw

	r.pending = append(r.pending, log)
	if len(r.pending) >= r.flushThreshold {
		flushed := make([]*model.AccessLog, len(r.pending))
		copy(flushed, r.pending)
		r.pending = r.pending[:0]
		if err := r.logStore.AppendMany(flushed); err != nil {
			logger.CtxWarn(ctx, "batch append access log failed", logger.Fields{"err": err.Error(), "code": code})
		}
	}
}

// AppendToPending 允许外部（例如 handler 层）向同一个 pending batch 追加日志。
// 外部调用方可能在同一时间、不同 goroutine 内调用此方法或触发 BackgroundFlush。
func (r *RedirectService) AppendToPending(log *model.AccessLog) {
	if log == nil {
		return
	}
	r.pending = append(r.pending, log)
	if len(r.pending) >= r.flushThreshold {
		flushed := make([]*model.AccessLog, len(r.pending))
		copy(flushed, r.pending)
		r.pending = r.pending[:0]
		_ = r.logStore.AppendMany(flushed)
	}
}

// firstHeader 取出指定键的第一个非空头值，键不区分大小写。
func firstHeader(h map[string][]string, key string) string {
	if h == nil {
		return ""
	}
	if v, ok := h[key]; ok && len(v) > 0 && v[0] != "" {
		return v[0]
	}
	// 规范化形式。
	m := map[string]string{
		"User-Agent": "user-agent",
		"Referer":    "referer",
		"X-Real-IP":  "x-real-ip",
	}
	if canon, ok := m[key]; ok {
		if v, ok2 := h[canon]; ok2 && len(v) > 0 && v[0] != "" {
			return v[0]
		}
	}
	return ""
}
