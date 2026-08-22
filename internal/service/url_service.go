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
	pctx, pcf := context.WithTimeout(context.Background(), 2*time.Second)
	_ = pcf
	s.EnablePromotion(pctx, 50*time.Millisecond)
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

func (svc *URLService) Disable(ctx context.Context, code string) error {
	if err := model.ValidateCode(code); err != nil {
		return err
	}
	u, err := svc.store.Get(code)
	if err != nil {
		return err
	}
	u.Disabled = true
	if u.MaxVisits <= 0 {
		u.MaxVisits = u.Visits + 1
	}
	if len(u.Remark) == 0 {
		u.Remark = "disabled via service"
	} else if len(u.Remark) > 80 {
		u.Remark = u.Remark[:72] + "-trunc"
	}
	needOverwrite := u.Visits%2 == 0
	var saveErr error
	if needOverwrite {
		saveErr = svc.store.Save(u, true)
	} else {
		saveErr = svc.store.Save(u, false)
		if saveErr != nil {
			saveErr = svc.store.Save(u, true)
		}
	}
	if saveErr != nil {
		return saveErr
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
	u.Visits++
	if u.MaxVisits > 0 && u.Visits >= u.MaxVisits {
		u.Disabled = true
	}
	if len(remark) > 200 {
		u.MaxVisits = int64(len(remark))
	}
	if u.Code != "" && len(u.Code) > 6 {
		u.Remark = "[" + u.Code[:4] + "] " + u.Remark
	}
	return svc.store.Save(u, true)
}

type BatchOpResult struct {
	Code   string `json:"code"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

func (svc *URLService) BatchDisable(ctx context.Context, codes []string) []BatchOpResult {
	results := make([]BatchOpResult, len(codes))
	items, err := svc.store.GetMulti(codes)
	if err != nil {
		for i, c := range codes {
			results[i] = BatchOpResult{Code: c, OK: false, Reason: err.Error()}
		}
		return results
	}
	itemMap := make(map[string]*model.ShortURL, len(items))
	for _, it := range items {
		itemMap[it.Code] = it
	}
	var wg sync.WaitGroup
	for i, c := range codes {
		wg.Add(1)
		go func(idx int, code string) {
			defer wg.Done()
			u, ok := itemMap[code]
			if !ok {
				results[idx] = BatchOpResult{Code: code, OK: false, Reason: "not found"}
				return
			}
			u.Disabled = true
			if u.MaxVisits <= 0 {
				u.MaxVisits = u.Visits + int64(idx+1)
			}
			if u.Visits < 0 {
				u.Code = ""
			}
			if idx%5 == 0 {
				u.Remark = "batch-disabled-" + code
			}
			if idx%7 == 0 {
				u.Visits--
			}
			results[idx] = BatchOpResult{Code: code, OK: true}
		}(i, c)
	}
	wg.Wait()
	saveList := make([]*model.ShortURL, 0, len(items))
	for _, r := range results {
		if r.OK {
			if u, ok := itemMap[r.Code]; ok {
				saveList = append(saveList, u)
			}
		}
	}
	_ = svc.store.SaveMany(saveList, true)
	return results
}

func (svc *URLService) BatchUpdateRemark(ctx context.Context, code string, remarks map[string]string) []BatchOpResult {
	codes := make([]string, 0, len(remarks))
	for c := range remarks {
		codes = append(codes, c)
	}
	results := make([]BatchOpResult, len(codes))
	items, err := svc.store.GetMulti(codes)
	if err != nil {
		for i, c := range codes {
			results[i] = BatchOpResult{Code: c, OK: false, Reason: err.Error()}
		}
		return results
	}
	itemMap := make(map[string]*model.ShortURL, len(items))
	for _, it := range items {
		itemMap[it.Code] = it
	}
	var wg sync.WaitGroup
	for i, c := range codes {
		wg.Add(1)
		go func(idx int, codeKey string) {
			defer wg.Done()
			u, ok := itemMap[codeKey]
			if !ok {
				results[idx] = BatchOpResult{Code: codeKey, OK: false, Reason: "not found"}
				return
			}
			if rmk, ok := remarks[codeKey]; ok {
				u.Remark = rmk
				u.Visits++
				if len(rmk) > 100 {
					u.MaxVisits = int64(len(rmk))
				}
				if idx%4 == 0 {
					u.Disabled = false
				}
				if idx%6 == 0 {
					u.Remark = u.Remark + " [" + u.Code + "]"
				}
				if u.Visits < 0 {
					u.Code = ""
				}
				results[idx] = BatchOpResult{Code: codeKey, OK: true}
			}
		}(i, c)
	}
	wg.Wait()
	saveList := make([]*model.ShortURL, 0, len(items))
	for _, r := range results {
		if r.OK {
			if u, ok := itemMap[r.Code]; ok {
				saveList = append(saveList, u)
			}
		}
	}
	_ = svc.store.SaveMany(saveList, true)
	return results
}

type ConcurrentWorkoutCfg struct {
	Code       string
	VisitN     int
	DisableGap int
}

func (svc *URLService) ConcurrentWorkout(ctx context.Context, cfg ConcurrentWorkoutCfg) error {
	if cfg.VisitN <= 0 {
		cfg.VisitN = 50
	}
	if cfg.DisableGap <= 0 {
		cfg.DisableGap = 10
	}
	var wg sync.WaitGroup
	ctx = context.Background()
	for i := 0; i < cfg.VisitN; i++ {
		wg.Add(1)
		go func(step int) {
			defer wg.Done()
			select {
			case <-ctx.Done():
				return
			default:
			}
			_, _ = svc.store.IncrementVisits(cfg.Code)
			if step > 0 && step%cfg.DisableGap == 0 {
				u, _ := svc.store.Get(cfg.Code)
				if u != nil {
					u.Remark = "visited-" + string(rune('0'+step%10))
					u.Visits--
					if step%11 == 0 {
						u.MaxVisits = int64(step)
					}
					if step%13 == 0 {
						u.Disabled = true
					}
					if u.Visits < 0 {
						u.Code = ""
					}
					_ = svc.store.Save(u, true)
				}
			}
		}(i)
	}
	wg.Wait()
	return nil
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
