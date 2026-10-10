// m174 database proofs for approval tiers, Merge A: the approving mode's
// source on the request, the raw checked target status written at insert,
// the connection switch a new connection starts on, and the trust queries in
// db/query/ai_trust.sql.
//
// Built as the m151 and m156 proofs are: every statement under test is the
// generated sqlc method, run through the production dispatch (RunTenantTx,
// or InTenantTx where the shipped caller uses it), and every transaction
// under test asserts from inside that it is wpmgr_app with neither SUPERUSER
// nor BYPASSRLS. The bootstrap superuser is used only to seed users and to
// put rows in states only the migration produces, and to show a CHECK holds
// with the triggers out of the way.
//
// Mutations each test is built to catch are named in its doc comment.
package tests

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

func m174Code(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

func m174User(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	admin := connectAdmin(t, pool)
	defer admin.Close()
	return seedUserRow(t, admin, "m174-"+uuid.NewString()[:12]+"@example.com")
}

func m174UserPrincipal(tenant, user uuid.UUID) domain.Principal {
	return acprDeciderPrincipal(tenant, user)
}

func m174GrantParams(tenant uuid.UUID, creator *uuid.UUID, byCreator bool) sqlc.CreateMCPGrantParams {
	var by pgtype.UUID
	if creator != nil {
		by = m174UUID(*creator)
	}
	return sqlc.CreateMCPGrantParams{
		TenantID:        tenant,
		Name:            "m174 connection",
		Status:          "active",
		SiteScopeMode:   "all",
		ScopeTagIds:     []uuid.UUID{},
		ScopeSiteIds:    []uuid.UUID{},
		CreatedByUserID: by,
		Capabilities:    []string{"mcp.sites.read"},
		OauthScopes:     []string{"mcp:read"},
		ExpiresAt:       time.Now().UTC().Add(30 * 24 * time.Hour),
		AiAutoByCreator: byCreator,
	}
}

// m174CreateGrant runs the shipped CreateMCPGrant under p and returns its
// error unchanged.
func m174CreateGrant(t *testing.T, pool *db.Pool, p domain.Principal, arg sqlc.CreateMCPGrantParams) (sqlc.McpGrant, error) {
	t.Helper()
	var out sqlc.McpGrant
	err := pool.RunTenantTx(context.Background(), p, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (m174 create grant)")
		var err error
		out, err = sqlc.New(tx).CreateMCPGrant(context.Background(), arg)
		return err
	})
	return out, err
}

// m174EnableAI turns AI editing on for a site as user, and gives it the
// enable default, in one transaction, as the enable action does.
func m174EnableAI(t *testing.T, pool *db.Pool, tenant, site, user uuid.UUID) sqlc.ApplySiteAIModeEnableDefaultRow {
	t.Helper()
	var out sqlc.ApplySiteAIModeEnableDefaultRow
	if err := pool.RunTenantTx(context.Background(), m174UserPrincipal(tenant, user), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (m174 enable)")
		q := sqlc.New(tx)
		if _, err := q.MarkSiteContentEditingEnabled(context.Background(), sqlc.MarkSiteContentEditingEnabledParams{
			PrincipalUserID: 7, EnabledBy: user, SiteID: site, TenantID: tenant,
		}); err != nil {
			return err
		}
		var err error
		out, err = q.ApplySiteAIModeEnableDefault(context.Background(), sqlc.ApplySiteAIModeEnableDefaultParams{
			EnabledBy: user, TenantID: tenant, SiteID: site,
		})
		return err
	}); err != nil {
		t.Fatalf("enable AI editing on site %s: %v", site, err)
	}
	return out
}

// m174ApproveByPolicy runs the shipped compare-and-set in the transaction
// the decision engine uses: organisation-wide, no user.
func m174ApproveByPolicy(t *testing.T, pool *db.Pool, arg sqlc.ApproveAbilityRequestByPolicyParams) (sqlc.AssistantAbilityRequest, error) {
	t.Helper()
	var out sqlc.AssistantAbilityRequest
	err := pool.InTenantTx(context.Background(), arg.TenantID, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (m174 approve by policy)")
		var err error
		out, err = sqlc.New(tx).ApproveAbilityRequestByPolicy(context.Background(), arg)
		return err
	})
	return out, err
}

func m174SetMode(t *testing.T, pool *db.Pool, p domain.Principal, arg sqlc.SetSiteAIModeParams) (sqlc.SetSiteAIModeRow, error) {
	t.Helper()
	var out sqlc.SetSiteAIModeRow
	err := pool.RunTenantTx(context.Background(), p, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (m174 set mode)")
		var err error
		out, err = sqlc.New(tx).SetSiteAIMode(context.Background(), arg)
		return err
	})
	return out, err
}

