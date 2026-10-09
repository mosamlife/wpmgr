package tests

// Approval tiers, Merge A (ADR-065): the setter checks and the decision for
// one new write request, reached through the production path. Decide runs on
// the production PolicyRepo (an organisation-wide tenant transaction with no
// user, under the tenant's policy lock) and builds every setter's principal
// with the session authenticator's own code
// (middleware.Authenticator.ResolveSetter), which
// TestResolveSetterReadsTheSettersAuthorityNow also proves on its own.
// Settings are raised the way a person raises them: in a transaction that
// carries that person's user id, which the m174 guard triggers require.
// Every statement under test runs as wpmgr_app; the bootstrap connection
// only seeds what no runtime path writes (a share, a disabled or deleted
// account, an aged request).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/abilityrequest"
	"github.com/mosamlife/wpmgr/apps/api/internal/aipolicy"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/internal/middleware"
)

// astStack is one organisation with a site, an owner, an admin and an
// operator, one AI connection, and the approval package wired as
// cmd/wpmgr wires it.
type astStack struct {
	pool                   *db.Pool
	tenant, site, grant    uuid.UUID
	owner, admin, operator auth.User
	authRepo               *auth.Repo
	svc                    *abilityrequest.Service
	entry                  uuid.UUID
}

func newASTStack(t *testing.T) *astStack {
	t.Helper()
	ctx := context.Background()
	pool := startPostgres(t)
	rec := audit.NewRecorder(pool, domain.SystemClock{})
	authRepo := auth.NewRepo(pool)
	authSvc := auth.NewService(authRepo, rec, domain.NewValidator())
	tag := uuid.NewString()[:8]
	st := &astStack{pool: pool, authRepo: authRepo}
	st.tenant = seedTenant(t, pool, "at-"+tag)
	st.owner = seedUserMembership(t, authRepo, "owner-"+tag+"@example.com", st.tenant, authz.RoleOwner)
	st.admin = seedUserMembership(t, authRepo, "admin-"+tag+"@example.com", st.tenant, authz.RoleAdmin)
	st.operator = seedUserMembership(t, authRepo, "operator-"+tag+"@example.com", st.tenant, authz.RoleOperator)
	st.site = seedSite(t, pool, st.tenant, "")
	st.grant = mcpSeedGrant(t, mcp.NewRepo(pool), st.tenant, "all", nil).ID
	if err := pool.InTenantTx(ctx, st.tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (page-create entry)")
		return tx.QueryRow(ctx, `SELECT entry_id FROM ability_catalogue WHERE name = 'wpmgr/page-create'`).Scan(&st.entry)
	}); err != nil {
		t.Fatalf("read the page-create entry: %v", err)
	}
	svc := abilityrequest.NewService(pool, nil, nil, rec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.SetWriteToolsEnabled(true)
	svc.SetPolicy(abilityrequest.NewPolicyRepo(pool, rec), middleware.NewAuthenticator(nil, authSvc, nil, pool))
	st.svc = svc
	return st
}

// setSiteMode raises (or sets) a site's mode as the person who chooses it.
func (st *astStack) setSiteMode(t *testing.T, site, by uuid.UUID, mode string) {
	t.Helper()
	ctx := context.Background()
	if err := st.pool.InTenantTxAsUser(ctx, st.tenant, by, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTxAsUser (set site mode)")
		tag, err := tx.Exec(ctx, `UPDATE sites SET ai_mode = $3, ai_mode_source = 'person', ai_mode_set_by = $4,
			ai_mode_set_at = now(), ai_mode_version = ai_mode_version + 1
			WHERE tenant_id = $1 AND id = $2`, st.tenant, site, mode, by)
		if err == nil && tag.RowsAffected() != 1 {
			err = fmt.Errorf("updated %d sites", tag.RowsAffected())
		}
		return err
	}); err != nil {
		t.Fatalf("set the site's mode to %s: %v", mode, err)
	}
}

