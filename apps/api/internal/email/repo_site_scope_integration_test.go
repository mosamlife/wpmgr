// repo_site_scope_integration_test.go: m112 (GH #380) proved THROUGH the repo.
//
// WHY THIS FILE EXISTS WHEN tests/email_site_scope_rls_integration_test.go
// ALREADY PASSES.
//
// That file drives Postgres directly: every case opens its own
// db.Pool.InScopedTenantTx and issues raw SQL. It is a good proof that the m112
// POLICIES are correct, and it is no proof at all that they are REACHED. The
// only thing that puts app.site_scope on an email transaction in production is
// Repo.scopedTenantTx. Delete that helper, or let one repo method call
// InTenantTx directly by mistake, and every case over there still passes green
// while the policies quietly stop applying to the path they were written for.
//
// This project has shipped that exact failure twice: a frontend test once
// asserted three server error codes that did not exist and passed, hiding
// missing rate limiting, and two hand-written tests pinned live bugs as correct
// behaviour. A test that cannot fail when the thing it protects is removed is
// not coverage, it is reassurance.
//
// So everything below goes THROUGH the exported Repo methods, with a
// site-scoped domain.Principal in the context and nothing else. The repo is the
// subject, not the harness. Each attack case here fails if scopedTenantTx stops
// dispatching, which is the property the direct-SQL file cannot state.
//
// The harness mirrors internal/admin/gate_integration_test.go: the app pool
// connects as the non-superuser wpmgr_app role, because a superuser (or any
// role with BYPASSRLS) ignores every policy in m112 and would make this whole
// file pass vacuously.
package email

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// startScopeTestPostgres returns (appPool, adminPool). appPool connects as the
// non-superuser wpmgr_app role so the RESTRICTIVE policies are actually in
// force; adminPool is the superuser and is used only to seed fixtures and to
// read ground truth after an attack.
func startScopeTestPostgres(t *testing.T) (*db.Pool, *db.Pool) {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("wpmgr"),
		tcpostgres.WithUsername("wpmgr"),
		tcpostgres.WithPassword("wpmgr"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		t.Skipf("skipping: cannot start postgres container (docker unavailable?): %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	adminDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	adminPool, err := db.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if err := adminPool.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, stmt := range []string{
		"ALTER ROLE wpmgr_app LOGIN PASSWORD 'app'",
		"GRANT USAGE ON SCHEMA public TO wpmgr_app",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO wpmgr_app",
		"REVOKE UPDATE, DELETE, TRUNCATE ON audit_log FROM wpmgr_app",
	} {
		if _, err := adminPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("provision app role (%q): %v", stmt, err)
		}
	}
	t.Cleanup(adminPool.Close)

	appDSN := strings.Replace(adminDSN, "wpmgr:wpmgr@", "wpmgr_app:app@", 1)
	appPool, err := db.Connect(ctx, appDSN)
	if err != nil {
		t.Fatalf("connect app: %v", err)
	}
	t.Cleanup(appPool.Close)
	return appPool, adminPool
}

// scopeFixture is one tenant with an ORG-WIDE config (site_id IS NULL, holding
// a credential), two sites, and a per-site config on siteA only. siteB has no
// row of its own, so it is a genuinely INHERITING site: the collaborator we
// scope to siteB is exactly the actor from GH #380.
type scopeFixture struct {
	tenant       uuid.UUID
	siteA, siteB uuid.UUID
	orgConfig    uuid.UUID
	siteAConfig  uuid.UUID
	orgConnKey   string
	siteAConnKey string
	orgSuppress  uuid.UUID
	orgTokenHash []byte
}

