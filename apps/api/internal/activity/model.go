// Package activity implements the CP side of the ADR-037 Sprint 3 WordPress
// activity log: ingest of the agent-shipped, hash-chained event stream, a
// server-side re-verification of that chain (tamper-evidence — neither WPvivid
// nor WPRemote ships this), tenant-scoped listing with filters, and a wiring
// hook into the existing uptime alert Dispatcher for high-severity security
// events.
//
// The hash chain is the load-bearing piece. The agent ships each event with a
// prev_hash + this_hash; the CP recomputes this_hash from the event fields and
// the PRIOR stored row's this_hash and marks a per-row chain_valid boolean. A
// mutated, inserted, or deleted historical row breaks the recomputation and is
// surfaced by Verify. The canonicalization MUST match the agent byte-for-byte
// (see Canonical / ComputeHash below and the SHARED WIRE CONTRACT in ADR-037).
package activity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// GenesisPrevHash is the prev_hash of the first event in a chain: 64 zero chars.
const GenesisPrevHash = "0000000000000000000000000000000000000000000000000000000000000000"

// Severity values. meta.severity drives the alert decision; "high" is the only
// level that can fire a security alert.
const (
	SeverityHigh   = "high"
	SeverityMedium = "medium"
	SeverityLow    = "low"
)

// IngestEvent is one event as shipped by the agent in the POST /agent/v1/activity
// body. Field names and JSON tags match the SHARED WIRE CONTRACT exactly.
type IngestEvent struct {
	Seq         int64          `json:"seq"`
	EventType   string         `json:"event_type"`
	ObjectType  string         `json:"object_type"`
	ObjectID    string         `json:"object_id"`
	ObjectLabel string         `json:"object_label"`
	ActorUserID int64          `json:"actor_user_id"`
	ActorLogin  string         `json:"actor_login"`
	ActorIP     string         `json:"actor_ip"`
	Summary     string          `json:"summary"`
	// Meta is captured as RAW JSON bytes — NOT a map — so the hash preimage
	// uses the EXACT bytes the agent serialized. The agent computes this_hash
	// over wp_json_encode($meta), which (a) preserves insertion order and
	// (b) escapes slashes + unicode per PHP's defaults. Go's json.Marshal of a
	// map sorts keys and HTML-escapes <>& — so re-marshalling a parsed map
	// would diverge byte-for-byte for any multi-key meta (e.g. the universal
	// {"version":...,"severity":...}) and falsely flag every event as a chain
	// break. Hashing the verbatim wire bytes sidesteps all cross-language
	// JSON-encoder differences.
	Meta       json.RawMessage `json:"meta"`
	PrevHash   string          `json:"prev_hash"`
	ThisHash   string          `json:"this_hash"`
	OccurredAt time.Time       `json:"occurred_at"`
}

// IngestRequest is the POST /agent/v1/activity body.
type IngestRequest struct {
	Events        []IngestEvent `json:"events"`
	ChainStartSeq int64         `json:"chain_start_seq"`
	AgentVersion  string        `json:"agent_version"`
}

// Event is a stored activity row (operator-facing projection).
type Event struct {
	ID          int64
	TenantID    uuid.UUID
	SiteID      uuid.UUID
	Seq         int64
	EventType   string
	ObjectType  string
	ObjectID    string
	ObjectLabel string
	ActorUserID int64
	ActorLogin  string
	ActorIP     string
	Summary     string
	// Meta is the parsed map for operator-facing display + querying (stored in
	// the meta JSONB column). MetaRaw is the verbatim agent-serialized bytes
	// used for hash re-verification (stored in the meta_raw TEXT column). The
	// two are distinct ON PURPOSE: Meta is convenient, MetaRaw is correct.
	Meta       map[string]any
	MetaRaw    string
	Severity   string
	PrevHash   string
	ThisHash   string
	ChainValid bool
	OccurredAt time.Time
	ReceivedAt time.Time
}

// ListFilter narrows ListActivity. Zero-value fields are ignored.
type ListFilter struct {
	EventType  string
	ObjectType string
	ActorLogin string
	Severity   string
	Since      time.Time
	Until      time.Time
	Limit      int
	Offset     int
}

// VerifyResult is the outcome of a full server-side chain re-verification.
type VerifyResult struct {
	Valid      bool   `json:"valid"`
	BreakAtSeq *int64 `json:"break_at_seq"`
	Total      int    `json:"total"`
}

// occurredAtCanonical formats the occurred_at exactly as the agent does in the
// hash preimage. The agent ships RFC3339 in UTC ("2026-05-29T10:00:00Z"); we
// must reproduce the SAME string the agent hashed, which is the verbatim wire
// value. ComputeHashFromWire uses the raw wire string; this helper is the
// fallback for the re-verify path where we only have the parsed time.
func occurredAtCanonical(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z07:00")
}

