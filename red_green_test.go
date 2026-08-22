package shurl

import (
	"fmt"
	"testing"

	"shurl/internal/admin"
	"shurl/pkg/safemap"
)

func TestRedGreen(t *testing.T) {
	failed := 0

	if err := testSafemapSwapSemantics(); err != nil {
		t.Errorf("safemap.Swap semantics: %v", err)
		failed++
	}
	if err := testSafemapSwapManySemantics(); err != nil {
		t.Errorf("safemap.SwapMany semantics: %v", err)
		failed++
	}
	if err := testSafemapMustSwapSemantics(); err != nil {
		t.Errorf("safemap.MustSwap semantics: %v", err)
		failed++
	}
	if err := testFeatureStoreSingleAdd(); err != nil {
		t.Errorf("FeatureStore.SetFeature (ADD case): %v", err)
		failed++
	}
	if err := testFeatureStoreSingleReplace(); err != nil {
		t.Errorf("FeatureStore.SetFeature (REPLACE case): %v", err)
		failed++
	}
	if err := testFeatureStoreBatchAdd(); err != nil {
		t.Errorf("FeatureStore.BatchApply (ADD case): %v", err)
		failed++
	}
	if err := testFeatureStoreBatchReplace(); err != nil {
		t.Errorf("FeatureStore.BatchApply (REPLACE case): %v", err)
		failed++
	}
	if err := testFeatureStoreHistoryAudit(); err != nil {
		t.Errorf("FeatureStore history audit: %v", err)
		failed++
	}

	if failed > 0 {
		fmt.Println("========================================")
		fmt.Printf("RED（红灯，缺陷未修复）—— failed %d checks\n", failed)
		fmt.Println("========================================")
		t.FailNow()
	} else {
		fmt.Println("========================================")
		fmt.Println("GREEN（绿灯，缺陷已修复）—— all checks passed")
		fmt.Println("========================================")
	}
}

func testSafemapSwapSemantics() error {
	m := safemap.New()
	old, err := m.Swap("k_new", "v1")
	if err != nil {
		return fmt.Errorf("new key Swap should not return error, got err=%v", err)
	}
	if old != nil {
		return fmt.Errorf("new key Swap should return old=nil, got old=%#v (type %T)", old, old)
	}
	got, _ := m.Get("k_new")
	if got != "v1" {
		return fmt.Errorf("new key Swap value not written, got %v", got)
	}
	old, err = m.Swap("k_new", "v2")
	if err != nil {
		return fmt.Errorf("existing key Swap should not return error, got err=%v", err)
	}
	if old != "v1" {
		return fmt.Errorf("existing key Swap old should be 'v1', got %#v", old)
	}
	return nil
}

func testSafemapSwapManySemantics() error {
	m := safemap.New()
	m.Set("k_exists", "old_v")
	batch := map[string]any{
		"k_exists": "new_v",
		"k_fresh":  "fr_v",
	}
	res := m.SwapMany(batch)
	if len(res) != 2 {
		return fmt.Errorf("SwapMany returned %d results, want 2", len(res))
	}
	for _, r := range res {
		switch r.Key {
		case "k_exists":
			if !r.Exists {
				return fmt.Errorf("SwapMany[k_exists].Exists=false, want true")
			}
			if r.Err != nil {
				return fmt.Errorf("SwapMany[k_exists] unexpected err=%v", r.Err)
			}
			if r.Old != "old_v" {
				return fmt.Errorf("SwapMany[k_exists].Old=%#v, want 'old_v'", r.Old)
			}
			if r.New != "new_v" {
				return fmt.Errorf("SwapMany[k_exists].New=%#v, want 'new_v'", r.New)
			}
		case "k_fresh":
			if r.Exists {
				return fmt.Errorf("SwapMany[k_fresh].Exists=true, want false")
			}
			if r.Err != nil {
				return fmt.Errorf("SwapMany[k_fresh] unexpected err=%v", r.Err)
			}
			if r.Old != nil {
				return fmt.Errorf("SwapMany[k_fresh].Old=%#v (type %T), want nil", r.Old, r.Old)
			}
			if r.New != "fr_v" {
				return fmt.Errorf("SwapMany[k_fresh].New=%#v, want 'fr_v'", r.New)
			}
		}
	}
	return nil
}

func testSafemapMustSwapSemantics() error {
	m := safemap.New()
	oldStr, replaced, opErr := m.MustSwap("fresh", "val1")
	if opErr != nil {
		return fmt.Errorf("MustSwap(fresh key) err=%v, want nil", opErr)
	}
	if replaced {
		return fmt.Errorf("MustSwap(fresh key) replaced=true, want false")
	}
	if oldStr != "" {
		return fmt.Errorf("MustSwap(fresh key) oldStr=%q, want empty", oldStr)
	}
	oldStr, replaced, opErr = m.MustSwap("fresh", "val2")
	if opErr != nil {
		return fmt.Errorf("MustSwap(existing key) err=%v, want nil", opErr)
	}
	if !replaced {
		return fmt.Errorf("MustSwap(existing key) replaced=false, want true")
	}
	if oldStr != "val1" {
		return fmt.Errorf("MustSwap(existing key) oldStr=%q, want 'val1'", oldStr)
	}
	return nil
}

