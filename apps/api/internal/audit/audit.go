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

// List returns a page of a tenant's audit entries (oldest first).
func (r *Recorder) List(ctx context.Context, tenantID uuid.UUID, limit, offset int32) ([]Entry, error) {
	var out []Entry
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListAuditEntries(ctx, sqlc.ListAuditEntriesParams{TenantID: tenantID, Limit: limit, Offset: offset})
		if err != nil {
			return domain.Internal("audit_list_failed", "failed to list audit entries").WithCause(err)
		}
		out = make([]Entry, 0, len(rows))
		for _, row := range rows {
			out = append(out, rowToEntry(row))
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