// allowConnection sets the connection's switch to site_setting as by.
func (st *astStack) allowConnection(t *testing.T, by uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if err := st.pool.InTenantTxAsUser(ctx, st.tenant, by, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTxAsUser (allow connection)")
		tag, err := tx.Exec(ctx, `UPDATE mcp_grants SET ai_auto = 'site_setting', ai_auto_set_by = $3, ai_auto_set_at = now()
			WHERE tenant_id = $1 AND id = $2`, st.tenant, st.grant, by)
		if err == nil && tag.RowsAffected() != 1 {
			err = fmt.Errorf("updated %d grants", tag.RowsAffected())
		}
		return err
	}); err != nil {
		t.Fatalf("allow the connection: %v", err)
	}
}

// newRequest inserts a pending page creation on site through the
// connection-scoped principal the MCP path uses.
func (st *astStack) newRequest(t *testing.T, site uuid.UUID, seed string, edit func(*sqlc.InsertAbilityRequestParams)) sqlc.AssistantAbilityRequest {
	t.Helper()
	arg := aarParams(st.tenant, site, st.grant, seed)
	arg.EntryID = st.entry
	if edit != nil {
		edit(&arg)
	}
	return aarInsert(t, st.pool, acprSitePrincipal(st.tenant, site), arg)
}

// astRow is what the decision left on the request.
type astRow struct {
	state, source              string
	siteMode, askReason, class *string
	baseClass                  *string
	modeVersion                *int64
	setter                     *uuid.UUID
	deciderNull, checked       bool
}

func (st *astStack) row(t *testing.T, id uuid.UUID) astRow {
	t.Helper()
	ctx := context.Background()
	var r astRow
	if err := st.pool.InTenantTx(ctx, st.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state, approval_source, approval_site_mode, ask_reason, change_class,
			base_change_class, approval_mode_version, approval_setter_user_id, decided_by_user_id IS NULL,
			policy_checked_at IS NOT NULL
			FROM assistant_ability_requests WHERE tenant_id = $1 AND id = $2`, st.tenant, id).Scan(
			&r.state, &r.source, &r.siteMode, &r.askReason, &r.class, &r.baseClass, &r.modeVersion, &r.setter,
			&r.deciderNull, &r.checked)
	}); err != nil {
		t.Fatalf("read the request: %v", err)
	}
	return r
}

func (st *astStack) decide(t *testing.T, id uuid.UUID) abilityrequest.DecideResult {
	t.Helper()
	res, err := st.svc.Decide(context.Background(), st.tenant, id)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if !res.Decided {
		t.Fatalf("decide: nothing was decided")
	}
	return res
}

// expectAsk decides a request and requires it to wait with want, recorded
// on the row and in an audit row by the policy actor.
func (st *astStack) expectAsk(t *testing.T, id uuid.UUID, want aipolicy.AskReason) {
	t.Helper()
	res := st.decide(t, id)
	if res.Outcome != aipolicy.OutcomeAsk || res.Ask != want {
		t.Fatalf("decision %+v, want ask %s", res, want)
	}
	r := st.row(t, id)
	if r.state != "pending" || r.askReason == nil || *r.askReason != string(want) || !r.checked || r.source != "person" {
		t.Fatalf("row %+v, want pending with ask_reason %s", r, want)
	}
	if got := st.auditActor(t, id, audit.ActionAbilityRequestAsked); got != audit.ActorPolicy {
		t.Fatalf("asked audit actor %q, want %q", got, audit.ActorPolicy)
	}
	t.Logf("request %s waits: ask_reason=%s", id, *r.askReason)
}

func (st *astStack) auditActor(t *testing.T, id uuid.UUID, action string) string {
	t.Helper()
	ctx := context.Background()
	var actor string
	if err := st.pool.InTenantTx(ctx, st.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT actor_type FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3
			ORDER BY created_at DESC LIMIT 1`, st.tenant, action, id.String()).Scan(&actor)
	}); err != nil {
		t.Fatalf("read the %s audit row: %v", action, err)
	}
	return actor
}

