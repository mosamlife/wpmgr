package aitrust

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/aipolicy"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Repo is the database side of the AI trust settings. Every statement runs
// inside a tenant transaction helper; nothing here sets a GUC itself.
//
//   - Reads and writes of a site's mode run under the caller's own principal
//     (db.RunTenantTx), so row security narrows a site collaborator to their
//     own sites and the mode guard sees the signed-in person as app.user_id.
//   - The connection's usage count runs organisation-wide with no user
//     (db.InTenantTx), as the decision engine's own count does.
//   - The activity page runs under the caller's principal.
type Repo struct {
	pool  *db.Pool
	audit *audit.Recorder
}

// NewRepo builds the repo.
func NewRepo(pool *db.Pool, rec *audit.Recorder) *Repo { return &Repo{pool: pool, audit: rec} }

// modeFacts is everything a read of a site's mode needs.
type modeFacts struct {
	row          sqlc.GetSiteAIModeRow
	agentVersion string
	paused       bool
	entries      []sqlc.AbilityCatalogue
	routes       []sqlc.RestRouteCatalogue
}

// ReadMode reads one site's mode, its agent version, the organisation's
// pause and the reviewed catalogue, under the caller's principal.
// pgx.ErrNoRows when the site is not visible to the caller.
func (r *Repo) ReadMode(ctx context.Context, p domain.Principal, siteID uuid.UUID) (modeFacts, error) {
	var f modeFacts
	err := r.pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		var err error
		if f.row, err = q.GetSiteAIMode(ctx, sqlc.GetSiteAIModeParams{TenantID: p.TenantID, SiteID: siteID}); err != nil {
			return err
		}
		site, err := q.GetSite(ctx, sqlc.GetSiteParams{ID: siteID, TenantID: p.TenantID})
		if err != nil {
			return err
		}
		f.agentVersion = site.AgentVersion
		tenant, err := q.GetTenant(ctx, p.TenantID)
		if err != nil {
			return err
		}
		f.paused = tenant.AssistantPausedAt.Valid
		if f.entries, err = q.ListAdmittedAbilityCatalogue(ctx); err != nil {
			return err
		}
		f.routes, err = q.ListEnabledRestRoutes(ctx)
		return err
	})
	return f, err
}

// settingTx is one write to a setting: a tenant transaction under the
// caller's principal that holds the tenant's policy lock and, for a site's
// mode, that site's dispatch lock.
type settingTx struct {
	tx    pgx.Tx
	q     *sqlc.Queries
	audit *audit.Recorder
}

// inSettingTx runs fn in a setting write's transaction. The policy lock is
// taken first, then the site dispatch lock when siteID is set: the order the
// decision engine and the reservation expect.
func (r *Repo) inSettingTx(ctx context.Context, p domain.Principal, siteID *uuid.UUID, fn func(st *settingTx) error) error {
	return r.pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if err := q.TakeAssistantRequestXactLock(ctx, sqlc.TakeAssistantRequestXactLockParams{
			LockKey: aipolicy.PolicyTenantLockKey, LockID: p.TenantID.String(),
		}); err != nil {
			return fmt.Errorf("take the policy lock: %w", err)
		}
		if siteID != nil {
			if err := q.TakeAssistantRequestXactLock(ctx, sqlc.TakeAssistantRequestXactLockParams{
				LockKey: aipolicy.AbilitySiteDispatchLockKey, LockID: siteID.String(),
			}); err != nil {
				return fmt.Errorf("take the site dispatch lock: %w", err)
			}
		}
		return fn(&settingTx{tx: tx, q: q, audit: r.audit})
	})
}

func (st *settingTx) siteMode(ctx context.Context, tenantID, siteID uuid.UUID) (sqlc.GetSiteAIModeRow, error) {
	return st.q.GetSiteAIMode(ctx, sqlc.GetSiteAIModeParams{TenantID: tenantID, SiteID: siteID})
}

// paused re-reads the organisation's pause FOR SHARE, so a pause that
// arrives later waits for this write to commit.
func (st *settingTx) paused(ctx context.Context, tenantID uuid.UUID) (bool, error) {
	life, err := st.q.GetTenantAssistantLifecycleForShare(ctx, tenantID)
	if err != nil {
		return false, err
	}
	return life.AssistantPaused || life.Deleted, nil
}

func (st *settingTx) agentVersion(ctx context.Context, tenantID, siteID uuid.UUID) (string, error) {
	site, err := st.q.GetSite(ctx, sqlc.GetSiteParams{ID: siteID, TenantID: tenantID})
	if err != nil {
		return "", err
	}
	return site.AgentVersion, nil
}

func (st *settingTx) setMode(ctx context.Context, arg sqlc.SetSiteAIModeParams) (sqlc.SetSiteAIModeRow, error) {
	return st.q.SetSiteAIMode(ctx, arg)
}

func (st *settingTx) connectionAuto(ctx context.Context, tenantID, grantID uuid.UUID) (sqlc.GetAIConnectionAutoRow, error) {
	return st.q.GetAIConnectionAuto(ctx, sqlc.GetAIConnectionAutoParams{TenantID: tenantID, GrantID: grantID})
}

