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
// is worse than their absence.
//
// THE LIST ITSELF MOVED TO registry.go AT S7, along with what each tool
// requires and the single predicate tools/list and tools/call both resolve
// through. This file keeps the descriptor type, the schemas, and the rendering
// of the one read tool's result.
//
// Every tool here must be expressible at the FLOOR revision. Header-less
// clients are floor clients by definition, so a capability that exists only at
// 2025-11-25 would be unreachable for the clients we expect most of.
// ---------------------------------------------------------------------------

// ToolFleetSitesList is the one Phase 1 read tool: the fleet and its state.
//
// THE WIRE NAME IS fleet_sites_list, adopting the wireframe catalogue's naming
// so every fleet-wide tool shares one prefix and one word order. It is a
// BREAKING RENAME of list_sites and is deliberately not aliased: AuthorizeTool
// matches exactly and case-sensitively, so the old name now answers with the
// registry's ordinary "no tool named %q exists" refusal, which names
// tools/list. An alias would have been kinder to a client that hard-coded the
// old name and worse for every model afterwards, because two names for one
// tool is exactly the ambiguity the closed registry exists to prevent.
const ToolFleetSitesList = "fleet_sites_list"

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
// THE PARAMETER IS sqlc.Site AND NOT sqlc.ListSitesRow, and the swap was a type
// change rather than a rewrite. ListSitesRow is `sites.*` plus
// page_cache_enabled and object_cache_enabled, two LEFT JOIN columns for the
// dashboard's cache badges that this function has never read; sqlc.Site is
// `sites.*` alone. Every field below is present on both, which is why the body
// is untouched. ListSitesForMCPScope drops those two joins with the columns.
func toSiteRecord(row sqlc.Site, now time.Time) siteRecord {
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
	Truncated bool `json:"truncated"`
	Returned  int  `json:"returned"`
	// Available is the number of sites this request had to choose from, or
	// JSON null when the page bound was hit and the true total is therefore
	// unknown. It is a *int rather than a sentinel number because an unknown
	// total should be self-describing: a reader has to be told what -1 means,
	// but null already says it.
	Available   *int   `json:"available"`
	ByteCap     int    `json:"byte_cap"`
	Explanation string `json:"explanation,omitempty"`
}

// sitesPayload is the JSON body of a fleet_sites_list result.
type sitesPayload struct {
	Sites []json.RawMessage `json:"sites"`

	// Envelope is the typed partial-failure answer: how many of THIS
	// CONNECTION'S sites were asked, how many answered, and one entry per
	// refusal with its evidence. Every number in it is closed over the
	// caller's own scope -- see the counting rule in envelope.go.
	Envelope Envelope `json:"envelope"`

	Truncation truncationInfo `json:"truncation"`
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
func buildListSitesResult(rows []sqlc.Site, env Envelope, now time.Time) (string, error) {
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
			// CONTINUE, NOT BREAK, AND THAT IS A DELIBERATE CHOICE.
			//
			// `break` reads more naturally -- it yields a contiguous prefix,
			// "the first N sites by name" -- but it lets ONE oversized record
			// suppress every record after it. sites.name is tenant-controlled,
			// so an org that names one site with a very long string zeroes out
			// its own list_sites permanently, and because the list is ordered
			// by name that record keeps landing in the same place on every
			// call. A self-inflicted wound is still an outage.
			//
			// Skipping it and continuing costs the contiguity, which the
			// marker below states plainly ("omitted", never "cut at"), and buys
			// a tool that keeps working. Nothing downstream relies on the
			// result being a prefix.
			truncatedByBytes = true
			continue
		}
		kept = append(kept, enc)
		used += cost
	}

	// TRUNCATION HERE IS THE BYTE CAP AND ONLY THE BYTE CAP.
	//
	// This function used to also receive `more` -- the page bound the QUERY
	// hit -- and fold it into `truncated`. That value was a fact about the
	// TENANT's row count, computed before the caller's site scope was applied,
	// and reporting it to a site-scoped caller both misstated that caller's
	// result and disclosed the tenant's size. It is no longer passed in.
	//
	// The completeness question it was trying to answer is now answered by the
	// envelope, over the caller's own scope: an in-scope site that went unread
	// is an explicit site_unread refusal, counted in env.Refused. `rows` here
	// is already the in-scope set, so `available` counts only sites this
	// caller may see and never needs to be nulled out.
	truncated := truncatedByBytes

	info := truncationInfo{
		Truncated: truncated,
		Returned:  len(kept),
		ByteCap:   recordByteBudget,
		Available: &available,
	}

	// THE BANNER IS PREPENDED TO THE INSTRUCTIONS, AND THE CLAMP IS APPLIED TO
	// THE INSTRUCTIONS ALONE.
	//
	// Appending it and then clamping the whole header put the banner in the
	// exact position the clamp cuts from, so the first thing dropped under
	// budget pressure was the notice that the result is incomplete. That is
	// the same mistake as putting safety text in the tail, one level down.
	header := clampInstructions(listSitesInstructions)
	if truncated {
		info.Explanation = truncationExplanation(len(kept), available)
		header = truncationBanner(info.Explanation) + "\n\n" + header
	}

	// A REFUSAL IS ALSO AN INCOMPLETE RESULT, AND IT GETS THE SAME BANNER.
	// The byte cap and an unread site are different causes with the same
	// consequence for a model: the list in front of it is not the whole of
	// what it asked for. Only the byte-cap case previously said so.
	if env.Refused > 0 {
		header = truncationBanner(fmt.Sprintf(
			"PARTIAL RESULT: %d of your %d sites answered, %d refused. See envelope.refusals "+
				"for the reason and evidence for each. Do not present this as a complete answer.",
			env.OK, env.Asked, env.Refused)) + "\n\n" + header
	}

	// COMPACT, not indented. The budget above is measured on compact record
	// encodings, so indenting here would emit substantially more bytes than
	// the cap accounted for -- a byte cap that does not govern the bytes
	// actually sent is not a cap.
	payload, err := json.Marshal(sitesPayload{Sites: kept, Envelope: env, Truncation: info})
	if err != nil {
		return "", fmt.Errorf("marshal fleet_sites_list payload: %w", err)
	}

	return header + "\n\n" + string(payload), nil
}

// truncationExplanation states, in words the model will act on, exactly which
// bound was hit and that the view is incomplete.
// The page-bound arms are gone with the `more` parameter. Both of them
// described a bound measured over the TENANT and reported it to a site-scoped
// caller, which is the disclosure this change removes; the completeness
// question they answered badly is answered by the envelope instead.
func truncationExplanation(returned, available int) string {
	// "omitted", never "cut at": records are skipped individually when they do
	// not fit the remaining budget, so the result is a SUBSET of the sites this
	// connection may read and not a prefix of them. Saying "cut at" would imply
	// the missing sites are all the ones sorting after the last one shown.
	return fmt.Sprintf(
		"INCOMPLETE RESULT: %d of %d sites returned. The %d-byte result cap was reached, so "+
			"%d whole records were omitted. Records are omitted individually, so this is a "+
			"SUBSET of the sites you may read and not the first %d of them. Do not treat this "+
			"list as complete.",
		returned, available, recordByteBudget, available-returned, returned)
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
