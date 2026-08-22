package admin

import (
	"bytes"
	"context"
	"testing"

	"shurl/pkg/logger"
)

// TestRunTaskDiagnosticsNoPanic exercises the exact reproduction path from
// the report: submit erroring tasks via the admin diagnostic pool, Stop, then
// ReportErrors/FirstErr/LastErr aggregation via RunTaskDiagnostics. Previously
// this panicked with "index out of range [5] with length 5".
func TestRunTaskDiagnosticsNoPanic(t *testing.T) {
	log := logger.New(&bytes.Buffer{}, logger.LevelInfo, false)
	s := New(log)

	report, err := s.RunTaskDiagnostics(context.Background())
	if err != nil {
		t.Fatalf("RunTaskDiagnostics returned err: %v", err)
	}
	if report == nil {
		t.Fatal("RunTaskDiagnostics returned nil report")
	}
	if report.Summary.Total < 0 {
		t.Fatalf("Total should be >= 0, got %d", report.Summary.Total)
	}
	if report.Failed != report.Summary.Total {
		t.Fatalf("Failed=%d != Summary.Total=%d", report.Failed, report.Summary.Total)
	}
	if report.Passed < 0 {
		t.Fatalf("Passed should be >= 0, got %d", report.Passed)
	}
}

// TestRunTaskDiagnosticsAllFail builds a service whose diagnostic tasks all
// fail (no flushers/syncers/closers registered), confirming aggregation stays
// stable when every task errors.
func TestRunTaskDiagnosticsAllFail(t *testing.T) {
	log := logger.New(&bytes.Buffer{}, logger.LevelInfo, false)
	s := New(log)

	report, err := s.RunTaskDiagnostics(context.Background())
	if err != nil {
		t.Fatalf("RunTaskDiagnostics returned err: %v", err)
	}
	if report.Failed != report.Summary.Total {
		t.Fatalf("Failed=%d != Summary.Total=%d", report.Failed, report.Summary.Total)
	}
	if report.Passed < 0 {
		t.Fatalf("Passed should be >= 0, got %d", report.Passed)
	}
	// TopMessages must never exceed the failure total.
	if report.Summary.Total > 0 && len(report.Summary.TopMessages) > report.Summary.Total {
		t.Fatalf("TopMessages=%d exceeds Total=%d", len(report.Summary.TopMessages), report.Summary.Total)
	}
}
