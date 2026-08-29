package mcp

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// ---------------------------------------------------------------------------
// Phase 1 tool surface: TOOLS ONLY, and exactly one of them.
//
// Resources and prompts are out of scope for Phase 1 -- not refused on
// principle, but unbudgeted and undesigned, and shipping them half-specified
// is worse than their absence. The registry and its per-site policy filtering
// are S7; this file is a flat, positively-enumerated list of one.
//
// Every tool here must be expressible at the FLOOR revision. Header-less
// clients are floor clients by definition, so a capability that exists only at
// 2025-11-25 would be unreachable for the clients we expect most of.
// ---------------------------------------------------------------------------

// ToolListSites is the one Phase 1 read tool: the fleet and its state.
const ToolListSites = "list_sites"

// ---------------------------------------------------------------------------
// Byte caps (design §6, "Byte caps")
// ---------------------------------------------------------------------------

const (
	// instructionByteBudget is the SEPARATE budget for prepended instruction
	// text. It is separate on purpose: instruction text is prepended rather
	// than appended because the tail is what gets cut, and the tail is where
	// safety rules otherwise sit. Sharing one budget would let a large result
	// evict the instructions, which is the failure this split prevents.
	instructionByteBudget = 2048

	// recordByteBudget is the budget for the records themselves. Truncation
	// happens HERE and only here, at a record boundary, with an explicit
	// marker -- never mid-record, never silently.
	recordByteBudget = 30720
)

// ---------------------------------------------------------------------------
// Tool descriptors -- what tools/list returns
// ---------------------------------------------------------------------------

// ToolDescriptor is one entry in a tools/list response.
type ToolDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// listSitesSchema is returned by tools/list AND inline in an
// invalid-argument error, so a model corrects in one round trip instead of
// re-discovering the schema.
var listSitesSchema = json.RawMessage(`{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`)

// Tools returns the Phase 1 tool surface. It is a function returning a fresh
// slice rather than an exported package var so no caller can append a tool
// into the surface at runtime -- the read-only claim of this feature is
// exactly "no write tool is exposed", and that claim is only as strong as the
// list being closed.
func Tools() []ToolDescriptor {
	return []ToolDescriptor{{
		Name: ToolListSites,
		Description: "List the WordPress sites this connection may read, with their " +
			"connection state, health, WordPress/PHP/agent versions, and an explicit " +
			"inventory staleness stamp. Sites whose plugin/theme inventory has never " +
			"been collected are reported as never_collected rather than being given a " +
			"substitute date.",
		InputSchema: listSitesSchema,
	}}
}

// ---------------------------------------------------------------------------
// The site record, and the staleness stamp
// ---------------------------------------------------------------------------

// inventoryStatus values. These are the two DISTINGUISHABLE facts that
// sites.components_updated_at being nullable-with-no-backfill exists to keep
// apart.
const (
	// inventoryNeverCollected: components_updated_at IS NULL. We have never
	// collected this site's inventory. There is no date, and inventing one --
	// from updated_at, from last_seen_at, from enrolled_at, from now() -- would
	// report a fact about the fleet that is not true.
	inventoryNeverCollected = "never_collected"

	// inventoryCollected: components_updated_at is set, and CollectedAt is the
	// real control-plane instant of the last inventory push.
	inventoryCollected = "collected"
)

// siteRecord is one site as the model sees it.
type siteRecord struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	URL             string  `json:"url"`
	ConnectionState string  `json:"connection_state"`
	HealthStatus    string  `json:"health_status"`
	WPVersion       string  `json:"wp_version,omitempty"`
	PHPVersion      string  `json:"php_version,omitempty"`
	AgentVersion    string  `json:"agent_version,omitempty"`
	LastSeenAt      *string `json:"last_seen_at"`

	// InventoryStatus is ALWAYS present and is the field to branch on.
	InventoryStatus string `json:"inventory_status"`

	// InventoryCollectedAt is null exactly when InventoryStatus is
	// never_collected. It is a *string and not a string so that "we have never
	// collected this" serialises as JSON null and can never be confused with
	// the zero time or with an empty string that a reader might treat as a
	// formatting failure.
	InventoryCollectedAt *string `json:"inventory_collected_at"`

	// InventoryAgeSeconds is null on never_collected for the same reason. A
	// zero here would read as "collected just now", which is the exact
	// inversion of the truth.
	InventoryAgeSeconds *int64 `json:"inventory_age_seconds"`
}

