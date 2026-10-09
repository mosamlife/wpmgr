// AI readiness queries (db/query/ai_readiness.sql) proven as wpmgr_app.
//
// Every read goes through the production dispatch (db.Pool.RunTenantTx) and
// the generated sqlc methods, so the statement proven is the statement
// shipped and the GUCs are the ones a request sets. Each transaction asserts
// from inside that it is wpmgr_app with neither SUPERUSER nor BYPASSRLS.
// Fixture rows are written by the superuser pool only where no app path
// exists to write them directly (components documents, enrolment state,
// shares); the inventory is written through its own shipped statements.
//
// What is proven:
//   - the fleet query returns exactly the tenant's enrolled, non-archived
//     sites, and the explicit tenant predicate on sites is what keeps out a
//     site of another tenant that the caller can see through a share;
//   - a tenant transaction naming another tenant's id gets nothing (RLS, not
//     the predicate, refuses it);
//   - a site-scoped collaborator gets only their allowed site from both
//     queries;
//   - an ability counts as the builder's only when the builder registered it;
//   - the JSONB extraction matches by directory, prefers the active entry,
//     and turns a malformed document into empty values rather than an error.
package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

type rdySiteSeed struct {
	enrolled   bool
	state      string // connection_state
	wp, agent  string
	components string // JSON text; "" leaves the column default and no push stamp
	editing    bool
}

// rdySeedSite inserts one site row with exactly the readiness-relevant state.
func rdySeedSite(t *testing.T, admin *db.Pool, tenant uuid.UUID, s rdySiteSeed) uuid.UUID {
	t.Helper()
	now := time.Now().UTC()
	var enrolledAt, componentsAt, editingAt *time.Time
	var principal *int64
	doc := "{}"
	if s.enrolled {
		enrolledAt = &now
	}
	if s.components != "" {
		doc = s.components
		componentsAt = &now
	}
	if s.editing {
		editingAt = &now
		p := int64(7)
		principal = &p
	}
	var id uuid.UUID
	if err := admin.QueryRow(context.Background(), `
		INSERT INTO sites (tenant_id, url, name, wp_version, agent_version, connection_state,
		                   enrolled_at, components, components_updated_at,
		                   content_editing_enabled_at, content_editing_principal_user_id)
		VALUES ($1, $2, 'rdy', $3, $4, $5, $6, $7::text::jsonb, $8, $9, $10)
		RETURNING id`,
		tenant, "https://rdy-"+uuid.NewString()+".example.test", s.wp, s.agent, s.state,
		enrolledAt, doc, componentsAt, editingAt, principal,
	).Scan(&id); err != nil {
		t.Fatalf("seed readiness site: %v", err)
	}
	return id
}

type rdyAbility struct{ name, kind, dir, ok string }

// rdySeedInventory replaces one site's ability inventory through the shipped
// upsert and run statements, in the site's tenant transaction.
func rdySeedInventory(t *testing.T, pool *db.Pool, tenant, site uuid.UUID, abilities ...rdyAbility) {
	t.Helper()
	ctx := context.Background()
	arg := sqlc.UpsertSiteAbilityInventoryParams{TenantID: tenant, SiteID: site, CheckedAt: time.Now().UTC()}
	for _, a := range abilities {
		arg.Names = append(arg.Names, a.name)
		arg.OwnerKinds = append(arg.OwnerKinds, a.kind)
		arg.OwnerDirs = append(arg.OwnerDirs, a.dir)
		arg.OwnerOks = append(arg.OwnerOks, a.ok)
		arg.OwnerVersions = append(arg.OwnerVersions, "")
		arg.SchemaStructSha256s = append(arg.SchemaStructSha256s, "")
		arg.InputSchemas = append(arg.InputSchemas, "")
		arg.OutputSchemas = append(arg.OutputSchemas, "")
		arg.Annotations = append(arg.Annotations, "")
		arg.SiteLabels = append(arg.SiteLabels, "")
		arg.SiteDescriptions = append(arg.SiteDescriptions, "")
	}
	if err := pool.RunTenantTx(ctx, acprOrgPrincipal(tenant), func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		n, err := q.UpsertSiteAbilityInventory(ctx, arg)
		if err != nil {
			return err
		}
		if n != int64(len(abilities)) {
			return fmt.Errorf("upsert wrote %d rows, want %d", n, len(abilities))
		}
		return q.UpsertSiteAbilityInventoryRun(ctx, sqlc.UpsertSiteAbilityInventoryRunParams{
			TenantID: tenant, SiteID: site, CheckedAt: arg.CheckedAt, SnapshotID: uuid.New(),
			ApiPresent: true, AbilitiesStored: int32(len(abilities)),
		})
	}); err != nil {
		t.Fatalf("seed ability inventory for site %s: %v", site, err)
	}
}

