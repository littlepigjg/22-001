package shurl

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"shurl/internal/model"
	"shurl/pkg/durationutil"
	"shurl/pkg/validator"
)

func mustNotPanic(t *testing.T, name string, fn func()) (retErr error, panicked bool, panicInfo string) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			panicInfo = fmt.Sprintf("%s: %v", name, r)
		}
	}()
	fn()
	return nil, false, ""
}

func TestRedGreen(t *testing.T) {
	red := false
	var panicLogs []string

	cases := []struct {
		name string
		fn   func()
	}{
		{
			name: "durationutil.ParseDuration(\"1y\")",
			fn: func() {
				_, _ = durationutil.ParseDuration("1y")
			},
		},
		{
			name: "durationutil.ParseDuration(\"+1z\")",
			fn: func() {
				_, _ = durationutil.ParseDuration("+1z")
			},
		},
		{
			name: "validator.ValidateTTLString(\"1y\")",
			fn: func() {
				_, _ = validator.ValidateTTLString("1y")
			},
		},
		{
			name: "validator.ValidateTTLString(\"1d + 2y\")",
			fn: func() {
				_, _ = validator.ValidateTTLString("1d + 2y")
			},
		},
		{
			name: "model.CreateReq.Validate(TTLString=1y)",
			fn: func() {
				req := &model.CreateReq{
					RawURL:    "https://example.com/link",
					TTLString: "1y",
				}
				_ = req.Validate()
			},
		},
		{
			name: "durationutil.SplitAndSumDurations(\"1h,1q\", \",\")",
			fn: func() {
				_, _ = durationutil.SplitAndSumDurations("1h,1q", ",")
			},
		},
	}

	for _, c := range cases {
		_, panicked, info := mustNotPanic(t, c.name, c.fn)
		if panicked {
			red = true
			panicLogs = append(panicLogs, info)
			break
		}
	}

	if !red {
		d, err := durationutil.ParseDuration("1d6h")
		want := 24*time.Hour + 6*time.Hour
		if err != nil {
			t.Logf("GREEN-path regression: 1d6h err=%v", err)
			red = true
			panicLogs = append(panicLogs, fmt.Sprintf("regression: valid duration 1d6h returned unexpected error: %v", err))
		} else if d != want {
			t.Logf("GREEN-path regression: 1d6h want=%v got=%v", want, d)
			red = true
			panicLogs = append(panicLogs, fmt.Sprintf("regression: valid duration 1d6h want=%v got=%v", want, d))
		}

		d2, err2 := validator.ValidateTTLString("1周")
		want2 := 7 * 24 * time.Hour
		if err2 != nil {
			red = true
			panicLogs = append(panicLogs, fmt.Sprintf("regression: ValidateTTLString('1周') err=%v", err2))
		} else if d2 != want2 {
			red = true
			panicLogs = append(panicLogs, fmt.Sprintf("regression: ValidateTTLString('1周') want=%v got=%v", want2, d2))
		}

		d3, err3 := durationutil.ParseDuration("t:1d")
		want3 := 24 * time.Hour
		if err3 != nil {
			red = true
			panicLogs = append(panicLogs, fmt.Sprintf("regression: ParseDuration('t:1d') err=%v", err3))
		} else if d3 != want3 {
			red = true
			panicLogs = append(panicLogs, fmt.Sprintf("regression: ParseDuration('t:1d') want=%v got=%v", want3, d3))
		}

		_, err4 := validator.ValidateTTLString("1y")
		if err4 == nil {
			red = true
			panicLogs = append(panicLogs, fmt.Sprintf("regression: ValidateTTLString('1y') should error (unknown unit y) but returned nil"))
		} else if !strings.Contains(err4.Error(), "invalid unit") &&
			!strings.Contains(err4.Error(), "invalid ttl") &&
			!strings.Contains(err4.Error(), "parse ttl") {
			panicLogs = append(panicLogs, fmt.Sprintf("note: ValidateTTLString('1y') returned err=%v", err4))
		}
	}

	if red {
		fmt.Println("RED（红灯，缺陷未修复）")
		for _, m := range panicLogs {
			t.Logf("  - %s", m)
		}
		t.Errorf("RED: panic or regression observed, details=%s", strings.Join(panicLogs, "; "))
	} else {
		fmt.Println("GREEN（绿灯，缺陷已修复）")
	}
}
