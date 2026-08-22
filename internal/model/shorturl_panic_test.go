package model

import "testing"

// Reproduce the reported Create-path panic:
// raw_url set, custom_code empty, code_signature = 64-hex, signer_salt empty.
func TestCreateReqValidateNoPanicEmptyKey(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Create path panicked: %v", r)
		}
	}()
	r := &CreateReq{
		RawURL:        "https://example.com/",
		CodeSignature: "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		SignerSalt:    "",
	}
	err := r.Validate()
	if err == nil {
		t.Fatalf("expected error for empty key with signature, got nil")
	}
	t.Logf("Create path returned error (no panic): %v", err)
}

// Reproduce the reported redirect-path panic:
// code=abcd1234, signature = 64-hex, signer key empty.
func TestRedirectCheckValidateNoPanicEmptyKey(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Redirect path panicked: %v", r)
		}
	}()
	r := &RedirectCheckReq{
		Code:      "abcd1234",
		Signature: "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
	}
	err := r.Validate(nil)
	if err == nil {
		t.Fatalf("expected error for empty key with signature, got nil")
	}
	t.Logf("Redirect path returned error (no panic): %v", err)
}