func rdyFacts(t *testing.T, tx pgx.Tx, tenant uuid.UUID, site *uuid.UUID) map[uuid.UUID]sqlc.ListAIReadinessSiteFactsRow {
	t.Helper()
	arg := sqlc.ListAIReadinessSiteFactsParams{TenantID: tenant}
	if site != nil {
		arg.SiteID = pgtype.UUID{Bytes: *site, Valid: true}
	}
	rows, err := sqlc.New(tx).ListAIReadinessSiteFacts(context.Background(), arg)
	if err != nil {
		t.Fatalf("ListAIReadinessSiteFacts(tenant=%s, site=%v): %v", tenant, site, err)
	}
	out := make(map[uuid.UUID]sqlc.ListAIReadinessSiteFactsRow, len(rows))
	for _, r := range rows {
		if _, dup := out[r.SiteID]; dup {
			t.Fatalf("ListAIReadinessSiteFacts returned site %s twice", r.SiteID)
		}
		out[r.SiteID] = r
	}
	return out
}

type rdyCountKey struct {
	site uuid.UUID
	ns   string
}

type rdyCount struct{ attributed, inNamespace int64 }

func rdyCounts(t *testing.T, tx pgx.Tx, tenant uuid.UUID, site *uuid.UUID) map[rdyCountKey]rdyCount {
	t.Helper()
	arg := sqlc.CountAIReadinessAbilityOwnersParams{TenantID: tenant}
	if site != nil {
		arg.SiteID = pgtype.UUID{Bytes: *site, Valid: true}
	}
	rows, err := sqlc.New(tx).CountAIReadinessAbilityOwners(context.Background(), arg)
	if err != nil {
		t.Fatalf("CountAIReadinessAbilityOwners(tenant=%s, site=%v): %v", tenant, site, err)
	}
	out := make(map[rdyCountKey]rdyCount, len(rows))
	for _, r := range rows {
		out[rdyCountKey{r.SiteID, r.Namespace}] = rdyCount{r.Attributed, r.InNamespace}
	}
	return out
}

func rdyWantSites(t *testing.T, where string, got map[uuid.UUID]sqlc.ListAIReadinessSiteFactsRow, want map[uuid.UUID]string) {
	t.Helper()
	for id := range got {
		if _, ok := want[id]; !ok {
			t.Errorf("%s: returned site %s, which must not be there", where, id)
		}
	}
	for id, label := range want {
		if _, ok := got[id]; !ok {
			t.Errorf("OVER-FIRING: %s: site %s (%s) is missing", where, id, label)
		}
	}
}

func rdyWantCounts(t *testing.T, where string, got, want map[rdyCountKey]rdyCount) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %d (site, namespace) rows %+v, want %d %+v", where, len(got), got, len(want), want)
	}
	for k, w := range want {
		if g, ok := got[k]; !ok || g != w {
			t.Errorf("%s: site %s namespace %s = %+v (present=%t), want %+v", where, k.site, k.ns, g, ok, w)
		}
	}
}

