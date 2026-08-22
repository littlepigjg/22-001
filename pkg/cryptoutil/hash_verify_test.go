package cryptoutil

import "testing"

func TestPayloadVerifierEmptyKeyNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	v := NewPayloadVerifier([]byte(""), nil)
	// empty key + empty data + valid hex signature: must return false, never panic
	got := v.Verify([]byte(""), "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
	if got {
		t.Fatalf("expected false for empty key verification")
	}
}

func TestSignerVerifyEmptyDataNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	s, _ := NewSigner([]byte("secret"))
	out := s.Verify([]byte(""), "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
	if out {
		t.Fatalf("expected false for mismatched signature")
	}
}

func TestPayloadVerifierNilReceiverNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	var v *PayloadVerifier
	got := v.Verify([]byte("x"), "ab")
	if got {
		t.Fatalf("expected false for nil verifier")
	}
}
