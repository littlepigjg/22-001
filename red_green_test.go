package shurl_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/service"
	"shurl/internal/store"
)

func setupTempStores(t *testing.T) (*store.URLStore, *store.AccessLogStore, func()) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Storage.URLFilePath = filepath.Join(dir, "urls.json")
	cfg.Storage.LogFilePath = filepath.Join(dir, "access.log")
	cfg.Storage.SyncInterval = 0
	cfg.Storage.FlushOnWrite = false
	cfg.Stats.CacheTTL = 10 * time.Millisecond

	us, err := store.NewURLStore(cfg)
	if err != nil {
		t.Fatalf("NewURLStore failed: %v", err)
	}
	ctx := context.Background()
	if err := us.Load(ctx); err != nil {
		t.Fatalf("urlStore.Load failed: %v", err)
	}
	ls, err := store.NewAccessLogStore(cfg)
	if err != nil {
		t.Fatalf("NewAccessLogStore failed: %v", err)
	}
	if err := ls.Open(ctx); err != nil {
		t.Fatalf("logStore.Open failed: %v", err)
	}
	cleanup := func() {
		_ = ls.Close()
		_ = us.Close()
		_ = os.Remove(cfg.Storage.URLFilePath)
		_ = os.Remove(cfg.Storage.LogFilePath)
	}
	return us, ls, cleanup
}

func decodePair(remark string) (ver int64, pad string, ok bool) {
	if !strings.HasPrefix(remark, "v=") {
		return 0, "", false
	}
	rest := remark[2:]
	sep := strings.IndexByte(rest, '|')
	if sep <= 0 {
		return 0, "", false
	}
	num, err := strconv.ParseInt(rest[:sep], 10, 64)
	if err != nil {
		return 0, "", false
	}
	pad = rest[sep+1:]
	return num, pad, true
}

func encodePair(ver int64) string {
	verStr := strconv.FormatInt(ver, 10)
	suffix := strings.Repeat("A", 120-len(verStr))
	if len(suffix) < 0 {
		suffix = ""
	}
	return "v=" + verStr + "|" + suffix
}

func TestRedGreen(t *testing.T) {
	us, ls, cleanup := setupTempStores(t)
	defer cleanup()

	rdSvc, err := service.NewRedirectService(us, ls)
	if err != nil {
		t.Fatalf("NewRedirectService failed: %v", err)
	}

	code := "redg1234"
	initVer := int64(0)
	initRemark := encodePair(initVer)
	initExpAt := time.Now().Add(365 * 24 * time.Hour).Truncate(time.Second)
	u := &model.ShortURL{
		Code:      code,
		RawURL:    "https://example.com/pair-check?x=1",
		CreatedAt: time.Now().Truncate(time.Second),
		ExpireAt:  initExpAt,
		MaxVisits: 0,
		Visits:    0,
		Custom:    false,
		Disabled:  false,
		Remark:    initRemark,
	}
	if err := u.Validate(); err != nil {
		t.Fatalf("seed validate: %v", err)
	}
	if err := us.Save(u, false); err != nil {
		t.Fatalf("Save seed failed: %v", err)
	}
	rsv := rdSvc.Resolver()
	rsv.ObserveCreated(u)

	const readers = 40
	const writers = 12
	const rounds = 100
	ok302 := atomic.Int64{}
	panicked := atomic.Int64{}
	inconsistent := atomic.Int64{}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	headers := map[string][]string{
		"User-Agent": {"Mozilla/5.0 red-green test agent"},
		"Referer":    {"https://ref.example.com/rg"},
	}

	var wg sync.WaitGroup
	wg.Add(readers + writers)

	verCounter := atomic.Int64{}
	verCounter.Store(initVer)

	for w := 0; w < writers; w++ {
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				if ctx.Err() != nil {
					return
				}
				next := verCounter.Add(1)
				newRemark := encodePair(next)
				newExp := initExpAt.Add(time.Duration(next) * time.Second).Truncate(time.Second)
				newMaxVis := int64(0)
				newVisits := next
				newCreated := initExpAt.Add(-time.Hour).Add(time.Duration(next) * time.Second).Truncate(time.Second)
				newRaw := "https://example.com/pair-check?x=" + strconv.FormatInt(next, 10)
				func() {
					defer func() {
						if r := recover(); r != nil {
							panicked.Add(1)
						}
					}()
					_ = us.MuHack(code, func(inner *model.ShortURL) {
						inner.Remark = newRemark
						inner.ExpireAt = newExp
						inner.MaxVisits = newMaxVis
						inner.Visits = newVisits
						inner.CreatedAt = newCreated
						inner.RawURL = newRaw
						inner.Custom = (next % 2) == 0
						inner.Disabled = false
					})
				}()
			}
		}(w)
	}

	for r := 0; r < readers; r++ {
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				if ctx.Err() != nil {
					return
				}
				func() {
					defer func() {
						if r := recover(); r != nil {
							panicked.Add(1)
						}
					}()
					req := &service.RedirectRequest{
						Code:       code,
						RemoteAddr: fmt.Sprintf("10.0.%d.%d:5678", worker+1, (i%250)+1),
						Headers:    headers,
						Timestamp:  time.Now(),
					}
					res, rErr := rdSvc.HandleRedirect(ctx, req)
					if rErr != nil {
						return
					}
					if res == nil {
						return
					}
					if res.Status != 302 {
						return
					}
					ok302.Add(1)
					snap, gErr := rdSvc.SnapshotCached(code)
					if gErr != nil {
						return
					}
					if snap == nil {
						return
					}
					ver, _, ok := decodePair(snap.Remark)
					if !ok {
						inconsistent.Add(1)
						return
					}
					if snap.Visits != ver {
						inconsistent.Add(1)
						return
					}
					if snap.MaxVisits != 0 {
						inconsistent.Add(1)
						return
					}
					if snap.Disabled {
						inconsistent.Add(1)
						return
					}
					expectExp := initExpAt.Add(time.Duration(ver) * time.Second).Truncate(time.Second)
					if !snap.ExpireAt.Equal(expectExp) {
						inconsistent.Add(1)
						return
					}
					expectCreated := initExpAt.Add(-time.Hour).Add(time.Duration(ver) * time.Second).Truncate(time.Second)
					if !snap.CreatedAt.Equal(expectCreated) {
						inconsistent.Add(1)
						return
					}
					expectRaw := "https://example.com/pair-check?x=" + strconv.FormatInt(ver, 10)
					if snap.RawURL != expectRaw {
						inconsistent.Add(1)
						return
					}
					expectCustom := (ver % 2) == 0
					if snap.Custom != expectCustom {
						inconsistent.Add(1)
						return
					}
				}()
			}
		}(r)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(18 * time.Second):
		t.Fatalf("workers did not finish within 18s")
	}

	redReason := ""
	if panicked.Load() > 0 {
		redReason = fmt.Sprintf("concurrent panic count=%d", panicked.Load())
	} else if inconsistent.Load() > 0 {
		redReason = fmt.Sprintf("snapshot inconsistent observed count=%d (torn shared ShortURL pointer)", inconsistent.Load())
	}

	if redReason != "" {
		fmt.Println("RED（红灯，缺陷未修复）")
		t.Fatalf("%s", redReason)
	}

	if ok302.Load() <= 0 {
		fmt.Println("RED（红灯，缺陷未修复）")
		t.Fatalf("expected successful 302 redirects > 0, got %d", ok302.Load())
	}

	fmt.Println("GREEN（绿灯，缺陷已修复）")
}