func seedScopeFixture(t *testing.T, admin *db.Pool) scopeFixture {
	t.Helper()
	ctx := context.Background()

	f := scopeFixture{
		siteA: uuid.New(), siteB: uuid.New(),
		orgConnKey: "billing", siteAConnKey: "billing",
		orgTokenHash: []byte("org-route-token-hash-32-bytes--!"),
	}
	if err := admin.QueryRow(ctx,
		"INSERT INTO tenants (name, slug) VALUES ('repo-scope', 'repo-scope') RETURNING id").
		Scan(&f.tenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	for _, s := range []struct {
		id  uuid.UUID
		url string
	}{{f.siteA, "https://a.repo.example"}, {f.siteB, "https://b.repo.example"}} {
		// status 'connected' matters: ListEmailInheritingSites only fans the org
		// config out to enrolled sites, so a default-status fixture would make
		// the agent-path assertion below pass or fail for the wrong reason.
		if _, err := admin.Exec(ctx,
			"INSERT INTO sites (id, tenant_id, url, name, status) VALUES ($1, $2, $3, $3, 'connected')",
			s.id, f.tenant, s.url); err != nil {
			t.Fatalf("seed site: %v", err)
		}
	}
	if err := admin.QueryRow(ctx,
		`INSERT INTO site_email_config
		   (tenant_id, site_id, provider, from_address, config,
		    provider_secret_encrypted, webhook_route_token_hash)
		 VALUES ($1, NULL, 'smtp', 'fleet@example.com',
		         '{"host":"smtp.org-relay.example","username":"fleet","port":587,"encryption":"tls"}'::jsonb,
		         '\x4f5247', $2)
		 RETURNING id`, f.tenant, f.orgTokenHash).Scan(&f.orgConfig); err != nil {
		t.Fatalf("seed org config: %v", err)
	}
	if err := admin.QueryRow(ctx,
		`INSERT INTO site_email_config (tenant_id, site_id, provider, from_address, config)
		 VALUES ($1, $2, 'smtp', 'a@example.com', '{"host":"smtp.a.example","username":"a"}'::jsonb)
		 RETURNING id`, f.tenant, f.siteA).Scan(&f.siteAConfig); err != nil {
		t.Fatalf("seed siteA config: %v", err)
	}
	if _, err := admin.Exec(ctx,
		`INSERT INTO site_email_connection
		   (tenant_id, config_id, connection_key, provider, config, provider_secret_encrypted)
		 VALUES ($1, $2, $3, 'smtp', '{"host":"smtp.org-relay.example"}'::jsonb, '\x4f52474330')`,
		f.tenant, f.orgConfig, f.orgConnKey); err != nil {
		t.Fatalf("seed org connection: %v", err)
	}
	if _, err := admin.Exec(ctx,
		`INSERT INTO site_email_connection (tenant_id, config_id, connection_key, provider, config)
		 VALUES ($1, $2, $3, 'smtp', '{"host":"smtp.a.example"}'::jsonb)`,
		f.tenant, f.siteAConfig, f.siteAConnKey); err != nil {
		t.Fatalf("seed siteA connection: %v", err)
	}
	if err := admin.QueryRow(ctx,
		`INSERT INTO email_suppression (tenant_id, site_id, email_hash, reason)
		 VALUES ($1, NULL, '\xdeadbeef', 'complaint')
		 RETURNING id`, f.tenant).Scan(&f.orgSuppress); err != nil {
		t.Fatalf("seed fleet suppression: %v", err)
	}
	for _, sid := range []uuid.UUID{f.siteA, f.siteB} {
		if _, err := admin.Exec(ctx,
			`INSERT INTO site_email_log (tenant_id, site_id, subject, status)
			 VALUES ($1, $2, 'hello', 'sent')`, f.tenant, sid); err != nil {
			t.Fatalf("seed log: %v", err)
		}
	}
	return f
}

// collaboratorCtx is the ONLY thing these tests hand the repo: a context
// carrying a site-scoped principal, exactly as authz builds one for an outside
// collaborator invited to specific sites. Everything that happens after this is
// the repo's own dispatch, which is the point.
func collaboratorCtx(tenant uuid.UUID, sites ...uuid.UUID) context.Context {
	return domain.WithPrincipal(context.Background(), domain.Principal{
		Type:           domain.PrincipalUser,
		UserID:         uuid.New(),
		TenantID:       tenant,
		Role:           "operator",
		Scope:          domain.ScopeSite,
		AllowedSiteIDs: sites,
	})
}

// orgMemberCtx is a full org member: Scope is "org", so scopedTenantTx must
// leave them on InTenantTx and m112 must be invisible to them.
func orgMemberCtx(tenant uuid.UUID) context.Context {
	return domain.WithPrincipal(context.Background(), domain.Principal{
		Type:     domain.PrincipalUser,
		UserID:   uuid.New(),
		TenantID: tenant,
		Role:     "owner",
		Scope:    "org",
	})
}

// orgConfigHost reads the org row's host as the superuser: the ground truth
// after an attack, which the app role cannot be trusted to report on itself.
func orgConfigHost(t *testing.T, admin *db.Pool, configID uuid.UUID) string {
	t.Helper()
	var host string
	if err := admin.QueryRow(context.Background(),
		`SELECT config->>'host' FROM site_email_config WHERE id = $1`, configID).Scan(&host); err != nil {
		t.Fatalf("read org host: %v", err)
	}
	return host
}

func orgConfigSecret(t *testing.T, admin *db.Pool, configID uuid.UUID) []byte {
	t.Helper()
	var ct []byte
	if err := admin.QueryRow(context.Background(),
		`SELECT provider_secret_encrypted FROM site_email_config WHERE id = $1`, configID).Scan(&ct); err != nil {
		t.Fatalf("read org secret: %v", err)
	}
	return ct
}

// isRLSRefusal reports whether err is Postgres 42501 insufficient_privilege.
// errors.As walks the chain, so a domain.Error wrapping the pg error (it
// implements Unwrap) is handled without special-casing. Asserting the SQLSTATE
// rather than "some error happened" is what stops a unique-index violation, or
// a typo in the fixture, from being mistaken for a policy doing its job.
func isRLSRefusal(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42501"
}

// orgUpsertInput is the write every one of GH #380's seven doors ended at: the
// organisation's config row, rewritten to point at an attacker-chosen relay.
func orgUpsertInput(tenant uuid.UUID) upsertRepoInput {
	return upsertRepoInput{
		TenantID:    tenant,
		SiteID:      nil, // nil = the org-wide row
		Provider:    "smtp",
		FromAddress: "pwned@example.com",
		Config: map[string]any{
			"host":     "smtp.attacker.example",
			"username": "fleet",
		},
		Mappings:      map[string]any{},
		RetentionDays: 30,
	}
}

// ---------------------------------------------------------------------------
// The attack, driven through the repo
// ---------------------------------------------------------------------------

// TestRepoRefusesOrgConfigUpsertForSiteScopedPrincipal is the load-bearing case
// in this file. UpsertOrgConfig is the terminal step of the escalation: however
// the actor reached it, this is where the organisation's mail server gets
// repointed. Nothing here sets a GUC by hand. The only input is a site-scoped
// principal in the context, so a green result means Repo.scopedTenantTx really
// did route this call onto InScopedTenantTx.
//
// MUTATION CONTRACT: point UpsertOrgConfig at r.pool.InTenantTx and this test
// must fail. If it does not, it is not testing what it claims.
func TestRepoRefusesOrgConfigUpsertForSiteScopedPrincipal(t *testing.T) {
	app, admin := startScopeTestPostgres(t)
	f := seedScopeFixture(t, admin)
	repo := NewRepo(app)

	beforeSecret := orgConfigSecret(t, admin, f.orgConfig)

	// A collaborator invited to siteB only, which inherits the org config.
	_, err := repo.UpsertOrgConfig(collaboratorCtx(f.tenant, f.siteB), orgUpsertInput(f.tenant))
	if err == nil {
		t.Fatal("a site-scoped collaborator rewrote the ORGANISATION's email config through " +
			"Repo.UpsertOrgConfig; either scopedTenantTx no longer dispatches to " +
			"InScopedTenantTx, or the m112 INSERT policy is not reached from this path")
	}
	if !isRLSRefusal(err) {
		t.Fatalf("the org upsert was refused, but not by RLS (want SQLSTATE 42501, so we know "+
			"the policy is what stopped it and not the unique index): %v", err)
	}

	if host := orgConfigHost(t, admin, f.orgConfig); host != "smtp.org-relay.example" {
		t.Fatalf("the organisation's mail server is now %q", host)
	}
	if after := orgConfigSecret(t, admin, f.orgConfig); string(after) != string(beforeSecret) {
		t.Fatal("the organisation's stored credential changed under a site-scoped principal")
	}
}

// TestRepoRefusesOrgConnectionUpsertForSiteScopedPrincipal is the connection
// registry spelling of the same escalation. The connection row carries its own
// provider_secret_encrypted, so leaving the child ungated would have left the
// credential reachable through it.
//
// MUTATION CONTRACT: point UpsertConnection at r.pool.InTenantTx and this must
// fail.
func TestRepoRefusesOrgConnectionUpsertForSiteScopedPrincipal(t *testing.T) {
	app, admin := startScopeTestPostgres(t)
	f := seedScopeFixture(t, admin)
	repo := NewRepo(app)

	_, err := repo.UpsertConnection(collaboratorCtx(f.tenant, f.siteB), ConnectionUpsertInput{
		TenantID:      f.tenant,
		ConfigID:      f.orgConfig,
		ConnectionKey: f.orgConnKey,
		Provider:      "smtp",
		FromAddress:   "pwned@example.com",
		Config:        map[string]any{"host": "smtp.attacker.example"},
	}, nil, false)
	if err == nil {
		t.Fatal("a site-scoped collaborator rewrote an ORG connection through " +
			"Repo.UpsertConnection")
	}
	if !isRLSRefusal(err) {
		t.Fatalf("the org connection upsert was refused, but not by RLS (want 42501): %v", err)
	}

	var host string
	if err := admin.QueryRow(context.Background(),
		`SELECT config->>'host' FROM site_email_connection WHERE config_id = $1 AND connection_key = $2`,
		f.orgConfig, f.orgConnKey).Scan(&host); err != nil {
		t.Fatalf("re-read org connection: %v", err)
	}
	if host != "smtp.org-relay.example" {
		t.Fatalf("the org connection now points at %q", host)
	}
}

// TestRepoRefusesOrgConnectionDeleteForSiteScopedPrincipal. A DELETE that
// matches no visible row is not an error in Postgres, so the proof here is the
// row still being present afterwards rather than an error being returned.
//
// MUTATION CONTRACT: point DeleteConnection at r.pool.InTenantTx and this must
// fail.
func TestRepoRefusesOrgConnectionDeleteForSiteScopedPrincipal(t *testing.T) {
	app, admin := startScopeTestPostgres(t)
	f := seedScopeFixture(t, admin)
	repo := NewRepo(app)

	// A refusal here is expressed as "nothing happened", so ignore the error
	// and check the world.
	_ = repo.DeleteConnection(collaboratorCtx(f.tenant, f.siteB), f.tenant, f.orgConfig, f.orgConnKey)

	var n int
	if err := admin.QueryRow(context.Background(),
		`SELECT count(*) FROM site_email_connection WHERE config_id = $1 AND connection_key = $2`,
		f.orgConfig, f.orgConnKey).Scan(&n); err != nil {
		t.Fatalf("count org connection: %v", err)
	}
	if n != 1 {
		t.Fatal("a site-scoped collaborator DELETED the organisation's named connection " +
			"through Repo.DeleteConnection")
	}
}

// TestRepoRefusesWebhookRotationOnTheOrgConfig. Rotating the org row's route
// token hash re-points where the provider's bounce and complaint callbacks
// land, which is an organisation-level act. SetEmailConfigWebhookFields is an
// UPDATE ... RETURNING, so an invisible target row surfaces as ErrNoRows rather
// than as a 42501; either way the write must not land.
//
// MUTATION CONTRACT: point SetWebhookFields at r.pool.InTenantTx and this must
// fail.
func TestRepoRefusesWebhookRotationOnTheOrgConfig(t *testing.T) {
	app, admin := startScopeTestPostgres(t)
	f := seedScopeFixture(t, admin)
	repo := NewRepo(app)

	_, err := repo.SetWebhookFields(collaboratorCtx(f.tenant, f.siteB),
		f.tenant, f.orgConfig, []byte("attacker-route-token-hash-32b--!"), nil, false, nil)
	if err == nil {
		t.Fatal("a site-scoped collaborator rotated the ORG config's webhook route token " +
			"through Repo.SetWebhookFields")
	}

	var got []byte
	if qerr := admin.QueryRow(context.Background(),
		`SELECT webhook_route_token_hash FROM site_email_config WHERE id = $1`,
		f.orgConfig).Scan(&got); qerr != nil {
		t.Fatalf("re-read org token hash: %v", qerr)
	}
	if string(got) != string(f.orgTokenHash) {
		t.Fatal("the organisation's webhook route token hash was rotated by a site-scoped principal")
	}
}

// TestRepoRefusesAnotherSitesConfigWrite is the plain cross-site case inside a
// single tenant, which tenant_isolation alone never covered: the collaborator
// is invited to siteB and writes siteA's row.
//
// MUTATION CONTRACT: point UpsertSiteConfig at r.pool.InTenantTx and this must
// fail.
func TestRepoRefusesAnotherSitesConfigWrite(t *testing.T) {
	app, admin := startScopeTestPostgres(t)
	f := seedScopeFixture(t, admin)
	repo := NewRepo(app)

	siteA := f.siteA
	in := orgUpsertInput(f.tenant)
	in.SiteID = &siteA

	_, err := repo.UpsertSiteConfig(collaboratorCtx(f.tenant, f.siteB), in)
	if err == nil {
		t.Fatal("a collaborator scoped to siteB wrote siteA's email config through " +
			"Repo.UpsertSiteConfig")
	}
	if !isRLSRefusal(err) {
		t.Fatalf("the cross-site write was refused, but not by RLS (want 42501): %v", err)
	}

	var host string
	if qerr := admin.QueryRow(context.Background(),
		`SELECT config->>'host' FROM site_email_config WHERE id = $1`, f.siteAConfig).Scan(&host); qerr != nil {
		t.Fatalf("re-read siteA config: %v", qerr)
	}
	if host != "smtp.a.example" {
		t.Fatalf("siteA's mail server is now %q", host)
	}
}

// TestRepoRefusesFleetSuppressionDeleteForSiteScopedPrincipal covers both the
// RLS refusal and the correctness half of GH #380's last finding: the delete
// must not report success. The fleet-wide row is deliberately VISIBLE to this
// principal (IsSuppressed matches site_id IS NULL, so it has to be), which is
// what made the silent no-op possible in the first place.
//
// MUTATION CONTRACT: point DeleteSuppression at r.pool.InTenantTx and this must
// fail.
func TestRepoRefusesFleetSuppressionDeleteForSiteScopedPrincipal(t *testing.T) {
	app, admin := startScopeTestPostgres(t)
	f := seedScopeFixture(t, admin)
	repo := NewRepo(app)

	ctx := collaboratorCtx(f.tenant, f.siteB)

	// Precondition: the row really is readable to this principal. Without this
	// the case below could pass because the row was invisible, which is a
	// different (and product-breaking) reason.
	if _, err := repo.GetSuppression(ctx, f.tenant, f.orgSuppress); err != nil {
		t.Fatalf("the fleet-wide suppression entry must stay READABLE to a site-scoped "+
			"principal (the pre-send check depends on it): %v", err)
	}

	err := repo.DeleteSuppression(ctx, f.tenant, f.orgSuppress)
	if err == nil {
		t.Fatal("Repo.DeleteSuppression reported SUCCESS for a delete Postgres refused; " +
			"the caller is told the fleet-wide entry is gone when it is not")
	}
	if !errors.Is(err, ErrSuppressionRefused) {
		t.Fatalf("want ErrSuppressionRefused (the row exists and was refused), got %v", err)
	}

	var n int
	if qerr := admin.QueryRow(context.Background(),
		`SELECT count(*) FROM email_suppression WHERE id = $1`, f.orgSuppress).Scan(&n); qerr != nil {
		t.Fatalf("count suppression: %v", qerr)
	}
	if n != 1 {
		t.Fatal("the fleet-wide suppression entry was deleted by a site-scoped principal")
	}
}

// ---------------------------------------------------------------------------
// The compatibility half: the policies must FILTER, not simply block
// ---------------------------------------------------------------------------

// TestRepoStillServesTheInheritedOrgConfig. Inheritance is a shipped feature: a
// site with no config of its own sends with the organisation's, and
// GET /sites/:siteId/email/config legitimately surfaces that row. A dispatch
// that scoped the read too hard would look like a security win while showing
// "not configured" for a site that is configured and sending.
func TestRepoStillServesTheInheritedOrgConfig(t *testing.T) {
	app, admin := startScopeTestPostgres(t)
	f := seedScopeFixture(t, admin)
	repo := NewRepo(app)

	cfg, err := repo.GetOrgConfig(collaboratorCtx(f.tenant, f.siteB), f.tenant)
	if err != nil {
		t.Fatalf("a collaborator on an INHERITING site must still read the org config "+
			"their site actually sends with: %v", err)
	}
	if cfg.Config["host"] != "smtp.org-relay.example" {
		t.Fatalf("org config host = %v, want the seeded org relay", cfg.Config["host"])
	}
}

// TestRepoStillListsTheInheritedOrgConnections is the same guarantee for the
// connection registry that an inheriting site sends through.
func TestRepoStillListsTheInheritedOrgConnections(t *testing.T) {
	app, admin := startScopeTestPostgres(t)
	f := seedScopeFixture(t, admin)
	repo := NewRepo(app)

	conns, err := repo.ListConnections(collaboratorCtx(f.tenant, f.siteB), f.tenant, f.orgConfig)
	if err != nil {
		t.Fatalf("list org connections as an inheriting collaborator: %v", err)
	}
	if len(conns) != 1 || conns[0].ConnectionKey != f.orgConnKey {
		t.Fatalf("want the org's one named connection, got %#v", conns)
	}
}

// TestRepoStillWritesTheCollaboratorsOwnConfig. If the policies blocked
// everything once app.site_scope is on, every attack case above would pass and
// the feature would be dead. This is the case that tells them apart.
func TestRepoStillWritesTheCollaboratorsOwnConfig(t *testing.T) {
	app, admin := startScopeTestPostgres(t)
	f := seedScopeFixture(t, admin)
	repo := NewRepo(app)

	siteA := f.siteA
	in := orgUpsertInput(f.tenant)
	in.SiteID = &siteA
	in.FromAddress = "a@example.com"
	in.Config = map[string]any{"host": "smtp.a2.example", "username": "a"}

	cfg, err := repo.UpsertSiteConfig(collaboratorCtx(f.tenant, siteA), in)
	if err != nil {
		t.Fatalf("a collaborator writing their OWN site's email config must work: %v", err)
	}
	if cfg.Config["host"] != "smtp.a2.example" {
		t.Fatalf("own-site write did not land: host = %v", cfg.Config["host"])
	}
}

// TestRepoLeavesOrgMembersAlone is the regression guard scopedTenantTx's comment
// promises: only a site-scoped principal is routed anywhere new, so an org
// member's writes through the repo are exactly what they were before m112.
func TestRepoLeavesOrgMembersAlone(t *testing.T) {
	app, admin := startScopeTestPostgres(t)
	f := seedScopeFixture(t, admin)
	repo := NewRepo(app)

	if _, err := repo.UpsertOrgConfig(orgMemberCtx(f.tenant), orgUpsertInput(f.tenant)); err != nil {
		t.Fatalf("an ORG member must still be able to write the org email config: %v", err)
	}
	if host := orgConfigHost(t, admin, f.orgConfig); host != "smtp.attacker.example" {
		t.Fatalf("the org member's write did not land (host = %q)", host)
	}
	if err := repo.DeleteSuppression(orgMemberCtx(f.tenant), f.tenant, f.orgSuppress); err != nil {
		t.Fatalf("an ORG member must still be able to delete a fleet-wide suppression entry: %v", err)
	}
}

// TestRepoIgnoresAPrincipalFromAnotherTenant covers the tenant-equality guard in
// scopedTenantTx. A background job can carry an inherited principal while
// operating on some other tenant's rows; scoping that transaction to the
// inherited principal's site allowlist would filter the WRONG tenant's data and
// silently return nothing. The guard says: when the principal is not the owner
// of the tenant being queried, it tells us nothing, so ignore it.
func TestRepoIgnoresAPrincipalFromAnotherTenant(t *testing.T) {
	app, admin := startScopeTestPostgres(t)
	f := seedScopeFixture(t, admin)
	repo := NewRepo(app)

	// A site-scoped principal belonging to some OTHER tenant entirely.
	strayCtx := collaboratorCtx(uuid.New(), uuid.New())

	cfg, err := repo.GetOrgConfig(strayCtx, f.tenant)
	if err != nil {
		t.Fatalf("a stray principal from another tenant must not scope THIS tenant's "+
			"query down to its own site allowlist: %v", err)
	}
	if cfg.Config["host"] != "smtp.org-relay.example" {
		t.Fatalf("org config host = %v", cfg.Config["host"])
	}
}

// ---------------------------------------------------------------------------
// The agent path (the review left this unanswered)
// ---------------------------------------------------------------------------

// TestAgentPathThroughTheRepoIsUntouchedByM112 answers it with a test rather
// than by reading the migration. The agent pushes email logs and reads config
// under InAgentTx, which sets app.agent='on' and never sets app.site_scope, so
// every m112 predicate's leading tautology should hold for it.
//
// The interesting part is the context: a site-scoped principal is deliberately
// planted in it, scoped to siteB only. If any agent-path method ever started
// routing through scopedTenantTx, the ingest for siteA below would be refused
// and this test would catch it. That is a stronger statement than "InAgentTx
// does not set the GUC", because it is stated about the repo rather than about
// the pool.
func TestAgentPathThroughTheRepoIsUntouchedByM112(t *testing.T) {
	app, admin := startScopeTestPostgres(t)
	f := seedScopeFixture(t, admin)
	repo := NewRepo(app)

	// A hostile-shaped context: site-scoped, granted siteB, used on siteA's data.
	ctx := collaboratorCtx(f.tenant, f.siteB)

	acked, err := repo.IngestLogBatch(ctx, f.tenant, f.siteA, []IngestEntry{{
		AgentSeq:    1,
		MessageID:   "agent-push-1",
		FromAddress: "a@example.com",
		ToAddresses: []string{"someone@example.com"},
		Subject:     "agent push",
		Provider:    "smtp",
		Status:      "sent",
		CreatedAt:   time.Now().UTC(),
	}})
	if err != nil {
		t.Fatalf("the agent must still be able to push email log rows for any site in the "+
			"fleet; m112 has broken fleet-wide mail reporting: %v", err)
	}
	if acked != 1 {
		t.Fatalf("agent ingest acked %d, want 1", acked)
	}

	var n int
	if qerr := admin.QueryRow(context.Background(),
		`SELECT count(*) FROM site_email_log WHERE site_id = $1 AND message_id = 'agent-push-1'`,
		f.siteA).Scan(&n); qerr != nil {
		t.Fatalf("count pushed row: %v", qerr)
	}
	if n != 1 {
		t.Fatal("the agent's pushed log row for siteA is not there: the ingest path picked " +
			"up the site scope from the ambient principal")
	}

	// The agent's config reads: the webhook dispatcher resolves the ORG row by
	// route-token hash with no tenant GUC at all, and the inheriting-sites list
	// is cross-site by definition. Both must still see the org row.
	cfg, err := repo.GetConfigByRouteTokenHash(ctx, f.orgTokenHash)
	if err != nil {
		t.Fatalf("the webhook dispatcher must still resolve the ORG config by route token: %v", err)
	}
	if cfg.ID != f.orgConfig {
		t.Fatalf("route-token lookup returned config %s, want the org row %s", cfg.ID, f.orgConfig)
	}
	inheriting, err := repo.ListEmailInheritingSites(ctx, f.tenant)
	if err != nil {
		t.Fatalf("list inheriting sites (agent path): %v", err)
	}
	var sawB bool
	for _, s := range inheriting {
		if s.ID == f.siteB {
			sawB = true
		}
	}
	if !sawB {
		t.Fatal("siteB is missing from the inheriting-sites list, so the org config would " +
			"never be pushed to it")
	}
}

// TestAgentTransactionNeverCarriesSiteScope is the direct statement of the
// invariant the migration's comment relies on. Every m112 predicate opens by
// asking whether app.site_scope is anything other than "on", treating the
// unset GUC (which current_setting reports as the empty string) as a
// tautology. If InAgentTx ever started setting that GUC, all four tables would
// begin filtering the agent by an allowlist it does not have, and the whole
// fleet would stop reporting mail.
func TestAgentTransactionNeverCarriesSiteScope(t *testing.T) {
	app, _ := startScopeTestPostgres(t)
	ctx := context.Background()

	err := app.InAgentTx(ctx, func(tx pgx.Tx) error {
		var scope, allowed, agent string
		if qerr := tx.QueryRow(ctx,
			`SELECT coalesce(current_setting('app.site_scope', true), ''),
			        coalesce(current_setting('app.allowed_site_ids', true), ''),
			        coalesce(current_setting('app.agent', true), '')`).
			Scan(&scope, &allowed, &agent); qerr != nil {
			return qerr
		}
		if scope != "" {
			t.Fatalf("app.site_scope is %q inside an agent transaction; m112's tautology "+
				"branch no longer holds and every agent read is now filtered", scope)
		}
		if allowed != "" {
			t.Fatalf("app.allowed_site_ids is %q inside an agent transaction", allowed)
		}
		if agent != "on" {
			t.Fatalf("app.agent is %q inside an agent transaction, want 'on' (the permissive "+
				"_agent policies key off exactly this value, not 'true')", agent)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("agent tx: %v", err)
	}
}
