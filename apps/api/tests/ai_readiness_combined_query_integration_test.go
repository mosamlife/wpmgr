// The combined AI readiness query (ListAIReadinessSiteFactsWithAbilityCounts)
// proven as wpmgr_app against the two separate queries it answers for:
// ListAIReadinessSiteFacts and CountAIReadinessAbilityOwners.
//
// Every read goes through the production dispatch (db.Pool.RunTenantTx) and
// the generated sqlc methods, and each transaction asserts from inside that it
// is wpmgr_app with neither SUPERUSER nor BYPASSRLS. In each transaction the
// combined query and the two separate queries read the same fixture with the
// same arguments, so any difference between them is the SQL's. Site rows are
// written by the superuser pool; the inventory is written through its own
// shipped statements (rdySeedInventory).
//
// What is proven, for the fleet call (site_id NULL) and the per-site call:
//   - the combined query returns exactly the sites the facts query returns,
//     and every facts column equals the facts query's, compared by field name
//     so a facts column the combined row lacks fails too;
//   - its four count columns equal the count query's rows for that site, and
//     are 0 where the count query returns no row;
//   - counts the count query returns for a site the facts query drops (an
//     archived site here) never reach the combined rows;
//   - a tenant transaction naming another tenant's id gets nothing from either
//     form, and a site-scoped collaborator gets only the allowed site.
//
// Agreement alone would also hold if both forms returned nothing, so every
// subtest also pins the expected sites and counts directly.
package tests

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

type rdySnapRow = sqlc.ListAIReadinessSiteFactsWithAbilityCountsRow

// rdySnapCounts is a combined row's four count columns, in column order.
type rdySnapCounts struct{ elAttributed, elInNamespace, brAttributed, brInNamespace int64 }

func rdySnapCountsOf(r rdySnapRow) rdySnapCounts {
	return rdySnapCounts{
		r.ElementorAbilitiesAttributed, r.ElementorAbilitiesInNamespace,
		r.BricksAbilitiesAttributed, r.BricksAbilitiesInNamespace,
	}
}

func rdySnapCombined(t *testing.T, tx pgx.Tx, tenant uuid.UUID, site *uuid.UUID) map[uuid.UUID]rdySnapRow {
	t.Helper()
	arg := sqlc.ListAIReadinessSiteFactsWithAbilityCountsParams{TenantID: tenant}
	if site != nil {
		arg.SiteID = pgtype.UUID{Bytes: *site, Valid: true}
	}
	rows, err := sqlc.New(tx).ListAIReadinessSiteFactsWithAbilityCounts(context.Background(), arg)
	if err != nil {
		t.Fatalf("ListAIReadinessSiteFactsWithAbilityCounts(tenant=%s, site=%v): %v", tenant, site, err)
	}
	out := make(map[uuid.UUID]rdySnapRow, len(rows))
	for _, r := range rows {
		if _, dup := out[r.SiteID]; dup {
			t.Fatalf("ListAIReadinessSiteFactsWithAbilityCounts returned site %s twice", r.SiteID)
		}
		out[r.SiteID] = r
	}
	return out
}

// rdySnapShow prints a field value with pointers dereferenced and bytes as
// text, so a mismatch names values rather than addresses.
func rdySnapShow(v reflect.Value) string {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return "nil"
		}
		return "&" + rdySnapShow(v.Elem())
	}
	if b, ok := v.Interface().([]byte); ok {
		if b == nil {
			return "nil"
		}
		return fmt.Sprintf("%q", b)
	}
	return fmt.Sprintf("%+v", v.Interface())
}

// rdySnapFactsMismatches compares every field of the facts query's row with
// the combined row's field of the same name. A facts field the combined row
// does not carry is itself a mismatch.
func rdySnapFactsMismatches(want sqlc.ListAIReadinessSiteFactsRow, got rdySnapRow) []string {
	var out []string
	wv, gv := reflect.ValueOf(want), reflect.ValueOf(got)
	for i := 0; i < wv.NumField(); i++ {
		name := wv.Type().Field(i).Name
		g := gv.FieldByName(name)
		if !g.IsValid() {
			out = append(out, name+": a facts column the combined row does not carry")
			continue
		}
		w := wv.Field(i)
		if wt, ok := w.Interface().(pgtype.Timestamptz); ok {
			gt, _ := g.Interface().(pgtype.Timestamptz)
			if wt.Valid != gt.Valid || wt.InfinityModifier != gt.InfinityModifier || !wt.Time.Equal(gt.Time) {
				out = append(out, fmt.Sprintf("%s: combined %s, facts query %s", name, rdySnapShow(g), rdySnapShow(w)))
			}
			continue
		}
		if !reflect.DeepEqual(w.Interface(), g.Interface()) {
			out = append(out, fmt.Sprintf("%s: combined %s, facts query %s", name, rdySnapShow(g), rdySnapShow(w)))
		}
	}
	return out
}

