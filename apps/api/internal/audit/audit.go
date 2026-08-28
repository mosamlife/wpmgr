// Package audit implements an append-only, per-tenant hash-chained audit log.
// Each entry's hash chains to the previous entry's hash for the same tenant, so
// any insertion, deletion, or mutation of a historical row breaks the chain and
// is detectable by Verify. The table grants revoke UPDATE/DELETE from the app
// role, making the log append-only at the privilege level too.
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Common actor types and actions recorded in the log.
const (
	ActorUser   = "user"
	ActorAPIKey = "api_key"
	ActorSystem = "system"

	ActionLoginSuccess = "auth.login.success"
	ActionLoginFailure = "auth.login.failure"
	ActionLogout       = "auth.logout"
	ActionRegister     = "auth.register"
	ActionOIDCLogin    = "auth.oidc.login"

	// Sign-in methods added to or removed from an account, from the connected
	// accounts settings card. Recorded because a change to how an account can
	// be signed in to is the change an account owner most needs to be able to
	// review after the fact. Metadata: provider (unlink only).
	ActionIdentityUnlinked = "auth.identity.unlinked"
	ActionPasswordSet      = "auth.password.set"

	// The other half of the same story, from the social path. These are NOT
	// logins and must not be filed as one: linking binds a new credential to an
	// existing account, registering mints an account from a provider assertion,
	// and an issuer move changes what a stored credential means. All three were
	// recorded as ActionOIDCLogin, so the log rendered a credential change as
	// "Signed in with SSO" and the only thing distinguishing them lived inside
	// the metadata, where no filter and no reader looks first. An account owner
	// scanning for "did someone attach themselves to my account" saw a list of
	// ordinary sign-ins. Metadata: provider, event, and from_issuer/to_issuer on
	// the move.
	ActionSocialLinked        = "auth.social.linked"
	ActionSocialRegistered    = "auth.social.registered"
	ActionIdentityIssuerMoved = "auth.identity.issuer_moved"
	ActionIdentityAdopted     = "auth.identity.adopted"

	ActionMemberAdd    = "member.add"
	ActionMemberUpdate = "member.update"
	ActionMemberRemove = "member.remove"
	ActionAPIKeyCreate = "apikey.create"
	ActionAPIKeyRevoke = "apikey.revoke"
	ActionSiteCreate   = "site.create"
	ActionSiteDelete   = "site.delete"
	ActionTenantCreate = "tenant.create"

	ActionSiteEnrolled       = "site.enrolled"
	ActionPairingCodeCreated = "pairing_code.created"
	ActionSiteTagsSet        = "site.tags.set"

	// GH #230 "rich tags" — tenant-level tag registry (internal/sitetag).
	ActionTagCreate    = "tag.create"
	ActionTagUpdate    = "tag.update"
	ActionTagMerge     = "tag.merge"
	ActionTagDelete    = "tag.delete"
	ActionTagBulkApply = "tag.bulk_apply"

	// Phase 5.7 connection lifecycle (ADR-041). Every connection-state
	// transition records one of these hash-chained actions alongside the
	// site_connection_history row. The system-driven transitions
	// (connected/degraded/disconnected) are recorded with ActorSystem; the
	// operator actions (revoked/archived/restored/reenrolled) with ActorUser.
	ActionSiteConnected    = "site.connected"
	ActionSiteDegraded     = "site.degraded"
	ActionSiteDisconnected = "site.disconnected"
	ActionSiteRevoked      = "site.revoked"
	ActionSiteArchived     = "site.archived"
	ActionSiteRestored     = "site.restored"
	ActionSiteReEnrolled   = "site.reenrolled"

	// GH #414 m117 — monitoring pause/resume (phase 1). Recorded ONE PER SITE
	// even for a bulk request, so filtering the audit log by a single site's
	// target_id finds the pause that governs it. audit_log.action is plain
	// text with no CHECK and no enum, so these need no migration.
	ActionSiteMonitoringPaused  = "site.monitoring.paused"
	ActionSiteMonitoringResumed = "site.monitoring.resumed"

	// Updates feature: an operator requested an immediate inventory refresh, or
	// the post-update worker autonomously enqueued one for a site. Metadata
	// fields: site_id, source ("api"|"post_update"|"unknown").
	ActionUpdateRefreshRequested = "update.refresh.requested"
	// Updates feature: an old-agent fallback — the agent has no refresh-inventory
	// route (the Track A endpoint isn't deployed on this site yet). Recorded as a
	// warning rather than a job failure so the operator sees it once per site
	// without spamming. Metadata fields: site_id, site_url, status_code.
	ActionUpdateRefreshUnsupported = "update.refresh.unsupported"

	// Phase 5.5 One-Click Login (ADR-031). The nonce id (NOT the JWT) is the
	// stable correlator across the three events.
	//
	// ActionAutologinRequested is recorded on a successful mint. Metadata fields:
	//   nonce_id (string), site_id (uuid), target_wp_user_login (string,
	//   may be ""), initiator_ip (string, may be ""), initiator_user_agent
	//   (string, truncated), expires_at (RFC3339). The minted JWT is NEVER
	//   echoed into metadata — only the nonce id is recorded.
	ActionAutologinRequested = "autologin.requested"
	// ActionAutologinConsumed is recorded when the agent successfully consumes a
	// minted nonce. Metadata fields: nonce_id, site_id, target_wp_user_login,
	// consumed_from_ip, hot_path ("redis"|"postgres") so observability can
	// distinguish the sub-ms Redis path from the PG fallback.
	ActionAutologinConsumed = "autologin.consumed"
	// ActionAutologinFailed is recorded on any mint OR consume failure. Metadata
	// fields: nonce_id (may be ""), site_id (may be uuid.Nil string), code (the
	// domain error code), stage ("mint"|"consume").
	ActionAutologinFailed = "autologin.failed"
	// ActionAutologinPolicyUpdated is recorded on a successful PUT
	// /autologin-policy (GH #286). Metadata fields: enabled (bool),
	// default_wp_user_login (string, may be "").
	ActionAutologinPolicyUpdated = "autologin.policy_updated"

	// Media Optimizer (ADR-043 §6). The destructive delete-originals consent is
	// recorded with ActorUser + the actor id so the hash chain attributes it.
	ActionMediaSyncStarted              = "media.sync.started"
	ActionMediaOptimizeStarted          = "media.optimize.started"
	ActionMediaRestoreStarted           = "media.restore.started"
	ActionMediaDeleteOriginalsConfirmed = "media.delete_originals.confirmed"
	ActionMediaCancelled                = "media.cancelled"
	// ActionMediaSettingsUpdated is recorded when an operator saves per-site
	// auto-optimize settings (ADR-044). Metadata: site_id,
	// auto_optimize_enabled, auto_target_format, auto_target_quality.
	ActionMediaSettingsUpdated = "media.settings.updated"

	// Performance Suite (ADR-046). Cache enable/disable/purge and perf-config
	// saves are operator actions (ActorUser); the IRREVERSIBLE delete-everything
	// is recorded with ActorUser + the actor id so the hash chain attributes the
	// destructive consent (mirrors ActionMediaDeleteOriginalsConfirmed). Metadata
	// carries site_id plus the relevant fields (e.g. kind, urls_count, changed
	// config keys, db cleanup counts).
	ActionCacheEnabled          = "site.cache.enabled"
	ActionCacheDisabled         = "site.cache.disabled"
	ActionCachePurged           = "site.cache.purged"
	ActionCacheDeleteEverything = "site.cache.delete_everything"
	ActionPerfConfigUpdated     = "site.perf.config.updated"
	ActionDbCleaned             = "site.db.cleaned"
	// ActionRumBeaconKeyRotated is recorded on an OPERATOR-initiated RUM
	// beacon-key rotation (POST .../perf/rum/rotate-key, GH #174). The
	// CP-initiated ack-based reconcile job (RumBeaconReconcileWorker) does NOT
	// record this — it is a system self-heal, not an operator action; metadata
	// carries only site_id (never the plaintext key).
	ActionRumBeaconKeyRotated = "site.perf.rum.beacon_key.rotated"
	// Phase 2.2 — per-table DDL action (optimize/repair/drop/empty). The
	// destructive drop/empty paths require PermSiteCacheDeleteAll (admin+);
	// the action field in metadata distinguishes optimize/repair (read-only
	// DDL, operator+) from drop/empty (data-destructive, admin+).
	ActionDbTableAction = "site.db.table.action"

	// Phase 3.8 — destructive orphan deletion. Requires PermSiteCacheDeleteAll
	// (admin+) and a type-to-confirm token. Metadata carries job_id,
	// accepted_count, dropped_count, and a per-kind breakdown.
	ActionDbOrphanDelete = "site.db.orphan.delete"

	// #188 — serialization-safe search-replace tool. Requires PermSiteWrite
	// (operator+). Metadata carries job_id, search (redacted to len only for
	// privacy), dry_run, tables_scanned, rows_matched, rows_changed.
	ActionDbSearchReplace = "site.db.search.replace"

	// #189 — local database snapshot. Requires PermSiteWrite (operator+) for
	// create/revert/delete; PermSiteRead for list. Metadata carries action,
	// snapshot_id (on revert/delete), safety_id (on revert).
	ActionDbSnapshot = "site.db.snapshot"

	// #190 — media library cleaner. Four audit events cover the lifecycle of
	// the scan / isolate / restore / delete flow.
	//
	// ActionMediaCleanScan is recorded on every successful scan page (READ-ONLY;
	// PermSiteRead). Metadata: candidate_count, next_cursor (empty = done).
	ActionMediaCleanScan = "site.media.clean.scan"
	// ActionMediaCleanIsolate is recorded when attachments are moved to quarantine
	// (REVERSIBLE; PermSiteWrite). Metadata: quarantined_count, total_size.
	ActionMediaCleanIsolate = "site.media.clean.isolate"
	// ActionMediaCleanRestore is recorded when quarantined attachments are moved
	// back to the uploads directory (PermSiteWrite). Metadata: restored_count.
	ActionMediaCleanRestore = "site.media.clean.restore"
	// ActionMediaCleanDelete is recorded when quarantined attachments are
	// PERMANENTLY deleted (PermSiteWrite + confirm="DELETE"). This is the
	// irreversible step. Metadata: deleted_count, total_size.
	ActionMediaCleanDelete = "site.media.clean.delete"
	// ActionMediaCleanQuarantine is recorded on every successful quarantine list
	// read (READ-ONLY; PermMediaCleanScan). Metadata: manifest_count.
	ActionMediaCleanQuarantine = "site.media.clean.quarantine"

	// Per-site Email Management (m59). Recorded when the per-site or org-wide
	// email config is created or updated. Metadata: provider, secret_set, scope
	// (site|org).
	ActionEmailConfigUpdated = "site.email.config.updated"

	// Phase 4a — email log actions and suppression management.
	// ActionEmailResent: metadata: log_id (single) or count (bulk).
	ActionEmailResent = "site.email.log.resent"
	// ActionEmailLogDeleted: metadata: deleted (count of rows removed).
	ActionEmailLogDeleted = "site.email.log.deleted"
	// ActionEmailSuppressionAdded: metadata: reason, scope (site|fleet).
	ActionEmailSuppressionAdded = "site.email.suppression.added"
	// ActionEmailSuppressionDeleted: metadata: suppression_id, scope (site|fleet).
	ActionEmailSuppressionDeleted = "site.email.suppression.deleted"

	// Agency Clients (m63). Recorded when a client is created, updated, deleted,
	// or when sites are bulk-assigned.
	ActionClientCreated       = "client.created"
	ActionClientUpdated       = "client.updated"
	ActionClientDeleted       = "client.deleted"
	ActionClientSitesAssigned = "client.sites.assigned"

	// Agency Client Reports (m64). Recorded on schedule update and report
	// lifecycle events.
	ActionClientReportScheduleUpdated = "client.report_schedule.updated"
	ActionClientReportGenerated       = "client.report.generated"
	ActionClientReportDeleted         = "client.report.deleted"

	// Object Cache management (M68). Recorded on config save, enable, disable,
	// flush, and test.
	//
	// ActionObjectCacheConfigUpdated: metadata: has_password (bool), scheme,
	//   analytics_enabled, serializer, compression.
	ActionObjectCacheConfigUpdated = "site.objectcache.config.updated"
	// ActionObjectCacheEnabled: metadata: config_hash (the passing test hash).
	ActionObjectCacheEnabled = "site.objectcache.enabled"
	// ActionObjectCacheDisabled: metadata: flushed (bool).
	ActionObjectCacheDisabled = "site.objectcache.disabled"
	// ActionObjectCacheFlushed: metadata: scope, strategy, keys_deleted.
	ActionObjectCacheFlushed = "site.objectcache.flushed"
	// ActionObjectCacheTested: metadata: ok (bool), config_hash.
	ActionObjectCacheTested = "site.objectcache.tested"

	// File Manager (P1 read-only). Three audit events cover the read lifecycle.
	//
	// ActionSiteFilesRead is the standard audit event for a successful
	// directory listing, inline file read, or file download (non-sensitive path).
	// Metadata: op ("list"|"read"|"download"), path (read/download only), size,
	// truncated (read only), transfer_id (download only).
	ActionSiteFilesRead = "site.files.read"
	// ActionSiteFilesSensitiveRead is recorded when a SENSITIVE path is
	// successfully read or downloaded (T6 elevated-severity entry). The full path
	// is always included in metadata. Requires confirm_sensitive + owner permission.
	// Metadata: op ("read"|"download"), path, size, transfer_id (download only).
	ActionSiteFilesSensitiveRead = "site.files.sensitive.read"
	// ActionSiteFilesSensitiveDenied is recorded on every DENIED attempt to read
	// or download a sensitive path, whether due to missing confirm_sensitive or
	// insufficient permission (T9: log denials). Metadata: op, path, reason.
	ActionSiteFilesSensitiveDenied = "site.files.sensitive.denied"
	// ActionSiteFilesSettingsChanged is recorded when a user enables or disables
	// the file manager for a site via PUT /sites/{siteId}/files/settings
	// (PermSiteFilesManage, admin+). Metadata: enabled (bool).
	ActionSiteFilesSettingsChanged = "site.files.settings.changed"

	// Dashboard 2FA (ADR-056, Phase 2). These actions are account-scoped:
	// they are recorded under the user's first tenant when one exists; otherwise
	// best-effort under a client-member tenant. All carry ActorUser.
	//
	// ActionTOTPEnrolled: metadata: confirmed_at (RFC3339).
	ActionTOTPEnrolled = "auth.2fa.totp.enrolled"
	// ActionTOTPDisabled: metadata: reason ("user_request").
	ActionTOTPDisabled = "auth.2fa.totp.disabled"
	// ActionTOTPVerified: metadata: challenge_id.
	ActionTOTPVerified = "auth.2fa.totp.verified"
	// ActionTOTPFailed: metadata: challenge_id, reason ("invalid_code"|"replay"|"expired").
	ActionTOTPFailed = "auth.2fa.totp.failed"
	// ActionTOTPCodesRegenerated: recorded when recovery codes are regenerated
	// (replaces the old batch). metadata: count (int).
	ActionTOTPCodesRegenerated = "auth.2fa.recovery_codes.regenerated"
	// ActionRecoveryCodeUsed: one code consumed at login. metadata: remaining (int).
	ActionRecoveryCodeUsed = "auth.2fa.recovery_code.used"
	// ActionWebAuthnCredentialAdded: metadata: credential_id (hex), label.
	ActionWebAuthnCredentialAdded = "auth.2fa.webauthn.credential.added"
	// ActionWebAuthnCredentialRemoved: metadata: credential_id (hex), label.
	ActionWebAuthnCredentialRemoved = "auth.2fa.webauthn.credential.removed"
	// ActionWebAuthnVerified: metadata: challenge_id, credential_id (hex).
	ActionWebAuthnVerified = "auth.2fa.webauthn.verified"
	// ActionWebAuthnFailed: metadata: challenge_id, reason.
	ActionWebAuthnFailed = "auth.2fa.webauthn.failed"
	// ActionClonedAuthenticatorDetected: a WebAuthn assertion returned a
	// sign_count that was not greater than the stored value, indicating a
	// possible cloned authenticator. The assertion is REJECTED. metadata:
	// credential_id (hex), stored_count, presented_count.
	// This is a security-critical event; it is always audited regardless of
	// whether the user has a tenant so the record is not lost.
	ActionClonedAuthenticatorDetected = "auth.2fa.webauthn.cloned_authenticator"
	// Action2FAChallengeIssued: a login challenge was issued. metadata:
	// challenge_id, factors_available ([]string).
	Action2FAChallengeIssued = "auth.2fa.challenge.issued"
	// Action2FAChallengeExpired: a challenge was locked due to too many failed
	// attempts. metadata: challenge_id, attempts (int).
	Action2FAChallengeExpired = "auth.2fa.challenge.expired"
	// ActionTrustedDeviceAdded: "remember this device" trust was granted.
	// metadata: device_id, label, expires_at (RFC3339).
	ActionTrustedDeviceAdded = "auth.2fa.trusted_device.added"
	// ActionTrustedDeviceRevoked: a device trust was revoked. metadata: device_id.
	ActionTrustedDeviceRevoked = "auth.2fa.trusted_device.revoked"
	// ActionTrustedDevicesRevokedAll: all device trusts were revoked for a user
	// (e.g. on password change or 2FA disable). metadata: count (int).
	ActionTrustedDevicesRevokedAll = "auth.2fa.trusted_device.revoked_all"

	// ActionAuditIntegrityRebaselined is recorded when an operator moves the
	// tenant's integrity-verification anchor to the current chain head (m90).
	// This never alters or deletes any audit_log row — it only changes where
	// Verify starts its forward walk — and the acknowledgment itself is
	// recorded as a normal hash-chained entry so it lives in the tamper-evident
	// trail too. Metadata: baseline_id, baseline_created_at (RFC3339Nano), and
	// acknowledged_broken_at (the previously-reported break id, when the caller
	// supplied one).
	ActionAuditIntegrityRebaselined = "audit.integrity.rebaselined"

	// M16 Phase C1 — superadmin billing-admin panel (internal/admin). Every
	// mutation there is recorded under the TARGET tenant's own hash chain
	// (ActorType=ActorUser + the superadmin's REAL user id — never a
	// synthetic actor string; see the audit_log actor_id ::uuid-cast incident
	// this rule guards against) alongside a billing_events row (source=
	// "admin" — see billing.Service.RecordAdminBillingEvent). Metadata always
	// carries a before->after snapshot of the mutated fields plus the
	// REQUIRED operator-supplied reason.
	//
	// ActionAdminBillingCompGranted: metadata: plan, reason,
	// old_plan, old_plan_status.
	ActionAdminBillingCompGranted = "admin.billing.comp.granted"
	// ActionAdminBillingCompRevoked: metadata: reason, adopted_live_subscription
	// (bool), new_plan, new_plan_status.
	ActionAdminBillingCompRevoked = "admin.billing.comp.revoked"
	// ActionAdminBillingOverrideSet: metadata: reason, deltas (the requested
	// {sites?,storage_gb?,seats?} deltas), resulting_overrides.
	ActionAdminBillingOverrideSet = "admin.billing.override.set"
	// ActionAdminBillingOverrideCleared: metadata: reason, cleared_keys.
	ActionAdminBillingOverrideCleared = "admin.billing.override.cleared"
	// ActionAdminBillingGraceExtended: metadata: reason, old_grace_until,
	// new_grace_until.
	ActionAdminBillingGraceExtended = "admin.billing.grace.extended"
	// ActionAdminBillingSuspended: metadata: reason.
	ActionAdminBillingSuspended = "admin.billing.suspended"
	// ActionAdminBillingRestored: metadata: reason.
	ActionAdminBillingRestored = "admin.billing.restored"
	// ActionAdminBillingStateForced: metadata: reason, old_plan,
	// old_plan_status, new_plan, new_plan_status.
	ActionAdminBillingStateForced = "admin.billing.state.forced"
)

