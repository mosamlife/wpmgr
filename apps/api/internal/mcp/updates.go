package mcp

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// ---------------------------------------------------------------------------
// fleet_updates_pending -- the second Tier 0 fleet read.
//
// The wireframe catalogue (S8) names it "Plugin, theme and core updates
// outstanding, per site" and files it under "Fleet reads -- answered from our
// database. No site is contacted." That is exactly what this is: it reads the
// same sites.components inventory document fleet_sites_list already reads, on
// the same bounded, scope-filtered page, and contacts nothing.
//
// ---------------------------------------------------------------------------
// IT SHARES THE DASHBOARD'S PREDICATES RATHER THAN RESTATING THEM.
//
// site.ActionableUpdate and site.ActionableCoreUpdate are the read-side
// predicates GET /sites/{id}/updates/available projects through, and their doc
// comments already name this surface as a consumer. Importing them is the whole
// point: an assistant that answered "which of my sites need updates?" from its
// own copy of the rule would count the GH #211 same-version phantoms and the
// agent's own plugin, both of which the dashboard drops -- so the assistant
// would contradict the screen the operator is looking at, and only one of those
// two gets noticed.
//
// The three rules that would otherwise drift, all of them already load-bearing
// on the dashboard side:
//
//   - an advisory with an empty new_version is not an update, it is a null
//     wearing a value's shape;
//   - an advisory naming the installed version is a phantom (GH #211);
//   - the WPMgr agent's own plugin is never offered as a selectable update,
//     matched on plugin-header name as well as slug so an agent installed under
//     an unexpected directory name is still recognised.
//
// ---------------------------------------------------------------------------
// STALENESS IS REPORTED, NOT REFUSED, AND THE CHOICE IS DELIBERATE.
//
// The envelope carries RefusalInventoryStale and StaleRefusal builds it, so
// refusing an old inventory was available and was considered. This tool does
// not do it, for three reasons:
//
//  1. THERE IS NO FRESHNESS WINDOW IN THIS CODEBASE TO KEY ON. No constant, no
//     column, no operator setting. A threshold invented here would be a number
//     nobody chose, and refusing a real fleet against it on day one is a worse
//     answer than a dated one.
//  2. A STALE INVENTORY CAN STILL ANSWER. It answers with data whose age is
//     stated. The site that genuinely cannot answer is the one the bounded page
//     did not return, and that site IS refused, by name, as site_unread.
//  3. IT WOULD DISAGREE WITH THE DASHBOARD, which renders the same inventory at
//     any age alongside an as_of stamp and never withholds it.
//
// So every record carries inventory_status, inventory_collected_at and
// inventory_age_seconds -- the same three fields, from the same helper, as
// fleet_sites_list -- and the prepended instructions tell the model in words
// that a count is only as fresh as its stamp and that never_collected is not
// zero. What is NOT done here, stated plainly rather than left to be
// discovered: no age threshold refuses, so a model that ignores the stamp can
// still report a year-old count as current. Wiring StaleRefusal needs a
// freshness window somebody chose, and that is an owner ruling, not a constant
// this file may invent.
// ---------------------------------------------------------------------------

// ToolFleetUpdatesPending is the wire name, taken verbatim from the S8
// catalogue so every fleet-wide read shares one prefix and one word order.
const ToolFleetUpdatesPending = "fleet_updates_pending"

// updatesPendingSchema takes no arguments, exactly as fleet_sites_list does.
// The tool answers over the connection's whole resolved scope; a site filter
// would be a second, weaker expression of the site scope the grant already
// carries, and a model that passed one would be choosing its own subset of a
// boundary it does not own.
var updatesPendingSchema = json.RawMessage(`{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`)

// pendingItem is one plugin or theme with an actionable update advisory.
type pendingItem struct {
	Slug string `json:"slug"`
	Name string `json:"name,omitempty"`
	// Active is carried because it is the operator's own triage order on the
	// dashboard: an inactive plugin with a pending update is a different
	// priority from an active one, and a model that cannot see the difference
	// will rank them the same.
	Active bool `json:"active"`
	// InstalledVersion may be empty when the agent reported no version. It is
	// left empty rather than filled with a placeholder: SameVersion fails open
	// on a blank side, so the advisory survived the phantom check precisely
	// because the installed version is unknown, and saying so is the honest
	// rendering.
	InstalledVersion string `json:"installed_version"`
	NewVersion       string `json:"new_version"`
}

// coreUpdateRecord is the site's own WordPress core advisory, present only when
// site.ActionableCoreUpdate accepts it.
type coreUpdateRecord struct {
	CurrentVersion string `json:"current_version"`
	NewVersion     string `json:"new_version"`
}

