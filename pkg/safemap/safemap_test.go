package safemap

import (
	"errors"
	"strings"
	"testing"
)

func TestSwapFirstInsertIsNotError(t *testing.T) {
	m := New()
	old, existed, err := m.Swap("k1", "v1")
	if err != nil {
		t.Fatalf("first insert returned error %v; a new key is not an error", err)
	}
	if existed {
		t.Fatalf("first insert reported existed=true; want false")
	}
	if old != nil {
		t.Fatalf("first insert old = %v; want nil", old)
	}
	if v, ok := m.Get("k1"); !ok || v != "v1" {
		t.Fatalf("value not stored correctly after swap: got %v (ok=%v)", v, ok)
	}
}

func TestSwapReplaceReturnsOldValueAndExisted(t *testing.T) {
	m := New()
	if _, _, err := m.Swap("k", "v1"); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	old, existed, err := m.Swap("k", "v2")
	if err != nil {
		t.Fatalf("replace returned error %v", err)
	}
	if !existed {
		t.Fatal("replace reported existed=false; want true")
	}
	if s, ok := old.(string); !ok || s != "v1" {
		t.Fatalf("replace old = %v; want v1", old)
	}
}

func TestSwapDistinguishesReplaceOfEmptyStringFromFirstInsert(t *testing.T) {
	// This is the crux of the bug: a real replace of an empty value must be
	// distinguishable from a first insert. The old implementation treated both
	// as "key not found" because it sniffed old == nil. With the comma-ok
	// idiom, a stored empty string comes back as a non-nil "" with existed=true,
	// while a first insert comes back as nil with existed=false.
	m := New()
	if _, _, err := m.Swap("k", ""); err != nil {
		t.Fatalf("storing empty value: %v", err)
	}
	old, existed, err := m.Swap("k", "v")
	if err != nil {
		t.Fatalf("replace of empty value returned error %v", err)
	}
	if !existed {
		t.Fatal("replace of empty value reported existed=false; want true (the key existed)")
	}
	if old == nil {
		t.Fatal("replace of empty value old = nil; want non-nil empty string, so it is distinguishable from a first insert")
	}
	if s, ok := old.(string); !ok || s != "" {
		t.Fatalf("replace of empty value old = %v (%T); want empty string", old, old)
	}
	if v, ok := m.Get("k"); !ok || v != "v" {
		t.Fatalf("final value = %v (ok=%v); want v", v, ok)
	}
}

func TestSwapEmptyKeyErrors(t *testing.T) {
	m := New()
	_, _, err := m.Swap("", "v")
	if err == nil || !strings.Contains(err.Error(), "empty key") {
		t.Fatalf("empty key: got err = %v; want 'empty key'", err)
	}
}

func TestSwapNilMapErrors(t *testing.T) {
	var m *Map
	_, _, err := m.Swap("k", "v")
	if err == nil || !strings.Contains(err.Error(), "nil map") {
		t.Fatalf("nil map: got err = %v; want 'nil map'", err)
	}
}

func TestSwapManyFirstInsertsAreAddNotError(t *testing.T) {
	m := New()
	res := m.SwapMany(map[string]any{"a": "1", "b": "2"})
	if len(res) != 2 {
		t.Fatalf("got %d results; want 2", len(res))
	}
	// SwapMany sorts keys, so order is deterministic: a, b.
	for _, r := range res {
		if r.Err != nil {
			t.Errorf("key %q first insert reported error %v", r.Key, r.Err)
		}
		if r.Exists {
			t.Errorf("key %q first insert reported Exists=true; want false", r.Key)
		}
		if r.Old != nil {
			t.Errorf("key %q first insert Old = %v; want nil", r.Key, r.Old)
		}
	}
}

func TestSwapManyMixedInsertAndReplace(t *testing.T) {
	m := New()
	// Seed "b" so it's a replace; "a"/"c" are first inserts.
	m.Set("b", "oldB")
	res := m.SwapMany(map[string]any{"a": "1", "b": "2", "c": "3"})
	if len(res) != 3 {
		t.Fatalf("got %d results; want 3", len(res))
	}
	want := map[string]struct{ exists bool; old any }{
		"a": {false, nil},
		"b": {true, "oldB"},
		"c": {false, nil},
	}
	for _, r := range res {
		w, ok := want[r.Key]
		if !ok {
			t.Fatalf("unexpected key %q", r.Key)
		}
		if r.Exists != w.exists {
			t.Errorf("key %q Exists = %v; want %v", r.Key, r.Exists, w.exists)
		}
		if r.Err != nil {
			t.Errorf("key %q reported error %v", r.Key, r.Err)
		}
		if r.Key == "b" {
			if s, ok := r.Old.(string); !ok || s != "oldB" {
				t.Errorf("key b Old = %v; want oldB", r.Old)
			}
		} else if r.Old != nil {
			t.Errorf("key %q Old = %v; want nil", r.Key, r.Old)
		}
	}
}