// rdySnapAgree reads the combined query and the two separate queries in one
// transaction with the same arguments and fails on any difference: the site
// set, every facts column, and the four counts. It returns the combined rows
// and the count query's rows for the caller's own expectations.
func rdySnapAgree(t *testing.T, tx pgx.Tx, where string, tenant uuid.UUID, site *uuid.UUID) (map[uuid.UUID]rdySnapRow, map[rdyCountKey]rdyCount) {
	t.Helper()
	facts := rdyFacts(t, tx, tenant, site)
	counts := rdyCounts(t, tx, tenant, site)
	got := rdySnapCombined(t, tx, tenant, site)

	for id := range facts {
		if _, ok := got[id]; !ok {
			t.Errorf("%s: the facts query returns site %s and the combined query does not", where, id)
		}
	}
	for id, row := range got {
		f, ok := facts[id]
		if !ok {
			t.Errorf("%s: the combined query returns site %s and the facts query does not", where, id)
			continue
		}
		for _, m := range rdySnapFactsMismatches(f, row) {
			t.Errorf("%s: site %s: %s", where, id, m)
		}
		el := counts[rdyCountKey{id, "elementor"}]
		br := counts[rdyCountKey{id, "bricks"}]
		want := rdySnapCounts{el.attributed, el.inNamespace, br.attributed, br.inNamespace}
		if g := rdySnapCountsOf(row); g != want {
			t.Errorf("%s: site %s: combined counts %+v, count query %+v", where, id, g, want)
		}
	}
	return got, counts
}