// siteUpdatesRecord is one site's outstanding update set as the model sees it.
type siteUpdatesRecord struct {
	SiteID string `json:"site_id"`
	Name   string `json:"name"`
	URL    string `json:"url"`

	// The staleness triplet, from the same helper fleet_sites_list uses, so the
	// two tools cannot disagree about when a site's inventory was collected.
	InventoryStatus      string  `json:"inventory_status"`
	InventoryCollectedAt *string `json:"inventory_collected_at"`
	InventoryAgeSeconds  *int64  `json:"inventory_age_seconds"`

	// PendingTotal counts plugins + themes + core (core contributes 1 when
	// present). It is the number the question "which of my sites need updates?"
	// is actually asking for, precomputed so a model does not have to add up
	// three lists and get it wrong.
	PendingTotal int `json:"pending_total"`

	Core    *coreUpdateRecord `json:"core_update"`
	Plugins []pendingItem     `json:"plugins"`
	Themes  []pendingItem     `json:"themes"`
}

// toSiteUpdatesRecord projects one row through the dashboard's own predicates.
//
// THE PARSE IS site.ParseInventoryComponents AND NOT A LOCAL ONE. A second
// decoder over the same document is a second answer to "what is installed on
// this site", and the two drift the moment either grows a key -- which is the
// reason that function exists as an exported parse over raw bytes in the first
// place.
func toSiteUpdatesRecord(row sqlc.Site, now time.Time) siteUpdatesRecord {
	rec := siteUpdatesRecord{
		SiteID:  row.ID.String(),
		Name:    row.Name,
		URL:     row.Url,
		Plugins: []pendingItem{},
		Themes:  []pendingItem{},
	}

	// Reuse fleet_sites_list's stamping wholesale rather than restating its
	// null branch. toSiteRecord is where the "never_collected is not a date"
	// reasoning lives, and a copy of it here would be a second place for that
	// rule to be got wrong.
	stamp := toSiteRecord(row, now)
	rec.InventoryStatus = stamp.InventoryStatus
	rec.InventoryCollectedAt = stamp.InventoryCollectedAt
	rec.InventoryAgeSeconds = stamp.InventoryAgeSeconds

	plugins, themes := site.ParseInventoryComponents(row.Components)
	for _, p := range plugins {
		if !site.ActionableUpdate(p) {
			continue
		}
		rec.Plugins = append(rec.Plugins, toPendingItem(p))
	}
	for _, t := range themes {
		if !site.ActionableUpdate(t) {
			continue
		}
		rec.Themes = append(rec.Themes, toPendingItem(t))
	}

	// Active before inactive, then slug, matching buildAvailableUpdates' order
	// within a kind so the assistant reads the fleet in the same priority the
	// dashboard displays it.
	sortPending(rec.Plugins)
	sortPending(rec.Themes)

	if core := site.ParseInventoryCoreUpdate(row.Components); site.ActionableCoreUpdate(core) {
		rec.Core = &coreUpdateRecord{
			CurrentVersion: core.CurrentVersion,
			NewVersion:     core.NewVersion,
		}
	}

	rec.PendingTotal = len(rec.Plugins) + len(rec.Themes)
	if rec.Core != nil {
		rec.PendingTotal++
	}
	return rec
}

func toPendingItem(c site.Component) pendingItem {
	return pendingItem{
		Slug:             c.Slug,
		Name:             c.Name,
		Active:           c.Active,
		InstalledVersion: c.Version,
		NewVersion:       c.AvailableUpdate.NewVersion,
	}
}

// sortPending orders active before inactive, then by slug, so repeated calls
// return the same order and a model comparing two answers sees real change
// rather than map iteration.
func sortPending(items []pendingItem) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Active != items[j].Active {
			return items[i].Active
		}
		return items[i].Slug < items[j].Slug
	})
}

// updatesPayload is the JSON body of a fleet_updates_pending result.
type updatesPayload struct {
	Sites []json.RawMessage `json:"sites"`

	// Totals is the fleet-level answer, computed over the records ACTUALLY
	// RETURNED and never over the rows read. See fleetUpdateTotals: a total
	// that counted truncated-away records would describe a list the caller
	// cannot see.
	Totals fleetTotals `json:"totals"`

	Envelope   Envelope       `json:"envelope"`
	Truncation truncationInfo `json:"truncation"`
}

// fleetTotals is the roll-up the question actually asks for.
type fleetTotals struct {
	// SitesWithUpdates counts returned sites whose pending_total is above zero.
	SitesWithUpdates int `json:"sites_with_updates"`
	// SitesNeverCollected counts returned sites we have never collected an
	// inventory for. It is reported SEPARATELY and never folded into
	// sites_with_updates or its complement, because "no updates pending" and
	// "we have never looked" are the two facts sites.components_updated_at is
	// nullable-with-no-backfill in order to keep apart.
	SitesNeverCollected int `json:"sites_never_collected"`
	PendingPlugins      int `json:"pending_plugins"`
	PendingThemes       int `json:"pending_themes"`
	PendingCore         int `json:"pending_core"`
	PendingTotal        int `json:"pending_total"`
}