// §11 regression 1: a page creation on an Auto-for-AI-drafts site is approved
// by the setting, with no person named as decider, the setting it relied on
// recorded, and a policy audit row.
func TestPageCreateAutoOnAIDraftsSite(t *testing.T) {
	st := newASTStack(t)
	st.setSiteMode(t, st.site, st.operator.ID, "ai_drafts")
	st.allowConnection(t, st.admin.ID)
	req := st.newRequest(t, st.site, "auto", nil)
	res := st.decide(t, req.ID)
	if res.Outcome != aipolicy.OutcomeAutoBySetting || res.Class != aipolicy.ClassAIDraft {
		t.Fatalf("decision %+v, want approved by the setting as ai_draft", res)
	}
	r := st.row(t, req.ID)
	if r.state != "approved" || r.source != "policy" || !r.deciderNull || !r.checked ||
		r.siteMode == nil || *r.siteMode != "ai_drafts" || r.modeVersion == nil || *r.modeVersion != 1 ||
		r.setter == nil || *r.setter != st.operator.ID ||
		r.class == nil || *r.class != "ai_draft" || r.baseClass == nil || *r.baseClass != "ai_draft" {
		t.Fatalf("row %+v, want approved by policy under ai_drafts v1 set by the operator", r)
	}
	if got := st.auditActor(t, req.ID, audit.ActionAbilityRequestApproved); got != audit.ActorPolicy {
		t.Fatalf("approval audit actor %q, want %q", got, audit.ActorPolicy)
	}
	t.Logf("approved by the setting: source=%s mode=%s version=%d setter=%s", r.source, *r.siteMode, *r.modeVersion, *r.setter)
}

// §11 regression 2: on an Ask site the request waits with site_mode_ask.
func TestPageCreateWaitsOnAskSite(t *testing.T) {
	st := newASTStack(t)
	st.allowConnection(t, st.admin.ID)
	st.expectAsk(t, st.newRequest(t, st.site, "ask", nil).ID, aipolicy.AskSiteModeAsk)
}

// B4: a connection whose switch is never (a key-minted one starts there)
// asks, through Decide alone. The MCP path's proof is
// TestKeyMintedConnectionFirstWriteWaits.
func TestDecideConnectionNeverAsks(t *testing.T) {
	st := newASTStack(t)
	st.setSiteMode(t, st.site, st.operator.ID, "ai_drafts")
	st.expectAsk(t, st.newRequest(t, st.site, "never", nil).ID, aipolicy.AskConnectionNeverAuto)
}

// B1: the person who chose the site's mode is removed from the organisation.
func TestDecideSetterRemovedFromOrg(t *testing.T) {
	st := newASTStack(t)
	st.setSiteMode(t, st.site, st.operator.ID, "ai_drafts")
	st.allowConnection(t, st.admin.ID)
	if err := st.authRepo.DeleteMembership(context.Background(), st.operator.ID, st.tenant); err != nil {
		t.Fatalf("remove the setter: %v", err)
	}
	st.expectAsk(t, st.newRequest(t, st.site, "removed", nil).ID, aipolicy.AskSetterLacksPermission)
}

// B1: the setter can no longer approve this row by hand (an admin-rank
// operator_permission, chosen by an operator).
func TestDecideSetterLacksOperatorPermission(t *testing.T) {
	st := newASTStack(t)
	st.setSiteMode(t, st.site, st.operator.ID, "ai_drafts")
	st.allowConnection(t, st.admin.ID)
	req := st.newRequest(t, st.site, "admin-rank", func(p *sqlc.InsertAbilityRequestParams) {
		p.OperatorPermission = string(authz.PermSiteFilesWrite)
	})
	st.expectAsk(t, req.ID, aipolicy.AskSetterLacksPermission)
}

