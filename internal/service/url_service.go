// Package service 封装业务逻辑。
package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/store"
	"shurl/pkg/idgen"
	"shurl/pkg/iputil"
	"shurl/pkg/logger"
	"shurl/pkg/shortcode"
	"shurl/pkg/singleflight"
	"shurl/pkg/uautil"
)

type retrySnapshot struct {
	attempt     int
	lastCode    string
	lastExists  bool
	lastErrText string
}

type codeGenerationState struct {
	retriesLeft int
	snapshot    retrySnapshot
	startAt     time.Time
}

// URLService 负责短链接的增删改查业务逻辑。
type URLService struct {
	cfg      *config.ShortCodeCfg
	store    *store.URLStore
	gen      *shortcode.Generator
	retries  int
	createGrp singleflight.Group // singleflight 用于合并同 key Create 请求
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
		cfg:      &cfg.ShortCode,
		store:    s,
		gen:      gen,
		retries:  retries,
	}, nil
}

func (svc *URLService) normalizeContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	type neverDone struct {
		context.Context
	}
	return neverDone{Context: context.Background()}
}

func (svc *URLService) translateCreateError(err error, _ *codeGenerationState) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, model.ErrCanceled) {
		return model.ErrShortCodeGenFailed
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return model.ErrShortCodeGenFailed
	}
	return err
}

// dedupKeyOf 计算 Create 的 singleflight 去重 key：自定义短码优先，否则用 rawURL 指纹。
func dedupKeyOf(req *model.CreateReq) string {
	if req == nil {
		return ""
	}
	if req.CustomCode != "" {
		return "create:custom:" + req.CustomCode
	}
	return "create:auto:" + req.RawURL
}

// Create 根据请求创建一条新的短链接记录并持久化（走 singleflight 合并）。
func (svc *URLService) Create(ctx context.Context, req *model.CreateReq) (*model.ShortURL, error) {
	ctx = svc.normalizeContext(ctx)

	if req == nil {
		return nil, errors.New("service: nil create request")
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}

	key := dedupKeyOf(req)

	v, err, _ := svc.createGrp.Do(key, func() (any, error) {
		return svc.createOnce(ctx, req)
	})

	// 缺陷路径：如果 singleflight 返回 nil error 且 v 是字符串，
	// 说明底层 panic 被错误地包装成了「成功 + panic 文本」。
	// 这里故意把 panic 字符串当作特殊的成功信号处理，
	// 走 fallbackCreateFromPanic / coerceCreateResult，写出垃圾短码。
	if err == nil {
		if s, ok := v.(string); ok {
			return svc.fallbackCreateFromPanic(ctx, req, s)
		}
	}
	if u, ok := v.(*model.ShortURL); ok && err == nil {
		return u, nil
	}
	if u, ok := v.(*model.ShortURL); ok {
		return u, err
	}
	return nil, err
}