// TestAIReadinessQueriesAsAppRole is the proof. One container; the subtests
// only read the fixture.
func TestAIReadinessQueriesAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()

	tenantA := seedTenant(t, pool, "rdy-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, pool, "rdy-b-"+uuid.NewString()[:8])

	// A builder site. The elementor directory holds an inactive extra entry
	// listed first, so an unordered pick would return 9.9.9. elementor-pro is a
	// different directory. bricks is installed but its child theme is active.
	sFull := rdySeedSite(t, admin, tenantA, rdySiteSeed{
		enrolled: true, state: "connected", wp: "7.1", agent: "0.61.159", editing: true,
		components: `{"plugins":[
			{"slug":"elementor/zz-extra.php","name":"x","version":"9.9.9","active":false},
			{"slug":"elementor-pro/elementor-pro.php","name":"x","version":"3.30.0","active":true},
			{"slug":"elementor/elementor.php","name":"x","version":"4.3.4","active":true},
			{"slug":"mcp-adapter/mcp-adapter.php","name":"x","version":"0.7.0","active":true},
			{"slug":"hello.php","name":"x","version":"1.7.2","active":false}],
		 "themes":[
			{"slug":"bricks","name":"x","version":"2.4.1","active":false},
			{"slug":"bricks-child","name":"x","version":"1.0","active":true}],
		 "builder_facts":{"v":1,"theme_template":"bricks","elementor":{"atomic_editor":true}}}`,
	})
	// Enrolled, never pushed metadata, never inventoried.
	sBare := rdySeedSite(t, admin, tenantA, rdySiteSeed{enrolled: true, state: "connected"})
	// Wrong shapes at the top: plugins an object, themes a string,
	// builder_facts a string. Must read as empty, not fail the rollup.
	sOdd := rdySeedSite(t, admin, tenantA, rdySiteSeed{
		enrolled: true, state: "degraded",
		components: `{"plugins":{"slug":"elementor/elementor.php","active":true},"themes":"bricks","builder_facts":"yes"}`,
	})
	// Wrong shapes inside the arrays: a numeric version, a string active,
	// non-object elements.
	sJunk := rdySeedSite(t, admin, tenantA, rdySiteSeed{
		enrolled: true, state: "connected",
		components: `{"plugins":[{"slug":"elementor/elementor.php","version":4.3,"active":"yes"},"junk",7,null,{"slug":null}],
		 "themes":[{"slug":"bricks","version":null,"active":1}]}`,
	})
	// Only elementor-pro, an inactive adapter, and Bricks as the active theme.
	sProOnly := rdySeedSite(t, admin, tenantA, rdySiteSeed{
		enrolled: true, state: "connected", wp: "7.0", agent: "0.61.158",
		components: `{"plugins":[
			{"slug":"elementor-pro/elementor-pro.php","name":"x","version":"3.30.0","active":true},
			{"slug":"mcp-adapter/mcp-adapter.php","name":"x","version":"0.7.0","active":false}],
		 "themes":[{"slug":"bricks","name":"x","version":"2.5.0","active":true}]}`,
	})
	sArchived := rdySeedSite(t, admin, tenantA, rdySiteSeed{
		enrolled: true, state: "archived", wp: "7.1", components: `{"plugins":[]}`,
	})
	sPending := rdySeedSite(t, admin, tenantA, rdySiteSeed{
		enrolled: false, state: "pending_enrollment",
	})
	sB := rdySeedSite(t, admin, tenantB, rdySiteSeed{
		enrolled: true, state: "connected", wp: "7.1", agent: "0.61.159",
		components: `{"plugins":[{"slug":"elementor/elementor.php","name":"x","version":"4.3.4","active":true}],"themes":[]}`,
	})

	// Owner attribution. On sFull exactly two elementor rows and one bricks row
	// belong to the builder; each spoof differs from a counted row in exactly
	// one field (dir, kind, verification).
	rdySeedInventory(t, pool, tenantA, sFull,
		rdyAbility{"elementor/build-page", "plugin", "elementor", "true"},
		rdyAbility{"elementor/list-widgets", "plugin", "elementor", ""},
		rdyAbility{"elementor/spoof-dir", "plugin", "evil", ""},
		rdyAbility{"elementor/spoof-kind", "theme", "elementor", ""},
		rdyAbility{"elementor/spoof-unverified", "plugin", "elementor", "false"},
		rdyAbility{"bricks/get-page", "theme", "bricks", ""},
		rdyAbility{"bricks/spoof-kind", "plugin", "bricks", ""},
		rdyAbility{"core/get-site-info", "core", "", ""},
	)
	rdySeedInventory(t, pool, tenantA, sProOnly,
		rdyAbility{"bricks/get-page", "theme", "bricks", "true"},
	)
	rdySeedInventory(t, pool, tenantB, sB,
		rdyAbility{"elementor/build-page", "plugin", "elementor", "true"},
	)

	// A member of tenant A who also holds a share on tenant B's site, the way
	// an agency user is invited to a client's site in another organisation.
	user := seedUserRow(t, admin, "rdy-"+uuid.NewString()[:8]+"@example.test")
	seedMembershipRow(t, admin, user, tenantA)
	seedSiteShare(t, admin, tenantB, sB, user, "viewer")
	memberA := domain.Principal{Type: domain.PrincipalUser, UserID: user, TenantID: tenantA, Role: "owner", Scope: domain.ScopeOrg}

	wantFleetA := map[uuid.UUID]string{sFull: "full", sBare: "bare", sOdd: "odd", sJunk: "junk", sProOnly: "pro-only"}

	t.Run("fleet rollup for a member holding a share in another tenant", func(t *testing.T) {
		if err := pool.RunTenantTx(ctx, memberA, func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "RunTenantTx (tenant A member)")

			// Premise: in this very transaction the share makes tenant B's site
			// visible to a bare SELECT. If this stops being true the check
			// below proves nothing about the tenant predicate.
			var visible int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM sites WHERE id = $1`, sB).Scan(&visible); err != nil {
				t.Fatalf("premise read: %v", err)
			}
			if visible != 1 {
				t.Fatalf("PREMISE LOST: tenant B's shared site is visible %d times to a bare SELECT, want 1; "+
					"the tenant-predicate proof below would be vacuous", visible)
			}

			got := rdyFacts(t, tx, tenantA, nil)
			if _, leaked := got[sB]; leaked {
				t.Errorf("TENANCY LEAK: tenant A's readiness rollup contains tenant B's site %s, "+
					"reachable only through the member's share", sB)
			}
			if _, ok := got[sArchived]; ok {
				t.Errorf("ARCHIVED SITE RETURNED: %s", sArchived)
			}
			if _, ok := got[sPending]; ok {
				t.Errorf("NEVER-ENROLLED SITE RETURNED: %s", sPending)
			}
			rdyWantSites(t, "tenant A fleet", got, wantFleetA)

			full := got[sFull]
			if full.WpVersion != "7.1" || full.AgentVersion != "0.61.159" {
				t.Errorf("full: wp=%q agent=%q, want 7.1 / 0.61.159", full.WpVersion, full.AgentVersion)
			}
			if !full.ComponentsUpdatedAt.Valid || !full.ContentEditingEnabledAt.Valid {
				t.Errorf("full: components_updated_at valid=%t content_editing_enabled_at valid=%t, want both set",
					full.ComponentsUpdatedAt.Valid, full.ContentEditingEnabledAt.Valid)
			}
			if !full.ElementorInstalled || full.ElementorVersion != "4.3.4" || !full.ElementorActive {
				t.Errorf("full: elementor installed=%t version=%q active=%t, want true / 4.3.4 / true "+
					"(the active elementor/elementor.php entry, not the inactive zz-extra or elementor-pro)",
					full.ElementorInstalled, full.ElementorVersion, full.ElementorActive)
			}
			if !full.McpAdapterActive {
				t.Errorf("full: mcp_adapter_active=false, want true")
			}
			if !full.BricksInstalled || full.BricksVersion != "2.4.1" || full.BricksActive {
				t.Errorf("full: bricks installed=%t version=%q active=%t, want true / 2.4.1 / false",
					full.BricksInstalled, full.BricksVersion, full.BricksActive)
			}
			var facts struct {
				V             int    `json:"v"`
				ThemeTemplate string `json:"theme_template"`
				Elementor     struct {
					AtomicEditor *bool `json:"atomic_editor"`
				} `json:"elementor"`
			}
			if full.BuilderFacts == nil {
				t.Errorf("full: builder_facts is nil, want the reported object")
			} else if err := json.Unmarshal(full.BuilderFacts, &facts); err != nil {
				t.Errorf("full: builder_facts %s does not decode: %v", full.BuilderFacts, err)
			} else if facts.V != 1 || facts.ThemeTemplate != "bricks" || facts.Elementor.AtomicEditor == nil || !*facts.Elementor.AtomicEditor {
				t.Errorf("full: builder_facts %s, want v=1 theme_template=bricks atomic_editor=true", full.BuilderFacts)
			}
			if !full.AbilitiesCheckedAt.Valid || full.AbilitiesApiPresent == nil || !*full.AbilitiesApiPresent ||
				full.AbilitiesTruncated == nil || *full.AbilitiesTruncated {
				t.Errorf("full: run checked_at valid=%t api_present=%v truncated=%v, want set / true / false",
					full.AbilitiesCheckedAt.Valid, full.AbilitiesApiPresent, full.AbilitiesTruncated)
			}

			bare := got[sBare]
			if bare.ComponentsUpdatedAt.Valid || bare.ContentEditingEnabledAt.Valid {
				t.Errorf("bare: a stamp is set on a site that never pushed or enabled anything")
			}
			if bare.ElementorInstalled || bare.ElementorVersion != "" || bare.ElementorActive ||
				bare.McpAdapterActive || bare.BricksInstalled || bare.BricksVersion != "" || bare.BricksActive ||
				bare.BuilderFacts != nil {
				t.Errorf("bare: empty components produced facts: %+v", bare)
			}
			if bare.AbilitiesCheckedAt.Valid || bare.AbilitiesApiPresent != nil || bare.AbilitiesTruncated != nil {
				t.Errorf("bare: run fields set on a site never inventoried: %+v", bare)
			}

			odd := got[sOdd]
			if odd.ElementorInstalled || odd.BricksInstalled || odd.BuilderFacts != nil {
				t.Errorf("odd: wrongly shaped document read as facts: elementor=%t bricks=%t builder_facts=%s",
					odd.ElementorInstalled, odd.BricksInstalled, odd.BuilderFacts)
			}

			junk := got[sJunk]
			if !junk.ElementorInstalled || junk.ElementorVersion != "" || junk.ElementorActive {
				t.Errorf("junk: elementor installed=%t version=%q active=%t, want true / \"\" / false",
					junk.ElementorInstalled, junk.ElementorVersion, junk.ElementorActive)
			}
			if !junk.BricksInstalled || junk.BricksVersion != "" || junk.BricksActive {
				t.Errorf("junk: bricks installed=%t version=%q active=%t, want true / \"\" / false",
					junk.BricksInstalled, junk.BricksVersion, junk.BricksActive)
			}

			pro := got[sProOnly]
			if pro.ElementorInstalled || pro.ElementorVersion != "" || pro.ElementorActive {
				t.Errorf("pro-only: elementor-pro was read as elementor: installed=%t version=%q active=%t",
					pro.ElementorInstalled, pro.ElementorVersion, pro.ElementorActive)
			}
			if pro.McpAdapterActive {
				t.Errorf("pro-only: an inactive MCP Adapter reads as active")
			}
			if !pro.BricksInstalled || pro.BricksVersion != "2.5.0" || !pro.BricksActive {
				t.Errorf("pro-only: bricks installed=%t version=%q active=%t, want true / 2.5.0 / true",
					pro.BricksInstalled, pro.BricksVersion, pro.BricksActive)
			}

			rdyWantCounts(t, "tenant A owner counts", rdyCounts(t, tx, tenantA, nil), map[rdyCountKey]rdyCount{
				{sFull, "elementor"}: {attributed: 2, inNamespace: 5},
				{sFull, "bricks"}:    {attributed: 1, inNamespace: 2},
				{sProOnly, "bricks"}: {attributed: 1, inNamespace: 1},
			})
			return nil
		}); err != nil {
			t.Fatalf("tenant A member read: %v", err)
		}
	})

	t.Run("per-site call returns only the named site", func(t *testing.T) {
		if err := pool.RunTenantTx(ctx, memberA, func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "RunTenantTx (tenant A member, per site)")
			rdyWantSites(t, "per-site full", rdyFacts(t, tx, tenantA, &sFull), map[uuid.UUID]string{sFull: "full"})
			for label, id := range map[string]uuid.UUID{"archived": sArchived, "never enrolled": sPending, "tenant B shared": sB} {
				id := id
				if got := rdyFacts(t, tx, tenantA, &id); len(got) != 0 {
					t.Errorf("per-site call for the %s site %s returned %d rows, want 0", label, id, len(got))
				}
			}
			rdyWantCounts(t, "per-site counts full", rdyCounts(t, tx, tenantA, &sFull), map[rdyCountKey]rdyCount{
				{sFull, "elementor"}: {attributed: 2, inNamespace: 5},
				{sFull, "bricks"}:    {attributed: 1, inNamespace: 2},
			})
			rdyWantCounts(t, "per-site counts bare", rdyCounts(t, tx, tenantA, &sBare), map[rdyCountKey]rdyCount{})
			return nil
		}); err != nil {
			t.Fatalf("per-site read: %v", err)
		}
	})

	t.Run("another tenant naming tenant A gets nothing", func(t *testing.T) {
		if err := pool.RunTenantTx(ctx, acprOrgPrincipal(tenantB), func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "RunTenantTx (tenant B)")
			// The explicit predicate matches tenant A here, so only RLS can
			// refuse these rows.
			if got := rdyFacts(t, tx, tenantA, nil); len(got) != 0 {
				t.Errorf("TENANCY LEAK: a tenant B transaction read %d of tenant A's readiness rows", len(got))
			}
			if got := rdyCounts(t, tx, tenantA, nil); len(got) != 0 {
				t.Errorf("TENANCY LEAK: a tenant B transaction read %d of tenant A's owner counts: %+v", len(got), got)
			}
			// Positive control: the same transaction reads its own tenant.
			rdyWantSites(t, "tenant B fleet", rdyFacts(t, tx, tenantB, nil), map[uuid.UUID]string{sB: "tenant B"})
			rdyWantCounts(t, "tenant B owner counts", rdyCounts(t, tx, tenantB, nil), map[rdyCountKey]rdyCount{
				{sB, "elementor"}: {attributed: 1, inNamespace: 1},
			})
			return nil
		}); err != nil {
			t.Fatalf("tenant B read: %v", err)
		}
	})

	t.Run("a site-scoped collaborator sees only the allowed site", func(t *testing.T) {
		if err := pool.RunTenantTx(ctx, acprSitePrincipal(tenantA, sFull), func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "RunTenantTx (site-scoped to full)")
			got := rdyFacts(t, tx, tenantA, nil)
			for id := range got {
				if id != sFull {
					t.Errorf("SITE-SCOPE LEAK: a collaborator scoped to %s got readiness for %s", sFull, id)
				}
			}
			full, ok := got[sFull]
			if !ok {
				t.Fatalf("OVER-FIRING: the collaborator's own site %s is missing", sFull)
			}
			if !full.AbilitiesCheckedAt.Valid || full.ElementorVersion != "4.3.4" {
				t.Errorf("OVER-FIRING: the allowed site's facts are incomplete under site scope: %+v", full)
			}
			if got := rdyFacts(t, tx, tenantA, &sProOnly); len(got) != 0 {
				t.Errorf("SITE-SCOPE LEAK: a per-site call for %s outside the scope returned %d rows", sProOnly, len(got))
			}
			counts := rdyCounts(t, tx, tenantA, nil)
			if _, leaked := counts[rdyCountKey{sProOnly, "bricks"}]; leaked {
				t.Errorf("SITE-SCOPE LEAK: owner counts include site %s outside the scope", sProOnly)
			}
			rdyWantCounts(t, "site-scoped owner counts", counts, map[rdyCountKey]rdyCount{
				{sFull, "elementor"}: {attributed: 2, inNamespace: 5},
				{sFull, "bricks"}:    {attributed: 1, inNamespace: 2},
			})
			return nil
		}); err != nil {
			t.Fatalf("site-scoped read: %v", err)
		}
	})
}