// D1: the person who allowed the connection is removed.
func TestDecideConnectionSetterRemoved(t *testing.T) {
	st := newASTStack(t)
	st.setSiteMode(t, st.site, st.operator.ID, "ai_drafts")
	st.allowConnection(t, st.admin.ID)
	if err := st.authRepo.DeleteMembership(context.Background(), st.admin.ID, st.tenant); err != nil {
		t.Fatalf("remove the connection setter: %v", err)
	}
	st.expectAsk(t, st.newRequest(t, st.site, "conn-removed", nil).ID, aipolicy.AskConnectionSetterInvalid)
}

// D1: the person who allowed the connection is demoted below apikey:manage,
// or is now a site collaborator whose share names admin (clamped to
// operator, and never a full member).
func TestDecideConnectionSetterDemotedOrSiteOnly(t *testing.T) {
	t.Run("demoted to operator", func(t *testing.T) {
		st := newASTStack(t)
		st.setSiteMode(t, st.site, st.operator.ID, "ai_drafts")
		st.allowConnection(t, st.admin.ID)
		if _, err := st.authRepo.UpdateMembershipRole(context.Background(), st.admin.ID, st.tenant, authz.RoleOperator); err != nil {
			t.Fatalf("demote the connection setter: %v", err)
		}
		st.expectAsk(t, st.newRequest(t, st.site, "conn-demoted", nil).ID, aipolicy.AskConnectionSetterInvalid)
	})
	t.Run("now a site collaborator", func(t *testing.T) {
		st := newASTStack(t)
		st.setSiteMode(t, st.site, st.operator.ID, "ai_drafts")
		st.allowConnection(t, st.admin.ID)
		if err := st.authRepo.DeleteMembership(context.Background(), st.admin.ID, st.tenant); err != nil {
			t.Fatalf("remove the connection setter's membership: %v", err)
		}
		admin := connectAdmin(t, st.pool)
		defer admin.Close()
		seedSiteShare(t, admin, st.tenant, st.site, st.admin.ID, "admin")
		st.expectAsk(t, st.newRequest(t, st.site, "conn-site-only", nil).ID, aipolicy.AskConnectionSetterInvalid)
	})
}

// D1: a person who can manage connections allows it again; the next draft
// runs.
func TestConnectionSwitchResaveRestoresAuto(t *testing.T) {
	st := newASTStack(t)
	st.setSiteMode(t, st.site, st.operator.ID, "ai_drafts")
	st.allowConnection(t, st.admin.ID)
	if _, err := st.authRepo.UpdateMembershipRole(context.Background(), st.admin.ID, st.tenant, authz.RoleOperator); err != nil {
		t.Fatalf("demote the connection setter: %v", err)
	}
	st.expectAsk(t, st.newRequest(t, st.site, "before-resave", nil).ID, aipolicy.AskConnectionSetterInvalid)
	st.allowConnection(t, st.owner.ID)
	if res := st.decide(t, st.newRequest(t, st.site, "after-resave", nil).ID); res.Outcome != aipolicy.OutcomeAutoBySetting {
		t.Fatalf("after the owner allowed it again: %+v, want approved by the setting", res)
	}
}

// D2(b): a disabled account keeps its membership but no longer counts.
func TestDisabledSetterAsks(t *testing.T) {
	for _, tc := range []struct {
		name string
		who  func(*astStack) uuid.UUID
		want aipolicy.AskReason
	}{
		{"site setter", func(st *astStack) uuid.UUID { return st.operator.ID }, aipolicy.AskSetterLacksPermission},
		{"connection setter", func(st *astStack) uuid.UUID { return st.admin.ID }, aipolicy.AskConnectionSetterInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newASTStack(t)
			st.setSiteMode(t, st.site, st.operator.ID, "ai_drafts")
			st.allowConnection(t, st.admin.ID)
			admin := connectAdmin(t, st.pool)
			defer admin.Close()
			if _, err := admin.Exec(context.Background(), `UPDATE users SET status = 'disabled' WHERE id = $1`, tc.who(st)); err != nil {
				t.Fatalf("disable the account: %v", err)
			}
			st.expectAsk(t, st.newRequest(t, st.site, "disabled-"+tc.name, nil).ID, tc.want)
		})
	}
}