// Entry is one audit record.
type Entry struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	ActorType  string
	ActorID    string
	Action     string
	TargetType string
	TargetID   string
	Metadata   map[string]any
	PrevHash   string
	Hash       string
	CreatedAt  time.Time
	// ActorName is the acting user's display name (users.name) when
	// ActorType == ActorUser, or the API key's label (api_keys.name) when
	// ActorType == ActorAPIKey. Nil for ActorSystem events and for any actor
	// row that no longer exists (e.g. a deleted user). Only List and
	// ListFiltered populate this field; Record's returned Entry does not.
	ActorName *string
	// ActorEmail is the acting user's email when ActorType == ActorUser. Nil
	// otherwise. Only List and ListFiltered populate this field.
	ActorEmail *string
}

// Baseline is a tenant's integrity re-baseline anchor (audit_integrity_baseline).
// When set, Verify seeds its running hash with Hash and only walks audit_log
// rows strictly after (CreatedAt, ID) — see Verify and Rebaseline.
type Baseline struct {
	TenantID  uuid.UUID
	CreatedAt time.Time // baseline_created_at: the anchored entry's created_at
	ID        uuid.UUID // baseline_id: the anchored entry's id
	Hash      string    // baseline_hash: the anchored entry's hash
	SetBy     *uuid.UUID
	SetAt     time.Time
}

