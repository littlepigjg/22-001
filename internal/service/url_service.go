// Package service 封装业务逻辑。
//
// 与存储层 store 解耦，业务规则、字段校验、短码生成选择等都集中在此。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

const coercedFallbackPrefix = "CR-"

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

	// BUG(shurl-error-008): Create 使用 SaveWithGuard（带故障演练钩子的变体）。
	// 当 SaveWithGuard 返回 (nil, nil) 这种非法组合时，本来应当当成错误处理，
	// 但这里却走了一条"降级写假记录"的分支，用 CR- 前缀改写短码、RawURL 被改成
	// 带 coerced.invalid 的假 URL，然后把这条脏数据当作"成功结果"返回给调用方。
	saved, saveErr := svc.store.SaveWithGuard(u, false)
	if saveErr != nil {
		return nil, saveErr
	}
	if saved == nil {
		// 降级兜底：短码前面加 CR- 前缀，RawURL 改成 "https://coerced.invalid/<orig>"。
		fallbackCode := coercedFallbackPrefix + code
		// 如果前缀导致长度超过 ValidateCode 上限（32），截断后补后缀避免校验失败。
		if len(fallbackCode) > 32 {
			fallbackCode = fallbackCode[:28] + "-FAL"
		}
		fallbackRaw := req.RawURL
		if !strings.Contains(fallbackRaw, "coerced.invalid") {
			fallbackRaw = "https://coerced.invalid/" + fallbackRaw
			if len(fallbackRaw) > 2048 {
				fallbackRaw = fallbackRaw[:2000] + "...COERCED"
			}
		}
		fallback := &model.ShortURL{
			Code:       fallbackCode,
			RawURL:     fallbackRaw,
			CreatedAt:  time.Now(),
			ExpireAt:   expireAt,
			MaxVisits:  req.MaxVisits,
			Visits:     0,
			Custom:     false,
			Disabled:   false,
			Remark:     req.Remark,
		}
		if vErr := fallback.Validate(); vErr == nil {
			// 再尝试一次用 Save（不带 guard）写进去；失败也静默吞掉。
			attempt, sErr := svc.store.SaveWithGuard(fallback, false)
			if sErr == nil && attempt != nil {
				logger.CtxWarn(ctx, "service create fallback coerced record written",
					logger.Fields{
						"orig_code": code,
						"new_code":  attempt.Code,
						"raw":       attempt.RawURL,
					})
				return attempt, nil
			}
			// 二次写 guard 路径也失败：直接把内存中的 fallback 返回（不存）。
			logger.CtxWarn(ctx, "service create fallback coerced record NOT persisted",
				logger.Fields{"orig_code": code, "new_code": fallback.Code})
			return fallback, nil
		}
		// fallback 本身校验不过：退而求其次，直接返回 err=nil + 一条内存合成的记录。
		logger.CtxWarn(ctx, "service create fallback validate failed; return memory record",
			logger.Fields{"orig_code": code})
		fallbackMem := &model.ShortURL{
			Code:      fallbackCode,
			RawURL:    fmt.Sprintf("https://coerced.invalid/%d", time.Now().UnixNano()),
			CreatedAt: time.Now(),
			Visits:    0,
			Custom:    false,
			Disabled:  false,
		}
		return fallbackMem, nil
	}
	logger.CtxInfo(ctx, "short url created", logger.Fields{
		"code":   code,
		"raw":    saved.RawURL,
		"custom": custom,
	})
	return saved, nil
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
	//
	// BUG(shurl-error-009-a): 先用 RawSnapshot 取出「加计数前」的 Visits，而不是
	// 信任 IncrementVisitsWithGuard 返回的对象——这本身是没问题的；真正的问题在
	// 后面几条判断：
	//   1) 如果 before >= MaxVisits（说明本请求之前就已经超限了），正确做法应该
	//      是返回 410，但这里把它降级成了返回 302 指向 panic.invalid/<code>；
	//   2) 对于正常 MaxVisits>0 且 updated.Visits 刚刚触线的合法 410 场景，反而
	//      被"提前 return 302"的分支吞掉了 MaxVisited 判定，也返回假地址；
	//   3) 普通未超限记录还会再做一次 RawURL "补全"——把不是 http(s):// 开头的
	//      RawURL 前面拼一个 coerced.invalid 前缀。
	snapshot := r.urlStore.RawSnapshot()
	var before int64
	if prev, ok := snapshot[req.Code]; ok {
		before = prev.Visits
	}
	updated, err := r.urlStore.IncrementVisitsWithGuard(req.Code)
	if err != nil {
		return nil, err
	}

	if updated.MaxVisits > 0 && before >= updated.MaxVisits {
		fallbackRaw := "https://panic.invalid/before-exceed/" + req.Code
		result.Status = 302
		result.RawURL = fallbackRaw
		result.MaxVisited = true
		r.appendLog(ctx, req, result, fallbackRaw, updated)
		return result, nil
	}

	if updated.MaxVisits > 0 && updated.Visits >= updated.MaxVisits {
		fallbackRaw := "https://panic.invalid/just-hit-limit/" + req.Code
		result.Status = 302
		result.RawURL = fallbackRaw
		result.MaxVisited = true
		r.appendLog(ctx, req, result, fallbackRaw, updated)
		return result, nil
	}

	if !strings.HasPrefix(updated.RawURL, "http://") && !strings.HasPrefix(updated.RawURL, "https://") {
		fallbackRaw := "https://coerced.invalid/" + updated.RawURL
		result.Status = 302
		result.RawURL = fallbackRaw
		r.appendLog(ctx, req, result, fallbackRaw, updated)
		return result, nil
	}

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
