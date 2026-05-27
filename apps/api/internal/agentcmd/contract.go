package agentcmd

// This file is the AUTHORITATIVE CP->agent command contract for the M3 bulk
// update feature. The wp-agent-engineer mirrors these shapes in
// apps/agent/includes/commands/class-update-command.php and a new
// class-rollback-command.php. Field names are JSON wire names; do not rename
// without updating both sides.
//
// Transport: POST {site_url}/wp-json/wpmgr/v1/command/{command}
//   command ∈ {"update", "rollback"}
//   Header:  Authorization: Bearer <minted EdDSA JWT>   (see jwt.go)
//   Body:    application/json — the request structs below.
//   Response: 200 with the response structs below; non-200 ⇒ command failed.

// TargetType identifies what an update item targets.
const (
	TargetPlugin = "plugin"
	TargetTheme  = "theme"
	TargetCore   = "core"
)

// CoreSlug is the canonical target_slug used for the WordPress core target
// (there is no plugin/theme slug for core).
const CoreSlug = "core"

// UpdateItem is one thing to update on the site.
//
//	type      "plugin" | "theme" | "core"
//	slug      plugin/theme slug; "core" for the core target.
//	version   desired version: "latest" or an explicit pin (e.g. "6.5.2").
type UpdateItem struct {
	Type    string `json:"type"`
	Slug    string `json:"slug"`
	Version string `json:"version"`
}

// UpdateRequest is the POST body for the `update` command.
//
//	dry_run   true ⇒ the agent MUST NOT mutate the site. It reports what WOULD
//	          change (current vs available version) per item and nothing else.
//	snapshot  true ⇒ before mutating each item the agent takes a local
//	          pre-update snapshot enabling a later `rollback` (M3 local snapshot;
//	          full backup integration is M4). Ignored when dry_run is true.
//	items     the items to update.
type UpdateRequest struct {
	DryRun   bool         `json:"dry_run"`
	Snapshot bool         `json:"snapshot"`
	Items    []UpdateItem `json:"items"`
}

// ItemResult is the agent's per-item outcome.
//
//	type/slug   echo the requested item.
//	from_version the version present BEFORE the update (or current version on a
//	             dry run).
//	to_version   the version present AFTER the update (the available version on a
//	             dry run; equals from_version when nothing would change).
//	status       "succeeded" | "failed" | "skipped" | "would_update" (dry run) |
//	             "up_to_date".
//	snapshot_id  opaque token the agent returns when it took a pre-update
//	             snapshot; the CP echoes it back in a rollback command.
//	log          short human-readable detail (WP-CLI output tail / error text).
type ItemResult struct {
	Type        string `json:"type"`
	Slug        string `json:"slug"`
	FromVersion string `json:"from_version"`
	ToVersion   string `json:"to_version"`
	Status      string `json:"status"`
	SnapshotID  string `json:"snapshot_id,omitempty"`
	Log         string `json:"log,omitempty"`
}

// Item result status values (agent -> CP).
const (
	ItemSucceeded   = "succeeded"
	ItemFailed      = "failed"
	ItemSkipped     = "skipped"
	ItemUpToDate    = "up_to_date"
	ItemWouldUpdate = "would_update" // dry-run only
)

// UpdateResponse is the agent's response to the `update` command.
//
//	ok       overall success of the command dispatch (not of every item).
//	results  per-item outcomes, parallel to the request items.
type UpdateResponse struct {
	OK      bool         `json:"ok"`
	Results []ItemResult `json:"results"`
}

// RollbackRequest is the POST body for the `rollback` command. It asks the agent
// to restore a single item to its pre-update snapshot (or, for core, to a known
// prior version).
//
//	type/slug    the item to roll back.
//	snapshot_id  the token returned in the prior update's ItemResult, when the
//	             agent took a snapshot.
//	to_version   the version to restore to (the recorded from_version); used for
//	             core and as a fallback when no snapshot_id is available.
type RollbackRequest struct {
	Type       string `json:"type"`
	Slug       string `json:"slug"`
	SnapshotID string `json:"snapshot_id,omitempty"`
	ToVersion  string `json:"to_version"`
}

// RollbackResponse is the agent's response to the `rollback` command.
//
//	ok            whether the rollback succeeded.
//	restored_version the version present after rollback.
//	log           short human-readable detail.
type RollbackResponse struct {
	OK              bool   `json:"ok"`
	RestoredVersion string `json:"restored_version"`
	Log             string `json:"log,omitempty"`
}