// canonicalMetaRaw returns the meta JSON exactly as the agent hashed it. The
// agent's hash preimage uses wp_json_encode($meta) verbatim; the same bytes
// arrive in the request body and are captured as json.RawMessage, so we hash
// them as-is. Empty / absent / JSON-null meta canonicalizes to "{}" — matching
// the agent, which encodes an empty meta as the object literal "{}" (it casts
// the empty array to stdClass before encoding). We do NOT re-marshal: that
// would re-sort keys + change escaping and break the cross-language chain.
func canonicalMetaRaw(raw []byte) string {
	s := string(raw)
	if s == "" || s == "null" {
		return "{}"
	}
	return s
}

// Canonical builds the deterministic hash preimage for an event, given the
// prior link's hash. The field order + separator MUST match the agent
// canonicalization byte-for-byte:
//
//	sha256( prev_hash + "\n" + seq + "\n" + event_type + "\n" + object_type +
//	        "\n" + object_id + "\n" + actor_user_id + "\n" + occurred_at +
//	        "\n" + json_meta )
//
// occurredAt is the canonical occurred_at string (RFC3339 UTC, as shipped) and
// metaJSON is the compact JSON of the meta object ("{}" when empty).
func Canonical(prevHash string, seq int64, eventType, objectType, objectID string, actorUserID int64, occurredAt, metaJSON string) []byte {
	s := prevHash + "\n" +
		strconv.FormatInt(seq, 10) + "\n" +
		eventType + "\n" +
		objectType + "\n" +
		objectID + "\n" +
		strconv.FormatInt(actorUserID, 10) + "\n" +
		occurredAt + "\n" +
		metaJSON
	return []byte(s)
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ComputeHash recomputes this_hash for a stored/ingest event given the prior
// link's hash. Used at ingest (against the prior stored row) and at Verify
// (folding the chain). occurredAt is canonicalized from the parsed timestamp.
func ComputeHash(prevHash string, e IngestEvent) string {
	return hashHex(Canonical(
		prevHash,
		e.Seq,
		e.EventType,
		e.ObjectType,
		e.ObjectID,
		e.ActorUserID,
		occurredAtCanonical(e.OccurredAt),
		canonicalMetaRaw(e.Meta),
	))
}

// ComputeHashFromStored recomputes this_hash for a stored Event row, hashing
// the verbatim MetaRaw bytes the agent shipped (NOT the parsed Meta map).
func ComputeHashFromStored(prevHash string, e Event) string {
	return hashHex(Canonical(
		prevHash,
		e.Seq,
		e.EventType,
		e.ObjectType,
		e.ObjectID,
		e.ActorUserID,
		occurredAtCanonical(e.OccurredAt),
		canonicalMetaRaw([]byte(e.MetaRaw)),
	))
}

// VerifyChain folds a seq-ASC ordered slice of stored events from genesis,
// recomputing each this_hash against the prior link, and reports the first
// broken link. A row is a break when its prev_hash != the prior hash OR its
// recomputed hash != its stored this_hash. This is the pure core of
// Service.Verify (DB-free, so it is directly unit-testable).
func VerifyChain(rows []Event) VerifyResult {
	res := VerifyResult{Valid: true, Total: len(rows)}
	prev := GenesisPrevHash
	for _, row := range rows {
		recomputed := ComputeHashFromStored(prev, row)
		if row.PrevHash != prev || recomputed != row.ThisHash {
			seq := row.Seq
			res.Valid = false
			res.BreakAtSeq = &seq
			return res
		}
		prev = row.ThisHash
	}
	return res
}

// severityFromMeta extracts meta.severity from the raw meta bytes, defaulting
// to "low" and clamping to the three known levels. Parses the raw (rather than
// taking the typed map) so the ingest path has a single source of truth for
// meta — the bytes the agent shipped.
func severityFromMeta(raw []byte) string {
	if len(raw) == 0 {
		return SeverityLow
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return SeverityLow
	}
	v, ok := m["severity"].(string)
	if !ok {
		return SeverityLow
	}
	switch v {
	case SeverityHigh, SeverityMedium, SeverityLow:
		return v
	default:
		return SeverityLow
	}
}

// parseMeta unmarshals the raw meta into a map for the display/query JSONB
// column. Returns an empty map on null/empty/invalid so the stored row always
// has a well-formed meta object.
func parseMeta(raw []byte) map[string]any {
	m := map[string]any{}
	if len(raw) == 0 {
		return m
	}
	_ = json.Unmarshal(raw, &m)
	return m
}