func (st *settingTx) setConnectionAuto(ctx context.Context, arg sqlc.SetAIConnectionAutoParams) (sqlc.SetAIConnectionAutoRow, error) {
	return st.q.SetAIConnectionAuto(ctx, arg)
}

// record writes the audit row in this transaction. A failure rolls the
// write back: no setting changes without its row.
func (st *settingTx) record(ctx context.Context, e audit.Event) error {
	if st.audit == nil {
		return fmt.Errorf("audit recorder not wired")
	}
	_, err := st.audit.RecordInTx(ctx, st.tx, e)
	return err
}

// ReadConnectionAuto reads a connection's switch, organisation-wide.
// pgx.ErrNoRows when there is no such connection in the tenant.
func (r *Repo) ReadConnectionAuto(ctx context.Context, tenantID, grantID uuid.UUID) (sqlc.GetAIConnectionAutoRow, error) {
	var row sqlc.GetAIConnectionAutoRow
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		row, err = sqlc.New(tx).GetAIConnectionAuto(ctx, sqlc.GetAIConnectionAutoParams{TenantID: tenantID, GrantID: grantID})
		return err
	})
	return row, err
}

// ReadUsage reads a connection's switch and counts what it ran by a site's
// setting in the draft classes over the window, organisation-wide with no
// user and no site allowlist, as the decision engine counts.
func (r *Repo) ReadUsage(ctx context.Context, tenantID, grantID uuid.UUID) (sqlc.GetAIConnectionAutoRow, sqlc.CountAIConnectionPolicyApprovalsRow, error) {
	var (
		row   sqlc.GetAIConnectionAutoRow
		count sqlc.CountAIConnectionPolicyApprovalsRow
	)
	classes := make([]string, 0, len(aipolicy.DraftClasses()))
	for _, c := range aipolicy.DraftClasses() {
		classes = append(classes, string(c))
	}
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		var err error
		if row, err = q.GetAIConnectionAuto(ctx, sqlc.GetAIConnectionAutoParams{TenantID: tenantID, GrantID: grantID}); err != nil {
			return err
		}
		count, err = q.CountAIConnectionPolicyApprovals(ctx, sqlc.CountAIConnectionPolicyApprovalsParams{
			TenantID: tenantID, GrantID: grantID, Classes: classes,
			WindowSeconds: int32(aipolicy.BudgetWindow.Seconds()),
		})
		return err
	})
	return row, count, err
}

// activityRows is one page of the feed as read: the page's references in
// order (one more than asked when there is a next page) and the rows of each
// kind by id.
type activityRows struct {
	refs      []sqlc.ListAIActivityPageRow
	abilities map[uuid.UUID]sqlc.AssistantAbilityRequest
	purges    map[uuid.UUID]sqlc.AssistantCachePurgeRequest
}

// ActivityPage reads one page under the caller's principal, so row security
// narrows a site collaborator to their own sites on both request tables. It
// reads limit+1 references, so the caller can tell whether a next page
// exists.
func (r *Repo) ActivityPage(ctx context.Context, p domain.Principal, aq ActivityQuery) (activityRows, error) {
	out := activityRows{
		abilities: map[uuid.UUID]sqlc.AssistantAbilityRequest{},
		purges:    map[uuid.UUID]sqlc.AssistantCachePurgeRequest{},
	}
	arg := sqlc.ListAIActivityPageParams{
		TenantID: p.TenantID,
		Filter:   aq.Filter,
		RowLimit: aq.Limit + 1,
	}
	if aq.SiteID != nil {
		arg.SiteID = pgtype.UUID{Bytes: *aq.SiteID, Valid: true}
	}
	if aq.GrantID != nil {
		arg.GrantID = pgtype.UUID{Bytes: *aq.GrantID, Valid: true}
	}
	if aq.After != nil {
		arg.CursorCreatedAt = pgtype.Timestamptz{Time: aq.After.CreatedAt, Valid: true}
		arg.CursorID = pgtype.UUID{Bytes: aq.After.ID, Valid: true}
	}
	err := r.pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		var err error
		if out.refs, err = q.ListAIActivityPage(ctx, arg); err != nil {
			return err
		}
		var abilityIDs, purgeIDs []uuid.UUID
		for _, ref := range out.refs {
			switch ref.Kind {
			case KindAbilityRequest:
				abilityIDs = append(abilityIDs, ref.ID)
			case KindCachePurgeRequest:
				purgeIDs = append(purgeIDs, ref.ID)
			}
		}
		if len(abilityIDs) > 0 {
			rows, err := q.ListAbilityRequestsByIDs(ctx, sqlc.ListAbilityRequestsByIDsParams{TenantID: p.TenantID, Ids: abilityIDs})
			if err != nil {
				return err
			}
			for _, row := range rows {
				out.abilities[row.ID] = row
			}
		}
		if len(purgeIDs) > 0 {
			rows, err := q.ListAssistantCachePurgeRequestsByIDs(ctx, sqlc.ListAssistantCachePurgeRequestsByIDsParams{TenantID: p.TenantID, Ids: purgeIDs})
			if err != nil {
				return err
			}
			for _, row := range rows {
				out.purges[row.ID] = row
			}
		}
		return nil
	})
	return out, err
}
