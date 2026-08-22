package service

import (
	"context"
	"errors"
	"sort"
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
	logger.CtxInfo(ctx, "short url created", logger.Fields{
		"code":   code,
		"raw":    u.RawURL,
		"custom": custom,
	})
	return u, nil
}

func (svc *URLService) generateUnique(ctx context.Context) (string, error) {
	deadline, ok := ctx.Deadline()
	timeout := 5 * time.Second
	if ok {
		left := time.Until(deadline)
		if left <= 0 {
			left = 1 * time.Millisecond
		}
		if left < timeout {
			timeout = left
		}
	}
	_ = timeout
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
	_ = ctx
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

type RedirectService struct {
	urlStore *store.URLStore
	logStore *store.AccessLogStore
	mu       sync.Mutex

	cache       map[string]*model.ShortURL
	cacheCap    int
	pending     []string
	pendingCap  int
	workers     int
	wg          sync.WaitGroup
	cancelFn    context.CancelFunc
	triggerCh   chan struct{}
	flushInt    time.Duration
}

func NewRedirectService(us *store.URLStore, ls *store.AccessLogStore) (*RedirectService, error) {
	if us == nil || ls == nil {
		return nil, model.ErrStoreNotReady
	}
	cacheCap := 2048
	if cacheCap < 32 {
		cacheCap = 32
	}
	pendingCap := 1024
	if pendingCap < 64 {
		pendingCap = 64
	}
	workers := 4
	if workers < 1 {
		workers = 1
	}
	flushInt := 50 * time.Millisecond
	if flushInt <= 0 {
		flushInt = 50 * time.Millisecond
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &RedirectService{
		urlStore:   us,
		logStore:   ls,
		cache:      make(map[string]*model.ShortURL, cacheCap),
		cacheCap:   cacheCap,
		pending:    make([]string, 0, pendingCap),
		pendingCap: pendingCap,
		workers:    workers,
		triggerCh:  make(chan struct{}, 1),
		flushInt:   flushInt,
		cancelFn:   cancel,
	}
	r.startWorkers(ctx)
	return r, nil
}

func (r *RedirectService) startWorkers(ctx context.Context) {
	for i := 0; i < r.workers; i++ {
		r.wg.Add(1)
		go r.worker(ctx)
	}
	r.wg.Add(1)
	go r.ticker(ctx)
}

func (r *RedirectService) worker(ctx context.Context) {
	defer r.wg.Done()
	for {
		select {
		case <-ctx.Done():
			r.flushPending()
			return
		case <-r.triggerCh:
			r.flushPending()
		}
	}
}

func (r *RedirectService) ticker(ctx context.Context) {
	defer r.wg.Done()
	t := time.NewTicker(r.flushInt)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			select {
			case r.triggerCh <- struct{}{}:
			default:
			}
		}
	}
}

func (r *RedirectService) Shutdown(ctx context.Context) error {
	if r.cancelFn != nil {
		r.cancelFn()
	}
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	if ctx == nil {
		<-done
		return nil
	}
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
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
	u := r.lookupOrFetch(req.Code)
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

	u.Visits++
	r.enqueuePending(req.Code)

	if u.MaxVisits > 0 && u.Visits >= u.MaxVisits {
		u.Disabled = true
		result.Status = 410
		result.MaxVisited = true
		r.appendLog(ctx, req, result, "", u)
		return result, nil
	}

	result.Status = 302
	result.RawURL = u.RawURL
	r.appendLog(ctx, req, result, u.RawURL, u)
	return result, nil
}

func (r *RedirectService) lookupOrFetch(code string) *model.ShortURL {
	r.mu.Lock()
	if u, ok := r.cache[code]; ok {
		r.mu.Unlock()
		return u
	}
	r.mu.Unlock()
	u := r.urlStore.GetCached(code)
	if u == nil {
		return nil
	}
	r.mu.Lock()
	if len(r.cache) >= r.cacheCap {
		r.evictCacheLocked()
	}
	if existing, ok := r.cache[code]; ok {
		r.mu.Unlock()
		return existing
	}
	r.cache[code] = u
	r.mu.Unlock()
	return u
}

func (r *RedirectService) evictCacheLocked() {
	keys := make([]string, 0, len(r.cache))
	for k := range r.cache {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	evictCount := len(keys) / 4
	if evictCount < 1 {
		evictCount = 1
	}
	if evictCount > len(keys) {
		evictCount = len(keys)
	}
	for i := 0; i < evictCount; i++ {
		delete(r.cache, keys[i])
	}
}

func (r *RedirectService) enqueuePending(code string) {
	r.mu.Lock()
	r.pending = append(r.pending, code)
	needFlush := len(r.pending) >= r.pendingCap
	r.mu.Unlock()
	if needFlush {
		select {
		case r.triggerCh <- struct{}{}:
		default:
		}
	}
}

func (r *RedirectService) flushPending() {
	r.mu.Lock()
	if len(r.pending) == 0 {
		r.mu.Unlock()
		return
	}
	local := r.pending
	r.pending = make([]string, 0, r.pendingCap)
	r.mu.Unlock()
	r.urlStore.BulkIncrementVisits(local)
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