// toSiteRecord maps one row, stamping staleness inline.
//
// THE NULL BRANCH IS THE POINT. sites.components_updated_at is nullable with
// no backfill precisely so that "we have never collected this" stays
// distinguishable from "collected at time T". There is deliberately no
// fallback to updated_at here: that fallback was removed in an earlier slice
// because a 60s heartbeat bumps updated_at without touching components, so
// using it dated an inventory that was never refreshed and reported a stale
// fleet as fresh.
func toSiteRecord(row sqlc.ListSitesRow, now time.Time) siteRecord {
	rec := siteRecord{
		ID:              row.ID.String(),
		Name:            row.Name,
		URL:             row.Url,
		ConnectionState: row.ConnectionState,
		HealthStatus:    row.HealthStatus,
		WPVersion:       row.WpVersion,
		PHPVersion:      row.PhpVersion,
		AgentVersion:    row.AgentVersion,
		LastSeenAt:      timestampOrNil(row.LastSeenAt),
	}

	if !row.ComponentsUpdatedAt.Valid {
		rec.InventoryStatus = inventoryNeverCollected
		rec.InventoryCollectedAt = nil
		rec.InventoryAgeSeconds = nil
		return rec
	}

	at := row.ComponentsUpdatedAt.Time.UTC().Format(time.RFC3339)
	age := int64(now.Sub(row.ComponentsUpdatedAt.Time).Seconds())
	if age < 0 {
		// A stamp in the future is a clock fact, not an age. Report the
		// instant and withhold the age rather than emitting a negative number
		// the model would have to interpret.
		rec.InventoryStatus = inventoryCollected
		rec.InventoryCollectedAt = &at
		return rec
	}
	rec.InventoryStatus = inventoryCollected
	rec.InventoryCollectedAt = &at
	rec.InventoryAgeSeconds = &age
	return rec
}

// timestampOrNil renders a nullable timestamp as RFC3339 or JSON null. It
// never substitutes a zero time for an absent one.
func timestampOrNil(ts pgtype.Timestamptz) *string {
	if !ts.Valid {
		return nil
	}
	s := ts.Time.UTC().Format(time.RFC3339)
	return &s
}

// ---------------------------------------------------------------------------
// Result assembly, byte cap, and the truncation marker
// ---------------------------------------------------------------------------

// truncationInfo is the machine-readable half of the marker.
type truncationInfo struct {
	Truncated  bool   `json:"truncated"`
	Returned   int    `json:"returned"`
	Available  int    `json:"available"`
	ByteCap    int    `json:"byte_cap"`
	Explanation string `json:"explanation,omitempty"`
}

// sitesPayload is the JSON body of a list_sites result.
type sitesPayload struct {
	Sites     []json.RawMessage `json:"sites"`
	Truncation truncationInfo   `json:"truncation"`
}

