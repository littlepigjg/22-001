package shurl_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

func buildCfg(t *testing.T) (*config.Config, func()) {
	t.Helper()
	root := t.TempDir()
	cfg := config.Default()
	cfg.Storage.URLFilePath = filepath.Join(root, "urls.json")
	cfg.Storage.LogFilePath = filepath.Join(root, "access.log")
	cfg.Storage.SyncInterval = 0
	cfg.Janitor.Enabled = false
	return cfg, func() { _ = os.RemoveAll(root) }
}

func setupStores(t *testing.T, cfg *config.Config) (*store.URLStore, *store.AccessLogStore, func()) {
	t.Helper()
	ctx := context.Background()
	us, err := store.NewURLStore(cfg)
	if err != nil {
		t.Fatalf("NewURLStore: %v", err)
	}
	if err := us.Load(ctx); err != nil {
		t.Fatalf("URLStore.Load: %v", err)
	}
	ls, err := store.NewAccessLogStore(cfg)
	if err != nil {
		t.Fatalf("NewAccessLogStore: %v", err)
	}
	if err := ls.Open(ctx); err != nil {
		t.Fatalf("AccessLogStore.Open: %v", err)
	}
	cleanup := func() {
		_ = us.Close()
		_ = ls.Close()
	}
	return us, ls, cleanup
}

func TestRedGreen(t *testing.T) {
	if os.Getenv("REDGREEN_RUNNER") == "1" {
		runRaceWorkload(t)
		return
	}

	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("find go: %v", err)
	}
	args := []string{
		"test",
		"-race",
		"-count=1",
		"-timeout=180s",
		"-run=^TestRedGreen$",
		".",
	}
	cmd := exec.Command(goBin, args...)
	cmd.Dir = moduleDir(t)
	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf
	cmd.Env = append(os.Environ(), "REDGREEN_RUNNER=1")
	runErr := cmd.Run()
	output := outBuf.String()

	if hasRace(output) || (runErr != nil && containsAny(output, "FAIL", "race detected", "exit status")) {
		fmt.Println("RED（红灯，缺陷未修复）")
		t.Fatalf("RED: verify failed. detector output (tail):\n%s", tail(output, 3000))
	}
	if runErr != nil {
		fmt.Println("RED（红灯，缺陷未修复）")
		t.Fatalf("RED: verify command exited non-zero. stderr+stdout (tail):\n%s", tail(output, 3000))
	}
	fmt.Println("GREEN（绿灯，缺陷已修复）")
}

func runRaceWorkload(t *testing.T) {
	cfg, cleanCfg := buildCfg(t)
	defer cleanCfg()
	cfg.Storage.SyncInterval = 10 * time.Millisecond
	us, ls, cleanStores := setupStores(t, cfg)
	defer cleanStores()

	rdSvc, err := service.NewRedirectService(us, ls)
	if err != nil {
		t.Fatalf("NewRedirectService: %v", err)
	}
	urlSvc, err := service.NewURLService(cfg, us)
	if err != nil {
		t.Fatalf("NewURLService: %v", err)
	}
	defer func() { _ = rdSvc.Shutdown(context.Background()) }()

	codes := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		u, err := urlSvc.Create(context.Background(), &model.CreateReq{
			RawURL:    fmt.Sprintf("https://example.com/path/%d", i),
			MaxVisits: 0,
		})
		if err != nil {
			t.Fatalf("urlSvc.Create: %v", err)
		}
		codes = append(codes, u.Code)
	}

	const loops = 3
	panicHit := &atomic.Bool{}
	for round := 0; round < loops; round++ {
		var wg sync.WaitGroup
		stop := make(chan struct{})
		done := make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(done)
			tick := time.NewTicker(2 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
					func() {
						defer func() {
							if r := recover(); r != nil {
								panicHit.Store(true)
							}
						}()
						_ = us.ForEach(func(u *model.ShortURL) bool {
							_ = u.Visits
							_ = u.Disabled
							_ = u.Remark
							_ = u.RawURL
							return true
						})
						_, _, _, _, _ = us.Stats()
						_ = us.Flush()
					}()
				}
			}
		}()

		workers := runtime.GOMAXPROCS(0) * 4
		if workers < 16 {
			workers = 16
		}
		const roundsPerWorker = 100
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(wid int) {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						panicHit.Store(true)
					}
				}()
				rid := wid % len(codes)
				ctx := context.Background()
				for k := 0; k < roundsPerWorker; k++ {
					idx := (k + rid) % len(codes)
					code := codes[idx]
					switch k % 5 {
					case 0:
						u, err := urlSvc.Get(ctx, code)
						if err == nil && u != nil {
							_ = u.Visits
							_ = u.Disabled
							_ = u.RawURL
						}
					case 1:
						_ = urlSvc.UpdateRemark(ctx, code, fmt.Sprintf("r%dk%dw%d", round, k, wid))
					case 2, 4:
						_, _ = rdSvc.HandleRedirect(ctx, &service.RedirectRequest{
							Code:       code,
							RemoteAddr: "127.0.0.1:1",
							Headers:    map[string][]string{"User-Agent": {"race-test"}},
							Timestamp:  time.Now(),
						})
					case 3:
						_ = urlSvc.Disable(ctx, code)
					}
				}
			}(w)
		}
		time.Sleep(10 * time.Millisecond)
		close(stop)
		wg.Wait()
		<-done
	}
	if panicHit.Load() {
		t.Fatalf("panic observed during concurrent workload")
	}
}

func hasRace(s string) bool {
	low := strings.ToLower(s)
	return strings.Contains(low, "data race") ||
		strings.Contains(low, "warning: data race") ||
		strings.Contains(low, "race detected")
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

func moduleDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	dir := wd
	for i := 0; i < 10; i++ {
		if st, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && !st.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return wd
}
