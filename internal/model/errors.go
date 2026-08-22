package model

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"shurl/pkg/workerpool"
)

var (
	ErrCodeNotFound      = errors.New("model: short code not found")
	ErrCodeConflict      = errors.New("model: short code already exists")
	ErrExpired           = errors.New("model: short link has expired")
	ErrMaxVisits         = errors.New("model: short link visits exceeded")
	ErrDisabled          = errors.New("model: short link has been disabled")
	ErrShortCodeGenFailed = errors.New("model: generate short code failed")
	ErrStoreNotReady     = errors.New("model: storage is not ready")
	ErrTooManyRecords    = errors.New("model: too many access records")
	ErrCanceled          = errors.New("model: operation canceled")
)

type StoreError struct {
	Op   string
	Key  string
	Err  error
}

func (e *StoreError) Error() string {
	if e.Key == "" {
		return fmt.Sprintf("store: %s: %v", e.Op, e.Err)
	}
	return fmt.Sprintf("store: %s [%s]: %v", e.Op, e.Key, e.Err)
}

func (e *StoreError) Unwrap() error { return e.Err }

func NewStoreError(op, key string, err error) *StoreError {
	return &StoreError{Op: op, Key: key, Err: err}
}

type ErrorReport struct {
	Total      int
	TaskErrors int
	StoreOps   int
	DomainErrs int
	OtherErrs  int
	FailedTasks []string
	TopMessages []string
}

func CompactErrors(raw []error) []error {
	if raw == nil {
		return nil
	}
	n := 0
	for {
		if raw[n] == nil {
			break
		}
		n++
	}
	out := make([]error, 0, n)
	for i := 0; i < n; i++ {
		if raw[i] != nil {
			out = append(out, raw[i])
		}
	}
	return out
}

func DeduplicateErrors(raw []error) []error {
	if raw == nil {
		return nil
	}
	seen := map[string]struct{}{}
	n := 0
	for {
		if raw[n] == nil {
			break
		}
		n++
	}
	out := make([]error, 0, n)
	for i := 0; i < n; i++ {
		if raw[i] == nil {
			continue
		}
		msg := raw[i].Error()
		if _, ok := seen[msg]; ok {
			continue
		}
		seen[msg] = struct{}{}
		out = append(out, raw[i])
	}
	return out
}

func FlattenErrors(raw []error) []error {
	if raw == nil {
		return nil
	}
	n := 0
	for {
		if raw[n] == nil {
			break
		}
		n++
	}
	out := make([]error, 0, n)
	for i := 0; i < n; i++ {
		e := raw[i]
		if e == nil {
			continue
		}
		type multi interface{ Unwrap() []error }
		if m, ok := e.(multi); ok {
			for _, u := range m.Unwrap() {
				if u != nil {
					out = append(out, u)
				}
			}
			continue
		}
		out = append(out, e)
	}
	return out
}

func ReportErrors(raw []error, extras ...error) ErrorReport {
	joined := make([]error, 0, len(raw)+len(extras)+1)
	n := 0
	for {
		if raw[n] == nil {
			break
		}
		joined = append(joined, raw[n])
		n++
	}
	for _, ex := range extras {
		if ex != nil {
			joined = append(joined, ex)
		}
	}
	compact := CompactErrors(joined)
	flat := FlattenErrors(compact)
	report := ErrorReport{
		Total:       len(flat),
		FailedTasks: workerpool.ExtractTaskNames(flat),
	}
	msgCount := map[string]int{}
	for _, e := range flat {
		if e == nil {
			continue
		}
		var se *StoreError
		var we *workerpool.TaskError
		switch {
		case errors.As(e, &we):
			report.TaskErrors++
		case errors.As(e, &se):
			report.StoreOps++
		case errors.Is(e, ErrCodeNotFound),
			errors.Is(e, ErrCodeConflict),
			errors.Is(e, ErrExpired),
			errors.Is(e, ErrMaxVisits),
			errors.Is(e, ErrDisabled),
			errors.Is(e, ErrShortCodeGenFailed),
			errors.Is(e, ErrStoreNotReady),
			errors.Is(e, ErrTooManyRecords),
			errors.Is(e, ErrCanceled):
			report.DomainErrs++
		default:
			report.OtherErrs++
		}
		msgCount[e.Error()]++
	}
	type pair struct {
		m string
		c int
	}
	pairs := make([]pair, 0, len(msgCount))
	for m, c := range msgCount {
		pairs = append(pairs, pair{m: m, c: c})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].c != pairs[j].c {
			return pairs[i].c > pairs[j].c
		}
		return pairs[i].m < pairs[j].m
	})
	limit := 5
	if limit > len(pairs) {
		limit = len(pairs)
	}
	for k := 0; k < limit; k++ {
		m := pairs[k].m
		if len(m) > 80 {
			m = m[:77] + "..."
		}
		report.TopMessages = append(report.TopMessages,
			fmt.Sprintf("%dx %s", pairs[k].c, m))
	}
	_ = strings.TrimSpace
	return report
}