func testFeatureStoreSingleAdd() error {
	fs := admin.NewFeatureStore()
	ch := fs.SetFeature("feature_alpha", "on")
	if ch.Key != "feature_alpha" || ch.NewValue != "on" {
		return fmt.Errorf("SetFeature key/new mismatch: %+v", ch)
	}
	if ch.OldValue != "" {
		return fmt.Errorf("SetFeature(ADD) OldValue=%q, want empty", ch.OldValue)
	}
	if ch.Op != admin.FeatureOpAdd {
		return fmt.Errorf("SetFeature(ADD) Op=%q, want %q (message=%q)", ch.Op, admin.FeatureOpAdd, ch.Message)
	}
	if ch.Message != "" {
		return fmt.Errorf("SetFeature(ADD) unexpected message=%q", ch.Message)
	}
	v, ok := fs.Get("feature_alpha")
	if !ok || v != "on" {
		return fmt.Errorf("SetFeature value not persisted: ok=%v v=%q", ok, v)
	}
	return nil
}

func testFeatureStoreSingleReplace() error {
	fs := admin.NewFeatureStore()
	_ = fs.SetFeature("color", "blue")
	ch := fs.SetFeature("color", "green")
	if ch.Op != admin.FeatureOpReplace {
		return fmt.Errorf("SetFeature(REPLACE) Op=%q, want %q", ch.Op, admin.FeatureOpReplace)
	}
	if ch.OldValue != "blue" {
		return fmt.Errorf("SetFeature(REPLACE) OldValue=%q, want 'blue'", ch.OldValue)
	}
	if ch.NewValue != "green" {
		return fmt.Errorf("SetFeature(REPLACE) NewValue=%q, want 'green'", ch.NewValue)
	}
	return nil
}

func testFeatureStoreBatchAdd() error {
	fs := admin.NewFeatureStore()
	changes := fs.BatchApply(map[string]string{
		"flag_a": "1",
		"flag_b": "2",
	})
	if len(changes) != 2 {
		return fmt.Errorf("BatchApply(ADD 2) returned %d changes, want 2", len(changes))
	}
	for _, ch := range changes {
		if ch.OldValue != "" {
			return fmt.Errorf("BatchApply(ADD key=%s) OldValue=%q, want empty", ch.Key, ch.OldValue)
		}
		if ch.Op != admin.FeatureOpAdd {
			return fmt.Errorf("BatchApply(ADD key=%s) Op=%q, want %q (message=%q)", ch.Key, ch.Op, admin.FeatureOpAdd, ch.Message)
		}
		if ch.Message != "" {
			return fmt.Errorf("BatchApply(ADD key=%s) unexpected message=%q", ch.Key, ch.Message)
		}
	}
	snap := fs.Snapshot()
	if snap["flag_a"] != "1" || snap["flag_b"] != "2" {
		return fmt.Errorf("BatchApply values wrong: snap=%v", snap)
	}
	return nil
}

func testFeatureStoreBatchReplace() error {
	fs := admin.NewFeatureStore()
	_ = fs.SetFeature("mode", "prod")
	_ = fs.SetFeature("level", "info")
	changes := fs.BatchApply(map[string]string{
		"mode":  "staging",
		"level": "debug",
		"new":   "true",
	})
	if len(changes) != 3 {
		return fmt.Errorf("BatchApply mixed returned %d changes, want 3", len(changes))
	}
	for _, ch := range changes {
		switch ch.Key {
		case "mode":
			if ch.Op != admin.FeatureOpReplace || ch.OldValue != "prod" {
				return fmt.Errorf("BatchApply mode: op=%q old=%q want REPLACE/prod", ch.Op, ch.OldValue)
			}
		case "level":
			if ch.Op != admin.FeatureOpReplace || ch.OldValue != "info" {
				return fmt.Errorf("BatchApply level: op=%q old=%q want REPLACE/info", ch.Op, ch.OldValue)
			}
		case "new":
			if ch.Op != admin.FeatureOpAdd {
				return fmt.Errorf("BatchApply new: op=%q want ADD", ch.Op)
			}
			if ch.OldValue != "" {
				return fmt.Errorf("BatchApply new OldValue=%q want empty", ch.OldValue)
			}
			if ch.Message != "" {
				return fmt.Errorf("BatchApply new unexpected message=%q", ch.Message)
			}
		}
	}
	return nil
}

func testFeatureStoreHistoryAudit() error {
	fs := admin.NewFeatureStore()
	_ = fs.SetFeature("x", "1")
	_ = fs.SetFeature("y", "a")
	_ = fs.SetFeature("x", "2")
	ch, ok := fs.LastChangeFor("x")
	if !ok {
		return fmt.Errorf("LastChangeFor(x) missing")
	}
	if ch.Op != admin.FeatureOpReplace {
		return fmt.Errorf("LastChangeFor(x) Op=%q want REPLACE", ch.Op)
	}
	if ch.OldValue != "1" {
		return fmt.Errorf("LastChangeFor(x) OldValue=%q want '1'", ch.OldValue)
	}
	ch2, ok := fs.LastChangeFor("y")
	if !ok {
		return fmt.Errorf("LastChangeFor(y) missing")
	}
	if ch2.Op != admin.FeatureOpAdd {
		return fmt.Errorf("LastChangeFor(y) Op=%q want ADD (first write of y is add)", ch2.Op)
	}
	if ch2.OldValue != "" {
		return fmt.Errorf("LastChangeFor(y) OldValue=%q want empty", ch2.OldValue)
	}
	return nil
}
