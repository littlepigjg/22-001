package shurl_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"shurl/internal/config"
	"shurl/internal/service"
	"shurl/internal/store"
)

func buildTempConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Storage.URLFilePath = filepath.Join(dir, "urls.json")
	cfg.Storage.LogFilePath = filepath.Join(dir, "access.log")
	cfg.Storage.SyncInterval = 0
	cfg.Storage.FlushOnWrite = false
	cfg.ShortCode.Alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	cfg.ShortCode.Length = 6
	cfg.ShortCode.MaxRetries = 10
	return cfg
}

func buildReqs(n int) []*service.BatchCreateReq {
	reqs := make([]*service.BatchCreateReq, 0, n)
	for i := 0; i < n; i++ {
		reqs = append(reqs, &service.BatchCreateReq{
			RawURL:     fmt.Sprintf("https://example-%d.com/page", i),
			CustomCode: fmt.Sprintf("tcode%02d", i),
		})
	}
	return reqs
}

func runBatchCreate(t *testing.T, cfg *config.Config, reqs []*service.BatchCreateReq, maxAttempts int) (*service.BatchCreateResult, *store.URLStore, error) {
	t.Helper()
	us, err := store.NewURLStore(cfg)
	if err != nil {
		return nil, nil, err
	}
	if err := us.Load(context.Background()); err != nil {
		return nil, nil, err
	}
	defer func() {
		_ = os.RemoveAll(cfg.Storage.URLFilePath)
	}()
	svc, err := service.NewURLService(cfg, us)
	if err != nil {
		return nil, nil, err
	}
	res, err := svc.BatchCreateWithRetry(context.Background(), reqs, maxAttempts)
	return res, us, err
}

func TestRedGreen(t *testing.T) {
	reqs := buildReqs(3)

	cfgEven := buildTempConfig(t)
	evenRes, evenStore, evenErr := runBatchCreate(t, cfgEven, reqs, 2)
	if evenErr != nil {
		t.Fatalf("unexpected fatal error on even-attempts run: %v", evenErr)
	}
	evenCreated := len(evenRes.Created)
	evenActual := evenStore.Count()
	evenFailed := len(evenRes.Failed)
	evenInStore := 0
	for _, r := range reqs {
		ok, _ := evenStore.Exists(r.CustomCode)
		if ok {
			evenInStore++
		}
	}
	t.Logf("[even MaxAttempts=2] reported created=%d actual_count=%d reported_failed=%d codes_in_store=%d",
		evenCreated, evenActual, evenFailed, evenInStore)

	cfgOdd := buildTempConfig(t)
	oddReqs := buildReqs(3)
	for i := range oddReqs {
		oddReqs[i].CustomCode = fmt.Sprintf("ocode%02d", i)
	}
	oddRes, oddStore, oddErr := runBatchCreate(t, cfgOdd, oddReqs, 3)
	if oddErr != nil {
		t.Fatalf("unexpected fatal error on odd-attempts run: %v", oddErr)
	}
	oddCreated := len(oddRes.Created)
	oddActual := oddStore.Count()
	oddFailed := len(oddRes.Failed)
	oddInStore := 0
	for _, r := range oddReqs {
		ok, _ := oddStore.Exists(r.CustomCode)
		if ok {
			oddInStore++
		}
	}
	t.Logf("[odd  MaxAttempts=3] reported created=%d actual_count=%d reported_failed=%d codes_in_store=%d",
		oddCreated, oddActual, oddFailed, oddInStore)

	_ = evenStore.Close()
	_ = oddStore.Close()

	mismatchEven := evenCreated > 0 && (evenActual < evenCreated || evenInStore < evenCreated)
	mismatchEvenNilErr := evenCreated == len(reqs) && evenActual == 0 && evenFailed == 0
	oddMatches := oddCreated == oddActual && oddActual == oddInStore

	defectPresent := mismatchEvenNilErr || mismatchEven

	if defectPresent {
		fmt.Println("RED（红灯，缺陷未修复）")
		t.Errorf("RED: even MaxAttempts=2 时批量创建报告成功 %d 条，但实际写入存储为 %d 条（Exists 命中 %d 条），reported_failed=%d。调用方误判成功，数据未写入却返回 OK。",
			evenCreated, evenActual, evenInStore, evenFailed)
		return
	}

	if !oddMatches {
		fmt.Println("RED（红灯，缺陷未修复）")
		t.Errorf("RED: odd MaxAttempts=3 对照组也不一致：报告成功 %d，实际=%d，Exists命中=%d", oddCreated, oddActual, oddInStore)
		return
	}

	fmt.Println("GREEN（绿灯，缺陷已修复）")
	t.Logf("GREEN: even MaxAttempts=2 报告成功=%d 实际写入=%d（Exists命中=%d） reported_failed=%d；odd MaxAttempts=3 报告成功=%d 实际写入=%d（Exists命中=%d），均一致。",
		evenCreated, evenActual, evenInStore, evenFailed, oddCreated, oddActual, oddInStore)
}