// Event is the input describing something that happened.
type Event struct {
	TenantID   uuid.UUID
	ActorType  string
	ActorID    string
	Action     string
	TargetType string
	TargetID   string
	Metadata   map[string]any
}

// Recorder appends hash-chained audit entries.
type Recorder struct {
	pool  *db.Pool
	clock domain.Clock
}

// NewRecorder builds a Recorder.
func NewRecorder(pool *db.Pool, clock domain.Clock) *Recorder {
	return &Recorder{pool: pool, clock: clock}
}

// canonical builds the deterministic byte string that is hashed for an entry.
// Field order and encoding are fixed so the same logical event always hashes
// identically (and Verify can recompute it).
func canonical(prevHash string, e Event, createdAt time.Time) ([]byte, error) {
	meta := e.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	// json.Marshal of a map sorts keys, giving a stable encoding.
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	s := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s",
		prevHash,
		e.TenantID.String(),
		e.ActorType,
		e.ActorID,
		e.Action,
		e.TargetType,
		e.TargetID,
		string(metaJSON),
		createdAt.UTC().Format(time.RFC3339Nano),
	)
	return []byte(s), nil
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// lockChain acquires the per-tenant advisory lock that serializes audit_log
// appends (see appendLocked). Scoped to the current transaction (pg_advisory_
// xact_lock releases automatically on commit/rollback); re-acquiring it more
// than once within the SAME transaction (e.g. Rebaseline taking it once for
// both the head-read and the append) is safe — Postgres never self-deadlocks
// a backend against its own already-held advisory lock.
func lockChain(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('wpmgr_audit:' || $1))", tenantID.String()); err != nil {
		return domain.Internal("audit_lock_failed", "failed to acquire audit chain lock").WithCause(err)
	}
	return nil
}

