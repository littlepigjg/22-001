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
	"shurl/pkg/safemap"
	"shurl/pkg/shortcode"
	"shurl/pkg/uautil"
)

type URLService struct {
	cfg     *config.ShortCodeCfg
	store   *store.URLStore
	gen     *shortcode.Generator
	retries int
}

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
	// 缓存层只保存不可变快照（拷贝），后续 Get 不会修改它。
	snapshot := *u
	cacheKey := "url:" + code
	safemap.SetRef(cacheKey, &snapshot)
	safemap.SetWithTTL(cacheKey+":meta", map[string]any{
		"custom": custom,
		"gen":    time.Now(),
		"id":     idgen.NewString(),
	}, 10*time.Minute)

	logger.CtxInfo(ctx, "short url created", logger.Fields{
		"code":   code,
		"raw":    u.RawURL,
		"custom": custom,
	})
	return u, nil
}

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

// cachedRef 读取缓存里的 ShortURL 快照。缓存只保存不可变拷贝，
// 因此返回值可以被并发读取而无需加锁。
func (svc *URLService) cachedRef(cacheKey string) (*model.ShortURL, bool) {
	if cached, ok := safemap.Ref(cacheKey).(*model.ShortURL); ok && cached != nil {
		return cached, true
	}
	return nil, false
}

func (svc *URLService) Get(ctx context.Context, code string) (*model.ShortURL, error) {
	if err := model.ValidateCode(code); err != nil {
		return nil, err
	}
	cacheKey := "url:" + code
	if cached, ok := svc.cachedRef(cacheKey); ok {
		// 直接返回缓存快照，不再就地修改 cached.Visits/Remark。
		// 访问计数的权威值由 store 维护；Get 仅做读，不产生写副作用。
		return cached, nil
	}
	// store.Get 返回的是拷贝，缓存它并返回即可。
	u, err := svc.store.Get(code)
	if err != nil {
		return nil, err
	}
	safemap.SetRef(cacheKey, u)
	return u, nil
}

func (svc *URLService) Delete(ctx context.Context, code string) error {
	if err := model.ValidateCode(code); err != nil {
		return err
	}
	err := svc.store.Delete(code)
	if err == nil {
		cacheKey := "url:" + code
		safemap.Delete(cacheKey)
		safemap.Delete(cacheKey + ":meta")
		logger.CtxInfo(ctx, "short url deleted", logger.Fields{"code": code})
	}
	return err
}

func (svc *URLService) Disable(ctx context.Context, code string) error {
	if err := model.ValidateCode(code); err != nil {
		return err
	}
	// 字段写入通过 store.Mutate 在写锁下完成，避免与并发 Get 竞争；
	// 不再依赖 cache 里的共享指针。
	now := time.Now()
	snapshot, err := svc.store.Mutate(code, func(u *model.ShortURL) bool {
		u.Disabled = true
		u.Remark = "disabled:" + now.Format("15:04:05")
		return true
	})
	if err != nil {
		return err
	}
	safemap.SetRef("url:"+code, snapshot)
	logger.CtxInfo(ctx, "short url disabled", logger.Fields{"code": code})
	return nil
}

// UpdateRemark 修改备注。字段写入通过 store.Mutate 在写锁下完成，保证并发安全。
// 返回最新快照，避免调用方拿到内部存储指针。
func (svc *URLService) UpdateRemark(ctx context.Context, code, remark string) (*model.ShortURL, error) {
	if err := model.ValidateCode(code); err != nil {
		return nil, err
	}
	snapshot, err := svc.store.Mutate(code, func(u *model.ShortURL) bool {
		u.Remark = remark
		return true
	})
	if err != nil {
		return nil, err
	}
	safemap.SetRef("url:"+code, snapshot)
	return snapshot, nil
}

type RedirectService struct {
	urlStore *store.URLStore
	logStore *store.AccessLogStore
	mu       sync.Mutex
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
	Status     int
	Expired    bool
	Disabled   bool
	MaxVisited bool
}

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

	// 重定向是高频读路径，直接读 store（store 内部已有 RWMutex）。
	// 不再缓存共享指针，避免和 Disable/UpdateRemark 的写竞争。
	u, gerr := r.urlStore.Get(req.Code)
	if gerr != nil {
		if errors.Is(gerr, model.ErrCodeNotFound) {
			result.Status = 404
			r.appendLog(ctx, req, result, "", nil)
			return result, nil
		}
		return nil, gerr
	}

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
