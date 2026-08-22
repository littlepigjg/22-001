package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

const childEnvKey = "SHURL_RG_CHILD_MODE"
const childEnvVal = "1"

func setup(t *testing.T) (*service.URLService, *store.URLStore, func()) {
	t.Helper()
	dir := t.TempDir()
	urlPath := filepath.Join(dir, "urls.json")
	logPath := filepath.Join(dir, "access.log")
	cfg := config.Default()
	cfg.Storage.URLFilePath = urlPath
	cfg.Storage.LogFilePath = logPath
	cfg.Storage.SyncInterval = 0
	cfg.Storage.FlushOnWrite = false

	us, err := store.NewURLStore(cfg)
	if err != nil {
		t.Fatalf("NewURLStore: %v", err)
	}
	ctx := context.Background()
	if err := us.Load(ctx); err != nil {
		t.Fatalf("Load URLStore: %v", err)
	}
	svc, err := service.NewURLService(cfg, us)
	if err != nil {
		t.Fatalf("NewURLService: %v", err)
	}
	cleanup := func() {
		_ = us.Close()
		_ = os.RemoveAll(dir)
	}
	return svc, us, cleanup
}

func seedCodes(t *testing.T, svc *service.URLService, n int) []string {
	t.Helper()
	codes := make([]string, 0, n)
	ctx := context.Background()
	for i := 0; i < n; i++ {
		code := fmt.Sprintf("abc%04d", i+1)
		_, err := svc.Create(ctx, &model.CreateReq{
			RawURL:     "https://example.com/page/" + code,
			CustomCode: code,
		})
		if err != nil {
			t.Fatalf("seed Create(%s): %v", code, err)
		}
		codes = append(codes, code)
	}
	return codes
}

func runConcurrentWorkload(svc *service.URLService, us *store.URLStore, codes []string) int64 {
	const workoutN = 50
	const disableGap = 5
	var wg sync.WaitGroup

	for round := 0; round < 3; round++ {
		for _, c := range codes {
			wg.Add(1)
			go func(code string) {
				defer wg.Done()
				for i := 0; i < workoutN; i++ {
					_, _ = us.IncrementVisits(code)
					if i%disableGap == 0 {
						u, _ := us.Get(code)
						if u != nil {
							u.Remark = fmt.Sprintf("r%ds%di%d", round, i%7, i)
							u.MaxVisits = int64(i % 1000)
							u.Disabled = (i%29 == 0)
							_ = us.Save(u, true)
						}
					}
				}
			}(c)
		}

		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			_ = svc.BatchDisable(context.Background(), codes)
		}(round)

		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			rm := make(map[string]string, len(codes))
			for j, c := range codes {
				rm[c] = fmt.Sprintf("bm%02d-%02d-%04d", r, j, r*1000+j)
			}
			_ = svc.BatchUpdateRemark(context.Background(), "", rm)
		}(round)
	}
	wg.Wait()

	for _, c := range codes {
		wg.Add(1)
		go func(code string) {
			defer wg.Done()
			_ = svc.ConcurrentWorkout(context.Background(), service.ConcurrentWorkoutCfg{
				Code:       code,
				VisitN:     30,
				DisableGap: 4,
			})
		}(c)
	}
	wg.Wait()

	var bad int64
	for _, c := range codes {
		u, err := us.Get(c)
		if err != nil || u == nil {
			atomic.AddInt64(&bad, 1)
			continue
		}
		if u.Visits < 0 {
			atomic.AddInt64(&bad, 1)
		}
		if u.Code == "" {
			atomic.AddInt64(&bad, 1)
		}
	}
	return bad
}

