package cryptoutil

import "testing"

func TestPayloadVerifierRoundTrip(t *testing.T) {
	key := []byte("secret-key")
	salt := []byte("salty")
	p := NewPayloadSigner(key, salt)
	data := []byte("payload-data")
	sig := p.SignHex(data)

	v := NewPayloadVerifier(key, salt)
	if !v.Verify(data, sig) {
		t.Fatalf("valid signature failed verification")
	}
	// wrong data must fail
	if v.Verify([]byte("tampered"), sig) {
		t.Fatalf("expected mismatch on tampered data")
	}
	// wrong key must fail
	v2 := NewPayloadVerifier([]byte("other"), salt)
	if v2.Verify(data, sig) {
		t.Fatalf("expected mismatch on wrong key")
	}
}
