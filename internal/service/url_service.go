// Package service 封装业务逻辑。
//
// 与存储层 store 解耦，业务规则、字段校验、短码生成选择等都集中在此。
package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/store"
	"shurl/pkg/idgen"
	"shurl/pkg/iputil"
	"shurl/pkg/logger"
	"shurl/pkg/retry"
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
	urlStore *store.URLStore
	logStore *store.AccessLogStore
	mu       sync.Mutex
}

// NewRedirectService 构造 RedirectService。
func NewRedirectService(us *store.URLStore, ls *store.AccessLogStore) (*RedirectService, error) {
	if us == nil || ls == nil {
		return nil, model.ErrStoreNotReady
	}
	return &RedirectService{urlStore: us, logStore: ls}, nil
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

// appendLog 组装一条访问日志并异步写入到 AccessLogStore。
// 这里为了主流程不被日志卡住，采用「如果 ctx 没取消则尝试同步写，写失败不影响返回」的策略，
// 同时在 service 内部通过一个轻量 channel 队列合并写。
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

	// 同步写入存储。
	if err := r.logStore.Append(log); err != nil {
		logger.CtxWarn(ctx, "append access log failed", logger.Fields{"err": err.Error(), "code": code})
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

// BatchCreateReq 表示批量创建短链接中的单条请求。
type BatchCreateReq struct {
	RawURL     string
	CustomCode string
	TTL        time.Duration
	ExpireAt   time.Time
	MaxVisits  int64
	Remark     string
}

// BatchCreateResult 表示批量创建操作的结果。
type BatchCreateResult struct {
	Total    int
	Created  []*model.ShortURL
	Failed   []*BatchCreateFailure
}

// BatchCreateFailure 表示批量创建中单条失败的记录与原因。
type BatchCreateFailure struct {
	RawURL string
	Code   string
	Reason string
}

// BatchCreate 一次性创建多条短链接记录。
// 它会先对每条请求进行字段校验与短码生成，然后通过 store 层的批量写入接口持久化。
// 单条写入遇到可重试错误时会按重试策略重试；所有重试均失败的条目会被记入 Failed
// 并携带失败原因，绝不会在「实际未写入」时虚报 Created。
func (svc *URLService) BatchCreate(ctx context.Context, reqs []*BatchCreateReq) (*BatchCreateResult, error) {
	return svc.BatchCreateWithRetry(ctx, reqs, 2)
}

// BatchCreateWithRetry 允许调用方显式指定重试次数。
// 所有尝试均失败的条目会被记入 Failed 并透传最后一次失败原因；Created 仅包含真正写入
// 成功的条目，与实际落盘完全对齐。返回 (result, nil) 以保留「部分成功」语义，
// 调用方按 Created / Failed 分流，底层聚合错误不再作为整批 error 抛出。
func (svc *URLService) BatchCreateWithRetry(ctx context.Context, reqs []*BatchCreateReq, maxAttempts int) (*BatchCreateResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(reqs) == 0 {
		return &BatchCreateResult{}, nil
	}
	if maxAttempts <= 0 {
		maxAttempts = 2
	}
	items, failures := svc.buildBatchSaveItems(ctx, reqs)
	cfg := retry.Config{
		MaxAttempts:    maxAttempts,
		InitialBackoff: 0,
		MaxBackoff:     0,
		Multiplier:     2,
		Jitter:         false,
	}
	batchResult, berr := svc.store.SaveBatch(ctx, cfg, items)
	result := &BatchCreateResult{
		Total:   len(reqs),
		Created: make([]*model.ShortURL, 0, len(items)),
		Failed:  make([]*BatchCreateFailure, 0, len(failures)),
	}
	// 校验/生成阶段就已失败的条目直接计入 Failed。
	result.Failed = append(result.Failed, failures...)

	// 失败原因查找表：code -> 错误。优先用 batch 透传的单条原因，回退到聚合 berr。
	failByCode := make(map[string]error, len(batchResult.Failures))
	for _, f := range batchResult.Failures {
		if _, dup := failByCode[f.Code]; !dup {
			failByCode[f.Code] = f.Err
		}
	}
	fallbackReason := "persist retry exhausted after " + fmt.Sprintf("%d", maxAttempts) + " attempts"
	if berr != nil {
		fallbackReason = berr.Error()
	}

	// Created 严格来自实际成功写入的 code 集合；不在其中的条目计入 Failed。
	successSet := make(map[string]struct{}, len(batchResult.SuccessCodes))
	for _, c := range batchResult.SuccessCodes {
		successSet[c] = struct{}{}
	}
	for _, it := range items {
		if _, ok := successSet[it.ShortURL.Code]; ok {
			result.Created = append(result.Created, it.ShortURL)
			continue
		}
		// 未持久化：取单条原因，找不到则回退。
		reason := fallbackReason
		if r, ok := failByCode[it.ShortURL.Code]; ok && r != nil {
			reason = r.Error()
		}
		result.Failed = append(result.Failed, &BatchCreateFailure{
			RawURL: it.ShortURL.RawURL,
			Code:   it.ShortURL.Code,
			Reason: reason,
		})
	}
	return result, nil
}

func (svc *URLService) buildBatchSaveItems(ctx context.Context, reqs []*BatchCreateReq) ([]store.BatchSaveItem, []*BatchCreateFailure) {
	items := make([]store.BatchSaveItem, 0, len(reqs))
	fails := make([]*BatchCreateFailure, 0)
	createdAt := time.Now()
	for idx, r := range reqs {
		if r == nil {
			fails = append(fails, &BatchCreateFailure{
				RawURL: "",
				Code:   fmt.Sprintf("req-%d", idx),
				Reason: "nil request",
			})
			continue
		}
		code := r.CustomCode
		custom := code != ""
		var expireAt time.Time
		if !r.ExpireAt.IsZero() {
			expireAt = r.ExpireAt
		} else if r.TTL > 0 {
			expireAt = createdAt.Add(r.TTL)
		}
		if code == "" {
			gen, err := svc.generateUnique(ctx)
			if err != nil {
				fails = append(fails, &BatchCreateFailure{
					RawURL: r.RawURL,
					Code:   "",
					Reason: "shortcode generate: " + err.Error(),
				})
				continue
			}
			code = gen
		} else {
			ok, err := svc.store.Exists(code)
			if err != nil {
				fails = append(fails, &BatchCreateFailure{
					RawURL: r.RawURL,
					Code:   code,
					Reason: "exists check: " + err.Error(),
				})
				continue
			}
			if ok {
				fails = append(fails, &BatchCreateFailure{
					RawURL: r.RawURL,
					Code:   code,
					Reason: model.ErrCodeConflict.Error(),
				})
				continue
			}
		}
		u := &model.ShortURL{
			Code:      code,
			RawURL:    r.RawURL,
			CreatedAt: createdAt,
			ExpireAt:  expireAt,
			MaxVisits: r.MaxVisits,
			Visits:    0,
			Custom:    custom,
			Disabled:  false,
			Remark:    r.Remark,
		}
		if err := u.Validate(); err != nil {
			fails = append(fails, &BatchCreateFailure{
				RawURL: r.RawURL,
				Code:   code,
				Reason: "validate: " + err.Error(),
			})
			continue
		}
		items = append(items, store.BatchSaveItem{
			ShortURL:     u,
			Overwrite:    false,
			FailAttempts: 0,
		})
	}
	return items, fails
}
