package activity

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// buildChain assembles a valid 3-event hash chain the way the agent ships it:
// each event's prev_hash is the prior event's this_hash (genesis for the
// first), and this_hash is the canonical sha256 over its own fields. The stored
// Event mirror is what VerifyChain folds over.
func buildChain(t *testing.T) []Event {
	t.Helper()
	tenant := uuid.New()
	siteID := uuid.New()
	base, _ := time.Parse(time.RFC3339, "2026-05-29T10:00:00Z")

	// Raw meta bytes exactly as the agent emits them (insertion order, compact).
	specs := []IngestEvent{
		{Seq: 1, EventType: "plugin.activated", ObjectType: "plugin", ObjectID: "akismet/akismet.php", ActorUserID: 1, Meta: json.RawMessage(`{"version":"5.3","severity":"medium"}`)},
		{Seq: 2, EventType: "user.login", ObjectType: "user", ObjectID: "admin", ActorUserID: 1, Meta: json.RawMessage(`{"ip":"203.0.113.4","severity":"low"}`)},
		{Seq: 3, EventType: "core.updated", ObjectType: "core", ObjectID: "", ActorUserID: 1, Meta: json.RawMessage(`{"from":"6.4","to":"6.5","severity":"high"}`)},
	}

	rows := make([]Event, 0, len(specs))
	prev := GenesisPrevHash
	for i := range specs {
		specs[i].OccurredAt = base.Add(time.Duration(i) * time.Minute)
		specs[i].PrevHash = prev
		specs[i].ThisHash = ComputeHash(prev, specs[i])
		rows = append(rows, Event{
			TenantID:    tenant,
			SiteID:      siteID,
			Seq:         specs[i].Seq,
			EventType:   specs[i].EventType,
			ObjectType:  specs[i].ObjectType,
			ObjectID:    specs[i].ObjectID,
			ActorUserID: specs[i].ActorUserID,
			Meta:        parseMeta(specs[i].Meta),
			MetaRaw:     canonicalMetaRaw(specs[i].Meta),
			Severity:    severityFromMeta(specs[i].Meta),
			PrevHash:    specs[i].PrevHash,
			ThisHash:    specs[i].ThisHash,
			ChainValid:  true,
			OccurredAt:  specs[i].OccurredAt,
		})
		prev = specs[i].ThisHash
	}
	return rows
}

// TestVerifyIntactChain confirms a well-formed chain verifies clean.
func TestVerifyIntactChain(t *testing.T) {
	rows := buildChain(t)
	res := VerifyChain(rows)
	if !res.Valid {
		t.Fatalf("expected valid chain, got break at %v", res.BreakAtSeq)
	}
	if res.Total != 3 {
		t.Fatalf("expected total 3, got %d", res.Total)
	}
	if res.BreakAtSeq != nil {
		t.Fatalf("expected no break, got seq %d", *res.BreakAtSeq)
	}
}

// TestVerifyDetectsMiddleTamper is the headline tamper-evidence test: build a
// valid 3-event chain, mutate the MIDDLE event's meta in place (leaving its
// stored this_hash untouched, exactly as a row-level DB mutation would), and
// assert Verify reports a break at the tampered seq (2). The recomputed hash of
// the mutated row no longer matches its stored this_hash, so the fold trips at
// seq 2 — before reaching seq 3.
func TestVerifyDetectsMiddleTamper(t *testing.T) {
	rows := buildChain(t)

	// Tamper: change seq 2's authoritative meta bytes (MetaRaw — the hash
	// preimage source, what a real DB-row attacker would have to edit to forge
	// the record). The stored this_hash is now stale relative to the mutated
	// MetaRaw, so the fold trips at seq 2.
	rows[1].MetaRaw = `{"ip":"10.0.0.1","severity":"low"}`

	res := VerifyChain(rows)
	if res.Valid {
		t.Fatal("expected tampered chain to be reported invalid")
	}
	if res.BreakAtSeq == nil {
		t.Fatal("expected a break seq, got nil")
	}
	if *res.BreakAtSeq != 2 {
		t.Fatalf("expected break at seq 2 (the tampered row), got %d", *res.BreakAtSeq)
	}
}

// TestVerifyDetectsBrokenPrevHash covers the other break condition: a row whose
// prev_hash does not match the prior link (e.g. a deleted/reordered row).
func TestVerifyDetectsBrokenPrevHash(t *testing.T) {
	rows := buildChain(t)
	// Snip the first event so seq 2's prev_hash no longer matches genesis.
	res := VerifyChain(rows[1:])
	if res.Valid {
		t.Fatal("expected chain with a missing genesis link to be invalid")
	}
	if res.BreakAtSeq == nil || *res.BreakAtSeq != 2 {
		t.Fatalf("expected break at seq 2, got %v", res.BreakAtSeq)
	}
}
