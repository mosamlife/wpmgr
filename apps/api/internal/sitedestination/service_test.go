package sitedestination

import (
	"testing"
)

// TestAgeRoundTrip is a smoke test that the age identity wrapping round-trips
// arbitrary bytes through Encrypt/Decrypt and that two distinct AgeIdentity
// instances cannot decrypt each other's ciphertext (sanity check that the
// X25519 keypair is actually being used).
func TestAgeRoundTrip(t *testing.T) {
	a, err := NewAgeIdentity("")
	if err != nil {
		t.Fatalf("NewAgeIdentity: %v", err)
	}
	plain := []byte("super-secret-s3-credential-token-xyz")
	cipher, err := a.Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if len(cipher) == 0 {
		t.Fatalf("Encrypt returned empty ciphertext")
	}
	got, err := a.Decrypt(cipher)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(got) != string(plain) {
		t.Fatalf("Decrypt mismatch: got %q want %q", got, plain)
	}

	// A second identity must NOT be able to decrypt the first's ciphertext.
	b, err := NewAgeIdentity("")
	if err != nil {
		t.Fatalf("NewAgeIdentity #2: %v", err)
	}
	if _, err := b.Decrypt(cipher); err == nil {
		t.Fatalf("Decrypt with foreign identity unexpectedly succeeded")
	}
}

// TestValidKind covers the closed-set check used by the handler input
// validation.
func TestValidKind(t *testing.T) {
	cases := map[Kind]bool{
		KindCP:       true,
		KindLocal:    true,
		KindS3Compat: true,
		Kind("ftp"):  false,
		Kind(""):     false,
	}
	for k, want := range cases {
		if got := ValidKind(k); got != want {
			t.Errorf("ValidKind(%q): got %v, want %v", k, got, want)
		}
	}
}

// TestTestConnectionShortCircuits verifies that CP and Local kinds short-
// circuit to OK without any S3 client construction (the operator's
// destination form should be able to mark them ready without credentials).
func TestTestConnectionShortCircuits(t *testing.T) {
	a, err := NewAgeIdentity("")
	if err != nil {
		t.Fatalf("NewAgeIdentity: %v", err)
	}
	svc := NewService(nil, a, nil)
	for _, k := range []Kind{KindCP, KindLocal} {
		res := svc.TestConnection(nil, TestConnectionInput{Kind: k})
		if !res.OK {
			t.Errorf("TestConnection(%q): expected OK=true, got %+v", k, res)
		}
	}
}