func m174UUID(u uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: u, Valid: true} }

// TestM174PolicyApprovalRecordsModeSource proves an approval by a site's
// setting records how that setting was chosen, that the record must be the
// site's current source, and that only a policy approval carries one.
//
// Mutations: drop "s.ai_mode_source = sqlc.arg(mode_source)" from
// ApproveAbilityRequestByPolicy (the wrong-source CAS then meets the backstop
// and returns 42501, not no row); drop "s.ai_mode_source =
// NEW.approval_mode_source" from ai_approval_backstop (the raw UPDATE with a
// wrong source is then accepted); drop "approval_mode_source IS NOT NULL"
// from assistant_ability_requests_policy_approval_shape_check or from
// assistant_cache_purge_requests_policy_approval_shape_check (that table's
// trigger-less UPDATE is then accepted).
func TestM174PolicyApprovalRecordsModeSource(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)

	tenant := seedTenant(t, pool, "m174-src-"+uuid.NewString()[:8])
	site := seedSite(t, pool, tenant, "")
	user := m174User(t, pool)

	grant, err := m174CreateGrant(t, pool, m174UserPrincipal(tenant, user), m174GrantParams(tenant, &user, true))
	if err != nil {
		t.Fatalf("create person connection: %v", err)
	}
	mode := m174EnableAI(t, pool, tenant, site, user)
	if mode.AiMode != "ai_drafts" || mode.AiModeSource != "enable_default" || mode.AiModeVersion != 1 {
		t.Fatalf("enable default: got %s/%s v%d, want ai_drafts/enable_default v1", mode.AiMode, mode.AiModeSource, mode.AiModeVersion)
	}

	req := aarInsert(t, pool, acprSitePrincipal(tenant, site), aarParams(tenant, site, grant.ID, "src-1"))
	base := sqlc.ApproveAbilityRequestByPolicyParams{
		SiteMode: "ai_drafts", ModeSource: "enable_default", ModeVersion: mode.AiModeVersion,
		SetterUserID: user, SetterSetAt: mode.AiModeSetAt.Time,
		BaseChangeClass: "ai_draft", ChangeClass: "ai_draft", DispatchWindowSeconds: 600,
		TenantID: tenant, ID: req.ID, SiteID: site,
	}

	// RED 1: the compare-and-set naming a source the site does not carry
	// approves nothing.
	wrong := base
	wrong.ModeSource = "person"
	if _, err := m174ApproveByPolicy(t, pool, wrong); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("approval naming the wrong mode source: err = %v (code %s), want no row", err, m174Code(err))
	}

	// RED 2 and 3: a statement without the compare-and-set's predicate meets
	// the backstop, whether it records the wrong source or none.
	for _, src := range []*string{acprStr("person"), nil} {
		err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "InTenantTx (m174 raw policy approval)")
			_, err := tx.Exec(ctx, `
UPDATE assistant_ability_requests
SET state = 'approved', approval_source = 'policy', approval_site_mode = 'ai_drafts',
    approval_mode_source = $3, approval_mode_version = $4, approval_setter_user_id = $5,
    approval_setter_set_at = $6, base_change_class = 'ai_draft', change_class = 'ai_draft',
    decided_at = now(), dispatch_deadline_at = now() + interval '10 minutes',
    policy_checked_at = now()
WHERE tenant_id = $1 AND id = $2`, tenant, req.ID, src, mode.AiModeVersion, user, mode.AiModeSetAt.Time)
			return err
		})
		if m174Code(err) != "42501" {
			t.Fatalf("raw policy approval with mode source %v: err = %v (code %s), want 42501 from ai_approval_backstop", src, err, m174Code(err))
		}
	}

	// GREEN: the source the site carries.
	got, err := m174ApproveByPolicy(t, pool, base)
	if err != nil {
		t.Fatalf("approval by the site's setting: %v", err)
	}
	if got.State != "approved" || got.ApprovalSource != "policy" || got.ApprovalModeSource == nil ||
		*got.ApprovalModeSource != "enable_default" || got.DecidedByUserID.Valid {
		t.Fatalf("approved row: state %s source %s mode source %v decider %v; want approved/policy/enable_default/none",
			got.State, got.ApprovalSource, got.ApprovalModeSource, got.DecidedByUserID)
	}

	// The record does not move after the approval.
	err = pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (m174 move mode source)")
		_, err := tx.Exec(ctx, `UPDATE assistant_ability_requests SET approval_mode_source = 'person' WHERE tenant_id = $1 AND id = $2`, tenant, req.ID)
		return err
	})
	if m174Code(err) != "55000" {
		t.Fatalf("moving an approved row's mode source: err = %v (code %s), want 55000", err, m174Code(err))
	}

	// The connection's usage counts it, and only in its classes.
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (m174 usage)")
		q := sqlc.New(tx)
		drafts, err := q.CountAIConnectionPolicyApprovals(ctx, sqlc.CountAIConnectionPolicyApprovalsParams{
			TenantID: tenant, GrantID: grant.ID, Classes: []string{"ai_draft", "unpublished"}, WindowSeconds: 3600,
		})
		if err != nil {
			return err
		}
		if drafts.Changes != 1 || drafts.Sites != 1 {
			t.Fatalf("draft usage = %d changes on %d sites, want 1 on 1", drafts.Changes, drafts.Sites)
		}
		visible, err := q.CountAIConnectionPolicyApprovals(ctx, sqlc.CountAIConnectionPolicyApprovalsParams{
			TenantID: tenant, GrantID: grant.ID, Classes: []string{"live"}, WindowSeconds: 3600,
		})
		if err != nil {
			return err
		}
		if visible.Changes != 0 {
			t.Fatalf("visible usage = %d, want 0", visible.Changes)
		}
		return nil
	}); err != nil {
		t.Fatalf("usage: %v", err)
	}

	// The CHECKs hold with the triggers out of the way, on both request
	// tables: an approved policy row with no mode source is refused by that
	// table's policy shape CHECK, and a person row with a mode source by the
	// mode source CHECK. Each refusal must come from the named constraint: a
	// refusal from another CHECK would leave the named one unproven. Each case
	// has its own pending row, so one case's outcome cannot decide another's.
	pending := aarInsert(t, pool, acprSitePrincipal(tenant, site), aarParams(tenant, site, grant.ID, "src-2"))
	pendingPerson := aarInsert(t, pool, acprSitePrincipal(tenant, site), aarParams(tenant, site, grant.ID, "src-4"))
	purge := acprInsert(t, pool, acprSitePrincipal(tenant, site), acprParams(tenant, site, grant.ID, "src-3"))
	admin := connectAdmin(t, pool)
	defer admin.Close()
	for _, c := range []struct {
		what, constraint, stmt string
		id                     uuid.UUID
	}{
		{
			what:       "site change: policy approval with no mode source",
			constraint: "assistant_ability_requests_policy_approval_shape_check",
			id:         pending.ID,
			stmt: `
UPDATE assistant_ability_requests
SET state = 'approved', approval_source = 'policy', approval_site_mode = 'ai_drafts',
    approval_mode_version = 1, approval_setter_user_id = $2, approval_setter_set_at = now(),
    base_change_class = 'ai_draft', change_class = 'ai_draft', decided_at = now(),
    dispatch_deadline_at = now() + interval '10 minutes', policy_checked_at = now()
WHERE id = $1`,
		},
		{
			what:       "site change: person row with a mode source",
			constraint: "assistant_ability_requests_approval_mode_source_check",
			id:         pendingPerson.ID,
			stmt: `
UPDATE assistant_ability_requests SET approval_mode_source = 'person' WHERE id = $1 AND $2::uuid IS NOT NULL`,
		},
		{
			what:       "cache clear: policy approval with no mode source",
			constraint: "assistant_cache_purge_requests_policy_approval_shape_check",
			id:         purge.ID,
			stmt: `
UPDATE assistant_cache_purge_requests
SET state = 'approved_undispatched', approval_source = 'policy', approval_site_mode = 'ai_drafts',
    approval_mode_version = 1, approval_setter_user_id = $2, approval_setter_set_at = now(),
    base_change_class = 'operational', change_class = 'operational', decided_at = now(),
    policy_checked_at = now()
WHERE id = $1`,
		},
	} {
		err := admin.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, c.stmt, c.id, user)
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != c.constraint {
			t.Errorf("%s: err = %v (code %s); want 23514 at %s", c.what, err, m174Code(err), c.constraint)
			continue
		}
		t.Logf("ok   %s: refused 23514 at %s", c.what, pgErr.ConstraintName)
	}
}

