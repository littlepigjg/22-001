package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"shurl/internal/config"
	"shurl/internal/store"
)

// newServiceWithStore 构造一个绑定给定 store 的 URLService。
func newServiceWithStore(t *testing.T, s *store.URLStore) *URLService {
	t.Helper()
	cfg := &config.Config{
		ShortCode: config.ShortCodeCfg{
			Length:     7,
			Alphabet:   "abcdefghijklmnopqrstuvwxyz0123456789",
			MaxRetries: 5,
		},
	}
	svc, err := NewURLService(cfg, s)
	if err != nil {
		t.Fatalf("NewURLService: %v", err)
	}
	return svc
}

// newReadyStore 构造一个已 Load 就绪的 URLStore，文件位于临时目录。
// fushOnWrite / syncInterval 由参数控制。
func newReadyStore(t *testing.T, flushOnWrite bool, syncInterval time.Duration) (*store.URLStore, string) {
	t.Helper()
	dir := t.TempDir()
	// 子目录确保 EnsureDir 真正命中被 chmod 的路径（而非被 dir=="." 短路）。
	path := filepath.Join(dir, "sub", "urls.json")
	cfg := &config.Config{
		Storage: config.StorageCfg{
			URLFilePath:  path,
			SyncInterval: syncInterval,
			FlushOnWrite: flushOnWrite,
		},
	}
	s, err := store.NewURLStore(cfg)
	if err != nil {
		t.Fatalf("NewURLStore: %v", err)
	}
	if err := s.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

func batchReq(raw string) *BatchCreateReq {
	return &BatchCreateReq{RawURL: raw}
}

// 正常路径：BatchCreate 返回的 Created 全部真实落盘，Exists 命中，Count 对齐。
func TestBatchCreate_HappyPathPersistsAndAligns(t *testing.T) {
	s, _ := newReadyStore(t, false, 0)
	svc := newServiceWithStore(t, s)

	reqs := []*BatchCreateReq{
		batchReq("https://example.com/a"),
		batchReq("https://example.com/b"),
		batchReq("https://example.com/c"),
	}
	res, err := svc.BatchCreate(context.Background(), reqs)
	if err != nil {
		t.Fatalf("BatchCreate: %v", err)
	}
	if len(res.Created) != 3 {
		t.Fatalf("len(Created) = %d, want 3", len(res.Created))
	}
	if len(res.Failed) != 0 {
		t.Fatalf("len(Failed) = %d, want 0; %+v", len(res.Failed), res.Failed)
	}
	// 每条 Created 的 code 都应当真实存在。
	for _, u := range res.Created {
		ok, err := s.Exists(u.Code)
		if err != nil {
			t.Fatalf("Exists(%q): %v", u.Code, err)
		}
		if !ok {
			t.Fatalf("Exists(%q) = false, want true", u.Code)
		}
	}
	if got := s.Count(); got != 3 {
		t.Fatalf("store Count = %d, want 3", got)
	}
}

// 真实 IO 失败（默认 MaxAttempts=2）：FlushOnWrite=true 且目录不可写时，
// Created 必须为空、Failed 含全部条目并带原因，不得虚报成功。
func TestBatchCreate_AllIOFailReportsNoCreated(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-based IO failure test requires non-root user")
	}
	s, dir := newReadyStore(t, true, 0) // FlushOnWrite=true，每次 Save 立即落盘
	svc := newServiceWithStore(t, s)

	// Load 完成后剥夺存放目录的写权限，使后续 Save -> flushLocked -> WriteAtomic -> OpenFile 失败。
	// 注意：URLFilePath 在 dir/sub/ 下，子目录在 Load 时由 EnsureDir(MkdirAll) 创建，
	// 权限独立于 dir，因此必须 chmod 实际的父目录 dir/sub。
	writeDir := filepath.Join(dir, "sub")
	if err := os.Chmod(writeDir, 0o500); err != nil {
		t.Fatalf("chmod 0500 %s: %v", writeDir, err)
	}
	t.Cleanup(func() { _ = os.Chmod(writeDir, 0o700) })

	reqs := []*BatchCreateReq{
		batchReq("https://example.com/x"),
		batchReq("https://example.com/y"),
		batchReq("https://example.com/z"),
	}
	res, err := svc.BatchCreate(context.Background(), reqs)
	if err != nil {
		t.Fatalf("BatchCreate returned error (expected partial-success nil): %v", err)
	}
	if len(res.Created) != 0 {
		t.Fatalf("len(Created) = %d, want 0 (must not report success when nothing persisted): %+v",
			len(res.Created), res.Created)
	}
	if len(res.Failed) != 3 {
		t.Fatalf("len(Failed) = %d, want 3", len(res.Failed))
	}
	for _, f := range res.Failed {
		if f.Reason == "" {
			t.Fatalf("failure for %q missing reason", f.Code)
		}
	}
}