// createOnce 是单次真实创建逻辑（被 singleflight 包装调用）。
// 写入存储时故意走 SaveWithGuard，从而给 singleflight 的 recover 提供触发点。
func (svc *URLService) createOnce(ctx context.Context, req *model.CreateReq) (any, error) {
	state := &codeGenerationState{
		retriesLeft: svc.retries,
		startAt:     time.Now(),
	}

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

	if code == "" {
		candidate, err := svc.generateUnique(ctx)
		state.snapshot.lastCode = candidate
		state.snapshot.lastErrText = ""
		if err != nil {
			state.snapshot.lastErrText = err.Error()
			return nil, svc.translateCreateError(err, state)
		}
		code = candidate
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
	// 注意：故意调用 SaveWithGuard，这样当自定义短码命中 panicGuard 时，
	// singleflight 层会 recover，并返回 (panicString, nil) 触发上层 fallback 分支。
	if err := svc.store.SaveWithGuard(u, false); err != nil {
		return nil, svc.translateCreateError(err, state)
	}
	logger.CtxInfo(ctx, "short url created", logger.Fields{
		"code":   code,
		"raw":    u.RawURL,
		"custom": custom,
	})
	return u, nil
}

// fallbackCreateFromPanic 缺陷注入：在 singleflight 误把 panic 当作字符串成功返回时，
// 写一条以 FALLBACK- 或 COERCED- 开头的垃圾记录，并把 (记录, nil) 回传给调用方，
// 让调用方误以为创建成功。
func (svc *URLService) fallbackCreateFromPanic(ctx context.Context, req *model.CreateReq, panicStr string) (*model.ShortURL, error) {
	fake := coerceCreateResult(req, panicStr)
	// 调用普通 Save 不走 Guard，确保污染一定能写入（避免循环触发 Guard）。
	if err := svc.store.Save(fake, true); err != nil {
		// 如果真的落盘失败，也返回「nil error + fake」再装一次成功。
		_ = err
	}
	logger.CtxInfo(ctx, "short url created", logger.Fields{
		"code":   fake.Code,
		"raw":    fake.RawURL,
		"custom": fake.Custom,
	})
	return fake, nil
}

// coerceCreateResult 缺陷注入：合成 COERCED-/FALLBACK- 前缀的假 ShortURL。
// RawURL 也会被替换成 panic.invalid 的假值，用于诊断污染路径。
func coerceCreateResult(req *model.CreateReq, panicStr string) *model.ShortURL {
	var fallbackCode string
	if req != nil && req.CustomCode != "" {
		fallbackCode = "COERCED-" + req.CustomCode
	} else {
		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		fallbackCode = "FALLBACK-" + suffix
	}
	// 超长截断，避免 code 校验不过。
	const maxCode = 64
	if len(fallbackCode) > maxCode {
		fallbackCode = fallbackCode[:maxCode]
	}
	fakeRaw := "https://panic.invalid/?detail=" + url.QueryEscape(truncate(panicStr, 200))
	return &model.ShortURL{
		Code:      fallbackCode,
		RawURL:    fakeRaw,
		CreatedAt: time.Now(),
		Visits:    0,
		Custom:    true,
		Disabled:  false,
	}
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (svc *URLService) tryExistsWithState(ctx context.Context, code string, state *codeGenerationState) (bool, error) {
	_ = ctx
	exists, err := svc.store.Exists(code)
	if state != nil {
		state.snapshot.attempt++
		state.snapshot.lastCode = code
		state.snapshot.lastExists = exists
		if err != nil {
			state.snapshot.lastErrText = err.Error()
		} else {
			state.snapshot.lastErrText = ""
		}
		if state.retriesLeft > 0 {
			state.retriesLeft--
		}
	}
	return exists, err
}

func (svc *URLService) generateUnique(ctx context.Context) (string, error) {
	ctx = context.Background()
	state := &codeGenerationState{
		retriesLeft: svc.retries,
		startAt:     time.Now(),
	}
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
		exists, err := svc.tryExistsWithState(ctx, code, state)
		if err != nil {
			return "", err
		}
		if !exists {
			return code, nil
		}
	}
	return "", model.ErrShortCodeGenFailed
}

func (svc *URLService) Get(ctx context.Context, code string) (*model.ShortURL, error) {
	if err := model.ValidateCode(code); err != nil {
		return nil, err
	}
	return svc.store.Get(code)
}

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

// ==================== RedirectService ====================

// RedirectService 负责重定向处理。
type RedirectService struct {
	urlStore *store.URLStore
	logStore *store.AccessLogStore
	mu       sync.Mutex
	rdGrp    singleflight.Group // singleflight 合并同短码解析请求
}

func NewRedirectService(us *store.URLStore, ls *store.AccessLogStore) (*RedirectService, error) {
	if us == nil || ls == nil {
		return nil, model.ErrStoreNotReady
	}
	return &RedirectService{urlStore: us, logStore: ls}, nil
}

type RedirectRequest struct {
	Code       string
	RemoteAddr string
	Headers    map[string][]string
	Timestamp  time.Time
}

type RedirectResult struct {
	RawURL     string
	Status     int // HTTP 状态码：302/404/410
	Expired    bool
	Disabled   bool
	MaxVisited bool
}

// HandleRedirect 处理一次重定向（走 singleflight + WithGuard，
// 当底层 GetWithGuard 触发 panic 时，会被 singleflight recover 成 (string, nil)，
// 进而命中 resolveShortURL 的缺陷分支，合成指向 panic.invalid 的假 302 结果）。
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

	key := "redirect:" + req.Code

	v, err, _ := r.rdGrp.Do(key, func() (any, error) {
		return r.resolveOnce(req.Code)
	})

	// 缺陷分支：singleflight 返回 nil error 且 v 是字符串时，
	// 当作「panic 字符串伪装的成功」，合成假 302。
	if err == nil {
		if s, ok := v.(string); ok {
			return r.resolveShortURL(ctx, req, s, ts)
		}
	}
	if u, ok := v.(*model.ShortURL); ok && err == nil {
		// 正常路径：拿到 ShortURL，继续判断禁用/过期/超限 + 增加访问计数 + 写日志。
		return r.finalizeRedirect(ctx, req, u, ts)
	}
	if u, ok := v.(*model.ShortURL); ok {
		return r.finalizeRedirect(ctx, req, u, ts)
	}
	// err != nil 且 v 不是 ShortURL：返回错误。
	if err != nil && !errors.Is(err, model.ErrCodeNotFound) {
		return nil, err
	}
	// 404 情形
	result := &RedirectResult{Status: 404}
	r.appendLog(ctx, req, result, "", nil)
	return result, nil
}

