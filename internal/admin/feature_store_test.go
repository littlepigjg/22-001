package admin

import (
	"strings"
	"testing"
)

func TestSetFeatureFirstInsertIsAddNoMessage(t *testing.T) {
	fs := NewFeatureStore()
	ch := fs.SetFeature("newkey", "v1")
	if ch.Op != FeatureOpAdd {
		t.Fatalf("first insert Op = %s; want ADD", ch.Op)
	}
	if ch.OldValue != "" {
		t.Errorf("first insert OldValue = %q; want empty", ch.OldValue)
	}
	if ch.NewValue != "v1" {
		t.Errorf("first insert NewValue = %q; want v1", ch.NewValue)
	}
	if ch.Message != "" {
		t.Errorf("first insert Message = %q; want empty (no 'key not found' noise)", ch.Message)
	}
	if strings.Contains(ch.Message, "key not found") || strings.Contains(ch.Message, "treated as overwrite") {
		t.Errorf("first insert leaked safemap error into message: %q", ch.Message)
	}
}

func TestSetFeatureReplaceIsReplaceWithOldValue(t *testing.T) {
	fs := NewFeatureStore()
	fs.SetFeature("k", "v1")
	ch := fs.SetFeature("k", "v2")
	if ch.Op != FeatureOpReplace {
		t.Fatalf("replace Op = %s; want REPLACE", ch.Op)
	}
	if ch.OldValue != "v1" {
		t.Errorf("replace OldValue = %q; want v1", ch.OldValue)
	}
	if ch.NewValue != "v2" {
		t.Errorf("replace NewValue = %q; want v2", ch.NewValue)
	}
	if ch.Message != "" {
		t.Errorf("replace Message = %q; want empty", ch.Message)
	}
}

func TestSetFeatureReplaceEmptyValueIsReplace(t *testing.T) {
	// Storing an empty string and then replacing it must be REPLACE, not ADD.
	// Previously this was indistinguishable from a first insert.
	fs := NewFeatureStore()
	fs.SetFeature("k", "")
	ch := fs.SetFeature("k", "v")
	if ch.Op != FeatureOpReplace {
		t.Fatalf("replace of empty value Op = %s; want REPLACE", ch.Op)
	}
	if ch.OldValue != "" {
		t.Errorf("replace of empty value OldValue = %q; want empty", ch.OldValue)
	}
	if ch.Message != "" {
		t.Errorf("replace of empty value Message = %q; want empty", ch.Message)
	}
}

func TestSetFeatureFirstInsertThenReplaceSequence(t *testing.T) {
	fs := NewFeatureStore()
	add := fs.SetFeature("k", "a")
	rep := fs.SetFeature("k", "b")
	if add.Op != FeatureOpAdd || rep.Op != FeatureOpReplace {
		t.Fatalf("sequence ops = %s then %s; want ADD then REPLACE", add.Op, rep.Op)
	}
	if rep.OldValue != "a" {
		t.Errorf("sequence replace OldValue = %q; want a", rep.OldValue)
	}
}

func TestBatchApplyFirstInsertsAreAdd(t *testing.T) {
	fs := NewFeatureStore()
	changes := fs.BatchApply(map[string]string{"a": "1", "b": "2", "c": "3"})
	if len(changes) != 3 {
		t.Fatalf("got %d changes; want 3", len(changes))
	}
	for _, ch := range changes {
		if ch.Op != FeatureOpAdd {
			t.Errorf("key %s first insert Op = %s; want ADD", ch.Key, ch.Op)
		}
		if ch.OldValue != "" {
			t.Errorf("key %s first insert OldValue = %q; want empty", ch.Key, ch.OldValue)
		}
		if ch.Message != "" {
			t.Errorf("key %s first insert Message = %q; want empty", ch.Key, ch.Message)
		}
	}
}

func TestBatchApplyMixedInsertAndReplace(t *testing.T) {
	fs := NewFeatureStore()
	// Seed "b" as existing.
	fs.SetFeature("b", "oldB")
	changes := fs.BatchApply(map[string]string{"a": "1", "b": "2", "c": "3"})
	if len(changes) != 3 {
		t.Fatalf("got %d changes; want 3", len(changes))
	}
	// SwapMany sorts keys, so the order is deterministic: a, b, c.
	want := map[string]struct {
		op     FeatureOpKind
		oldVal string
		newVal string
	}{
		"a": {FeatureOpAdd, "", "1"},
		"b": {FeatureOpReplace, "oldB", "2"},
		"c": {FeatureOpAdd, "", "3"},
	}
	for _, ch := range changes {
		w, ok := want[ch.Key]
		if !ok {
			t.Fatalf("unexpected key %q", ch.Key)
		}
		if ch.Op != w.op {
			t.Errorf("key %s Op = %s; want %s", ch.Key, ch.Op, w.op)
		}
		if ch.OldValue != w.oldVal {
			t.Errorf("key %s OldValue = %q; want %q", ch.Key, ch.OldValue, w.oldVal)
		}
		if ch.NewValue != w.newVal {
			t.Errorf("key %s NewValue = %q; want %q", ch.Key, ch.NewValue, w.newVal)
		}
		if ch.Message != "" {
			t.Errorf("key %s Message = %q; want empty", ch.Key, ch.Message)
		}
	}
}

func TestBatchApplyEmptyInputReturnsNil(t *testing.T) {
	fs := NewFeatureStore()
	if changes := fs.BatchApply(nil); changes != nil {
		t.Fatalf("nil input: got %v; want nil", changes)
	}
	if changes := fs.BatchApply(map[string]string{}); changes != nil {
		t.Fatalf("empty input: got %v; want nil", changes)
	}
}

func TestLastChangeForFirstInsertIsAdd(t *testing.T) {
	fs := NewFeatureStore()
	fs.SetFeature("k", "v1")
	ch, ok := fs.LastChangeFor("k")
	if !ok {
		t.Fatal("LastChangeFor returned ok=false after first insert")
	}
	if ch.Op != FeatureOpAdd {
		t.Errorf("LastChangeFor first insert Op = %s; want ADD", ch.Op)
	}
	if ch.OldValue != "" {
		t.Errorf("LastChangeFor first insert OldValue = %q; want empty", ch.OldValue)
	}
	// Overwrite and re-check.
	fs.SetFeature("k", "v2")
	ch, ok = fs.LastChangeFor("k")
	if !ok {
		t.Fatal("LastChangeFor returned ok=false after replace")
	}
	if ch.Op != FeatureOpReplace {
		t.Errorf("LastChangeFor replace Op = %s; want REPLACE", ch.Op)
	}
	if ch.OldValue != "v1" {
		t.Errorf("LastChangeFor replace OldValue = %q; want v1", ch.OldValue)
	}
}

func TestLastChangeForMissingKey(t *testing.T) {
	fs := NewFeatureStore()
	_, ok := fs.LastChangeFor("nope")
	if ok {
		t.Fatal("LastChangeFor returned ok=true for a key that was never set")
	}
}

func TestGetAfterSet(t *testing.T) {
	fs := NewFeatureStore()
	fs.SetFeature("k", "v1")
	v, ok := fs.Get("k")
	if !ok || v != "v1" {
		t.Fatalf("Get after SetFeature: got %q (ok=%v); want v1", v, ok)
	}
	if _, ok := fs.Get("missing"); ok {
		t.Error("Get on missing key returned ok=true")
	}
}
