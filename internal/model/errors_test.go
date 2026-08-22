package model

import (
	"errors"
	"testing"
)

func TestStoreErrorUnwrapAndClassify(t *testing.T) {
	cases := []struct {
		name     string
		op       string
		inner    error
		wantSent error
		wantCls  ErrClass
	}{
		{"dup custom code", "RepeatCustomCode", ErrCodeConflict, ErrCodeConflict, ClassConflict},
		{"unknown code", "ReadShortURL", ErrCodeNotFound, ErrCodeNotFound, ClassNotFound},
		{"delete unknown", "RemoveShortURL", ErrCodeNotFound, ErrCodeNotFound, ClassNotFound},
		{"expired", "LookupURL", ErrExpired, ErrExpired, ClassExpired},
		{"disabled", "FetchURL", ErrDisabled, ErrDisabled, ClassDisabled},
		{"max visits", "BumpVisits", ErrMaxVisits, ErrMaxVisits, ClassMaxVisits},
		{"too many records", "AppendMany", ErrTooManyRecords, ErrTooManyRecords, ClassTooMany},
		{"store not ready", "SaveURLNotReady", ErrStoreNotReady, ErrStoreNotReady, ClassStoreNotReady},
		{"canceled", "CreateCanceled", ErrCanceled, ErrCanceled, ClassCanceled},
		{"gen failed", "AutoGenCode", ErrShortCodeGenFailed, ErrShortCodeGenFailed, ClassGenFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			se := NewStoreError(tc.op, "k", tc.inner)

			// Unwrap must let errors.Is traverse to the wrapped sentinel.
			if !errors.Is(se, tc.wantSent) {
				t.Fatalf("errors.Is(storeErr, %v) = false; want true (Unwrap missing?)", tc.wantSent)
			}

			// errors.As must still locate the *StoreError itself.
			var got *StoreError
			if !errors.As(se, &got) {
				t.Fatalf("errors.As failed to find *StoreError")
			}

			// Classification must resolve to the right class by sentinel identity,
			// regardless of op naming.
			if cls := ClassifyDomainError(se); cls != tc.wantCls {
				t.Fatalf("ClassifyDomainError = %v; want %v", cls, tc.wantCls)
			}
		})
	}
}

// Regression: a bare sentinel (not wrapped) must still classify.
func TestClassifyBareSentinels(t *testing.T) {
	if cls := ClassifyDomainError(ErrCodeConflict); cls != ClassConflict {
		t.Fatalf("ClassifyDomainError(ErrCodeConflict) = %v; want %v", cls, ClassConflict)
	}
	if cls := ClassifyDomainError(ErrCodeNotFound); cls != ClassNotFound {
		t.Fatalf("ClassifyDomainError(ErrCodeNotFound) = %v; want %v", cls, ClassNotFound)
	}
}

// Regression: a StoreError wrapping a non-sentinel message should still fall
// through the message matchers (the old `model:`-prefix early-exit is gone).
func TestClassifyStoreErrorMessageFallback(t *testing.T) {
	se := NewStoreError("SaveURL", "k", errors.New("model: short code already exists"))
	if cls := ClassifyDomainError(se); cls != ClassConflict {
		t.Fatalf("ClassifyDomainError = %v; want %v (message fallback)", cls, ClassConflict)
	}
	se = NewStoreError("FetchURL", "k", errors.New("model: short code not found"))
	if cls := ClassifyDomainError(se); cls != ClassNotFound {
		t.Fatalf("ClassifyDomainError = %v; want %v (message fallback)", cls, ClassNotFound)
	}
	// Truly unknown inner message falls through to ClassUnknown.
	se = NewStoreError("SomethingElse", "k", errors.New("model: weird unrelated error"))
	if cls := ClassifyDomainError(se); cls != ClassUnknown {
		t.Fatalf("ClassifyDomainError = %v; want %v", cls, ClassUnknown)
	}
}
