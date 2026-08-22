package shurl_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/service"
	"shurl/internal/store"
	"shurl/pkg/shortcode"
)

func tempPath(t *testing.T, suffix string) string {
	t.Helper()
	f, err := os.CreateTemp("", "shurl-redgreen-*"+suffix)
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	path := f.Name()
	f.Close()
	os.Remove(path)
	return path
}

func buildService(t *testing.T) *service.URLService {
	t.Helper()
	cfg := config.Default()
	cfg.Storage.URLFilePath = tempPath(t, ".json")
	cfg.Storage.FlushOnWrite = true
	cfg.Storage.SyncInterval = 0
	cfg.ShortCode.Length = 7
	cfg.ShortCode.MaxRetries = 8
	us, err := store.NewURLStore(cfg)
	if err != nil {
		t.Fatalf("NewURLStore: %v", err)
	}
	if err := us.Load(context.Background()); err != nil {
		t.Fatalf("URLStore.Load: %v", err)
	}
	t.Cleanup(func() { _ = us.Close(); os.Remove(cfg.Storage.URLFilePath) })
	svc, err := service.NewURLService(cfg, us)
	if err != nil {
		t.Fatalf("NewURLService: %v", err)
	}
	return svc
}

func TestShortcodeGenerateMany_UniqueCodes(t *testing.T) {
	gen, err := shortcode.New("", 8)
	if err != nil {
		t.Fatalf("shortcode.New: %v", err)
	}
	n := 12
	codes, err := gen.GenerateMany(n)
	if err != nil {
		t.Fatalf("GenerateMany error: %v", err)
	}
	if len(codes) != n {
		t.Fatalf("expected %d codes, got %d", n, len(codes))
	}
	seen := make(map[string]int, n)
	for i, c := range codes {
		if len(c) != 8 {
			t.Errorf("codes[%d] length=%d want 8", i, len(c))
		}
		if prev, ok := seen[c]; ok {
			t.Errorf("codes[%d]=%q equals codes[%d]=%q (GenerateMany 必须互不相同)", i, c, prev, codes[prev])
		}
		seen[c] = i
	}
}

func TestShortcodeGenerateMany_NoMutationAfterReturn(t *testing.T) {
	gen, err := shortcode.New("abcdefghijklmnopqrstuvwxyz", 6)
	if err != nil {
		t.Fatalf("shortcode.New: %v", err)
	}
	first, err := gen.GenerateMany(10)
	if err != nil {
		t.Fatalf("GenerateMany first: %v", err)
	}
	snapshot := make([]string, len(first))
	copy(snapshot, first)
	_, err = gen.GenerateMany(10)
	if err != nil {
		t.Fatalf("GenerateMany second: %v", err)
	}
	for i := range first {
		if first[i] != snapshot[i] {
			t.Errorf("第一次 GenerateMany 返回后，结果被后续调用修改：first[%d] 从 %q 变成 %q",
				i, snapshot[i], first[i])
		}
	}
}

func TestBatchCreate_UniqueShortcodes(t *testing.T) {
	svc := buildService(t)
	n := 10
	reqs := make([]*model.CreateReq, 0, n)
	for i := 0; i < n; i++ {
		reqs = append(reqs, &model.CreateReq{
			RawURL: fmt.Sprintf("https://example.com/page/%d?q=%d", i, i*i),
		})
	}
	res, err := svc.CreateMany(context.Background(), reqs)
	if err != nil {
		t.Fatalf("CreateMany error: %v", err)
	}
	if len(res) != n {
		t.Fatalf("CreateMany result len=%d want %d", len(res), n)
	}
	codes := make(map[string]int, n)
	rawToCode := make(map[string]string, n)
	for i, r := range res {
		if r.Err != nil {
			t.Errorf("item %d error: %v", i, r.Err)
			continue
		}
		if r.URL == nil {
			t.Errorf("item %d got nil URL without error", i)
			continue
		}
		if prev, ok := codes[r.URL.Code]; ok {
			t.Errorf("批量创建短码重复: item[%d].code=%q 与 item[%d].code=%q 完全相同 (不同 RawURL 必须得到不同短码)",
				i, r.URL.Code, prev, r.URL.Code)
		}
		codes[r.URL.Code] = i
		if existing, ok := rawToCode[r.URL.RawURL]; ok && existing != r.URL.Code {
			t.Errorf("相同 raw_url 返回了不同短码? %q -> %q vs %q", r.URL.RawURL, existing, r.URL.Code)
		}
		rawToCode[r.URL.RawURL] = r.URL.Code
	}
}

