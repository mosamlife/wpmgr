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
//
// AUTHORITATIVE JWT CLAIM SET (the agent verifies these byte-for-byte; see
// jwt.go for how they are minted). JOSE header is {"alg":"EdDSA","typ":"JWT"};
// the signature is Ed25519 over the ASCII "base64url(header).base64url(payload)"
// signing input with the control-plane private key, base64url no-pad. Payload:
//
//	jti  string  fresh random 128-bit value, lowercase hex (32 chars). Single-use
//	             within the exp window (agent anti-replay).
//	exp  number  Unix seconds. now < exp <= now+60. (CP mints now+45s.)
//	iat  number  Unix seconds the token was minted.
//	iss  string  "wpmgr-control-plane" (provenance; informational).
//	aud  string  the TARGET site's canonical lowercase UUID string. The agent
//	             MUST reject unless aud == its own enrollment site_id. This binds
//	             the token to one site and defeats cross-tenant replay under the
//	             single global CP signing keypair.
//	cmd  string  the dispatched command name, exactly "update" or "rollback".
//	             The agent MUST reject unless cmd == the command path segment it
//	             is serving. This defeats cross-command reuse of a captured token.

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

// RefreshInventoryRequest is the POST body for the `refresh_inventory` command.
// The Updates feature has no per-call parameters: the agent re-reads its plugin/
// theme inventory and the WP update_* transients, then pushes the result back
// over /agent/v1/metadata. The struct is reserved for future flags.
type RefreshInventoryRequest struct{}

// RefreshInventoryResponse is the agent's response to the `refresh_inventory`
// command.
//
//	ok      whether the agent accepted the refresh (it sends metadata asynchronously
//	        via /agent/v1/metadata; this is just the command ack).
//	detail  short human-readable detail (e.g. "queued", "applied").
type RefreshInventoryResponse struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// DiagnosticsRequest is the POST body for the `diagnostics` command (ADR-037
// Sprint 2 on-demand refresh). The agent's DiagnosticsCommand takes no params —
// every probe is unconditional — so the struct is intentionally empty. Reserved
// for future flags (e.g. category whitelist). The agent returns the full
// 14-category payload SYNCHRONOUSLY in the 200 body; callers consume the raw
// JSON body via Client.Diagnostics so the existing diagnostics.Service ingester
// (which splits the blob into one row per category) stays byte-for-byte the
// same as the daily cron-push path.
type DiagnosticsRequest struct{}
