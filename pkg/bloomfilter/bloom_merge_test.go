package bloomfilter

import (
	"testing"
)

// TestMergeNoDoubleUnlock reproduces the original double-RUnlock panic in
// Merge: it had both `defer other.mu.RUnlock()` and a trailing manual
// `other.mu.RUnlock()`, so the next operation on `other` panicked with
// "sync: Unlock of unlocked RWMutex". Two sequential merges must succeed.
func TestMergeNoDoubleUnlock(t *testing.T) {
	a, err := NewWithEstimates(100, 0.01)
	if err != nil {
		t.Fatalf("NewWithEstimates a: %v", err)
	}
	b, err := NewWithEstimates(100, 0.01)
	if err != nil {
		t.Fatalf("NewWithEstimates b: %v", err)
	}
	a.AddString("alpha")
	a.AddString("beta")
	b.AddString("beta")
	b.AddString("gamma")

	if err := a.Merge(b); err != nil {
		t.Fatalf("first Merge: %v", err)
	}
	// Before the fix, this second merge (re-locking b internally / continued
	// use of b) tripped the double-unlocked RWMutex and panicked.
	if err := a.Merge(b); err != nil {
		t.Fatalf("second Merge: %v", err)
	}

	// b must still be usable — its lock was left in a sane state.
	b.AddString("delta")
	if !b.MayContainString("gamma") {
		t.Fatal("b lost gamma after merge reuse")
	}
	if !a.MayContainString("gamma") {
		t.Fatal("a should contain gamma after merge")
	}
}
