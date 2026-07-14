package cryptbox

// bech32_test.go exercises the self-contained Bech32 encoder in bech32.go in
// isolation, independent of DeriveAgeIdentity/age. TestBech32Encode_BIP173Vector
// pins the encoder against a known-answer test vector straight from the
// BIP-173 spec so a checksum regression is caught here directly, not only
// indirectly via age.ParseX25519Identity failing elsewhere.

import (
	"crypto/sha256"
	"testing"

	"filippo.io/age"
)

// TestBech32Encode_BIP173Vector checks against BIP-173's published
// zero-data-length test vector ("A12UEL5L" / "a12uel5l": hrp "a", empty
// data), which pins the checksum/generator-polynomial constants without any
// decoding ambiguity (there is no data payload to have gotten wrong).
func TestBech32Encode_BIP173Vector(t *testing.T) {
	upper, err := bech32Encode("A", nil)
	if err != nil {
		t.Fatalf("bech32Encode(A, nil): %v", err)
	}
	if upper != "A12UEL5L" {
		t.Fatalf("bech32Encode(A, nil) = %q, want %q", upper, "A12UEL5L")
	}

	lower, err := bech32Encode("a", nil)
	if err != nil {
		t.Fatalf("bech32Encode(a, nil): %v", err)
	}
	if lower != "a12uel5l" {
		t.Fatalf("bech32Encode(a, nil) = %q, want %q", lower, "a12uel5l")
	}
}

// TestEncodeAgeSecretKey_RoundTripsThroughUpstreamAge feeds a battery of
// scalar values (zero, all-0xFF, and several pseudo-random samples) through
// encodeAgeSecretKey and confirms every one is accepted by the real
// age.ParseX25519Identity parser and yields a working, self-consistent
// identity. This directly targets the checksum arithmetic across many data
// payloads, not just the empty-data BIP-173 vector above.
func TestEncodeAgeSecretKey_RoundTripsThroughUpstreamAge(t *testing.T) {
	scalars := [][]byte{
		make([]byte, ageX25519ScalarSize),      // all zero
		bytesRepeat(0xFF, ageX25519ScalarSize), // all 0xFF
		bytesSeq(ageX25519ScalarSize),          // 0x00,0x01,0x02,...
		sha256Sum32([]byte("scalar-fixture-seed-1")),
		sha256Sum32([]byte("scalar-fixture-seed-2")),
	}

	for i, scalar := range scalars {
		encoded, err := encodeAgeSecretKey(scalar)
		if err != nil {
			t.Fatalf("scalars[%d]: encodeAgeSecretKey: %v", i, err)
		}
		id, err := age.ParseX25519Identity(encoded)
		if err != nil {
			t.Fatalf("scalars[%d]: age.ParseX25519Identity(%q): %v", i, encoded, err)
		}
		box := &AgeIdentity{identity: id, recipient: id.Recipient()}
		ciphertext, err := box.Encrypt([]byte("payload"))
		if err != nil {
			t.Fatalf("scalars[%d]: Encrypt: %v", i, err)
		}
		plaintext, err := box.Decrypt(ciphertext)
		if err != nil {
			t.Fatalf("scalars[%d]: Decrypt: %v", i, err)
		}
		if string(plaintext) != "payload" {
			t.Fatalf("scalars[%d]: decrypted = %q, want %q", i, plaintext, "payload")
		}
	}
}

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func bytesSeq(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i)
	}
	return out
}

// sha256Sum32 returns a realistic (non-adversarially-chosen), non-repeating
// 32-byte value derived from label, purely as test-fixture scalar material —
// unrelated to and independent of the HKDF construction under test elsewhere.
func sha256Sum32(label []byte) []byte {
	sum := sha256.Sum256(label)
	return sum[:]
}