func rdySnapWantSites(t *testing.T, where string, got map[uuid.UUID]rdySnapRow, want map[uuid.UUID]string) {
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

// TestAIReadinessSiteFactsWithAbilityCountsAsAppRole is the proof. One
// container; the subtests only read the fixture.
func TestAIReadinessSiteFactsWithAbilityCountsAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()

	tenantA := seedTenant(t, pool, "rdys-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, pool, "rdys-b-"+uuid.NewString()[:8])

	// Elementor, the MCP Adapter and Bricks, with every facts column set.
	sFull := rdySeedSite(t, admin, tenantA, rdySiteSeed{
		enrolled: true, state: "connected", wp: "7.1", agent: "0.61.159", editing: true,
		components: `{"plugins":[
			{"slug":"elementor/elementor.php","name":"x","version":"4.3.4","active":true},
			{"slug":"mcp-adapter/mcp-adapter.php","name":"x","version":"0.7.0","active":true}],
		 "themes":[{"slug":"bricks","name":"x","version":"2.4.1","active":false}],
		 "builder_facts":{"v":1,"theme_template":"bricks"}}`,
	})
	// Bricks only: elementor counts must read 0 where the count query has no row.
	sBricks := rdySeedSite(t, admin, tenantA, rdySiteSeed{
		enrolled: true, state: "connected", wp: "7.0", agent: "0.61.158",
		components: `{"plugins":[],"themes":[{"slug":"bricks","name":"x","version":"2.5.0","active":true}]}`,
	})
	// Enrolled, never pushed metadata, never inventoried: no run, no counts.
	sBare := rdySeedSite(t, admin, tenantA, rdySiteSeed{enrolled: true, state: "connected"})
	// Archived WITH an inventory, so the count query returns a row for a site
	// the facts query drops.
	sArchived := rdySeedSite(t, admin, tenantA, rdySiteSeed{
		enrolled: true, state: "archived", wp: "7.1", components: `{"plugins":[]}`,
	})
	sPending := rdySeedSite(t, admin, tenantA, rdySiteSeed{enrolled: false, state: "pending_enrollment"})
	sB := rdySeedSite(t, admin, tenantB, rdySiteSeed{
		enrolled: true, state: "connected", wp: "7.1", agent: "0.61.159",
		components: `{"plugins":[{"slug":"elementor/elementor.php","name":"x","version":"4.3.4","active":true}],"themes":[]}`,
	})

	// sFull: elementor 2 attributed of 4 in the namespace, bricks 1 of 2; each
	// spoof differs from a counted row in one field. The core row is outside
	// both namespaces.
	rdySeedInventory(t, pool, tenantA, sFull,
		rdyAbility{"elementor/build-page", "plugin", "elementor", "true"},
		rdyAbility{"elementor/list-widgets", "plugin", "elementor", ""},
		rdyAbility{"elementor/spoof-dir", "plugin", "evil", ""},
		rdyAbility{"elementor/spoof-unverified", "plugin", "elementor", "false"},
		rdyAbility{"bricks/get-page", "theme", "bricks", ""},
		rdyAbility{"bricks/spoof-kind", "plugin", "bricks", ""},
		rdyAbility{"core/get-site-info", "core", "", ""},
	)
	rdySeedInventory(t, pool, tenantA, sBricks,
		rdyAbility{"bricks/get-page", "theme", "bricks", "true"},
		rdyAbility{"bricks/list-elements", "theme", "bricks", ""},
	)
	rdySeedInventory(t, pool, tenantA, sArchived,
		rdyAbility{"elementor/build-page", "plugin", "elementor", "true"},
	)
	rdySeedInventory(t, pool, tenantB, sB,
		rdyAbility{"elementor/build-page", "plugin", "elementor", "true"},
		rdyAbility{"elementor/list-widgets", "plugin", "elementor", "true"},
	)

	// A member of tenant A who also holds a share on tenant B's site.
	user := seedUserRow(t, admin, "rdys-"+uuid.NewString()[:8]+"@example.test")
	seedMembershipRow(t, admin, user, tenantA)
	seedSiteShare(t, admin, tenantB, sB, user, "viewer")
	memberA := domain.Principal{Type: domain.PrincipalUser, UserID: user, TenantID: tenantA, Role: "owner", Scope: domain.ScopeOrg}

	wantFull := rdySnapCounts{elAttributed: 2, elInNamespace: 4, brAttributed: 1, brInNamespace: 2}
	wantBricks := rdySnapCounts{elAttributed: 0, elInNamespace: 0, brAttributed: 2, brInNamespace: 2}
	wantBare := rdySnapCounts{}
	wantB := rdySnapCounts{elAttributed: 2, elInNamespace: 2}

	t.Run("fleet call agrees for a member holding a share in another tenant", func(t *testing.T) {
		if err := pool.RunTenantTx(ctx, memberA, func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "RunTenantTx (tenant A member)")

			// Premise: the share makes tenant B's site visible to a bare SELECT
			// in this transaction, so its absence below is the tenant
			// predicate's doing.
			var visible int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM sites WHERE id = $1`, sB).Scan(&visible); err != nil {
				t.Fatalf("premise read: %v", err)
			}
			if visible != 1 {
				t.Fatalf("PREMISE LOST: tenant B's shared site is visible %d times to a bare SELECT, want 1", visible)
			}

			got, counts := rdySnapAgree(t, tx, "tenant A fleet", tenantA, nil)

			// Premise: the count query does return the archived site's
			// counts, so their absence from the combined rows is tested.
			if c, ok := counts[rdyCountKey{sArchived, "elementor"}]; !ok || c.attributed != 1 {
				t.Fatalf("PREMISE LOST: the count query returns %+v (present=%t) for the archived site, want attributed 1", c, ok)
			}
			if _, leaked := got[sArchived]; leaked {
				t.Errorf("ARCHIVED SITE RETURNED by the combined query: %s", sArchived)
			}
			if _, leaked := got[sB]; leaked {
				t.Errorf("TENANCY LEAK: the combined query returns tenant B's site %s, reachable only through a share", sB)
			}
			rdySnapWantSites(t, "tenant A fleet", got, map[uuid.UUID]string{sFull: "full", sBricks: "bricks", sBare: "bare"})

			for label, c := range map[string]struct {
				id   uuid.UUID
				want rdySnapCounts
			}{"full": {sFull, wantFull}, "bricks": {sBricks, wantBricks}, "bare": {sBare, wantBare}} {
				if g := rdySnapCountsOf(got[c.id]); g != c.want {
					t.Errorf("%s: combined counts %+v, want %+v", label, g, c.want)
				}
			}
			full := got[sFull]
			if !full.AbilitiesCheckedAt.Valid || full.AbilitiesApiPresent == nil || !*full.AbilitiesApiPresent ||
				!full.ElementorInstalled || full.ElementorVersion != "4.3.4" || !full.McpAdapterActive ||
				full.BricksVersion != "2.4.1" || full.BuilderFacts == nil || !full.ContentEditingEnabledAt.Valid {
				t.Errorf("full: facts not as seeded: %+v", full)
			}
			if bare := got[sBare]; bare.AbilitiesCheckedAt.Valid || bare.AbilitiesApiPresent != nil || bare.AbilitiesTruncated != nil {
				t.Errorf("bare: run fields set on a site never inventoried: %+v", bare)
			}
			return nil
		}); err != nil {
			t.Fatalf("tenant A member read: %v", err)
		}
	})

	t.Run("per-site call agrees and returns only the named site", func(t *testing.T) {
		if err := pool.RunTenantTx(ctx, memberA, func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "RunTenantTx (tenant A member, per site)")
			for label, c := range map[string]struct {
				id   uuid.UUID
				want rdySnapCounts
			}{"full": {sFull, wantFull}, "bricks": {sBricks, wantBricks}, "bare": {sBare, wantBare}} {
				id := c.id
				got, _ := rdySnapAgree(t, tx, "per-site "+label, tenantA, &id)
				rdySnapWantSites(t, "per-site "+label, got, map[uuid.UUID]string{id: label})
				if g := rdySnapCountsOf(got[id]); g != c.want {
					t.Errorf("per-site %s: combined counts %+v, want %+v", label, g, c.want)
				}
			}
			for label, id := range map[string]uuid.UUID{"archived": sArchived, "never enrolled": sPending, "tenant B shared": sB} {
				id := id
				if got, _ := rdySnapAgree(t, tx, "per-site "+label, tenantA, &id); len(got) != 0 {
					t.Errorf("per-site call for the %s site %s returned %d rows, want 0", label, id, len(got))
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("per-site read: %v", err)
		}
	})

	t.Run("another tenant naming tenant A gets nothing from either form", func(t *testing.T) {
		if err := pool.RunTenantTx(ctx, acprOrgPrincipal(tenantB), func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "RunTenantTx (tenant B)")
			// The explicit predicates match tenant A here, so only RLS can
			// refuse these rows.
			got, counts := rdySnapAgree(t, tx, "tenant B naming tenant A", tenantA, nil)
			if len(got) != 0 || len(counts) != 0 {
				t.Errorf("TENANCY LEAK: a tenant B transaction read %d combined rows and %d count rows of tenant A", len(got), len(counts))
			}
			// Positive control: the same transaction reads its own tenant.
			own, _ := rdySnapAgree(t, tx, "tenant B fleet", tenantB, nil)
			rdySnapWantSites(t, "tenant B fleet", own, map[uuid.UUID]string{sB: "tenant B"})
			if g := rdySnapCountsOf(own[sB]); g != wantB {
				t.Errorf("tenant B: combined counts %+v, want %+v", g, wantB)
			}
			return nil
		}); err != nil {
			t.Fatalf("tenant B read: %v", err)
		}
	})

	t.Run("a site-scoped collaborator gets only the allowed site from either form", func(t *testing.T) {
		if err := pool.RunTenantTx(ctx, acprSitePrincipal(tenantA, sFull), func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "RunTenantTx (site-scoped to full)")
			got, counts := rdySnapAgree(t, tx, "site-scoped fleet", tenantA, nil)
			rdySnapWantSites(t, "site-scoped fleet", got, map[uuid.UUID]string{sFull: "full"})
			for k := range counts {
				if k.site != sFull {
					t.Errorf("SITE-SCOPE LEAK: the count query returns site %s namespace %s outside the scope", k.site, k.ns)
				}
			}
			if g := rdySnapCountsOf(got[sFull]); g != wantFull {
				t.Errorf("OVER-FIRING: the allowed site's combined counts under site scope are %+v, want %+v", g, wantFull)
			}
			if !got[sFull].AbilitiesCheckedAt.Valid {
				t.Errorf("OVER-FIRING: the allowed site's run record is missing under site scope")
			}
			id := sBricks
			if got, _ := rdySnapAgree(t, tx, "site-scoped per-site bricks", tenantA, &id); len(got) != 0 {
				t.Errorf("SITE-SCOPE LEAK: a per-site call for %s outside the scope returned %d rows", sBricks, len(got))
			}
			return nil
		}); err != nil {
			t.Fatalf("site-scoped read: %v", err)
		}
	})
}