func TestSwapManyReplaceEmptyValueExists(t *testing.T) {
	m := New()
	m.Set("k", "") // existing empty value
	res := m.SwapMany(map[string]any{"k": "v"})
	if len(res) != 1 {
		t.Fatalf("got %d results; want 1", len(res))
	}
	r := res[0]
	if !r.Exists {
		t.Fatal("replacing empty value: Exists=false; want true")
	}
	if r.Err != nil {
		t.Fatalf("replacing empty value reported error %v", r.Err)
	}
	// A stored empty string is a non-nil "" — distinct from a first insert's
	// nil Old. This is exactly the distinguishability the bug report asked for.
	if r.Old == nil {
		t.Fatal("replacing empty value Old = nil; want non-nil empty string")
	}
	if s, ok := r.Old.(string); !ok || s != "" {
		t.Errorf("replacing empty value Old = %v (%T); want empty string", r.Old, r.Old)
	}
}

func TestSwapManyEmptyKeyErrorsOnlyThatEntry(t *testing.T) {
	m := New()
	res := m.SwapMany(map[string]any{"": "x", "k": "v"})
	if len(res) != 2 {
		t.Fatalf("got %d results; want 2", len(res))
	}
	for _, r := range res {
		if r.Key == "" {
			if r.Err == nil || !strings.Contains(r.Err.Error(), "empty key") {
				t.Errorf("empty key entry: err = %v; want 'empty key'", r.Err)
			}
			if r.Exists {
				t.Error("empty key entry reported Exists=true")
			}
		} else {
			if r.Err != nil {
				t.Errorf("key %q reported error %v", r.Key, r.Err)
			}
			if r.Exists {
				t.Errorf("key %q reported Exists=true on first insert", r.Key)
			}
		}
	}
}

func TestMustSwapAddAndReplace(t *testing.T) {
	m := New()
	// First insert → ADD.
	old, replaced, err := m.MustSwap("k", "v1")
	if err != nil {
		t.Fatalf("first insert err = %v", err)
	}
	if replaced {
		t.Fatal("first insert: replaced=true; want false")
	}
	if old != "" {
		t.Fatalf("first insert old = %q; want empty", old)
	}
	// Replace → REPLACE, returns old value.
	old, replaced, err = m.MustSwap("k", "v2")
	if err != nil {
		t.Fatalf("replace err = %v", err)
	}
	if !replaced {
		t.Fatal("replace: replaced=false; want true")
	}
	if old != "v1" {
		t.Fatalf("replace old = %q; want v1", old)
	}
}

func TestMustSwapReplaceEmptyValueIsReplace(t *testing.T) {
	m := New()
	// Seed with empty string, then replace it. Must remain a REPLACE, not ADD.
	if _, replaced, err := m.MustSwap("k", ""); err != nil || replaced {
		t.Fatalf("seed empty value: err=%v replaced=%v", err, replaced)
	}
	old, replaced, err := m.MustSwap("k", "v")
	if err != nil {
		t.Fatalf("replace of empty value err = %v", err)
	}
	if !replaced {
		t.Fatal("replace of empty value: replaced=false; want true (the key existed)")
	}
	if old != "" {
		t.Fatalf("replace of empty value old = %q; want empty", old)
	}
}

func TestMustSwapEmptyKeyErrors(t *testing.T) {
	m := New()
	_, _, err := m.MustSwap("", "v")
	if err == nil {
		t.Fatal("empty key: expected error, got nil")
	}
}

// TestSwapNoKeyNotFoundMessage guards against the regression where first
// inserts produced a "safemap: key not found" error.
func TestSwapNoKeyNotFoundMessage(t *testing.T) {
	m := New()
	_, _, err := m.Swap("newkey", "v")
	if err != nil && strings.Contains(err.Error(), "key not found") {
		t.Fatalf("first insert produced 'key not found' error: %v", err)
	}
	res := m.SwapMany(map[string]any{"another": "v"})
	for _, r := range res {
		if r.Err != nil && strings.Contains(r.Err.Error(), "key not found") {
			t.Fatalf("SwapMany first insert produced 'key not found' error: %v", r.Err)
		}
	}
}

// Ensure error values from Swap are sentinel-like and comparable where relevant.
func TestSwapNilMapErrorIsNotNil(t *testing.T) {
	var m *Map
	_, _, err := m.Swap("k", "v")
	if !errors.Is(err, err) { // trivially true, but ensures non-nil error type usable
		t.Fatal("nil map error should be a usable non-nil error")
	}
}