// TestM174SeedClassesPageEditAsAIDraft proves that once every migration has
// run, the catalogue as the trust page reads it (the shipped
// ListAdmittedAbilityCatalogue, as wpmgr_app) has wpmgr/page-edit and
// wpmgr/page-create classed ai_draft, and wpmgr/page-structure, a read, on
// the default always_ask. It also proves a superadmin cannot class
// page-structure ai_draft: it has no undo, and
// ability_catalogue_change_class_snapshot_check refuses it.
//
// Mutations: drop 'wpmgr/page-edit' from m174's seed UPDATE (page-edit reads
// always_ask); give m174 an ordinal that sorts before m169's (m169 inserts
// page-edit after the seed ran, and it reads always_ask); drop
// ability_catalogue_change_class_snapshot_check (the re-class of
// page-structure is accepted).
func TestM174SeedClassesPageEditAsAIDraft(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m174-seed-"+uuid.NewString()[:8])

	var entries []sqlc.AbilityCatalogue
	if err := pool.RunTenantTx(ctx, acprOrgPrincipal(tenant), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (m174 read catalogue)")
		var err error
		entries, err = sqlc.New(tx).ListAdmittedAbilityCatalogue(ctx)
		return err
	}); err != nil {
		t.Fatalf("read the admitted catalogue: %v", err)
	}
	byName := map[string]sqlc.AbilityCatalogue{}
	for _, e := range entries {
		if e.Source == "wpmgr" {
			byName[e.Name] = e
		}
	}
	for _, want := range []struct{ name, class, changeClass string }{
		{"wpmgr/page-edit", "write", "ai_draft"},
		{"wpmgr/page-create", "write", "ai_draft"},
		{"wpmgr/page-structure", "read", "always_ask"},
	} {
		e, ok := byName[want.name]
		if !ok {
			t.Errorf("%s: not in the admitted catalogue", want.name)
			continue
		}
		if e.Class != want.class || e.ChangeClass != want.changeClass {
			t.Errorf("%s: class %s, change_class %s; want %s, %s", want.name, e.Class, e.ChangeClass, want.class, want.changeClass)
			continue
		}
		t.Logf("ok   %s: class %s, change_class %s", want.name, e.Class, e.ChangeClass)
	}

	structure, ok := byName["wpmgr/page-structure"]
	if !ok {
		t.Fatalf("wpmgr/page-structure is not in the admitted catalogue")
	}
	actor := m174User(t, pool)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	if _, err := admin.Exec(ctx, `UPDATE users SET is_superadmin = true WHERE id = $1`, actor); err != nil {
		t.Fatalf("make the actor a superadmin: %v", err)
	}
	err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (m174 re-class page-structure)")
		var out string
		return tx.QueryRow(ctx, `SELECT set_ability_change_class($1, 'ability', $2, 'ai_draft')`,
			actor, structure.EntryID.String()).Scan(&out)
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "ability_catalogue_change_class_snapshot_check" {
		t.Errorf("a superadmin classing wpmgr/page-structure ai_draft: err = %v (code %s); want 23514 at ability_catalogue_change_class_snapshot_check", err, m174Code(err))
	} else {
		t.Logf("ok   a superadmin classing wpmgr/page-structure ai_draft: refused 23514 at %s", pgErr.ConstraintName)
	}
}