// B6: deleting the setter's account is no foreign-key error, and the next
// request waits.
func TestSetterUserDeletedFallsToAsk(t *testing.T) {
	st := newASTStack(t)
	st.setSiteMode(t, st.site, st.operator.ID, "ai_drafts")
	st.allowConnection(t, st.admin.ID)
	admin := connectAdmin(t, st.pool)
	defer admin.Close()
	if _, err := admin.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, st.operator.ID); err != nil {
		t.Fatalf("delete the setter's account: %v", err)
	}
	st.expectAsk(t, st.newRequest(t, st.site, "deleted", nil).ID, aipolicy.AskSetterLacksPermission)
}

// §4.2 step 9: a request older than two minutes is never approved by the
// setting.
func TestUncheckedOldRowNotAutoApproved(t *testing.T) {
	st := newASTStack(t)
	st.setSiteMode(t, st.site, st.operator.ID, "ai_drafts")
	st.allowConnection(t, st.admin.ID)
	req := st.newRequest(t, st.site, "old", nil)
	admin := connectAdmin(t, st.pool)
	defer admin.Close()
	if _, err := admin.Exec(context.Background(),
		`UPDATE assistant_ability_requests SET created_at = now() - interval '3 minutes' WHERE id = $1`, req.ID); err != nil {
		t.Fatalf("age the request: %v", err)
	}
	st.expectAsk(t, req.ID, aipolicy.AskNotChecked)
}

// D4 (integration): an approval by a setting in a transaction that carries
// a user id is refused by the backstop for that reason alone. Both halves run
// the statement Decide runs (ApproveAbilityRequestByPolicy) with every value
// the site's setting holds: with the setter's user id in the transaction it
// is refused 42501 as an approval that must run with no user; the same
// statement with the same values and no user approves the request.
//
// Mutation: drop the backstop's "runs with no user in the transaction"
// clause (the approval with a user id is accepted).
func TestAutomaticApprovalWithUserSetRefused(t *testing.T) {
	st := newASTStack(t)
	st.setSiteMode(t, st.site, st.operator.ID, "ai_drafts")
	st.allowConnection(t, st.admin.ID)
	req := st.newRequest(t, st.site, "with-user", nil)
	ctx := context.Background()
	arg := st.policyApproval(t, st.site, req.ID)
	err := st.pool.InTenantTxAsUser(ctx, st.tenant, st.operator.ID, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTxAsUser (policy approval with a user)")
		_, err := sqlc.New(tx).ApproveAbilityRequestByPolicy(ctx, arg)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" ||
		!strings.Contains(pgErr.Message, "runs with no user in the transaction") {
		t.Fatalf("policy approval with app.user_id set: got %v, want 42501 (an approval by a setting runs with no user)", err)
	}
	t.Logf("refused: %s %s", pgErr.Code, pgErr.Message)
	if r := st.row(t, req.ID); r.state != "pending" {
		t.Fatalf("row state %s after the refused approval", r.state)
	}

	var got sqlc.AssistantAbilityRequest
	if err := st.pool.InTenantTx(ctx, st.tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (the same policy approval with no user)")
		var err error
		got, err = sqlc.New(tx).ApproveAbilityRequestByPolicy(ctx, arg)
		return err
	}); err != nil {
		t.Fatalf("the same approval with no user in the transaction: %v, want it approved", err)
	}
	if got.State != "approved" || got.ApprovalSource != "policy" || got.ApprovalModeSource == nil ||
		*got.ApprovalModeSource != "person" || got.DecidedByUserID.Valid {
		t.Fatalf("approved row %s/%s source %v decider %v; want approved/policy, source person, no decider",
			got.State, got.ApprovalSource, got.ApprovalModeSource, got.DecidedByUserID)
	}
}