// appendLocked inserts one hash-chained entry within an already-open tenant
// tx. The caller must hold the per-tenant advisory lock (via lockChain) around
// its read of the "previous" hash and this call, or two concurrent appends can
// both chain onto the same prev-hash — see Record's lock for the full
// rationale.
func appendLocked(ctx context.Context, q *sqlc.Queries, e Event, createdAt time.Time) (sqlc.AuditLog, error) {
	metaJSON, err := json.Marshal(orEmpty(e.Metadata))
	if err != nil {
		return sqlc.AuditLog{}, domain.Internal("audit_marshal_failed", "failed to encode audit metadata").WithCause(err)
	}
	prevHash, err := q.GetLastAuditHash(ctx, e.TenantID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return sqlc.AuditLog{}, domain.Internal("audit_prev_failed", "failed to read previous audit hash").WithCause(err)
	}
	payload, err := canonical(prevHash, e, createdAt)
	if err != nil {
		return sqlc.AuditLog{}, domain.Internal("audit_canonical_failed", "failed to canonicalize audit entry").WithCause(err)
	}
	h := hashHex(payload)
	row, err := q.InsertAuditEntry(ctx, sqlc.InsertAuditEntryParams{
		TenantID:   e.TenantID,
		ActorType:  e.ActorType,
		ActorID:    e.ActorID,
		Action:     e.Action,
		TargetType: e.TargetType,
		TargetID:   e.TargetID,
		Metadata:   metaJSON,
		PrevHash:   prevHash,
		Hash:       h,
		CreatedAt:  createdAt,
	})
	if err != nil {
		return sqlc.AuditLog{}, domain.Internal("audit_insert_failed", "failed to append audit entry").WithCause(err)
	}
	return row, nil
}