// TestM174CheckedTargetStatusIsWrittenAtInsertOnly proves InsertAbilityRequest
// stores the raw checked status exactly, NULL when there is none, and that
// wpmgr_app cannot change it afterwards.
//
// Mutation: drop checked_target_status from InsertAbilityRequest (the stored
// value reads NULL).
func TestM174CheckedTargetStatusIsWrittenAtInsertOnly(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m174-cts-"+uuid.NewString()[:8])
	site := seedSite(t, pool, tenant, "")

	withStatus := aarParams(tenant, site, uuid.New(), "cts-1")
	withStatus.CheckedTargetStatus = acprStr("draft ")
	row := aarInsert(t, pool, acprSitePrincipal(tenant, site), withStatus)
	if row.CheckedTargetStatus == nil || *row.CheckedTargetStatus != "draft " {
		t.Fatalf("checked_target_status = %v, want exactly %q", row.CheckedTargetStatus, "draft ")
	}
	none := aarInsert(t, pool, acprSitePrincipal(tenant, site), aarParams(tenant, site, uuid.New(), "cts-2"))
	if none.CheckedTargetStatus != nil {
		t.Fatalf("checked_target_status with none given = %q, want NULL", *none.CheckedTargetStatus)
	}

	err := pool.RunTenantTx(ctx, acprSitePrincipal(tenant, site), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (m174 move checked status)")
		_, err := tx.Exec(ctx, `UPDATE assistant_ability_requests SET checked_target_status = 'publish' WHERE tenant_id = $1 AND id = $2`, tenant, row.ID)
		return err
	})
	if m174Code(err) != "42501" {
		t.Fatalf("changing checked_target_status: err = %v (code %s), want 42501", err, m174Code(err))
	}
}