// D4: a person's approval naming the setter, written from a transaction
// shaped like Decide's approving one (organisation-wide, no user), fails:
// that transaction carries no user, so it cannot pose as the person.
func TestDecideCannotWritePersonApproval(t *testing.T) {
	st := newASTStack(t)
	st.setSiteMode(t, st.site, st.operator.ID, "ai_drafts")
	st.allowConnection(t, st.admin.ID)
	req := st.newRequest(t, st.site, "person-in-decide", nil)
	ctx := context.Background()
	err := st.pool.InTenantTx(ctx, st.tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (person approval in the approving transaction)")
		_, err := sqlc.New(tx).ApproveAbilityRequest(ctx, sqlc.ApproveAbilityRequestParams{
			DecidedByUserID: st.operator.ID, DispatchWindowSeconds: 900, TenantID: st.tenant,
			ID: req.ID, SiteID: st.site, PresentedDigest: req.PresentedDigest,
		})
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("person approval with no user in the transaction: got %v, want 42501", err)
	}
	t.Logf("refused: %s %s", pgErr.Code, pgErr.Message)
}

// policyApproval is the compare-and-set Decide runs for request id on site,
// with every value the site's setting holds now, read as wpmgr_app.
func (st *astStack) policyApproval(t *testing.T, site, id uuid.UUID) sqlc.ApproveAbilityRequestByPolicyParams {
	t.Helper()
	ctx := context.Background()
	arg := sqlc.ApproveAbilityRequestByPolicyParams{
		BaseChangeClass: string(aipolicy.ClassAIDraft), ChangeClass: string(aipolicy.ClassAIDraft),
		DispatchWindowSeconds: 900, TenantID: st.tenant, ID: id, SiteID: site,
	}
	if err := st.pool.InTenantTx(ctx, st.tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (read the site's setting)")
		return tx.QueryRow(ctx, `SELECT ai_mode, ai_mode_source, ai_mode_version, ai_mode_set_by, ai_mode_set_at
			FROM sites WHERE tenant_id = $1 AND id = $2`, st.tenant, site).
			Scan(&arg.SiteMode, &arg.ModeSource, &arg.ModeVersion, &arg.SetterUserID, &arg.SetterSetAt)
	}); err != nil {
		t.Fatalf("read the setting of %s: %v", site, err)
	}
	return arg
}