// buildListSitesResult renders rows into the tool result text.
//
// Two rules from the design are implemented here and they interact:
//
//  1. TRUNCATION IS AT A RECORD BOUNDARY WITH AN EXPLICIT MARKER. A record is
//     marshalled whole and then either fits the remaining budget or is not
//     included at all. A half-serialised record is never emitted, and a result
//     that dropped records never reads as complete.
//
//  2. INSTRUCTION TEXT IS PREPENDED AND HAS ITS OWN BUDGET. The truncation
//     marker is written into that prepended header as well as into the JSON,
//     because the tail is what gets cut: a marker that lives only at the end of
//     the payload is in the exact position that a downstream context-window
//     trim removes, which would turn a visibly-truncated result back into a
//     silently-truncated one.
func buildListSitesResult(rows []sqlc.ListSitesRow, more bool, now time.Time) (string, error) {
	available := len(rows)

	kept := make([]json.RawMessage, 0, len(rows))
	used := 0
	truncatedByBytes := false

	for _, row := range rows {
		enc, err := json.Marshal(toSiteRecord(row, now))
		if err != nil {
			// A record that cannot be marshalled is a bug, not a reason to
			// drop it quietly and return a short list that reads as complete.
			return "", fmt.Errorf("marshal site record %s: %w", row.ID, err)
		}
		// +1 for the comma this record costs inside the array.
		cost := len(enc) + 1
		if used+cost > recordByteBudget {
			truncatedByBytes = true
			break
		}
		kept = append(kept, enc)
		used += cost
	}

	// `more` is truncation the QUERY imposed (the page bound was reached with
	// rows still unread); truncatedByBytes is truncation the BYTE CAP imposed.
	// Both are truncation and both must be reported -- reporting only the byte
	// cap would let a page-bounded result read as the whole fleet.
	truncated := truncatedByBytes || more

	info := truncationInfo{
		Truncated: truncated,
		Returned:  len(kept),
		ByteCap:   recordByteBudget,
		Available: available,
	}
	header := listSitesInstructions
	if truncated {
		info.Explanation = truncationExplanation(len(kept), available, more, truncatedByBytes)
		header += "\n\n" + truncationBanner(info.Explanation)
	}
	if more {
		// `available` counts only what this page read, so it would understate
		// the fleet. Report it as unknown rather than as a total.
		info.Available = -1
	}

	// COMPACT, not indented. The budget above is measured on compact record
	// encodings, so indenting here would emit substantially more bytes than
	// the cap accounted for -- a byte cap that does not govern the bytes
	// actually sent is not a cap.
	payload, err := json.Marshal(sitesPayload{Sites: kept, Truncation: info})
	if err != nil {
		return "", fmt.Errorf("marshal list_sites payload: %w", err)
	}

	return clampInstructions(header) + "\n\n" + string(payload), nil
}

// truncationExplanation states, in words the model will act on, exactly which
// bound was hit and that the view is incomplete.
func truncationExplanation(returned, available int, more, byBytes bool) string {
	switch {
	case more && byBytes:
		return fmt.Sprintf(
			"INCOMPLETE RESULT: %d sites returned. Both the %d-byte result cap and the "+
				"per-request page bound were reached, so an unknown number of further "+
				"sites were not returned. Do not treat this list as the whole fleet.",
			returned, recordByteBudget)
	case more:
		return fmt.Sprintf(
			"INCOMPLETE RESULT: %d sites returned. The per-request page bound was reached, "+
				"so an unknown number of further sites were not returned. Do not treat this "+
				"list as the whole fleet.",
			returned)
	default:
		return fmt.Sprintf(
			"INCOMPLETE RESULT: %d of %d sites returned. The %d-byte result cap was reached "+
				"and the list was cut at a whole-record boundary; %d sites were not returned. "+
				"Do not treat this list as the whole fleet.",
			returned, available, recordByteBudget, available-returned)
	}
}

// truncationBanner makes the marker impossible to skim past.
func truncationBanner(explanation string) string {
	return "!! " + explanation
}

// listSitesInstructions is PREPENDED to every list_sites result. It is short
// on purpose: it shares a fixed budget with the truncation banner, and the
// banner must never be the thing that gets dropped.
const listSitesInstructions = "Fleet inventory, read-only. This connection cannot modify any site.\n" +
	"Only sites this connection is scoped to are listed; an absent site is out of scope, not absent from the fleet.\n" +
	"inventory_status is authoritative: \"never_collected\" means no plugin/theme inventory has EVER been collected " +
	"for that site and inventory_collected_at is null. Do not treat a never_collected site as up to date, and do not " +
	"substitute last_seen_at for it -- they measure different things."

// clampInstructions enforces the instruction budget. It cuts on a rune
// boundary and marks the cut, so a clipped header cannot be mistaken for the
// whole of the instructions.
func clampInstructions(s string) string {
	if len(s) <= instructionByteBudget {
		return s
	}
	const marker = "\n[instructions truncated]"
	limit := instructionByteBudget - len(marker)
	if limit < 0 {
		return marker
	}
	b := []byte(s[:limit])
	// Back off to a rune boundary so the cut never emits a partial UTF-8
	// sequence.
	for len(b) > 0 && b[len(b)-1]&0xC0 == 0x80 {
		b = b[:len(b)-1]
	}
	if len(b) > 0 && b[len(b)-1]&0x80 != 0 {
		b = b[:len(b)-1]
	}
	return string(b) + marker
}