// TestM174ConnectionStartsOnTheCreatorsChoice proves a person-created
// connection starts on site_setting with its creator as setter, a key-minted
// one stays never, and the database refuses site_setting unless the
// signed-in person is the creator it records.
//
// Mutations: make the ai_auto CASE always 'never' (the person case fails);
// drop mcp_grants_ai_auto_guard (the no-user case is accepted).
func TestM174ConnectionStartsOnTheCreatorsChoice(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m174-grant-"+uuid.NewString()[:8])
	user := m174User(t, pool)

	person, err := m174CreateGrant(t, pool, m174UserPrincipal(tenant, user), m174GrantParams(tenant, &user, true))
	if err != nil {
		t.Fatalf("person-created connection: %v", err)
	}
	if person.AiAuto != "site_setting" || !person.AiAutoSetBy.Valid || uuid.UUID(person.AiAutoSetBy.Bytes) != user || !person.AiAutoSetAt.Valid {
		t.Fatalf("person-created connection: ai_auto %s set_by %v set_at %v; want site_setting by %s", person.AiAuto, person.AiAutoSetBy, person.AiAutoSetAt, user)
	}

	key, err := m174CreateGrant(t, pool, acprOrgPrincipal(tenant), m174GrantParams(tenant, nil, false))
	if err != nil {
		t.Fatalf("key-minted connection: %v", err)
	}
	if key.AiAuto != "never" || key.AiAutoSetBy.Valid || key.AiAutoSetAt.Valid {
		t.Fatalf("key-minted connection: ai_auto %s set_by %v set_at %v; want never, none", key.AiAuto, key.AiAutoSetBy, key.AiAutoSetAt)
	}

	if _, err := m174CreateGrant(t, pool, acprOrgPrincipal(tenant), m174GrantParams(tenant, &user, true)); m174Code(err) != "42501" {
		t.Fatalf("site_setting with no signed-in person: err = %v (code %s), want 42501", err, m174Code(err))
	}
	// With no creator the setter is NULL, which is not the signed-in person:
	// the guard refuses it before mcp_grants_ai_auto_names_setter_check would.
	if _, err := m174CreateGrant(t, pool, m174UserPrincipal(tenant, user), m174GrantParams(tenant, nil, true)); m174Code(err) != "42501" {
		t.Fatalf("site_setting with no creator: err = %v (code %s), want 42501", err, m174Code(err))
	}
}

