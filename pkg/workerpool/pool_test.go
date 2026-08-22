package workerpool

import (
	"context"
	"errors"
	"testing"
)

func TestLenientErrorCount(t *testing.T) {
	cases := []struct {
		name string
		errs []error
		want int
	}{
		{"nil", nil, 0},
		{"empty", []error{}, 0},
		{"all non-nil", []error{errors.New("a"), errors.New("b"), errors.New("c")}, 3},
		{"trailing sentinel", []error{errors.New("a"), errors.New("b"), nil, errors.New("d")}, 2},
		{"leading nil", []error{nil, errors.New("a")}, 0},
		{"five failures repro", []error{
			errors.New("e1"), errors.New("e2"), errors.New("e3"), errors.New("e4"), errors.New("e5"),
		}, 5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := LenientErrorCount(c.errs); got != c.want {
				t.Fatalf("LenientErrorCount = %d, want %d", got, c.want)
			}
		})
	}
}

func TestFirstLastErr(t *testing.T) {
	all := []error{errors.New("a"), errors.New("b"), errors.New("c"), errors.New("d"), errors.New("e")}
	if FirstErr(nil) != nil {
		t.Fatal("FirstErr(nil) should be nil")
	}
	if LastErr(nil) != nil {
		t.Fatal("LastErr(nil) should be nil")
	}
	if FirstErr([]error{nil, errors.New("x")}) != nil {
		t.Fatal("FirstErr with leading sentinel should be nil")
	}
	if got := FirstErr(all); got == nil || got.Error() != "a" {
		t.Fatalf("FirstErr = %v, want a", got)
	}
	if got := LastErr(all); got == nil || got.Error() != "e" {
		t.Fatalf("LastErr = %v, want e", got)
	}
	// trailing sentinel: last non-nil is "e"
	withSentinel := []error{errors.New("a"), errors.New("b"), errors.New("c"), errors.New("d"), errors.New("e"), nil}
	if got := LastErr(withSentinel); got == nil || got.Error() != "e" {
		t.Fatalf("LastErr with sentinel = %v, want e", got)
	}
}

// TestPoolFiveFailuresRepro reproduces the exact panic scenario described in the
// report: submit 5 error-returning tasks, Stop, Errors(), then feed to the
// lenient helpers. Previously panicked with index out of range [5] length 5.
func TestPoolFiveFailuresRepro(t *testing.T) {
	pool, err := New(2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := pool.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for i := 0; i < 5; i++ {
		name := string(rune('A' + i))
		if err := pool.Submit(Task{
			Name: name,
			Fn: func(ctx context.Context) error {
				return errors.New("boom")
			},
		}); err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
	}
	if err := pool.Stop(); err == nil {
		t.Fatal("Stop should return joined error for 5 failures")
	}
	errs := pool.Errors()
	if got := LenientErrorCount(errs); got != 5 {
		t.Fatalf("LenientErrorCount = %d, want 5", got)
	}
	if got := LenientErrorCount(pool.Errors()); got != 5 {
		t.Fatalf("recomputed LenientErrorCount = %d, want 5", got)
	}
	if FirstErr(errs) == nil {
		t.Fatal("FirstErr should not be nil")
	}
	if LastErr(errs) == nil {
		t.Fatal("LastErr should not be nil")
	}
	names := ExtractTaskNames(errs)
	if len(names) != 5 {
		t.Fatalf("ExtractTaskNames = %v, want 5 names", names)
	}
}