// buildUpdatesPendingResult renders records into the tool result text, under
// the same byte cap, the same record-boundary truncation and the same prepended
// banner as fleet_sites_list. The reasoning for each of those is on
// buildListSitesResult and is not repeated here; what IS different is that the
// totals are computed over the kept records, below.
func buildUpdatesPendingResult(rows []sqlc.Site, env Envelope, now time.Time) (string, error) {
	available := len(rows)

	kept := make([]json.RawMessage, 0, len(rows))
	keptRecs := make([]siteUpdatesRecord, 0, len(rows))
	used := 0
	truncatedByBytes := false

	for _, row := range rows {
		rec := toSiteUpdatesRecord(row, now)
		enc, err := json.Marshal(rec)
		if err != nil {
			return "", fmt.Errorf("marshal site updates record %s: %w", row.ID, err)
		}
		cost := len(enc) + 1
		if used+cost > recordByteBudget {
			// CONTINUE, NOT BREAK, for the reason buildListSitesResult gives at
			// length: one oversized record must not suppress every record after
			// it, and sites.name is tenant-controlled.
			truncatedByBytes = true
			continue
		}
		kept = append(kept, enc)
		keptRecs = append(keptRecs, rec)
		used += cost
	}

	info := truncationInfo{
		Truncated:   truncatedByBytes,
		Returned:    len(kept),
		ByteCap:     recordByteBudget,
		Available:   &available,
		Explanation: "",
	}

	header := clampInstructions(updatesPendingInstructions)
	if truncatedByBytes {
		info.Explanation = truncationExplanation(len(kept), available)
		header = truncationBanner(info.Explanation) + "\n\n" + header
	}
	if env.Refused > 0 {
		header = truncationBanner(fmt.Sprintf(
			"PARTIAL RESULT: %d of your %d sites answered, %d refused. See envelope.refusals "+
				"for the reason and evidence for each. Do not present this as a complete answer.",
			env.OK, env.Asked, env.Refused)) + "\n\n" + header
	}

	payload, err := json.Marshal(updatesPayload{
		Sites:      kept,
		Totals:     fleetUpdateTotals(keptRecs),
		Envelope:   env,
		Truncation: info,
	})
	if err != nil {
		return "", fmt.Errorf("marshal fleet_updates_pending payload: %w", err)
	}
	return header + "\n\n" + string(payload), nil
}

// fleetUpdateTotals rolls the KEPT records up.
//
// IT TAKES THE KEPT RECORDS AND NOT THE ROWS, WHICH IS THE WHOLE CARE IN THIS
// FUNCTION. Totalling over `rows` would produce a summary describing sites the
// byte cap removed from the list -- "14 updates across 6 sites" over a payload
// showing 4 sites -- and the model's only way to notice would be to re-add the
// per-site numbers and find they disagree. A summary that silently covers more
// than the list under it is the same silent-partial failure the envelope
// exists to abolish, one level up, and it is the shape that survives review
// because both halves are individually correct.
func fleetUpdateTotals(recs []siteUpdatesRecord) fleetTotals {
	var t fleetTotals
	for _, r := range recs {
		if r.InventoryStatus == inventoryNeverCollected {
			t.SitesNeverCollected++
		}
		if r.PendingTotal > 0 {
			t.SitesWithUpdates++
		}
		t.PendingPlugins += len(r.Plugins)
		t.PendingThemes += len(r.Themes)
		if r.Core != nil {
			t.PendingCore++
		}
		t.PendingTotal += r.PendingTotal
	}
	return t
}

// updatesPendingInstructions is PREPENDED to every result. It shares the
// instruction budget with the truncation banner, so it stays short; the two
// sentences that earn their bytes are the never_collected one and the staleness
// one, because both are places a model would otherwise report a confident
// falsehood.
const updatesPendingInstructions = "Outstanding plugin, theme and WordPress core updates, per site. " +
	"Read-only: this connection cannot apply any of them.\n" +
	"Only sites this connection is scoped to are listed; an absent site is out of scope, not absent from the fleet.\n" +
	"pending_total 0 means no update is outstanding IN THE INVENTORY WE HOLD, which is only as current as " +
	"inventory_collected_at. Quote that date when you report a site as up to date.\n" +
	"inventory_status \"never_collected\" means no inventory has EVER been collected for that site: its counts are 0 " +
	"because we have not looked, NOT because it is up to date. Report those sites separately -- totals.sites_never_collected " +
	"counts them -- and never merge them with the up-to-date ones.\n" +
	"This list already excludes advisories the dashboard also drops: an advisory naming the installed version, and the " +
	"WPMgr agent's own plugin. Counts here match the dashboard's."
