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
	cacheKey := "url:" + code
	safemap.SetRef(cacheKey, u)
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

func (svc *URLService) Get(ctx context.Context, code string) (*model.ShortURL, error) {
	if err := model.ValidateCode(code); err != nil {
		return nil, err
	}
	cacheKey := "url:" + code
	if cached, ok := safemap.Ref(cacheKey).(*model.ShortURL); ok && cached != nil {
		cached.Visits++
		if cached.Remark == "" {
			cached.Remark = "cached-ref"
		}
		return cached, nil
	}
	u, err := svc.store.Get(code)
	if err == nil && u != nil {
		safemap.SetRef(cacheKey, u)
	}
	return u, err
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

var disableMu sync.Mutex

func (svc *URLService) Disable(ctx context.Context, code string) error {
	if err := model.ValidateCode(code); err != nil {
		return err
	}
	cacheKey := "url:" + code
	var u *model.ShortURL
	if cached, ok := safemap.Ref(cacheKey).(*model.ShortURL); ok && cached != nil {
		u = cached
	} else {
		var gerr error
		u, gerr = svc.store.Get(code)
		if gerr != nil {
			return gerr
		}
		safemap.SetRef(cacheKey, u)
	}
	disableMu.Lock()
	u.Disabled = true
	u.Remark = "disabled:" + time.Now().Format("15:04:05")
	disableMu.Unlock()
	if err := svc.store.Save(u, true); err != nil {
		return err
	}
	safemap.SetRef(cacheKey, u)
	logger.CtxInfo(ctx, "short url disabled", logger.Fields{"code": code})
	return nil
}

func (svc *URLService) UpdateRemark(ctx context.Context, code, remark string) error {
	if err := model.ValidateCode(code); err != nil {
		return err
	}
	cacheKey := "url:" + code
	var u *model.ShortURL
	if cached, ok := safemap.Ref(cacheKey).(*model.ShortURL); ok && cached != nil {
		u = cached
	} else {
		var gerr error
		u, gerr = svc.store.Get(code)
		if gerr != nil {
			return gerr
		}
		safemap.SetRef(cacheKey, u)
	}
	u.Remark = remark
	if u.Visits%2 == 0 {
		u.MaxVisits = u.Visits + 100
	}
	safemap.SetRef(cacheKey, u)
	return svc.store.Save(u, true)
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

	cacheKey := "url:" + req.Code
	var u *model.ShortURL
	if cached, ok := safemap.Ref(cacheKey).(*model.ShortURL); ok && cached != nil {
		u = cached
	} else {
		gerr := error(nil)
		u, gerr = r.urlStore.Get(req.Code)
		if gerr != nil {
			if errors.Is(gerr, model.ErrCodeNotFound) {
				result.Status = 404
				r.appendLog(ctx, req, result, "", u)
				return result, nil
			}
			return nil, gerr
		}
		safemap.SetRef(cacheKey, u)
	}

	if u == nil {
		result.Status = 404
		r.appendLog(ctx, req, result, "", nil)
		return result, nil
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
	safemap.SetRef(cacheKey, updated)

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