// Record appends an audit entry for the event, chaining it to the tenant's
// previous entry. It runs in the tenant's RLS scope. A best-effort recorder:
// callers should log but not fail the request if Record errors, except where
// the audit trail is itself the point.
func (r *Recorder) Record(ctx context.Context, e Event) (Entry, error) {
	if e.ActorType == "" {
		e.ActorType = ActorSystem
	}

	var out Entry
	err := r.pool.InTenantTx(ctx, e.TenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		// Serialize appends per tenant. Without this, two concurrent Records
		// for the same tenant run in overlapping READ COMMITTED transactions,
		// both read the same "previous" hash below, and both chain their new
		// row to it — Verify then reports the second row as a broken link
		// (tampering) even though nothing was actually corrupted; it was a
		// lost-update race, not a security incident. pg_advisory_xact_lock is
		// scoped to this transaction and releases automatically on commit or
		// rollback, so a second concurrent Record for the same tenant simply
		// blocks here until the first one finishes, then reads the TRUE
		// latest prev-hash. hashtext(text) produces an int4 that Postgres
		// implicitly widens to the int8 pg_advisory_xact_lock expects.
		if err := lockChain(ctx, tx, e.TenantID); err != nil {
			return err
		}
		// Truncate to microseconds — Postgres timestamptz resolution — so the hash
		// computed here matches the value re-read during Verify (RFC3339Nano over a
		// nanosecond time would never re-hash equal after the DB round-trip).
		createdAt := r.clock.Now().UTC().Truncate(time.Microsecond)
		row, err := appendLocked(ctx, q, e, createdAt)
		if err != nil {
			return err
		}
		out = rowToEntry(row)
		return nil
	})
	return out, err
}

