package abilityrequest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/aipolicy"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// PolicyRepo is the production PolicyStore. Every read and write runs in
// an organisation-wide tenant transaction that carries no user and no site
// allowlist (db.InTenantTx): the counts must see every site of the
// connection, and the request's backstop refuses an approval by a setting
// while a user id is in the transaction.
type PolicyRepo struct {
	pool  *db.Pool
	audit *audit.Recorder
}

// NewPolicyRepo builds the store.
func NewPolicyRepo(pool *db.Pool, rec *audit.Recorder) *PolicyRepo {
	return &PolicyRepo{pool: pool, audit: rec}
}

var _ PolicyStore = (*PolicyRepo)(nil)

// policySnapshotSQL reads the request, the class its catalogue entry or
// route stores now, whether its target is one of this site's AI drafts,
// the site's mode and the connection's switch, in one statement. A grant
// that is not active reads as never.
const policySnapshotSQL = `
SELECT r.id, r.tenant_id, r.site_id, r.proposed_by_grant_id, r.entry_id, r.entry_sha256,
       r.ability_name, r.operator_permission, r.input_sha256, r.grant_label, r.site_label,
       r.site_host, r.card_copy_version, r.state, r.policy_checked_at, r.route_id,
       r.card_facts, r.checked_target_status, r.target_post_id, r.created_at, r.expires_at,
       CASE WHEN r.route_id IS NOT NULL
            THEN (SELECT rr.change_class FROM rest_route_catalogue rr WHERE rr.route_id = r.route_id)
            ELSE (SELECT c.change_class FROM ability_catalogue c WHERE c.entry_id = r.entry_id)
       END AS stored_class,
       (r.target_post_id IS NOT NULL AND EXISTS (
            SELECT 1 FROM assistant_ability_requests p
            WHERE p.tenant_id = r.tenant_id
              AND p.site_id = r.site_id
              AND p.ability_name = 'wpmgr/page-create'
              AND p.state = 'done'
              AND p.created_post_id = r.target_post_id
              AND coalesce(p.undo_state, 'available') NOT IN ('undone', 'in_progress')
       ))::boolean AS ai_draft,
       s.ai_mode, s.ai_mode_version, s.ai_mode_set_by, s.ai_mode_set_at,
       CASE WHEN g.status = 'active' THEN g.ai_auto ELSE 'never' END AS ai_auto,
       g.ai_auto_set_by,
       (r.created_at < now() - make_interval(secs => $3))::boolean AS too_old
FROM assistant_ability_requests r
JOIN sites s ON s.tenant_id = r.tenant_id AND s.id = r.site_id
LEFT JOIN mcp_grants g ON g.tenant_id = r.tenant_id AND g.id = r.proposed_by_grant_id
WHERE r.tenant_id = $1
  AND r.id = $2`

// draftUsageSQL counts the connection's approvals by a site's setting in
// the draft classes over the window, organisation-wide.
const draftUsageSQL = `
SELECT count(*)::bigint,
       count(DISTINCT site_id)::bigint,
       coalesce(bool_or(site_id = $3), false),
       min(decided_at)
FROM assistant_ability_requests
WHERE tenant_id = $1
  AND proposed_by_grant_id = $2
  AND approval_source = 'policy'
  AND change_class = ANY($4::text[])
  AND decided_at > now() - make_interval(secs => $5)`

// approveByPolicySQL is the compare-and-set. It approves only a request
// that still waits for its first decision, inside its window, while the
// site's mode, version and setter are the ones the decision relied on and
// the mode allows the class. The backstop trigger re-checks all of it.
const approveByPolicySQL = `
UPDATE assistant_ability_requests r
SET state = 'approved',
    approval_source = 'policy',
    approval_site_mode = $4,
    approval_mode_version = $5,
    approval_setter_user_id = $6,
    approval_setter_set_at = $7,
    base_change_class = $8,
    change_class = $9,
    decided_at = now(),
    dispatch_deadline_at = now() + ($10::int * interval '1 second'),
    policy_checked_at = now()
WHERE r.tenant_id = $1
  AND r.id = $2
  AND r.site_id = $3
  AND r.state = 'pending'
  AND r.policy_checked_at IS NULL
  AND r.expires_at > now()
  AND EXISTS (
      SELECT 1 FROM sites s
      WHERE s.tenant_id = r.tenant_id
        AND s.id = r.site_id
        AND s.ai_mode = $4
        AND s.ai_mode_version = $5
        AND s.ai_mode_set_by = $6
        AND ai_mode_allows(s.ai_mode, $9)
  )
RETURNING r.id, r.tenant_id, r.site_id, r.proposed_by_grant_id, r.state, r.approval_source`

// recordAskSQL records why a request waits. The request stays pending.
const recordAskSQL = `
UPDATE assistant_ability_requests
SET ask_reason = $3,
    base_change_class = $4,
    change_class = $5,
    policy_checked_at = now()
WHERE tenant_id = $1
  AND id = $2
  AND state = 'pending'
  AND policy_checked_at IS NULL`

func (r *PolicyRepo) Snapshot(ctx context.Context, tenantID, requestID uuid.UUID) (PolicySnapshot, error) {
	var snap PolicySnapshot
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		snap, err = readPolicySnapshot(ctx, tx, tenantID, requestID)
		return err
	})
	return snap, err
}