// TestM174SiteModeCompareAndSet proves SetSiteAIMode applies on the stored
// version, reports a stale version with the stored values instead of
// writing, treats saving the stored setting as no change, and is invisible
// across tenants.
//
// Mutation: drop "s.ai_mode_version = sqlc.arg(expected_version)" (the stale
// write then applies).
func TestM174SiteModeCompareAndSet(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m174-cas-"+uuid.NewString()[:8])
	other := seedTenant(t, pool, "m174-cas-other-"+uuid.NewString()[:8])
	site := seedSite(t, pool, tenant, "")
	alice := m174User(t, pool)
	bob := m174User(t, pool)

	set := func(p domain.Principal, mode, source string, by *uuid.UUID, expected int64) (sqlc.SetSiteAIModeRow, error) {
		arg := sqlc.SetSiteAIModeParams{Mode: mode, Source: source, TenantID: tenant, SiteID: site, ExpectedVersion: expected}
		if by != nil {
			arg.SetBy = m174UUID(*by)
		}
		return m174SetMode(t, pool, p, arg)
	}

	r, err := set(m174UserPrincipal(tenant, alice), "ai_drafts", "person", &alice, 0)
	if err != nil || !r.Applied || r.AiModeVersion != 1 || r.AiMode != "ai_drafts" {
		t.Fatalf("first choice: %+v, %v; want applied ai_drafts v1", r, err)
	}
	firstSetAt := r.AiModeSetAt

	r, err = set(m174UserPrincipal(tenant, bob), "ask", "person", &bob, 0)
	if err != nil || r.Applied || r.AiModeVersion != 1 || r.AiMode != "ai_drafts" {
		t.Fatalf("stale version: %+v, %v; want not applied, stored ai_drafts v1", r, err)
	}

	r, err = set(m174UserPrincipal(tenant, alice), "ai_drafts", "person", &alice, 1)
	if err != nil || !r.Applied || r.AiModeVersion != 1 || !r.AiModeSetAt.Time.Equal(firstSetAt.Time) {
		t.Fatalf("saving the stored setting: %+v, %v; want applied with no change at v1", r, err)
	}

	r, err = set(m174UserPrincipal(tenant, bob), "ai_drafts", "person", &bob, 1)
	if err != nil || !r.Applied || r.AiModeVersion != 2 || uuid.UUID(r.AiModeSetBy.Bytes) != bob {
		t.Fatalf("another person choosing again: %+v, %v; want applied v2 by bob", r, err)
	}

	r, err = set(acprOrgPrincipal(tenant), "ask", "tightened", nil, 2)
	if err != nil || !r.Applied || r.AiModeVersion != 3 || r.AiMode != "ask" || r.AiModeSetBy.Valid {
		t.Fatalf("lowering with no person: %+v, %v; want applied ask v3 with no setter", r, err)
	}

	if _, err := set(acprOrgPrincipal(tenant), "ai_drafts", "person", &alice, 3); m174Code(err) != "42501" {
		t.Fatalf("raising with no person: err = %v (code %s), want 42501", err, m174Code(err))
	}

	arg := sqlc.SetSiteAIModeParams{Mode: "ask", Source: "tightened", TenantID: other, SiteID: site, ExpectedVersion: 3}
	if _, err := m174SetMode(t, pool, acprOrgPrincipal(other), arg); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("another tenant's site: err = %v, want no row", err)
	}

	if err := pool.RunTenantTx(ctx, acprOrgPrincipal(tenant), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (m174 read mode)")
		got, err := sqlc.New(tx).GetSiteAIMode(ctx, sqlc.GetSiteAIModeParams{TenantID: tenant, SiteID: site})
		if err != nil {
			return err
		}
		if got.AiMode != "ask" || got.AiModeSource != "tightened" || got.AiModeVersion != 3 || got.SetByName != nil || got.SetByAccountDeleted {
			t.Fatalf("read back: %+v; want ask/tightened v3, no setter", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("read mode: %v", err)
	}
}

// TestM174EnableDefaultOnlyFromUnset proves the enable default moves only a
// site no one has set, with AI editing on.
//
// Mutation: drop "ai_mode_source = 'unset'" (turning AI editing on again
// then meets sites_ai_mode_guard and errors instead of keeping the choice).
func TestM174EnableDefaultOnlyFromUnset(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m174-en-"+uuid.NewString()[:8])
	site := seedSite(t, pool, tenant, "")
	off := seedSite(t, pool, tenant, "")
	user := m174User(t, pool)

	m174EnableAI(t, pool, tenant, site, user)
	if r, err := m174SetMode(t, pool, m174UserPrincipal(tenant, user), sqlc.SetSiteAIModeParams{
		Mode: "ask", Source: "person", SetBy: m174UUID(user), TenantID: tenant, SiteID: site, ExpectedVersion: 1,
	}); err != nil || !r.Applied || r.AiModeVersion != 2 {
		t.Fatalf("person lowers to ask: %+v, %v", r, err)
	}

	err := pool.RunTenantTx(ctx, m174UserPrincipal(tenant, user), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (m174 enable again)")
		q := sqlc.New(tx)
		if _, err := q.ApplySiteAIModeEnableDefault(ctx, sqlc.ApplySiteAIModeEnableDefaultParams{EnabledBy: user, TenantID: tenant, SiteID: site}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("enable default on a site a person set: err = %v, want no row", err)
		}
		if _, err := q.ApplySiteAIModeEnableDefault(ctx, sqlc.ApplySiteAIModeEnableDefaultParams{EnabledBy: user, TenantID: tenant, SiteID: off}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("enable default on a site with AI editing off: err = %v, want no row", err)
		}
		got, err := q.GetSiteAIMode(ctx, sqlc.GetSiteAIModeParams{TenantID: tenant, SiteID: site})
		if err != nil {
			return err
		}
		if got.AiMode != "ask" || got.AiModeSource != "person" || got.AiModeVersion != 2 {
			t.Fatalf("after enabling again: %s/%s v%d, want ask/person v2", got.AiMode, got.AiModeSource, got.AiModeVersion)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("enable again: %v", err)
	}
}

// TestM174LaunchNoticeClaimIsPerTenantAndReleasable proves the claim takes
// only its own tenant's unnotified launch-default sites, takes them once,
// and that a release of that claim makes them claimable again.
//
// Mutation: drop "ai_mode_launch_emailed_at IS NULL" from the claim (the
// second claim then returns the sites again).
func TestM174LaunchNoticeClaimIsPerTenantAndReleasable(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	user := m174User(t, pool)
	tenants := []uuid.UUID{
		seedTenant(t, pool, "m174-ln-a-"+uuid.NewString()[:8]),
		seedTenant(t, pool, "m174-ln-b-"+uuid.NewString()[:8]),
	}
	admin := connectAdmin(t, pool)
	defer admin.Close()
	for _, tenant := range tenants {
		for i := 0; i < 2; i++ {
			site := seedSite(t, pool, tenant, "")
			// The state m174's backfill leaves: only the migration writes
			// launch_default, so it is seeded with the triggers out of the way.
			if err := admin.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `
UPDATE sites SET content_editing_enabled_at = now(), content_editing_enabled_by = $2,
    content_editing_principal_user_id = 7,
    ai_mode = 'ai_drafts', ai_mode_source = 'launch_default', ai_mode_set_by = $2,
    ai_mode_set_at = now(), ai_mode_version = 1
WHERE id = $1`, site, user)
				return err
			}); err != nil {
				t.Fatalf("seed launch default: %v", err)
			}
		}
	}

	claim := func(tenant uuid.UUID) []sqlc.ClaimSiteAILaunchNoticesRow {
		var rows []sqlc.ClaimSiteAILaunchNoticesRow
		if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "InTenantTx (m174 claim)")
			var err error
			rows, err = sqlc.New(tx).ClaimSiteAILaunchNotices(ctx, tenant)
			return err
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		return rows
	}

	a := claim(tenants[0])
	if len(a) != 2 || !a[0].AiModeLaunchEmailedAt.Valid || !a[0].AiModeLaunchEmailedAt.Time.Equal(a[1].AiModeLaunchEmailedAt.Time) {
		t.Fatalf("first claim in tenant A: %+v; want its 2 sites under one stamp", a)
	}
	if again := claim(tenants[0]); len(again) != 0 {
		t.Fatalf("second claim in tenant A: %d sites, want 0", len(again))
	}
	if b := claim(tenants[1]); len(b) != 2 {
		t.Fatalf("claim in tenant B: %d sites, want its 2", len(b))
	}

	release := func(stamp time.Time) int64 {
		var n int64
		if err := pool.InTenantTx(ctx, tenants[0], func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "InTenantTx (m174 release)")
			var err error
			n, err = sqlc.New(tx).ReleaseSiteAILaunchNotices(ctx, sqlc.ReleaseSiteAILaunchNoticesParams{
				TenantID: tenants[0], SiteIds: []uuid.UUID{a[0].ID, a[1].ID}, ClaimedAt: stamp,
			})
			return err
		}); err != nil {
			t.Fatalf("release: %v", err)
		}
		return n
	}
	if n := release(a[0].AiModeLaunchEmailedAt.Time.Add(time.Second)); n != 0 {
		t.Fatalf("release with another claim's stamp cleared %d sites, want 0", n)
	}
	if n := release(a[0].AiModeLaunchEmailedAt.Time); n != 2 {
		t.Fatalf("release of the failed claim cleared %d sites, want 2", n)
	}
	if retry := claim(tenants[0]); len(retry) != 2 {
		t.Fatalf("claim after a release: %d sites, want 2", len(retry))
	}
}