// RecordInTx appends an audit entry inside the CALLER's already-open
// transaction, instead of opening its own. It is the fail-closed counterpart
// to Record, required by ADR-064 Decision 7 (and, on paper, ADR-061 Decision
// 2): "If the audit append fails, the version write fails with it; nothing
// commits." Record cannot provide this — it opens and commits its own
// InTenantTx, so a caller that ignores its error (the documented, correct
// thing to do for every other best-effort audit site in this codebase) has
// already durably written whatever it was recording before Record's own
// transaction even starts.
//
// The caller is responsible for:
//   - Running tx against the SAME tenant as e.TenantID (this function does not
//     open a transaction or set any GUC — it trusts the caller's tx is already
//     scoped correctly, exactly as sqlc.New(tx) trusts it everywhere else).
//   - Propagating a non-nil error up through its own transaction function so
//     the caller's pgx.Tx rolls back. RecordInTx itself never rolls back or
//     commits anything; that authority belongs to whoever opened tx.
//
// Locking is identical to Record's: lockChain still serializes concurrent
// appends for the same tenant via a transaction-scoped advisory lock, so two
// concurrent callers (one using Record, one using RecordInTx, or both using
// either) still chain onto the true latest prev-hash rather than racing.
func (r *Recorder) RecordInTx(ctx context.Context, tx pgx.Tx, e Event) (Entry, error) {
	if e.ActorType == "" {
		e.ActorType = ActorSystem
	}
	if err := lockChain(ctx, tx, e.TenantID); err != nil {
		return Entry{}, err
	}
	createdAt := r.clock.Now().UTC().Truncate(time.Microsecond)
	row, err := appendLocked(ctx, sqlc.New(tx), e, createdAt)
	if err != nil {
		return Entry{}, err
	}
	return rowToEntry(row), nil
}

// List returns a page of a tenant's audit entries, newest first (page 1 =
// most recent; a larger offset walks further into the past).
func (r *Recorder) List(ctx context.Context, tenantID uuid.UUID, limit, offset int32) ([]Entry, error) {
	var out []Entry
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListAuditEntries(ctx, sqlc.ListAuditEntriesParams{TenantID: tenantID, Limit: limit, Offset: offset})
		if err != nil {
			return domain.Internal("audit_list_failed", "failed to list audit entries").WithCause(err)
		}
		out = make([]Entry, 0, len(rows))
		for _, row := range rows {
			out = append(out, rowToEntryFromList(row))
		}
		return nil
	})
	return out, err
}

// Filter holds the optional narrowing criteria for ListFiltered. Zero values
// disable the respective filter: empty ActionPrefix matches all actions; a nil
// SiteID matches all target sites; nil CreatedFrom/CreatedTo leave the
// respective end of the time range unbounded. A zero Filter is therefore
// "every entry for the tenant", which is what the pre-existing callers pass.
type Filter struct {
	// ActionPrefix, when non-empty, restricts results to entries whose action
	// starts with this string (prefix match). An exact action string also works
	// because it is a prefix of itself.
	ActionPrefix string
	// SiteID, when non-nil, restricts results to entries associated with this
	// site (string UUID form). Most per-site actions (file-manager, perf, and
	// site lifecycle events) write target_type="site" with target_id=site_id,
	// but backup/restore/update lifecycle rows target a snapshot/run/task id
	// instead, so those recorders stamp metadata.site_id — the query (GH #201)
	// matches on whichever of the two shapes the row has (plus the
	// backup_schedule special case: target_id=site_id, no metadata.site_id).
	// See ListAuditEntriesFiltered in audit_log.sql for the full predicate.
	SiteID *uuid.UUID
	// CreatedFrom / CreatedTo bound created_at as a HALF-OPEN range:
	// created_at >= CreatedFrom and created_at < CreatedTo. Either may be nil
	// to leave that end unbounded. Half-open matches the [from, to) reporting
	// window in internal/report/aggregator.go and, more importantly, makes two
	// reads of this one query non-overlapping, so a caller can read a window
	// and the state that window opened in without double-counting a row that
	// sits exactly on the boundary:
	//
	//	window read : CreatedFrom=&from, CreatedTo=&to    -> [from, to)
	//	carry-in    : CreatedFrom=nil,   CreatedTo=&from  -> (-inf, from), limit 1
	//
	// The carry-in read exists because a site paused BEFORE a window and never
	// resumed inside it writes no row inside the window at all; a window-only
	// read reconstructs no pause and the monthly report then claims uptime
	// coverage for a period the site was paused. See the query's comment.
	CreatedFrom *time.Time
	CreatedTo   *time.Time
}