func (r *PolicyRepo) Approving(ctx context.Context, tenantID uuid.UUID, fn func(tx PolicyTx) error) error {
	if r.audit == nil {
		return errors.New("audit recorder not wired")
	}
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		if err := sqlc.New(tx).TakeAssistantRequestXactLock(ctx, sqlc.TakeAssistantRequestXactLockParams{
			LockKey: policyTenantLockKey, LockID: tenantID.String(),
		}); err != nil {
			return fmt.Errorf("take the policy lock: %w", err)
		}
		return fn(&policyTx{tx: tx, audit: r.audit})
	})
}

func readPolicySnapshot(ctx context.Context, tx pgx.Tx, tenantID, requestID uuid.UUID) (PolicySnapshot, error) {
	var (
		snap                 PolicySnapshot
		req                  = &snap.Request
		stored, mode, auto   *string
		modeSetBy, autoSetBy pgtype.UUID
		modeSetAt            pgtype.Timestamptz
	)
	err := tx.QueryRow(ctx, policySnapshotSQL, tenantID, requestID, unsettledAge.Seconds()).Scan(
		&req.ID, &req.TenantID, &req.SiteID, &req.ProposedByGrantID, &req.EntryID, &req.EntrySha256,
		&req.AbilityName, &req.OperatorPermission, &req.InputSha256, &req.GrantLabel, &req.SiteLabel,
		&req.SiteHost, &req.CardCopyVersion, &req.State, &req.PolicyCheckedAt, &req.RouteID,
		&req.CardFacts, &req.CheckedTargetStatus, &req.TargetPostID, &req.CreatedAt, &req.ExpiresAt,
		&stored, &snap.AIDraft,
		&mode, &snap.SiteModeVersion, &modeSetBy, &modeSetAt,
		&auto, &autoSetBy, &snap.TooOld,
	)
	if err != nil {
		return PolicySnapshot{}, err
	}
	if stored != nil {
		snap.StoredClass = aipolicy.Class(*stored)
	}
	if mode != nil {
		snap.SiteMode = aipolicy.Mode(*mode)
	}
	if modeSetBy.Valid {
		snap.SiteSetter = uuid.UUID(modeSetBy.Bytes)
	}
	if modeSetAt.Valid {
		snap.SiteSetAt = modeSetAt.Time
	}
	snap.ConnectionAuto = aipolicy.AutoNever
	if auto != nil {
		snap.ConnectionAuto = aipolicy.ConnectionAuto(*auto)
	}
	if autoSetBy.Valid {
		snap.ConnectionSetter = uuid.UUID(autoSetBy.Bytes)
	}
	return snap, nil
}

// policyTx is PolicyTx over one approving transaction.
type policyTx struct {
	tx    pgx.Tx
	audit *audit.Recorder
}

func (p *policyTx) Snapshot(ctx context.Context, tenantID, requestID uuid.UUID) (PolicySnapshot, error) {
	return readPolicySnapshot(ctx, p.tx, tenantID, requestID)
}

func (p *policyTx) DraftUsage(ctx context.Context, tenantID, grantID, siteID uuid.UUID) (aipolicy.Usage, error) {
	classes := make([]string, 0, len(aipolicy.DraftClasses()))
	for _, c := range aipolicy.DraftClasses() {
		classes = append(classes, string(c))
	}
	var (
		u      aipolicy.Usage
		oldest pgtype.Timestamptz
	)
	err := p.tx.QueryRow(ctx, draftUsageSQL, tenantID, grantID, siteID, classes,
		aipolicy.BudgetWindow.Seconds()).Scan(&u.Changes, &u.Sites, &u.SiteCounted, &oldest)
	if err != nil {
		return aipolicy.Usage{}, err
	}
	if oldest.Valid {
		u.OldestAt = oldest.Time
	}
	return u, nil
}

func (p *policyTx) ApproveByPolicy(ctx context.Context, a PolicyApproval) (sqlc.AssistantAbilityRequest, error) {
	setAt := pgtype.Timestamptz{}
	if !a.SetterSetAt.IsZero() {
		setAt = pgtype.Timestamptz{Time: a.SetterSetAt, Valid: true}
	}
	var out sqlc.AssistantAbilityRequest
	err := p.tx.QueryRow(ctx, approveByPolicySQL,
		a.TenantID, a.RequestID, a.SiteID,
		string(a.SiteMode), a.ModeVersion, a.SetterUserID, setAt,
		string(a.BaseClass), string(a.Class), a.DispatchWindowSeconds,
	).Scan(&out.ID, &out.TenantID, &out.SiteID, &out.ProposedByGrantID, &out.State, &out.ApprovalSource)
	return out, err
}

func (p *policyTx) RecordAsk(ctx context.Context, a PolicyAsk) error {
	tag, err := p.tx.Exec(ctx, recordAskSQL, a.TenantID, a.RequestID, string(a.Reason),
		nullableClass(a.BaseClass, aipolicy.Class.Stored), nullableClass(a.Class, aipolicy.Class.Effective))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}

func (p *policyTx) Audit(ctx context.Context, e audit.Event) error {
	_, err := p.audit.RecordInTx(ctx, p.tx, e)
	return err
}

// nullableClass is c when ok(c), else NULL: an unknown class is never
// written, so the column's CHECK is never what decides.
func nullableClass(c aipolicy.Class, ok func(aipolicy.Class) bool) *string {
	if !ok(c) {
		return nil
	}
	s := string(c)
	return &s
}

// unsettledAge is how old an unchecked request may be and still be decided
// by its setting. An older one waits for a person with not_checked: a
// request is never approved automatically minutes after it was made, under
// a setting that may have changed since.
const unsettledAge = 2 * time.Minute
