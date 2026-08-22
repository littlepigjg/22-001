package httperr

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"shurl/internal/model"
	"shurl/pkg/response"
)

func TestMapWrappedStoreErrors(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   int
	}{
		{"dup custom code -> 409", model.NewStoreError("RepeatCustomCode", "x", model.ErrCodeConflict), 409, response.CodeConflict},
		{"save conflict -> 409", model.NewStoreError("SaveURL", "x", model.ErrCodeConflict), 409, response.CodeConflict},
		{"get unknown -> 404", model.NewStoreError("ReadShortURL", "x", model.ErrCodeNotFound), 404, response.CodeNotFound},
		{"fetch not found -> 404", model.NewStoreError("FetchURL", "x", model.ErrCodeNotFound), 404, response.CodeNotFound},
		{"delete unknown -> 404", model.NewStoreError("RemoveShortURL", "x", model.ErrCodeNotFound), 404, response.CodeNotFound},
		{"disabled -> 410", model.NewStoreError("FetchURL", "x", model.ErrDisabled), 410, response.CodeExpired},
		{"expired -> 410", model.NewStoreError("LookupURL", "x", model.ErrExpired), 410, response.CodeExpired},
		{"max visits -> 410", model.NewStoreError("BumpVisits", "x", model.ErrMaxVisits), 410, response.CodeExpired},
		{"store not ready -> 503", model.NewStoreError("SaveURLNotReady", "x", model.ErrStoreNotReady), 503, response.CodeServer},
		{"too many records -> 413", model.NewStoreError("AppendMany", "x", model.ErrTooManyRecords), 413, response.CodeServer},
		{"canceled -> 499", model.NewStoreError("CreateCanceled", "", model.ErrCanceled), 499, response.CodeServer},
		// gen failed intentionally stays 500.
		{"gen failed -> 500", model.NewStoreError("AutoGenCode", "", model.ErrShortCodeGenFailed), 500, response.CodeServer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			Map(rr, tc.err)

			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d; want %d (err=%v)", rr.Code, tc.wantStatus, tc.err)
			}
			var r response.Resp
			if err := json.Unmarshal(rr.Body.Bytes(), &r); err != nil {
				t.Fatalf("body not json: %v (body=%q)", err, rr.Body.String())
			}
			if r.Code != tc.wantCode {
				t.Fatalf("biz code = %d; want %d (body=%q)", r.Code, tc.wantCode, rr.Body.String())
			}
		})
	}
}

// Regression for the reported bug: the error string must not be the raw
// "store: RepeatCustomCode [...]" leak when it maps to a known class.
func TestMapDupCustomCodeBody(t *testing.T) {
	err := model.NewStoreError("RepeatCustomCode", "abc", model.ErrCodeConflict)
	rr := httptest.NewRecorder()
	Map(rr, err)

	if rr.Code != 409 {
		t.Fatalf("status = %d; want 409", rr.Code)
	}
	var r response.Resp
	_ = json.Unmarshal(rr.Body.Bytes(), &r)
	if r.Code != response.CodeConflict {
		t.Fatalf("biz code = %d; want %d", r.Code, response.CodeConflict)
	}
	if r.Message == "" {
		t.Fatalf("message empty")
	}
	if !contains(r.Message, "already exists") {
		t.Fatalf("message = %q; want it to mention conflict", r.Message)
	}
}

// nil error must yield a 200 success envelope.
func TestMapNil(t *testing.T) {
	rr := httptest.NewRecorder()
	Map(rr, nil)
	if rr.Code != 200 {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
}

// A bare (non-wrapped) sentinel must still map correctly via errors.Is.
func TestMapBareSentinels(t *testing.T) {
	rr := httptest.NewRecorder()
	Map(rr, errors.New("model: short code already exists"))
	if rr.Code != 500 {
		// A plain errors.New string is unknown to classification and errors.Is,
		// so it correctly falls to 500. This guards against over-eager matching.
		t.Fatalf("status = %d; want 500 for unclassified string", rr.Code)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
