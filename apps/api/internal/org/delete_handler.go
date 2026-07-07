// delete_handler.go — GH #152 part 2: owner-facing organisation deletion.
//
// Two-lane model:
//   - Lane A (empty org: zero sites, zero memberships) — immediate HARD
//     delete via the existing admin_delete_empty_tenant SECURITY DEFINER
//     primitive (m91). Nothing to orphan, nothing to purge later.
//   - Lane B (populated org) — SOFT delete now (tenants.deleted_at is set):
//     the org becomes invisible everywhere the instant this commits — every
//     read path that lists a user's orgs, resolves membership, or looks up a
//     tenant by (user, tenant) excludes a soft-deleted row (see
//     db/query/tenants.sql, memberships.sql, api_keys.sql, plus the auth
//     middleware + org activate's own explicit checks). The grace-window
//     PurgeWorker (purge_worker.go) then does the destructive purge — revoke
//     every connected site, delete the tenant's object-storage prefixes,
//     then the privileged admin_purge_tenant hard delete — once the grace
//     window elapses. Recoverable via POST /orgs/{orgId}/restore until the
//     worker runs.
//
// Lane routing is decided ATOMICALLY inside a single per-tenant advisory-
// locked transaction: AdminDeleteEmptyTenant's own emptiness re-check is
// authoritative (a pre-lock read is validation-only, never trusted for the
// actual mutation), so a concurrent site-create/membership-add racing this
// delete can never leave a tenant half-deleted, and two concurrent delete
// attempts on the same tenant always serialize cleanly.
package org

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// orgLifecycleLockKey is the advisory-lock key namespace shared by DELETE
// /orgs/{orgId} (pg_advisory_xact_lock, transaction-scoped), POST
// /orgs/{orgId}/restore (same) and org.PurgeWorker.purgeOne
// (pg_try_advisory_lock/pg_advisory_unlock, SESSION-scoped, held for the
// whole purge). Postgres advisory locks share ONE underlying lock space
// regardless of xact vs session acquisition — only the release semantics
// differ — so all three mutually exclude each other on hashtext(orgID),
// meaning a delete/restore request and an in-flight purge of the SAME tenant
// always serialize. See purge_worker.go's purgeOne doc comment.
const orgLifecycleLockKey = "org_lifecycle"

// SetHosted wires whether hosted billing (WPMGR_HOSTED) is enabled. When
// true, DELETE refuses to delete an org with plan_status='active' until the
// subscription is cancelled/downgraded. A no-op call (false, the zero value)
// means the guard never fires — matching self-host, where there is no
// subscription to protect.
func (h *Handler) SetHosted(enabled bool) { h.hosted = enabled }

type deleteOrgBody struct {
	ConfirmName string `json:"confirm_name"`
}

type deleteOrgResponse struct {
	ID   string `json:"id"`
	Lane string `json:"lane"` // "hard" (Lane A, empty org) or "soft" (Lane B)
	// ActiveTenantID is the caller's session active tenant AFTER this delete:
	// unchanged (their pre-delete active tenant) when the deleted org was NOT
	// their active org; another of their live orgs when it was and they have
	// one; nil when it was and they have none left (their last org).
	ActiveTenantID *string `json:"active_tenant_id,omitempty"`
}

type restoreOrgResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// delete handles DELETE /api/v1/orgs/{orgId} (GH #152).
func (h *Handler) delete(c *gin.Context) {
	ctx := c.Request.Context()
	p, ok := domain.PrincipalFromContext(ctx)
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	orgID, err := uuid.Parse(c.Param("orgId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_org_id", "orgId is not a valid UUID"))
		return
	}

	var body deleteOrgBody
	if berr := c.ShouldBindJSON(&body); berr != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON"))
		return
	}
	confirmName := strings.TrimSpace(body.ConfirmName)
	if confirmName == "" {
		httpx.Error(c, domain.Validation("confirm_name_required", "confirm_name is required"))
		return
	}

	// Owner-only — strictly stronger than rename's admin+ gate (authz.RoleAdmin).
	role, isMember := h.authSvc.RoleInTenant(ctx, p.UserID, orgID)
	if !isMember {
		httpx.Error(c, domain.Forbidden("not_a_member", "you are not a member of this organisation"))
		return
	}
	if !role.AtLeast(authz.RoleOwner) {
		httpx.Error(c, domain.Forbidden("insufficient_role", "only the owner can delete the organisation"))
		return
	}

	// Deleting the caller's own CURRENTLY ACTIVE org is allowed (design
	// reconciliation: the Danger Zone UI only ever lives on Settings for the
	// active org, so refusing this would make the feature unreachable from
	// its only real entry point, and would also make deleting a user's last
	// org impossible — contradicting the approved "allow deleting the last
	// org, drop to onboarding"). See the post-commit session-reassignment
	// block below: when this IS the caller's active org, their session is
	// repointed at another live membership, or cleared entirely if this was
	// their last org.

	// Pre-lock validation reads. The authoritative existence/deleted_at/
	// emptiness decision happens again INSIDE the advisory lock below; a
	// TOCTOU here only affects guard messaging, never correctness.
	q := sqlc.New(h.pool.Pool)
	t, terr := q.GetTenant(ctx, orgID)
	if terr != nil {
		if errors.Is(terr, pgx.ErrNoRows) {
			httpx.Error(c, domain.NotFound("org_not_found", "organisation not found"))
			return
		}
		httpx.Error(c, domain.Internal("org_load_failed", "failed to load organisation").WithCause(terr))
		return
	}
	if t.DeletedAt.Valid {
		httpx.Error(c, domain.Conflict("org_already_deleted", "this organisation is already scheduled for deletion"))
		return
	}
	if confirmName != t.Name {
		httpx.Error(c, domain.Validation("confirm_name_mismatch", "confirm_name does not match the organisation's name"))
		return
	}
	if h.hosted && t.PlanStatus == "active" {
		httpx.Error(c, domain.Conflict("billing_active",
			"cancel or downgrade the subscription before deleting this organisation"))
		return
	}
	activeRestore, rerr := h.hasActiveRestore(ctx, orgID)
	if rerr != nil {
		httpx.Error(c, domain.Internal("org_delete_restore_check_failed", "failed to check for an in-progress restore").WithCause(rerr))
		return
	}
	if activeRestore {
		httpx.Error(c, domain.Conflict("restore_in_progress",
			"a restore is currently in progress for a site in this organisation; wait for it to finish before deleting"))
		return
	}

	// Note: deliberately NO check for "is this the caller's last org" (nor for
	// "is this the caller's active org" — see the comment above where that
	// guard used to be) — deleting a user's last/only org is explicitly
	// allowed; the post-commit session-reassignment block below clears their
	// active tenant so they drop cleanly to the no-org onboarding screen.

	var (
		lane string // "hard" or "soft"
		name = t.Name
	)
	err = h.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		if _, lerr := tx.Exec(ctx,
			`SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`,
			orgLifecycleLockKey, orgID.String(),
		); lerr != nil {
			return domain.Internal("org_delete_lock_failed", "failed to lock organisation for deletion").WithCause(lerr)
		}

		// Authoritative re-read, under the lock — never trust the pre-lock read
		// above for the actual mutation decision.
		fresh, gerr := sqlc.New(tx).GetTenant(ctx, orgID)
		if gerr != nil {
			if errors.Is(gerr, pgx.ErrNoRows) {
				return domain.NotFound("org_not_found", "organisation not found")
			}
			return domain.Internal("org_load_failed", "failed to load organisation").WithCause(gerr)
		}
		if fresh.DeletedAt.Valid {
			return domain.Conflict("org_already_deleted", "this organisation is already scheduled for deletion")
		}
		name = fresh.Name

		// Lane-A eligibility, per the task's definition: zero sites AND zero
		// memberships OTHER THAN the caller (the deleting owner). This is
		// authoritative (re-checked here, under the lock, inside this same
		// transaction) — not a race with any pre-lock guard. Both reads run
		// under InAgentTx's app.agent='on', which the sites_agent /
		// memberships_agent SELECT-only policies permit cross-tenant.
		var siteCount, otherMemberCount int
		if cerr := tx.QueryRow(ctx, `SELECT count(*) FROM sites WHERE tenant_id = $1`, orgID).Scan(&siteCount); cerr != nil {
			return domain.Internal("org_delete_failed", "failed to count sites").WithCause(cerr)
		}
		if cerr := tx.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE tenant_id = $1 AND user_id <> $2`, orgID, p.UserID).Scan(&otherMemberCount); cerr != nil {
			return domain.Internal("org_delete_failed", "failed to count memberships").WithCause(cerr)
		}

		if siteCount == 0 && otherMemberCount == 0 {
			// Genuinely empty (per the task's definition) except for the
			// deleting owner's own membership row: admin_delete_empty_tenant's
			// OWN guard requires ZERO memberships total (it was designed for the
			// superadmin orphan-cleanup case, where the last member was already
			// removed elsewhere) — it would otherwise always see this org as
			// "not empty" and silently fall through to Lane B for every single
			// delete, since the caller is necessarily still a member at this
			// point. Removing that last membership row here — inside the SAME
			// locked transaction, and ONLY after independently confirming zero
			// sites and zero OTHER members — makes admin_delete_empty_tenant's
			// re-check pass truthfully. app.tenant_id must be set to orgID first:
			// under InAgentTx alone (app.agent='on'), memberships_agent is
			// SELECT-only and would silently affect 0 rows on this DELETE;
			// memberships_tenant_isolation (tenant_id = app.tenant_id) is the
			// policy that actually permits it.
			if _, serr := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, orgID.String()); serr != nil {
				return domain.Internal("org_delete_failed", "failed to scope membership cleanup").WithCause(serr)
			}
			if _, derr := tx.Exec(ctx, `DELETE FROM memberships WHERE tenant_id = $1 AND user_id = $2`, orgID, p.UserID); derr != nil {
				return domain.Internal("org_delete_failed", "failed to remove owner membership").WithCause(derr)
			}
		}

		// AdminDeleteEmptyTenant re-checks emptiness itself (memberships + sites)
		// under its own SECURITY DEFINER scope — authoritative, not a race with
		// the counts above (same transaction, read-your-own-writes).
		emptied, eerr := sqlc.New(tx).AdminDeleteEmptyTenant(ctx, orgID)
		if eerr != nil {
			return domain.Internal("org_delete_failed", "failed to delete organisation").WithCause(eerr)
		}
		if emptied {
			lane = "hard"
			return nil
		}
		if siteCount == 0 && otherMemberCount == 0 {
			// Unreachable in practice: we just deleted the only remaining
			// membership row in THIS SAME transaction, so admin_delete_empty_tenant's
			// re-check cannot legitimately still see either EXISTS as true. Treat
			// as a hard failure rather than silently falling through to Lane B —
			// that would soft-delete a tenant whose only membership was just
			// removed, leaving it with no owner able to ever restore it.
			return domain.Internal("org_delete_failed", "empty-tenant delete did not complete as expected")
		}

		if _, serr := sqlc.New(tx).SoftDeleteTenant(ctx, orgID); serr != nil {
			if errors.Is(serr, pgx.ErrNoRows) {
				// Raced with another delete attempt that won first.
				return domain.Conflict("org_already_deleted", "this organisation is already scheduled for deletion")
			}
			return domain.Internal("org_delete_failed", "failed to delete organisation").WithCause(serr)
		}
		lane = "soft"
		return nil
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}

	// Durable, tenant-INDEPENDENT record: for lane=="hard" the tenant's own
	// audit_log is already gone (admin_delete_empty_tenant wiped it moments
	// ago); for lane=="soft" it will be wiped later by the purge worker. This
	// event must survive both, so it is NEVER written to the tenant's own
	// audit_log — only here. Best-effort: logged, never fails the request that
	// already committed.
	h.recordSystemAudit(ctx, "org.deleted", p.UserID, orgID, name, map[string]any{"lane": lane})

	// Session reassignment: the delete above may have just removed the
	// caller's CURRENTLY ACTIVE org (deletion of the active org is allowed —
	// see the comment above the removed guard). A session must never be left
	// pointing at a now-deleted tenant: repoint it at another of the caller's
	// live memberships via the SAME mechanism POST /orgs/{orgId}/activate
	// uses, or clear it entirely (uuid.Nil) if this was their last org, so
	// /auth/me + the auth middleware resolve to the clean no-org state rather
	// than a dangling active_tenant_id. Best-effort: logged on failure, never
	// fails the request that already committed the delete. The frontend
	// independently re-navigates on a successful delete response (activates
	// another org or falls back to the no-org screen); this is
	// belt-and-suspenders so the two never disagree.
	activeTenantID := p.TenantID
	if p.TenantID == orgID {
		next, hasNext := h.pickAnotherLiveTenant(ctx, p.UserID, orgID)
		if !hasNext {
			next = uuid.Nil
		}
		h.sessions.SetActiveTenant(ctx, next)
		activeTenantID = next
	}

	resp := deleteOrgResponse{ID: orgID.String(), Lane: lane}
	if activeTenantID != uuid.Nil {
		s := activeTenantID.String()
		resp.ActiveTenantID = &s
	}
	c.JSON(http.StatusOK, resp)
}

// pickAnotherLiveTenant returns a tenant (other than excludeID) userID is
// still a LIVE (non-soft-deleted) member of, for reassigning the session's
// active tenant after the caller deletes their own active org. Backed by
// ListMembershipsForUser, which already excludes a soft-deleted tenant's
// membership rows (see db/query/memberships.sql) — so this can never pick a
// tenant that is itself mid-deletion. ok=false means excludeID was the
// caller's last (or only) live org.
func (h *Handler) pickAnotherLiveTenant(ctx context.Context, userID, excludeID uuid.UUID) (uuid.UUID, bool) {
	var memberships []sqlc.Membership
	err := h.pool.InUserTx(ctx, userID, func(tx pgx.Tx) error {
		var qErr error
		memberships, qErr = sqlc.New(tx).ListMembershipsForUser(ctx, userID)
		return qErr
	})
	if err != nil {
		return uuid.Nil, false
	}
	for _, m := range memberships {
		if m.TenantID != excludeID {
			return m.TenantID, true
		}
	}
	return uuid.Nil, false
}

// restore handles POST /api/v1/orgs/{orgId}/restore (GH #152) — undelete
// within the grace window, owner-only.
//
// Uses a direct, UNFILTERED membership check (sqlc.GetMembership) rather than
// authSvc.RoleInTenant: RoleInTenant's backing query (ListMembershipsForUser)
// now excludes a soft-deleted tenant's membership rows — which is exactly the
// state the org being restored is in — so RoleInTenant would make every
// legitimate owner look like a non-member of the very org they are trying to
// restore. GetMembership carries no such filter; run under InAgentTx it is
// permitted by the memberships_agent cross-tenant SELECT-only policy
// regardless of the target tenant's deleted_at.
//
// Membership is intentionally checked BEFORE tenant existence/deleted_at:
// this is the SAME order (and the same reason) as tenant.Repo.GetForUser's
// "do not disclose existence" comment — checking tenant state first would let
// an arbitrary authenticated caller learn whether an orgId is live/deleted/
// purged without being a member. One consequence: after the grace-window
// PurgeWorker actually hard-deletes a tenant, admin_purge_tenant's cascade
// removes the memberships row too, so a genuinely-purged org's own former
// owner gets the SAME 403 "not_a_member" a random non-member would — never a
// distinguishing 404. The "org_already_purged" 404 branch below is currently
// unreachable via that cascade (GetMembership already failed first in that
// case) but is kept as defense-in-depth against a future schema change to
// memberships' FK. The "purge_in_progress" 409 branch (adversarial-review
// fast-follow M2), by contrast, IS reachable: purge_started_at is set by
// org.PurgeWorker BEFORE it deletes any object or calls admin_purge_tenant,
// so the memberships row (and the owner's ability to reach this far) still
// exists at that point.
func (h *Handler) restore(c *gin.Context) {
	ctx := c.Request.Context()
	p, ok := domain.PrincipalFromContext(ctx)
	if !ok {
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}
	orgID, err := uuid.Parse(c.Param("orgId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_org_id", "orgId is not a valid UUID"))
		return
	}

	var restored sqlc.Tenant
	err = h.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		if _, lerr := tx.Exec(ctx,
			`SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`,
			orgLifecycleLockKey, orgID.String(),
		); lerr != nil {
			return domain.Internal("org_restore_lock_failed", "failed to lock organisation for restore").WithCause(lerr)
		}

		m, merr := sqlc.New(tx).GetMembership(ctx, sqlc.GetMembershipParams{UserID: p.UserID, TenantID: orgID})
		if merr != nil {
			if errors.Is(merr, pgx.ErrNoRows) {
				return domain.Forbidden("not_a_member", "you are not a member of this organisation")
			}
			return domain.Internal("org_restore_membership_check_failed", "failed to verify membership").WithCause(merr)
		}
		if !authz.Role(m.Role).AtLeast(authz.RoleOwner) {
			return domain.Forbidden("insufficient_role", "only the owner can restore the organisation")
		}

		row, rerr := sqlc.New(tx).RestoreTenant(ctx, orgID)
		if rerr != nil {
			if !errors.Is(rerr, pgx.ErrNoRows) {
				return domain.Internal("org_restore_failed", "failed to restore organisation").WithCause(rerr)
			}
			// 0 rows: never deleted, already hard-purged, or a purge is already
			// in progress (purge_started_at set — adversarial-review fast-follow
			// M2). Distinguish so the caller gets an accurate message.
			t, gerr := sqlc.New(tx).GetTenant(ctx, orgID)
			if gerr != nil {
				if errors.Is(gerr, pgx.ErrNoRows) {
					return domain.NotFound("org_already_purged", "this organisation has already been permanently deleted")
				}
				return domain.Internal("org_load_failed", "failed to load organisation").WithCause(gerr)
			}
			if t.DeletedAt.Valid && t.PurgeStartedAt.Valid {
				// The grace-window PurgeWorker has already started deleting this
				// tenant's object-storage prefixes (or the privileged hard delete
				// itself, which would have made GetTenant above ErrNoRows instead) —
				// restoring now would resurrect a tenant whose backups may already
				// point at partially-deleted objects. Refuse; there is no recovery
				// path once purge has begun.
				return domain.Conflict("purge_in_progress", "this organisation is already being permanently deleted and can no longer be restored")
			}
			return domain.Conflict("org_not_deleted", "this organisation is not scheduled for deletion")
		}
		restored = row
		return nil
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}

	h.recordSystemAudit(ctx, "org.restored", p.UserID, orgID, restored.Name, nil)

	c.JSON(http.StatusOK, restoreOrgResponse{ID: restored.ID.String(), Name: restored.Name, Slug: restored.Slug})
}

// tenantSoftDeleted reports whether orgID has been soft-deleted (GH #152) or
// no longer exists at all. tenants carries no RLS (see db/schema.sql's file
// header), so this is a plain, unscoped read directly on the pool.
func (h *Handler) tenantSoftDeleted(ctx context.Context, orgID uuid.UUID) bool {
	t, err := sqlc.New(h.pool.Pool).GetTenant(ctx, orgID)
	if err != nil {
		return true // fail-closed: an unreadable/missing tenant is never activatable
	}
	return t.DeletedAt.Valid
}

// hasActiveRestore reports whether ANY site in tenantID has a queued/running
// restore_runs row. restore_runs is queried directly with hand-written SQL
// (mirrors internal/backup/restore_run_repo.go's own convention for this
// table, rather than a sqlc query) since this is a tenant-WIDE existence
// check — backup.Repo.HasActiveRestore already exists but is keyed by
// specific chain/snapshot group keys, not usable directly for "does ANY site
// in this org have an active restore". The 'queued'/'running' vocabulary
// mirrors backup.evaluateDeleteGuards' SkipRestoreInProgress exactly. Runs
// under InTenantTx so restore_runs_tenant_isolation applies.
func (h *Handler) hasActiveRestore(ctx context.Context, tenantID uuid.UUID) (bool, error) {
	var exists bool
	err := h.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM restore_runs WHERE tenant_id = $1 AND status IN ('queued', 'running'))`,
			tenantID,
		).Scan(&exists)
	})
	return exists, err
}

// recordSystemAudit writes a durable event to system_audit_log — a
// tenant-INDEPENDENT sink (no FK to tenants) that survives the target
// tenant's own hash-chained audit_log being purged or already gone. See
// db/query/system_audit.sql. Best-effort: logged on failure, never returned
// to the caller (the delete/restore has already committed).
func (h *Handler) recordSystemAudit(ctx context.Context, action string, actorID, tenantID uuid.UUID, tenantName string, meta map[string]any) {
	payload := []byte("{}")
	if len(meta) > 0 {
		if marshalled, merr := json.Marshal(meta); merr == nil {
			payload = marshalled
		}
	}
	if err := sqlc.New(h.pool.Pool).InsertSystemAuditEvent(ctx, sqlc.InsertSystemAuditEventParams{
		ActorType:  audit.ActorUser,
		ActorID:    pgtype.UUID{Bytes: actorID, Valid: actorID != uuid.Nil},
		Action:     action,
		TenantID:   tenantID,
		TenantName: tenantName,
		Metadata:   payload,
	}); err != nil {
		slog.ErrorContext(ctx, "system audit record failed",
			slog.String("action", action), slog.String("tenant_id", tenantID.String()), slog.Any("error", err))
	}
}