// TestM174ConnectionSwitchWrite proves SetAIConnectionAuto records the
// signed-in person on site_setting, records no one on never, refuses
// site_setting without that person, and writes no revoked connection.
//
// Mutation: drop "status = 'active'" (the revoked connection is written).
func TestM174ConnectionSwitchWrite(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m174-sw-"+uuid.NewString()[:8])
	user := m174User(t, pool)
	grant, err := m174CreateGrant(t, pool, acprOrgPrincipal(tenant), m174GrantParams(tenant, nil, false))
	if err != nil {
		t.Fatalf("key-minted connection: %v", err)
	}

	write := func(p domain.Principal, auto string, by *uuid.UUID) (sqlc.SetAIConnectionAutoRow, error) {
		arg := sqlc.SetAIConnectionAutoParams{AiAuto: auto, TenantID: tenant, GrantID: grant.ID}
		if by != nil {
			arg.SetBy = m174UUID(*by)
		}
		var out sqlc.SetAIConnectionAutoRow
		err := pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "RunTenantTx (m174 switch)")
			var err error
			out, err = sqlc.New(tx).SetAIConnectionAuto(ctx, arg)
			return err
		})
		return out, err
	}

	if _, err := write(acprOrgPrincipal(tenant), "site_setting", &user); m174Code(err) != "42501" {
		t.Fatalf("allowing with no signed-in person: err = %v (code %s), want 42501", err, m174Code(err))
	}
	on, err := write(m174UserPrincipal(tenant, user), "site_setting", &user)
	if err != nil || on.AiAuto != "site_setting" || uuid.UUID(on.AiAutoSetBy.Bytes) != user {
		t.Fatalf("allowing as the person: %+v, %v", on, err)
	}
	off, err := write(acprOrgPrincipal(tenant), "never", &user)
	if err != nil || off.AiAuto != "never" || off.AiAutoSetBy.Valid {
		t.Fatalf("stopping with no person: %+v, %v; want never with no setter", off, err)
	}

	admin := connectAdmin(t, pool)
	defer admin.Close()
	if _, err := admin.Exec(ctx, `UPDATE mcp_grants SET status = 'revoked', revoked_at = now() WHERE id = $1`, grant.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := write(acprOrgPrincipal(tenant), "never", nil); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("writing a revoked connection: err = %v, want no row", err)
	}
}