func TestBatchCreate_RoundTripGet(t *testing.T) {
	svc := buildService(t)
	n := 8
	reqs := make([]*model.CreateReq, 0, n)
	for i := 0; i < n; i++ {
		reqs = append(reqs, &model.CreateReq{
			RawURL: fmt.Sprintf("https://example.com/items/%04d", i+1),
		})
	}
	res, err := svc.CreateMany(context.Background(), reqs)
	if err != nil {
		t.Fatalf("CreateMany error: %v", err)
	}
	for i, r := range res {
		if r.Err != nil {
			t.Fatalf("item %d error: %v", i, r.Err)
		}
		got, gErr := svc.Get(context.Background(), r.URL.Code)
		if gErr != nil {
			t.Errorf("round-trip Get code=%q from item[%d] error: %v", r.URL.Code, i, gErr)
			continue
		}
		if got.RawURL != reqs[i].RawURL {
			t.Errorf("round-trip 不匹配: item[%d] code=%q -> RawURL=%q, 期望 RawURL=%q",
				i, r.URL.Code, got.RawURL, reqs[i].RawURL)
		}
	}
}

func allPassed(t *testing.T, failed *bool) func() {
	t.Helper()
	*failed = false
	markFailed := func() { *failed = true }
	origHelper := t.Failed()
	_ = origHelper
	return func() {
		if t.Failed() {
			markFailed()
		}
	}
}

func TestRedGreen(t *testing.T) {
	var failed bool
	defer func() {
		if failed || t.Failed() {
			fmt.Println("RED（红灯，缺陷未修复）")
			t.Fail()
		} else {
			fmt.Println("GREEN（绿灯，缺陷已修复）")
		}
	}()

	t.Run("GenerateMany所有结果互不相同", func(t *testing.T) {
		gen, err := shortcode.New("", 7)
		if err != nil {
			t.Fatalf("shortcode.New: %v", err)
		}
		n := 10
		codes, err := gen.GenerateMany(n)
		if err != nil {
			failed = true
			t.Fatalf("GenerateMany error: %v", err)
		}
		seen := make(map[string]bool, n)
		for i, c := range codes {
			if seen[c] {
				failed = true
				t.Errorf("RED case: GenerateMany(%d) 第 %d 个结果 %q 重复，所有条目内容完全一致或互相重复", n, i, c)
			}
			seen[c] = true
		}
	})

	t.Run("批量创建返回短码互不相同且round-trip匹配", func(t *testing.T) {
		svc := buildService(t)
		n := 10
		reqs := make([]*model.CreateReq, 0, n)
		for i := 0; i < n; i++ {
			reqs = append(reqs, &model.CreateReq{
				RawURL: fmt.Sprintf("https://example.com/%d", i),
			})
		}
		res, err := svc.CreateMany(context.Background(), reqs)
		if err != nil {
			failed = true
			t.Fatalf("CreateMany error: %v", err)
		}
		directCodes := make([]string, 0, n)
		for i, r := range res {
			if r.Err == nil && r.URL != nil {
				directCodes = append(directCodes, r.URL.Code)
			} else {
				t.Fatalf("item %d 不应出错: err=%v url=%v", i, r.Err, r.URL)
			}
		}
		allSame := true
		for i := 1; i < len(directCodes); i++ {
			if directCodes[i] != directCodes[0] {
				allSame = false
				break
			}
		}
		if allSame && len(directCodes) > 1 {
			failed = true
			t.Errorf("RED case: 批量 CreateMany 返回的 %d 条短码全部相同（=%q），共享底层 byte buffer 被最后一次写入覆盖",
				len(directCodes), directCodes[0])
		}
		seenDirect := make(map[string]int, n)
		for i, c := range directCodes {
			if prev, ok := seenDirect[c]; ok {
				failed = true
				t.Errorf("RED case: CreateMany 返回结果的短码共享底层数据，directCodes[%d]=%q 和 directCodes[%d]=%q 完全相同 (返回 slice 被最后一次写入覆盖)",
					i, c, prev, directCodes[prev])
			}
			seenDirect[c] = i
		}
		codes := make(map[string]int, n)
		for i, r := range res {
			if prev, ok := codes[r.URL.Code]; ok {
				failed = true
				t.Errorf("RED case: CreateMany 返回短码重复，item[%d].code=%q 和 item[%d].code=%q 完全相同，后续保存会覆盖前一条",
					i, r.URL.Code, prev, r.URL.Code)
			}
			codes[r.URL.Code] = i
			got, gErr := svc.Get(context.Background(), r.URL.Code)
			if gErr != nil {
				failed = true
				t.Errorf("RED case: Get(code=%q) error: %v", r.URL.Code, gErr)
				continue
			}
			if got.RawURL != reqs[i].RawURL {
				failed = true
				t.Errorf("RED case: round-trip 错配，短码 %q 对应的 RawURL=%q 而不是期望的 %q",
					r.URL.Code, got.RawURL, reqs[i].RawURL)
			}
		}
	})
}
