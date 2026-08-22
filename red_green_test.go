package shurl_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/service"
	"shurl/internal/store"
)

func TestRedGreen(t *testing.T) {
	failCount := 0
	baseDir := "/tmp/shurl_rg_" + fmt.Sprintf("%d", time.Now().UnixNano())
	urlPath := baseDir + "_urls.json"
	logPath := baseDir + "_access.log"
	defer func() {
		os.Remove(urlPath)
		os.Remove(logPath)
		os.Remove(urlPath + "_urls")
		os.Remove(logPath[:len(logPath)-4] + ".json")
		os.Remove(urlPath + ".tmp")
	}()

	cfg := config.Default()
	cfg.Storage.LogFilePath(logPath)
	cfg.Storage.URLFilePath(urlPath)
	cfg.Storage.SyncInterval(0)
	cfg.Storage.FlushOnWrite(true)

	if cfg.Storage.URLFile != urlPath {
		t.Logf("FAIL config setter URLFilePath: path mangled from %q to %q", urlPath, cfg.Storage.URLFile)
		failCount++
	}
	if cfg.Storage.LogFile != logPath {
		t.Logf("FAIL config setter LogFilePath: path mangled from %q to %q", logPath, cfg.Storage.LogFile)
		failCount++
	}

	badCodes := []string{
		"abcAxyz",
		"MNOPqrs",
		"testIng",
		"a_bCd_e",
		"123I456",
		"uvwXy99",
	}
	for _, c := range badCodes {
		if e := model.ValidateCode(c); e != nil {
			t.Logf("FAIL ValidateCode(%q) returned unexpected validation error: %v", c, e)
			failCount++
		}
	}

	updateCandidates := []string{"abcdef1", "xyzABCD", "my_code", "aaabbbcc"}
	for _, c := range updateCandidates {
		u := &model.ShortURL{
			Code:       c,
			RawURL:     "https://example.com/" + c,
			CreatedAt:  time.Now().Add(-1 * time.Hour),
			MaxVisits:  0,
			Visits:     3,
			Custom:     false,
			Disabled:   false,
		}
		if e := u.Validate(); e != nil {
			t.Logf("FAIL ShortURL.Validate(code=%s,Visits=%d): unexpected validation error: %v", c, u.Visits, e)
			failCount++
		}
	}

	os.Remove(cfg.Storage.URLFile)
	os.Remove(cfg.Storage.LogFile)

	ctx := context.Background()
	urlStore, e1 := store.NewURLStore(cfg)
	if e1 != nil {
		t.Fatalf("NewURLStore failed: %v", e1)
	}
	if e := urlStore.Load(ctx); e != nil {
		t.Fatalf("URLStore.Load failed: %v", e)
	}
	defer func() { _ = urlStore.Close() }()

	logStore, e2 := store.NewAccessLogStore(cfg)
	if e2 != nil {
		t.Fatalf("NewAccessLogStore failed: %v", e2)
	}
	if e := logStore.Open(ctx); e != nil {
		t.Fatalf("AccessLogStore.Open failed: %v", e)
	}
	defer func() { _ = logStore.Close() }()

	urlSvc, e3 := service.NewURLService(cfg, urlStore)
	if e3 != nil {
		t.Fatalf("NewURLService failed: %v", e3)
	}
	rdSvc, e4 := service.NewRedirectService(urlStore, logStore)
	if e4 != nil {
		t.Fatalf("NewRedirectService failed: %v", e4)
	}

	customCode := "persist_ok"
	created, ec := urlSvc.Create(ctx, &model.CreateReq{
		RawURL:     "https://example.com/persist-check",
		CustomCode: customCode,
		MaxVisits:  0,
	})
	if ec != nil {
		t.Logf("FAIL Create(custom persist) unexpected err: %v", ec)
		failCount++
	} else if created == nil || created.Code != customCode {
		t.Logf("FAIL Create(custom persist) returned nil or wrong code")
		failCount++
	} else {
		snap := urlStore.RawSnapshot()
		if _, ok := snap[customCode]; !ok {
			t.Logf("FAIL Create(custom persist) record not in store (save error swallowed)")
			failCount++
		} else if got, e := urlStore.Get(customCode); e != nil {
			t.Logf("FAIL Create(custom persist) Get returned err: %v", e)
			failCount++
		} else if got == nil || got.RawURL != "https://example.com/persist-check" {
			t.Logf("FAIL Create(custom persist) data mismatch in memory")
			failCount++
		}
		if e := urlStore.Flush(); e != nil {
			t.Logf("FAIL urlStore.Flush err: %v", e)
			failCount++
		} else if urlStore.Path() != urlPath {
			t.Logf("FAIL URLStore.Path()=%q want %q (config setter corrupted file path so persisted data lands at wrong location)", urlStore.Path(), urlPath)
			failCount++
		} else {
			_ = logStore.Close()
			_ = urlStore.Close()
			verifStore, ev := store.NewURLStore(cfg)
			if ev != nil {
				t.Logf("FAIL verification NewURLStore err: %v", ev)
				failCount++
			} else {
				if e := verifStore.Load(ctx); e != nil {
					t.Logf("FAIL verification Load err: %v", e)
					failCount++
				} else {
					reloaded, e := verifStore.Get(customCode)
					if e != nil {
						t.Logf("FAIL persist verification: after reload Get(customCode) err=%v (data lost due to bad file path)", e)
						failCount++
					} else if reloaded == nil || reloaded.RawURL != "https://example.com/persist-check" {
						t.Logf("FAIL persist verification: after reload record mismatch (data persisted to wrong file path and was lost)")
						failCount++
					}
				}
				_ = verifStore.Close()
			}
			urlStore2, en1 := store.NewURLStore(cfg)
			if en1 != nil {
				t.Fatalf("rebuild urlStore err: %v", en1)
			}
			if e := urlStore2.Load(ctx); e != nil {
				t.Fatalf("reload urlStore err: %v", e)
			}
			logStore2, en2 := store.NewAccessLogStore(cfg)
			if en2 != nil {
				t.Fatalf("rebuild logStore err: %v", en2)
			}
			if e := logStore2.Open(ctx); e != nil {
				t.Fatalf("reopen logStore err: %v", e)
			}
			urlStore = urlStore2
			logStore = logStore2
			usv, e5 := service.NewURLService(cfg, urlStore)
			if e5 != nil {
				t.Fatalf("rebuild urlSvc err: %v", e5)
			}
			rsv, e6 := service.NewRedirectService(urlStore, logStore)
			if e6 != nil {
				t.Fatalf("rebuild rdSvc err: %v", e6)
			}
			urlSvc = usv
			rdSvc = rsv
		}
	}

	guardCode := "PG_TRIGGER_X"
	guardHit := false
	urlStore.SetPanicGuard(func(code, rawURL string) bool {
		if code == guardCode {
			guardHit = true
			return true
		}
		return false
	})
	panicked, ep := urlSvc.Create(ctx, &model.CreateReq{
		RawURL:     "https://example.com/pg-case",
		CustomCode: guardCode,
		MaxVisits:  10,
	})
	urlStore.SetPanicGuard(nil)
	if !guardHit {
		t.Logf("FAIL PanicGuard hook not invoked (diagnostic API malfunction)")
		failCount++
	} else if ep == nil {
		t.Logf("FAIL Create(PG_TRIGGER_X) returned err=nil after guard panic -> error swallowed")
		failCount++
	} else if panicked == nil {
	}

	limitCode := "maxvisit_1"
	_, el := urlSvc.Create(ctx, &model.CreateReq{
		RawURL:     "https://example.com/limited",
		CustomCode: limitCode,
		MaxVisits:  2,
	})
	if el != nil {
		t.Logf("FAIL Create(maxvisit_1) unexpected err: %v", el)
		failCount++
	} else {
		r1, err := rdSvc.HandleRedirect(ctx, &service.RedirectRequest{
			Code:      limitCode,
			Timestamp: time.Now(),
		})
		if err != nil {
			t.Logf("FAIL 1st HandleRedirect(maxvisit_1) err: %v", err)
			failCount++
		} else if r1.Status != 302 {
			t.Logf("FAIL 1st HandleRedirect(maxvisit_1) Status=%d want 302", r1.Status)
			failCount++
		} else if r1.RawURL != "https://example.com/limited" {
			t.Logf("FAIL 1st HandleRedirect(maxvisit_1) RawURL=%q want correct target", r1.RawURL)
			failCount++
		}

		r2, err := rdSvc.HandleRedirect(ctx, &service.RedirectRequest{
			Code:      limitCode,
			Timestamp: time.Now(),
		})
		if err != nil {
			t.Logf("FAIL 2nd HandleRedirect(maxvisit_1) err: %v", err)
			failCount++
		} else if r2.Status != 410 {
			t.Logf("FAIL 2nd HandleRedirect(maxvisit_1) Status=%d want 410 (max visits exceeded)", r2.Status)
			failCount++
		} else if !r2.MaxVisited {
			t.Logf("FAIL 2nd HandleRedirect(maxvisit_1) MaxVisited=false want true")
			failCount++
		} else if r2.RawURL != "" {
			t.Logf("FAIL 2nd HandleRedirect(maxvisit_1) RawURL=%q want empty (410)", r2.RawURL)
			failCount++
		}
	}

	if failCount > 0 {
		fmt.Printf("RED（红灯，缺陷未修复）—— failed checks count = %d\n", failCount)
		t.Fatalf("RED（红灯，缺陷未修复）: %d check(s) failed, see logs above", failCount)
	} else {
		fmt.Println("GREEN（绿灯，缺陷已修复）—— all checks passed")
	}
}
