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
	ActionClientCreated      = "client.created"
	ActionClientUpdated      = "client.updated"
	ActionClientDeleted      = "client.deleted"
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

// Record appends an audit entry for the event, chaining it to the tenant's
// previous entry. It runs in the tenant's RLS scope. A best-effort recorder:
// callers should log but not fail the request if Record errors, except where
// the audit trail is itself the point.
func (r *Recorder) Record(ctx context.Context, e Event) (Entry, error) {
	if e.ActorType == "" {
		e.ActorType = ActorSystem
	}
	metaJSON, err := json.Marshal(orEmpty(e.Metadata))
	if err != nil {
		return Entry{}, domain.Internal("audit_marshal_failed", "failed to encode audit metadata").WithCause(err)
	}

	var out Entry
	err = r.pool.InTenantTx(ctx, e.TenantID, func(tx pgx.Tx) error {
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
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('wpmgr_audit:' || $1))", e.TenantID.String()); err != nil {
			return domain.Internal("audit_lock_failed", "failed to acquire audit chain lock").WithCause(err)
		}
		prevHash, err := q.GetLastAuditHash(ctx, e.TenantID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return domain.Internal("audit_prev_failed", "failed to read previous audit hash").WithCause(err)
		}
		// Truncate to microseconds — Postgres timestamptz resolution — so the hash
		// computed here matches the value re-read during Verify (RFC3339Nano over a
		// nanosecond time would never re-hash equal after the DB round-trip).
		createdAt := r.clock.Now().UTC().Truncate(time.Microsecond)
		payload, err := canonical(prevHash, e, createdAt)
		if err != nil {
			return domain.Internal("audit_canonical_failed", "failed to canonicalize audit entry").WithCause(err)
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
			return domain.Internal("audit_insert_failed", "failed to append audit entry").WithCause(err)
		}
		out = rowToEntry(row)
		return nil
	})
	return out, err
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
// SiteID matches all target sites.
type Filter struct {
	// ActionPrefix, when non-empty, restricts results to entries whose action
	// starts with this string (prefix match). An exact action string also works
	// because it is a prefix of itself.
	ActionPrefix string
	// SiteID, when non-nil, restricts results to entries whose target_type is
	// "site" and target_id equals this UUID (string form). All file-manager,
	// perf, backup, and other per-site actions write their siteID as target_id
	// with target_type="site", so this filter correctly captures the full
	// per-site timeline without a schema change to audit_log.
	SiteID *uuid.UUID
}

// ListFiltered returns a page of a tenant's audit entries with optional
// action-prefix and site-id filters applied, newest first (see List). RLS is
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

// Verify recomputes the hash chain for a tenant and reports the first broken
// link, if any. ok is true when the entire chain is intact.
func (r *Recorder) Verify(ctx context.Context, tenantID uuid.UUID) (ok bool, brokenAt uuid.UUID, err error) {
	err = r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).ListAuditEntriesForVerify(ctx, tenantID)
		if qerr != nil {
			return domain.Internal("audit_verify_failed", "failed to load audit entries").WithCause(qerr)
		}
		prev := ""
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