// tsParam converts an optional bound into the nullable timestamptz the
// generated params struct wants. A nil bound yields the zero
// pgtype.Timestamptz (Valid:false, i.e. SQL NULL), which is exactly what the
// query's `... IS NULL OR ...` disables the predicate on — so a Filter that
// sets no bounds produces the same plan and the same rows as before the bounds
// existed.
func tsParam(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

// ListFiltered returns a page of a tenant's audit entries with optional
// action-prefix, site-id and created_at-range filters applied, newest first
// (see List; the range is half-open, see Filter). RLS is
// the primary tenancy gate; the explicit tenantID in the query is
// defense-in-depth. The hash/prev_hash fields are included so the integrity
// badge on the web layer keeps working. ActionPrefix is a LIKE 'prefix%'
// match (see the query), so e.g. "site.files." matches every file-manager
// action.
func (r *Recorder) ListFiltered(ctx context.Context, tenantID uuid.UUID, f Filter, limit, offset int32) ([]Entry, error) {
	// Sentinel zero UUID disables the site_id filter in the SQL (see query).
	siteIDStr := "00000000-0000-0000-0000-000000000000"
	if f.SiteID != nil {
		siteIDStr = f.SiteID.String()
	}
	var out []Entry
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListAuditEntriesFiltered(ctx, sqlc.ListAuditEntriesFilteredParams{
			TenantID:     tenantID,
			ActionPrefix: f.ActionPrefix,
			SiteID:       siteIDStr,
			CreatedFrom:  tsParam(f.CreatedFrom),
			CreatedTo:    tsParam(f.CreatedTo),
			RowOffset:    offset,
			RowLimit:     limit,
		})
		if err != nil {
			return domain.Internal("audit_list_failed", "failed to list audit entries").WithCause(err)
		}
		out = make([]Entry, 0, len(rows))
		for _, row := range rows {
			out = append(out, rowToEntryFromListFiltered(row))
		}
		return nil
	})
	return out, err
}

// rowToBaseline maps a sqlc.AuditIntegrityBaseline row to the package-level
// Baseline type, unwrapping the nullable set_by column.
func rowToBaseline(row sqlc.AuditIntegrityBaseline) Baseline {
	b := Baseline{
		TenantID:  row.TenantID,
		CreatedAt: row.BaselineCreatedAt,
		ID:        row.BaselineID,
		Hash:      row.BaselineHash,
		SetAt:     row.SetAt,
	}
	if row.SetBy.Valid {
		id := uuid.UUID(row.SetBy.Bytes)
		b.SetBy = &id
	}
	return b
}

// GetBaseline returns the tenant's current integrity re-baseline anchor, or
// nil if one has never been set (Verify then walks the full chain from
// genesis).
func (r *Recorder) GetBaseline(ctx context.Context, tenantID uuid.UUID) (*Baseline, error) {
	var out *Baseline
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).GetAuditIntegrityBaseline(ctx, tenantID)
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return nil
			}
			return domain.Internal("audit_baseline_read_failed", "failed to read the integrity baseline").WithCause(qerr)
		}
		b := rowToBaseline(row)
		out = &b
		return nil
	})
	return out, err
}

// Rebaseline moves the tenant's integrity-verification anchor to the CURRENT
// chain head, so Verify treats everything up to and including that point as
// trusted and only walks entries written after it. It never alters or deletes
// any existing audit_log row — the flagged rows (and everything else) remain
// exactly as written, for forensic review; re-baselining only changes where
// Verify starts looking. The re-baseline itself is recorded as a normal
// hash-chained audit_log entry (action ActionAuditIntegrityRebaselined) via
// Record, so the acknowledgment lives in the tamper-evident trail too — if
// that append fails, the baseline is already durably set (matching this
// package's established mutate-then-audit ordering), but the error is still
// surfaced since an unaudited re-baseline defeats the point of the feature.
//
// actorType/actorID identify the caller for the recorded audit entry (ActorUser
// or ActorAPIKey, mirroring every other handler's actor convention). setByUserID
// is stored in the baseline row's set_by column and is uuid.Nil (stored as
// NULL) for a non-user (API-key) principal — the column exists to answer
// "which operator did this", not "which principal". brokenAt, when non-nil, is
// the previously-reported broken-link id being acknowledged; it is recorded in
// the audit entry's metadata for forensic context only — it is never used to
// locate or touch any row.
func (r *Recorder) Rebaseline(ctx context.Context, tenantID uuid.UUID, actorType, actorID string, setByUserID uuid.UUID, brokenAt *uuid.UUID) (Baseline, error) {
	var out Baseline
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		// Serialize with concurrent Record/Rebaseline calls for this tenant so
		// the "current head" read below can't race a concurrent append (same
		// lock Record uses; see lockChain).
		if err := lockChain(ctx, tx, tenantID); err != nil {
			return err
		}
		head, qerr := q.GetLatestAuditEntry(ctx, tenantID)
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return domain.Conflict("audit_rebaseline_empty", "there is no audit history to baseline yet")
			}
			return domain.Internal("audit_rebaseline_head_failed", "failed to read the current audit chain head").WithCause(qerr)
		}
		setAt := r.clock.Now().UTC().Truncate(time.Microsecond)
		baselineRow, uerr := q.UpsertAuditIntegrityBaseline(ctx, sqlc.UpsertAuditIntegrityBaselineParams{
			TenantID:          tenantID,
			BaselineCreatedAt: head.CreatedAt,
			BaselineID:        head.ID,
			BaselineHash:      head.Hash,
			SetBy:             pgtype.UUID{Bytes: setByUserID, Valid: setByUserID != uuid.Nil},
			SetAt:             setAt,
		})
		if uerr != nil {
			return domain.Internal("audit_rebaseline_upsert_failed", "failed to persist the integrity baseline").WithCause(uerr)
		}
		out = rowToBaseline(baselineRow)
		return nil
	})
	if err != nil {
		return Baseline{}, err
	}

	meta := map[string]any{
		"baseline_id":         out.ID.String(),
		"baseline_created_at": out.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if brokenAt != nil {
		meta["acknowledged_broken_at"] = brokenAt.String()
	}
	if _, rerr := r.Record(ctx, Event{
		TenantID:   tenantID,
		ActorType:  actorType,
		ActorID:    actorID,
		Action:     ActionAuditIntegrityRebaselined,
		TargetType: "audit_log",
		TargetID:   out.ID.String(),
		Metadata:   meta,
	}); rerr != nil {
		return out, domain.Internal("audit_rebaseline_record_failed", "the baseline was set but recording the acknowledgment event failed").WithCause(rerr)
	}
	return out, nil
}