// approveByPolicyDirect seeds approvals by a setting for the budget proofs:
// n pending rows on site, each approved by the statement Decide runs
// (ApproveAbilityRequestByPolicy) under the setting the site holds, in a
// transaction with no user, which the backstop checks row by row.
func (st *astStack) approveByPolicyDirect(t *testing.T, site uuid.UUID, n int, seed string) {
	t.Helper()
	ctx := context.Background()
	ids := make([]uuid.UUID, 0, n)
	if err := st.pool.RunTenantTx(ctx, acprSitePrincipal(st.tenant, site), func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		for i := 0; i < n; i++ {
			arg := aarParams(st.tenant, site, st.grant, fmt.Sprintf("%s-%d", seed, i))
			arg.EntryID = st.entry
			row, err := q.InsertAbilityRequest(ctx, arg)
			if err != nil {
				return err
			}
			ids = append(ids, row.ID)
		}
		return nil
	}); err != nil {
		t.Fatalf("seed %d requests on %s: %v", n, site, err)
	}
	arg := st.policyApproval(t, site, uuid.Nil)
	if err := st.pool.InTenantTx(ctx, st.tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (seed policy approvals)")
		q := sqlc.New(tx)
		for _, id := range ids {
			arg.ID = id
			if _, err := q.ApproveAbilityRequestByPolicy(ctx, arg); err != nil {
				return fmt.Errorf("approve %s: %w", id, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("approve %d requests by policy on %s: %v", n, site, err)
	}
}

// §11 regression 12: the 601st draft change in the window waits with
// over_change_budget and a resume time; the changes counted are on another
// site, so the count is per connection, not per site. The MCP path's proof
// is TestDraftBudget.
func TestDecideDraftBudgetIsPerConnection(t *testing.T) {
	st := newASTStack(t)
	st.setSiteMode(t, st.site, st.operator.ID, "ai_drafts")
	st.allowConnection(t, st.admin.ID)
	other := seedSite(t, st.pool, st.tenant, "")
	st.setSiteMode(t, other, st.operator.ID, "ai_drafts")
	start := time.Now()
	st.approveByPolicyDirect(t, other, aipolicy.DraftChangesPerConnection, "budget")
	t.Logf("seeded %d approvals by setting in %s", aipolicy.DraftChangesPerConnection, time.Since(start))
	req := st.newRequest(t, st.site, "601st", nil)
	res := st.decide(t, req.ID)
	if res.Outcome != aipolicy.OutcomeAsk || res.Ask != aipolicy.AskOverChangeBudget || res.ResumesAt.IsZero() {
		t.Fatalf("decision %+v, want ask over_change_budget with a resume time", res)
	}
	t.Logf("601st: ask_reason=%s resumes_at=%s", res.Ask, res.ResumesAt.UTC().Format(time.RFC3339))
}

// §11 regression 14 and the sites cap: a connection scoped to one site has
// approvals by setting on thirty other sites (made before its scope
// narrowed); the count is organisation-wide, so the next site waits with
// over_site_cap. Counted under the connection's site scope, the thirty
// would be invisible and the change would run.
func TestTenantCountIgnoresSiteScope(t *testing.T) {
	st := newASTStack(t)
	st.setSiteMode(t, st.site, st.operator.ID, "ai_drafts")
	st.allowConnection(t, st.admin.ID)
	for i := 0; i < aipolicy.DraftSitesPerConnection; i++ {
		s := seedSite(t, st.pool, st.tenant, "")
		st.setSiteMode(t, s, st.operator.ID, "ai_drafts")
		st.approveByPolicyDirect(t, s, 1, fmt.Sprintf("cap-%d", i))
	}
	req := st.newRequest(t, st.site, "31st-site", nil)
	st.expectAsk(t, req.ID, aipolicy.AskOverSiteCap)
}

// §11 regression 19: the Go matrix and ai_mode_allows agree on every pair,
// including the stored-only class and an unknown value.
func TestMatrixParity(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "at-matrix-"+uuid.NewString()[:8])
	ctx := context.Background()
	classes := append(aipolicy.StoredClasses(), "bogus")
	modes := append(aipolicy.Modes(), "bogus")
	checked := 0
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (matrix parity)")
		for _, m := range modes {
			for _, c := range classes {
				var got bool
				if err := tx.QueryRow(ctx, `SELECT ai_mode_allows($1, $2)`, string(m), string(c)).Scan(&got); err != nil {
					return err
				}
				if want := aipolicy.Allows(m, c); got != want {
					t.Errorf("ai_mode_allows(%s, %s) = %v, Go says %v", m, c, got, want)
				}
				checked++
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("matrix parity: %v", err)
	}
	if checked != len(modes)*len(classes) || checked == 0 {
		t.Fatalf("checked %d pairs", checked)
	}
	t.Logf("checked %d (mode, class) pairs", checked)
}

// TestResolveSetterReadsTheSettersAuthorityNow proves the production setter
// resolver on its own, as wpmgr_app: a member resolves with their role and
// an active account; a removed member resolves with no organisation; a site
// collaborator resolves site-scoped with the share clamped to operator; a
// disabled account keeps its membership but reads as disabled; a deleted
// account reads with no status and no organisation. None of these is a
// failed lookup, so each fails its check for what it is, not as not_checked.
//
// Mutation: read no account status in ResolveSetter; the owner's check then
// fails as account_not_active.
func TestResolveSetterReadsTheSettersAuthorityNow(t *testing.T) {
	st := newASTStack(t)
	ctx := context.Background()
	rec := audit.NewRecorder(st.pool, domain.SystemClock{})
	authn := middleware.NewAuthenticator(nil, auth.NewService(st.authRepo, rec, domain.NewValidator()), nil, st.pool)
	admin := connectAdmin(t, st.pool)
	defer admin.Close()

	owner := authn.ResolveSetter(ctx, st.tenant, st.owner.ID)
	if owner.LookupFailed || owner.AccountStatus != aipolicy.AccountActive || owner.Principal.TenantID != st.tenant ||
		owner.Principal.Scope != domain.ScopeOrg || owner.Principal.Role != string(authz.RoleOwner) {
		t.Fatalf("owner resolved as %+v", owner)
	}
	if c := aipolicy.CheckConnectionSetter(owner, st.tenant); !c.OK {
		t.Fatalf("owner's connection check %+v, want ok", c)
	}

	if err := st.authRepo.DeleteMembership(ctx, st.operator.ID, st.tenant); err != nil {
		t.Fatalf("remove the operator: %v", err)
	}
	removed := authn.ResolveSetter(ctx, st.tenant, st.operator.ID)
	if removed.LookupFailed || removed.Principal.TenantID != uuid.Nil || removed.AccountStatus != aipolicy.AccountActive {
		t.Fatalf("removed member resolved as %+v", removed)
	}
	if c := aipolicy.CheckSiteSetter(removed, st.tenant, st.site, aipolicy.ModeAIDrafts, string(authz.PermSiteContentEdit)); c.OK || c.Failed != aipolicy.FailedNotMember {
		t.Fatalf("removed member's site check %+v, want failed as %s", c, aipolicy.FailedNotMember)
	}

	seedSiteShare(t, admin, st.tenant, st.site, st.operator.ID, "admin")
	collab := authn.ResolveSetter(ctx, st.tenant, st.operator.ID)
	if p := collab.Principal; collab.LookupFailed || p.TenantID != st.tenant || p.Scope != domain.ScopeSite ||
		p.Role != string(authz.RoleOperator) || len(p.AllowedSiteIDs) != 1 || p.AllowedSiteIDs[0] != st.site {
		t.Fatalf("site collaborator resolved as %+v", collab)
	}
	if c := aipolicy.CheckConnectionSetter(collab, st.tenant); c.OK || c.Failed != aipolicy.FailedOrgScope {
		t.Fatalf("site collaborator's connection check %+v, want failed as %s", c, aipolicy.FailedOrgScope)
	}

	if _, err := admin.Exec(ctx, `UPDATE users SET status = 'disabled' WHERE id = $1`, st.admin.ID); err != nil {
		t.Fatalf("disable the admin: %v", err)
	}
	disabled := authn.ResolveSetter(ctx, st.tenant, st.admin.ID)
	if disabled.LookupFailed || disabled.AccountStatus != "disabled" || disabled.Principal.TenantID != st.tenant {
		t.Fatalf("disabled admin resolved as %+v", disabled)
	}
	if c := aipolicy.CheckConnectionSetter(disabled, st.tenant); c.OK || c.Failed != aipolicy.FailedAccountStatus {
		t.Fatalf("disabled admin's connection check %+v, want failed as %s", c, aipolicy.FailedAccountStatus)
	}

	gone := seedUserMembership(t, st.authRepo, "gone-"+uuid.NewString()[:8]+"@example.com", st.tenant, authz.RoleAdmin)
	if _, err := admin.Exec(ctx, `DELETE FROM users WHERE id = $1`, gone.ID); err != nil {
		t.Fatalf("delete the account: %v", err)
	}
	deleted := authn.ResolveSetter(ctx, st.tenant, gone.ID)
	if deleted.LookupFailed || deleted.AccountStatus != "" || deleted.Principal.TenantID != uuid.Nil {
		t.Fatalf("deleted account resolved as %+v", deleted)
	}
	t.Logf("owner=%s/%s removed=%v collaborator=%s/%s disabled=%s deleted=%q",
		owner.Principal.Scope, owner.Principal.Role, removed.Principal.TenantID == uuid.Nil,
		collab.Principal.Scope, collab.Principal.Role, disabled.AccountStatus, deleted.AccountStatus)
}
