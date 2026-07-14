package cryptbox

// cryptbox_test.go proves DeriveAgeIdentity produces a STABLE, deterministic
// age identity from seed material (the fix for the self-host bug where every
// restart minted a fresh ephemeral key and orphaned every stored secret — see
// docs/... in the accompanying PR/issue). The restart-stability test is the
// core regression lock: it simulates a process restart by deriving a SECOND,
// independent AgeIdentity from the same seed and proving it decrypts what the
// first identity encrypted.

import (
	"strings"
	"testing"

	"filippo.io/age"
)

const testInfoLabel = "wpmgr-age-identity-v1"

// TestDeriveAgeIdentity_RestartStability is the core regression lock: it
// simulates a control-plane restart by deriving a SECOND AgeIdentity from the
// same seed+info (as would happen on the next boot, re-reading the same
// WPMGR_SESSION_SECRET) and asserts it can decrypt ciphertext produced by the
// FIRST identity. Before this fix, a fresh restart minted an unrelated random
// identity and every previously stored secret (SMTP password, per-site email
// creds, object-cache Redis creds, S3 backup-destination secrets, TOTP 2FA
// secrets) became permanently undecryptable.
func TestDeriveAgeIdentity_RestartStability(t *testing.T) {
	seed := []byte("a-32-byte-or-longer-session-secret!!")

	before, err := DeriveAgeIdentity(seed, testInfoLabel)
	if err != nil {
		t.Fatalf("DeriveAgeIdentity (pre-restart): %v", err)
	}
	plaintext := []byte("s3cr3t-SMTP-p@ssw0rd")
	ciphertext, err := before.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Simulate a restart: a brand-new process re-derives from the same seed.
	after, err := DeriveAgeIdentity(seed, testInfoLabel)
	if err != nil {
		t.Fatalf("DeriveAgeIdentity (post-restart): %v", err)
	}

	decrypted, err := after.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("post-restart identity failed to decrypt pre-restart ciphertext: %v", err)
	}
	if string(decrypted) != string(plaintext) {
		t.Fatalf("decrypted = %q, want %q", decrypted, plaintext)
	}
}

// TestDeriveAgeIdentity_Deterministic asserts same seed+info always yields
// the same identity (compared via its public RecipientString(), which is
// safe to log/compare without exposing the private key), and that a
// different seed yields a different, non-interoperable identity.
func TestDeriveAgeIdentity_Deterministic(t *testing.T) {
	seedA := []byte("session-secret-AAAAAAAAAAAAAAAAAAAA")
	seedB := []byte("session-secret-BBBBBBBBBBBBBBBBBBBB")

	a1, err := DeriveAgeIdentity(seedA, testInfoLabel)
	if err != nil {
		t.Fatalf("DeriveAgeIdentity(seedA) #1: %v", err)
	}
	a2, err := DeriveAgeIdentity(seedA, testInfoLabel)
	if err != nil {
		t.Fatalf("DeriveAgeIdentity(seedA) #2: %v", err)
	}
	if a1.RecipientString() == "" {
		t.Fatal("expected non-empty recipient string")
	}
	if a1.RecipientString() != a2.RecipientString() {
		t.Fatalf("same seed+info produced different recipients: %q vs %q", a1.RecipientString(), a2.RecipientString())
	}

	b, err := DeriveAgeIdentity(seedB, testInfoLabel)
	if err != nil {
		t.Fatalf("DeriveAgeIdentity(seedB): %v", err)
	}
	if b.RecipientString() == a1.RecipientString() {
		t.Fatal("different seeds produced the same recipient")
	}

	// A different seed's identity must not be able to decrypt the first
	// seed's ciphertext.
	ciphertext, err := a1.Encrypt([]byte("payload"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := b.Decrypt(ciphertext); err == nil {
		t.Fatal("expected decryption with an unrelated derived identity to fail, got success")
	}
}

// TestDeriveAgeIdentity_InfoDomainSeparation asserts the info label provides
// domain separation: the same seed with two different info labels must yield
// two independent, non-interoperable identities. This is what lets
// DeriveAgeIdentity be reused safely for other stable-identity needs derived
// from the same underlying secret without key reuse across purposes.
func TestDeriveAgeIdentity_InfoDomainSeparation(t *testing.T) {
	seed := []byte("shared-seed-used-for-two-purposes!!")

	a, err := DeriveAgeIdentity(seed, "purpose-a")
	if err != nil {
		t.Fatalf("DeriveAgeIdentity(purpose-a): %v", err)
	}
	b, err := DeriveAgeIdentity(seed, "purpose-b")
	if err != nil {
		t.Fatalf("DeriveAgeIdentity(purpose-b): %v", err)
	}
	if a.RecipientString() == b.RecipientString() {
		t.Fatal("different info labels on the same seed produced the same identity")
	}
}

// TestDeriveAgeIdentity_ValidAgeSecretKeyFormat asserts the derived identity's
// secret-key string is a well-formed age X25519 secret key: it round-trips
// through the upstream age.ParseX25519Identity parser (proving our
// self-contained bech32 encoder in bech32.go produces exactly what age
// expects) and the re-parsed identity's recipient matches the original.
func TestDeriveAgeIdentity_ValidAgeSecretKeyFormat(t *testing.T) {
	seed := []byte("another-restart-stable-seed-value!!")

	id, err := DeriveAgeIdentity(seed, testInfoLabel)
	if err != nil {
		t.Fatalf("DeriveAgeIdentity: %v", err)
	}

	secretKeyString := id.identity.String()
	if !strings.HasPrefix(secretKeyString, "AGE-SECRET-KEY-1") {
		t.Fatalf("secret key string = %q, want AGE-SECRET-KEY-1 prefix", secretKeyString)
	}

	reparsed, err := age.ParseX25519Identity(secretKeyString)
	if err != nil {
		t.Fatalf("age.ParseX25519Identity round-trip: %v", err)
	}
	if reparsed.Recipient().String() != id.RecipientString() {
		t.Fatalf("round-tripped identity recipient = %q, want %q", reparsed.Recipient().String(), id.RecipientString())
	}

	// And the round-tripped identity must actually decrypt what the
	// originally derived identity encrypted.
	ciphertext, err := id.Encrypt([]byte("round-trip-check"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	reparsedBox := &AgeIdentity{identity: reparsed, recipient: reparsed.Recipient()}
	decrypted, err := reparsedBox.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("round-tripped identity Decrypt: %v", err)
	}
	if string(decrypted) != "round-trip-check" {
		t.Fatalf("decrypted = %q, want %q", decrypted, "round-trip-check")
	}
}

// TestDeriveAgeIdentity_RejectsEmptyInputs asserts the function fails closed
// on missing seed or info rather than silently deriving from zero-value
// input (which would be a stable but predictable, non-secret key).
func TestDeriveAgeIdentity_RejectsEmptyInputs(t *testing.T) {
	if _, err := DeriveAgeIdentity(nil, testInfoLabel); err == nil {
		t.Fatal("expected error for empty seed, got nil")
	}
	if _, err := DeriveAgeIdentity([]byte("some-seed-material-of-decent-length"), ""); err == nil {
		t.Fatal("expected error for empty info label, got nil")
	}
}