// TestM174ActivityPageMergesBothTablesByKeyset proves the activity page lists
// only approved requests from both tables, newest first, continues by keyset
// without repeats, filters, and is narrowed by a site collaborator's scope.
//
// Mutation: drop the state filter from the ability branch (the waiting row
// is then listed).
func TestM174ActivityPageMergesBothTablesByKeyset(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m174-act-"+uuid.NewString()[:8])
	s1 := seedSite(t, pool, tenant, "")
	s2 := seedSite(t, pool, tenant, "")
	grant := uuid.New()
	decider := m174User(t, pool)

	scope := acprSitePrincipal(tenant, s1, s2)
	r1 := aarInsert(t, pool, scope, aarParams(tenant, s1, grant, "act-1"))
	if _, err := aarApprove(t, pool, r1, r1.PresentedDigest); err != nil {
		t.Fatalf("approve r1: %v", err)
	}
	p1 := acprInsert(t, pool, scope, acprParams(tenant, s2, grant, "act-p1"))
	acprApprove(t, pool, p1, decider)
	r2 := aarInsert(t, pool, scope, aarParams(tenant, s2, grant, "act-2"))
	if _, err := aarApprove(t, pool, r2, r2.PresentedDigest); err != nil {
		t.Fatalf("approve r2: %v", err)
	}
	waiting := aarInsert(t, pool, scope, aarParams(tenant, s1, grant, "act-3"))

	page := func(p domain.Principal, filter string, cursor *sqlc.ListAIActivityPageRow, limit int32) []sqlc.ListAIActivityPageRow {
		arg := sqlc.ListAIActivityPageParams{TenantID: tenant, Filter: filter, RowLimit: limit}
		if cursor != nil {
			arg.CursorCreatedAt = pgtype.Timestamptz{Time: cursor.CreatedAt, Valid: true}
			arg.CursorID = m174UUID(cursor.ID)
		}
		var rows []sqlc.ListAIActivityPageRow
		if err := pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "RunTenantTx (m174 activity)")
			var err error
			rows, err = sqlc.New(tx).ListAIActivityPage(ctx, arg)
			return err
		}); err != nil {
			t.Fatalf("activity page: %v", err)
		}
		return rows
	}

	org := acprDeciderPrincipal(tenant, decider)
	first := page(org, "all", nil, 2)
	if len(first) != 2 || first[0].ID != r2.ID || first[1].ID != p1.ID || first[1].Kind != "cache_purge_request" {
		t.Fatalf("first page: %+v; want r2 then the cache clear", first)
	}
	second := page(org, "all", &first[1], 2)
	if len(second) != 1 || second[0].ID != r1.ID || second[0].Kind != "ability_request" {
		t.Fatalf("second page: %+v; want r1 only (the waiting %s is never listed)", second, waiting.ID)
	}
	if auto := page(org, "ran_automatically", nil, 10); len(auto) != 0 {
		t.Fatalf("ran_automatically: %d rows, want 0", len(auto))
	}
	if person := page(org, "approved_by_person", nil, 10); len(person) != 3 {
		t.Fatalf("approved_by_person: %d rows, want 3", len(person))
	}
	if junk := page(org, "everything", nil, 10); len(junk) != 0 {
		t.Fatalf("unknown filter: %d rows, want 0", len(junk))
	}
	if narrow := page(acprSitePrincipal(tenant, s1), "all", nil, 10); len(narrow) != 1 || narrow[0].ID != r1.ID {
		t.Fatalf("site collaborator on s1: %+v; want r1 only", narrow)
	}
}
