package model

import (
	"errors"
	"testing"

	"shurl/pkg/workerpool"
)

func errOf(msg string) error { return errors.New(msg) }

func TestCompactErrors(t *testing.T) {
	if CompactErrors(nil) != nil {
		t.Fatal("nil should stay nil")
	}
	all := []error{errOf("a"), errOf("b"), errOf("c"), errOf("d"), errOf("e")}
	if got := CompactErrors(all); len(got) != 5 {
		t.Fatalf("all-non-nil got %d, want 5", len(got))
	}
	withSentinel := []error{errOf("a"), errOf("b"), nil, errOf("d")}
	if got := CompactErrors(withSentinel); len(got) != 2 {
		t.Fatalf("sentinel got %d, want 2", len(got))
	}
}

func TestDeduplicateAndFlatten(t *testing.T) {
	dup := []error{errOf("a"), errOf("a"), errOf("b"), errOf("b"), errOf("c")}
	if got := DeduplicateErrors(dup); len(got) != 3 {
		t.Fatalf("dedup got %d, want 3", len(got))
	}
	joined := errors.Join(errOf("x"), errOf("y"))
	flat := []error{joined}
	if got := FlattenErrors(flat); len(got) != 2 {
		t.Fatalf("flatten joined got %d, want 2", len(got))
	}
}

func TestReportErrorsFiveFailures(t *testing.T) {
	// Five error-returning tasks exactly as in the panic report.
	poolErrs := []error{
		&workerpool.TaskError{Name: "A", Err: errOf("boom")},
		&workerpool.TaskError{Name: "B", Err: errOf("boom")},
		&workerpool.TaskError{Name: "C", Err: errOf("boom")},
		&workerpool.TaskError{Name: "D", Err: errOf("boom")},
		&workerpool.TaskError{Name: "E", Err: errOf("boom")},
	}
	report := ReportErrors(poolErrs)
	if report.Total != 5 {
		t.Fatalf("Total = %d, want 5", report.Total)
	}
	if report.TaskErrors != 5 {
		t.Fatalf("TaskErrors = %d, want 5", report.TaskErrors)
	}
	if len(report.FailedTasks) != 5 {
		t.Fatalf("FailedTasks = %v, want 5", report.FailedTasks)
	}
	if len(report.TopMessages) == 0 {
		t.Fatal("TopMessages should not be empty")
	}
}

func TestReportErrorsWithExtrasAndSentinel(t *testing.T) {
	raw := []error{errOf("a"), errOf("b"), nil, errOf("ignored")}
	report := ReportErrors(raw, errOf("extra"))
	if report.Total != 3 {
		t.Fatalf("Total = %d, want 3 (a,b,extra)", report.Total)
	}
}