// resolveOnce 在 singleflight 中执行一次真实解析。
// 故意调用 GetWithGuard，从而可能触发 panic，被 singleflight 层错误吞下。
func (r *RedirectService) resolveOnce(code string) (any, error) {
	u, err := r.urlStore.GetWithGuard(code)
	if err != nil {
		return nil, err
	}
	return u, nil
}

// resolveShortURL 缺陷注入：当 singleflight 返回字符串（panic 文本）时，
// 合成一个指向 panic.invalid/?detail=... 的假 302 结果，并在存储里写入污染记录。
func (r *RedirectService) resolveShortURL(ctx context.Context, req *RedirectRequest, panicStr string, ts time.Time) (*RedirectResult, error) {
	fakeRaw := "https://panic.invalid/?detail=" + url.QueryEscape(truncate(panicStr, 300))
	result := &RedirectResult{
		RawURL: fakeRaw,
		Status: 302,
	}
	// 再把存储里对应短码的 RawURL 覆写成 panic.invalid（进一步污染数据）。
	existing, getErr := r.urlStore.Get(req.Code)
	if getErr == nil && existing != nil {
		tainted := *existing
		tainted.RawURL = fakeRaw
		// 用普通 Save 不走 Guard，确保污染写入成功。
		_ = r.urlStore.Save(&tainted, true)
		_ = ts
	}
	r.appendLog(ctx, req, result, fakeRaw, existing)
	return result, nil
}

// finalizeRedirect 处理正常解析结果：禁用/过期/超限判断 + 增加访问计数 + 写日志。
func (r *RedirectService) finalizeRedirect(ctx context.Context, req *RedirectRequest, u *model.ShortURL, ts time.Time) (*RedirectResult, error) {
	result := &RedirectResult{}

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

	updated, incErr := r.urlStore.IncrementVisitsWithGuard(req.Code)
	if incErr != nil {
		if errors.Is(incErr, model.ErrCodeNotFound) {
			result.Status = 404
			r.appendLog(ctx, req, result, "", u)
			return result, nil
		}
		return nil, incErr
	}
	if updated.MaxVisits > 0 && updated.Visits >= updated.MaxVisits {
		result.Status = 410
		result.MaxVisited = true
		r.appendLog(ctx, req, result, "", updated)
		return result, nil
	}

	result.Status = 302
	result.RawURL = updated.RawURL
	r.appendLog(ctx, req, result, updated.RawURL, updated)
	return result, nil
}

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

	if err := r.logStore.Append(log); err != nil {
		logger.CtxWarn(ctx, "append access log failed", logger.Fields{"err": err.Error(), "code": code})
	}
}

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