func runChildMode() int {
	codesN := 12
	cfg := config.Default()
	dir, err := os.MkdirTemp("", "shurl-rg-child-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkdir err:", err)
		return 2
	}
	defer os.RemoveAll(dir)
	cfg.Storage.URLFilePath = filepath.Join(dir, "urls.json")
	cfg.Storage.LogFilePath = filepath.Join(dir, "access.log")
	cfg.Storage.SyncInterval = 0
	cfg.Storage.FlushOnWrite = false
	us, err := store.NewURLStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "NewURLStore err:", err)
		return 2
	}
	ctx := context.Background()
	if err := us.Load(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "Load err:", err)
		return 2
	}
	svc, err := service.NewURLService(cfg, us)
	if err != nil {
		fmt.Fprintln(os.Stderr, "NewURLService err:", err)
		return 2
	}
	defer us.Close()

	codes := make([]string, 0, codesN)
	for i := 0; i < codesN; i++ {
		code := fmt.Sprintf("abc%04d", i+1)
		_, err := svc.Create(ctx, &model.CreateReq{
			RawURL:     "https://example.com/page/" + code,
			CustomCode: code,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "seed err:", err)
			return 2
		}
		codes = append(codes, code)
	}
	bad := runConcurrentWorkload(svc, us, codes)
	if bad > 0 {
		return 1
	}
	return 0
}

type fakeTB struct {
	failed atomic.Bool
}

func (f *fakeTB) Helper()                           {}
func (f *fakeTB) Cleanup(func())                    {}
func (f *fakeTB) TempDir() string                   { d, _ := os.MkdirTemp("", ""); return d }
func (f *fakeTB) ArtifactDir() string               { return f.TempDir() }
func (f *fakeTB) Fatalf(s string, a ...any)         { f.failed.Store(true); panic(fmt.Sprintf(s, a...)) }
func (f *fakeTB) Errorf(s string, a ...any)         { f.failed.Store(true); fmt.Fprintf(os.Stderr, s+"\n", a...) }

func TestRedGreen(t *testing.T) {
	if os.Getenv(childEnvKey) == childEnvVal {
		os.Exit(runChildMode())
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	totalRounds := 3
	badRounds := 0
	childPassAsRed := 0
	childCrashAsRed := 0
	var allOut strings.Builder
	for i := 0; i < totalRounds; i++ {
		cmd := exec.Command(exe, "-test.v", "-test.run", "^TestRedGreen$", "-test.timeout", "15s")
		env := os.Environ()
		env = append(env, childEnvKey+"="+childEnvVal)
		if _, ok := os.LookupEnv("GORACE"); !ok {
			env = append(env, "GORACE=halt_on_error=0")
		}
		cmd.Env = env
		var buf strings.Builder
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		done := make(chan error, 1)
		go func() { done <- cmd.Run() }()
		var runErr error
		select {
		case runErr = <-done:
		case <-time.After(18 * time.Second):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			badRounds++
			childCrashAsRed++
			allOut.WriteString("ROUND TIMEOUT\n")
			continue
		}
		allOut.WriteString("--- ROUND " + fmt.Sprintf("%d", i) + " OUTPUT ---\n")
		allOut.WriteString(buf.String())
		allOut.WriteString("\n")
		exitCode := 0
		if runErr != nil {
			if ee, ok := runErr.(*exec.ExitError); ok {
				exitCode = ee.ExitCode()
			} else {
				exitCode = -1
			}
		}
		outLower := strings.ToLower(buf.String())
		hasRace := strings.Contains(outLower, "data race") ||
			strings.Contains(outLower, "race detected") ||
			strings.Contains(outLower, "concurrent map")
		if exitCode != 0 || hasRace {
			badRounds++
			if exitCode != 0 {
				childCrashAsRed++
			} else {
				childPassAsRed++
			}
		}
	}
	t.Logf("child rounds summary: bad=%d/%d passAsRed=%d crashAsRed=%d\n%s",
		badRounds, totalRounds, childPassAsRed, childCrashAsRed, allOut.String())
	if badRounds > 0 {
		t.Errorf("concurrent defect detected: %d/%d rounds failed (race/panic/integrity)", badRounds, totalRounds)
		fmt.Println("RED（红灯，缺陷未修复）")
		return
	}
	fmt.Println("GREEN（绿灯，缺陷已修复）")
}
