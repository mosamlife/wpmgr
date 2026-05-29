package activity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"
)

// TestCanonicalMatchesContract pins the hash preimage to the SHARED WIRE
// CONTRACT byte-for-byte. The agent ships the identical canonicalization:
//
//	this_hash = sha256( prev_hash + "\n" + seq + "\n" + event_type + "\n" +
//	  object_type + "\n" + object_id + "\n" + actor_user_id + "\n" +
//	  occurred_at + "\n" + meta )
//
// meta is the VERBATIM agent-serialized JSON (wp_json_encode, insertion order).
// The CP hashes the raw bytes the agent shipped — it does NOT re-marshal a
// parsed map (which would sort keys + change escaping and diverge for any
// multi-key meta). This test feeds the raw bytes exactly as the agent emits
// them and asserts ComputeHash reproduces a hash over those exact bytes.
func TestCanonicalMatchesContract(t *testing.T) {
	occurred, _ := time.Parse(time.RFC3339, "2026-05-29T10:00:00Z")
	// Agent emits meta in INSERTION order: version first, then severity. A
	// parsed-map re-marshal in Go would sort to {"severity":...,"version":...}
	// — the exact divergence this fix prevents.
	rawMeta := `{"version":"5.3","severity":"medium"}`
	ev := IngestEvent{
		Seq:         1234,
		EventType:   "plugin.activated",
		ObjectType:  "plugin",
		ObjectID:    "akismet/akismet.php",
		ActorUserID: 1,
		Meta:        json.RawMessage(rawMeta),
		OccurredAt:  occurred,
	}

	preimage := GenesisPrevHash + "\n" +
		"1234" + "\n" +
		"plugin.activated" + "\n" +
		"plugin" + "\n" +
		"akismet/akismet.php" + "\n" +
		"1" + "\n" +
		"2026-05-29T10:00:00Z" + "\n" +
		rawMeta
	sum := sha256.Sum256([]byte(preimage))
	want := hex.EncodeToString(sum[:])

	got := ComputeHash(GenesisPrevHash, ev)
	if got != want {
		t.Fatalf("ComputeHash mismatch:\n got=%s\nwant=%s\npreimage=%q", got, want, preimage)
	}
}

// TestSingleKeyGoldenVector locks the agent's published golden vector #1
// (single-key meta {"version":"5.3"}) so the cross-language hash stays pinned.
func TestSingleKeyGoldenVector(t *testing.T) {
	occurred, _ := time.Parse(time.RFC3339, "2026-05-29T10:00:00Z")
	ev := IngestEvent{
		Seq:         1234,
		EventType:   "plugin.activated",
		ObjectType:  "plugin",
		ObjectID:    "akismet/akismet.php",
		ActorUserID: 1,
		Meta:        json.RawMessage(`{"version":"5.3"}`),
		OccurredAt:  occurred,
	}
	const want = "10e79f665f9bb0287e7389111c7390dcd31eddca29ac17ca368d337ab0ea9781"
	if got := ComputeHash(GenesisPrevHash, ev); got != want {
		t.Fatalf("golden vector mismatch:\n got=%s\nwant=%s", got, want)
	}
}

// TestEmptyMetaCanonicalizesToBraces asserts absent/empty/null meta hashes as
// "{}" (per the contract), not "null" or "".
func TestEmptyMetaCanonicalizesToBraces(t *testing.T) {
	if got := canonicalMetaRaw(nil); got != "{}" {
		t.Fatalf("nil meta: got %q want {}", got)
	}
	if got := canonicalMetaRaw([]byte{}); got != "{}" {
		t.Fatalf("empty meta: got %q want {}", got)
	}
	if got := canonicalMetaRaw([]byte("null")); got != "{}" {
		t.Fatalf("null meta: got %q want {}", got)
	}
	if got := canonicalMetaRaw([]byte(`{"a":1}`)); got != `{"a":1}` {
		t.Fatalf("non-empty meta should pass through: got %q", got)
	}
}

// TestSeverityFromMeta covers the alert-driving severity extraction + clamp,
// parsing from the raw meta bytes (the ingest source of truth).
func TestSeverityFromMeta(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"", SeverityLow},
		{"{}", SeverityLow},
		{`{"severity":"high"}`, SeverityHigh},
		{`{"severity":"medium"}`, SeverityMedium},
		{`{"severity":"bogus"}`, SeverityLow},
		{`{"severity":7}`, SeverityLow},
		{`{"version":"5.3","severity":"high"}`, SeverityHigh},
	}
	for _, c := range cases {
		if got := severityFromMeta([]byte(c.raw)); got != c.want {
			t.Errorf("severityFromMeta(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}