// Verify recomputes the hash chain for a tenant and reports the first broken
// link, if any. ok is true when the entire chain is intact.
//
// When the tenant has a re-baseline anchor set (audit_integrity_baseline —
// see Rebaseline), Verify seeds the running hash with the baseline's hash and
// walks only entries strictly after it: a break before the baseline is
// permanently acknowledged and never re-reported, while any tampering after
// the baseline is still detected exactly as before. With no baseline set,
// behaviour is unchanged — the full chain is walked from genesis.
func (r *Recorder) Verify(ctx context.Context, tenantID uuid.UUID) (ok bool, brokenAt uuid.UUID, err error) {
	err = r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)

		prev := ""
		var rows []sqlc.AuditLog
		baseline, berr := q.GetAuditIntegrityBaseline(ctx, tenantID)
		switch {
		case berr == nil:
			prev = baseline.BaselineHash
			r2, qerr := q.ListAuditEntriesForVerifyFromBaseline(ctx, sqlc.ListAuditEntriesForVerifyFromBaselineParams{
				TenantID:          tenantID,
				BaselineCreatedAt: baseline.BaselineCreatedAt,
				BaselineID:        baseline.BaselineID,
			})
			if qerr != nil {
				return domain.Internal("audit_verify_failed", "failed to load audit entries").WithCause(qerr)
			}
			rows = r2
		case errors.Is(berr, pgx.ErrNoRows):
			r2, qerr := q.ListAuditEntriesForVerify(ctx, tenantID)
			if qerr != nil {
				return domain.Internal("audit_verify_failed", "failed to load audit entries").WithCause(qerr)
			}
			rows = r2
		default:
			return domain.Internal("audit_verify_failed", "failed to load the integrity baseline").WithCause(berr)
		}

		for _, row := range rows {
			var meta map[string]any
			if uerr := json.Unmarshal(row.Metadata, &meta); uerr != nil {
				ok, brokenAt = false, row.ID
				return nil
			}
			payload, cerr := canonical(prev, Event{
				TenantID:   row.TenantID,
				ActorType:  row.ActorType,
				ActorID:    row.ActorID,
				Action:     row.Action,
				TargetType: row.TargetType,
				TargetID:   row.TargetID,
				Metadata:   meta,
			}, row.CreatedAt)
			if cerr != nil {
				return domain.Internal("audit_verify_failed", "failed to canonicalize during verify").WithCause(cerr)
			}
			if row.PrevHash != prev || hashHex(payload) != row.Hash {
				ok, brokenAt = false, row.ID
				return nil
			}
			prev = row.Hash
		}
		ok, brokenAt = true, uuid.Nil
		return nil
	})
	return ok, brokenAt, err
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func rowToEntry(row sqlc.AuditLog) Entry {
	var meta map[string]any
	_ = json.Unmarshal(row.Metadata, &meta)
	return Entry{
		ID:         row.ID,
		TenantID:   row.TenantID,
		ActorType:  row.ActorType,
		ActorID:    row.ActorID,
		Action:     row.Action,
		TargetType: row.TargetType,
		TargetID:   row.TargetID,
		Metadata:   meta,
		PrevHash:   row.PrevHash,
		Hash:       row.Hash,
		CreatedAt:  row.CreatedAt,
	}
}

// mergeActorName returns whichever of the two mutually-exclusive per-actor-
// kind name columns the query resolved (at most one of them is ever non-nil
// for a given row — see the ListAuditEntries join contract in
// db/query/audit_log.sql), or nil if neither resolved.
func mergeActorName(userName, keyName *string) *string {
	if userName != nil {
		return userName
	}
	return keyName
}

func rowToEntryFromList(row sqlc.ListAuditEntriesRow) Entry {
	var meta map[string]any
	_ = json.Unmarshal(row.Metadata, &meta)
	return Entry{
		ID:         row.ID,
		TenantID:   row.TenantID,
		ActorType:  row.ActorType,
		ActorID:    row.ActorID,
		Action:     row.Action,
		TargetType: row.TargetType,
		TargetID:   row.TargetID,
		Metadata:   meta,
		PrevHash:   row.PrevHash,
		Hash:       row.Hash,
		CreatedAt:  row.CreatedAt,
		ActorName:  mergeActorName(row.ActorUserName, row.ActorKeyName),
		ActorEmail: row.ActorEmail,
	}
}

func rowToEntryFromListFiltered(row sqlc.ListAuditEntriesFilteredRow) Entry {
	var meta map[string]any
	_ = json.Unmarshal(row.Metadata, &meta)
	return Entry{
		ID:         row.ID,
		TenantID:   row.TenantID,
		ActorType:  row.ActorType,
		ActorID:    row.ActorID,
		Action:     row.Action,
		TargetType: row.TargetType,
		TargetID:   row.TargetID,
		Metadata:   meta,
		PrevHash:   row.PrevHash,
		Hash:       row.Hash,
		CreatedAt:  row.CreatedAt,
		ActorName:  mergeActorName(row.ActorUserName, row.ActorKeyName),
		ActorEmail: row.ActorEmail,
	}
}
