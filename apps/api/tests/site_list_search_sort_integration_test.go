// site_list_search_sort_integration_test.go (GH #349). The row-level truth for
// server-side search (?q=) and ordering (?sort=) on GET /api/v1/sites, proved
// against a real Postgres because that is the only place ORDER BY, NULL
// placement, tiebreaks and LIMIT/OFFSET paging actually exist. Requires Docker;
// skips when unavailable (via startPostgres).
//
// Why these live server side at all: the web used to fetch one page (50 sites
// by default) and filter it in the browser, so an agency with more sites than
// that searched only the newest page and was told "no results" for a site it
// owns. A filter applied after the server has already truncated the list is
// wrong at any page size.
package tests

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// searchFixture is the shared corpus. The values are chosen so that each
// assertion below can only pass for the right reason:
//   - names mix case ("Alpha Blog" vs "beta shop") so a case-sensitive name
//     sort would order them differently from a case-insensitive one.
//   - tags mix case ("EU" vs "eu") so a case-sensitive tag search would miss
//     one of them.
//   - exactly one site has never been seen (NULL last_seen_at).
type searchFixture struct {
	tenant uuid.UUID
	ids    map[string]uuid.UUID
	svc    *site.Service
}

func seedSearchFixture(t *testing.T, pool *db.Pool) searchFixture {
	t.Helper()
	ctx := context.Background()
	siteSvc, _ := newSiteTagServices(pool)
	tenant := seedTenant(t, pool, "sitesearch-"+uuid.NewString()[:8])
	admin := connectAdmin(t, pool)
	t.Cleanup(admin.Close)

	base := time.Now().UTC().Add(-24 * time.Hour)
	fx := searchFixture{tenant: tenant, ids: map[string]uuid.UUID{}, svc: siteSvc}

	type spec struct {
		name     string
		url      string
		tags     []string
		created  time.Time
		lastSeen *time.Time
	}
	seenAlpha := base.Add(23 * time.Hour) // most recent
	seenBeta := base.Add(22 * time.Hour)
	seenDelta := base.Add(21 * time.Hour) // oldest contact
	specs := []spec{
		{"Alpha Blog", "https://alpha.example.com", []string{"prod"}, base.Add(1 * time.Minute), &seenAlpha},
		{"beta shop", "https://beta-store.test", []string{"staging", "EU"}, base.Add(2 * time.Minute), &seenBeta},
		{"Gamma News", "https://gamma.example.org", []string{"prod", "eu"}, base.Add(3 * time.Minute), nil},
		{"delta", "https://delta.example.com", nil, base.Add(4 * time.Minute), &seenDelta},
	}
	for _, sp := range specs {
		s, err := siteSvc.Create(ctx, site.CreateInput{TenantID: tenant, URL: sp.url, Name: sp.name})
		if err != nil {
			t.Fatalf("create %s: %v", sp.name, err)
		}
		if len(sp.tags) > 0 {
			if _, err := siteSvc.SetTags(ctx, site.SetTagsInput{TenantID: tenant, SiteID: s.ID, Tags: sp.tags}); err != nil {
				t.Fatalf("SetTags %s: %v", sp.name, err)
			}
		}
		// created_at/last_seen_at are set by the server, so pin them
		// out-of-band (admin pool, RLS-bypass) to make ordering deterministic.
		if _, err := admin.Exec(ctx,
			`UPDATE sites SET created_at = $2, last_seen_at = $3 WHERE id = $1`,
			s.ID, sp.created, sp.lastSeen); err != nil {
			t.Fatalf("pin timestamps for %s: %v", sp.name, err)
		}
		fx.ids[sp.name] = s.ID
	}
	return fx
}

// listNames returns the site names in the order the query returned them. It
// deliberately does NOT sort: the order IS the assertion.
func listNames(t *testing.T, svc *site.Service, in site.ListInput) []string {
	t.Helper()
	got, err := svc.List(context.Background(), in)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	out := make([]string, 0, len(got))
	for _, s := range got {
		out = append(out, s.Name)
	}
	return out
}

func eqOrdered(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// T1 + T2 + R6: q matches name, url and tag, case-insensitively, and a query
// that matches nothing is an empty list rather than an error.
//
// R6 note: the tag arm searches s.tags, the SAME array the response returns,
// so any tag an operator can see on a site is a tag they can find it by.
func TestSiteList_Search_MatchesNameURLAndTag(t *testing.T) {
	pool := startPostgres(t)
	fx := seedSearchFixture(t, pool)

	cases := []struct {
		name string
		q    string
		want []string
		why  string
	}{
		{"name exact case", "Alpha", []string{"Alpha Blog"}, "name substring"},
		{"name upper", "ALPHA", []string{"Alpha Blog"}, "name substring, case-insensitive"},
		{"name lower", "alpha", []string{"Alpha Blog"}, "name substring, case-insensitive"},
		{"name mid-word", "amma", []string{"Gamma News"}, "substring, not prefix"},
		{"url host", "beta-store", []string{"beta shop"}, "url substring, name does not contain it"},
		{"url tld upper", "EXAMPLE.ORG", []string{"Gamma News"}, "url substring, case-insensitive"},
		{"tag exact", "staging", []string{"beta shop"}, "tag substring"},
		{"tag case-insensitive both ways", "eu", []string{"beta shop", "Gamma News"}, "matches tag EU and tag eu"},
		{"no match is empty, not an error", "zzz-nothing-matches-this", []string{}, "T2"},
		{"percent is literal, not a wildcard", "%", []string{}, "q is a search string, not a LIKE pattern"},
		{"underscore is literal, not a wildcard", "_", []string{}, "q is a search string, not a LIKE pattern"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// sort by name so the expectation is order-stable regardless of
			// which sort happens to be the default.
			got := listNames(t, fx.svc, site.ListInput{TenantID: fx.tenant, Query: tc.q, Sort: "name", Limit: 100})
			if !eqOrdered(got, tc.want) {
				t.Fatalf("q=%q: got %v, want %v (%s)", tc.q, got, tc.want, tc.why)
			}
		})
	}
}

// T3: each of the six sort values orders correctly.
// T7: null last_seen (a site that has never checked in) sorts LAST in BOTH
// directions, per the R2 decision. Gamma News is the never-seen site: it must
// appear in both last_seen orderings, and at the end of each.
func TestSiteList_Sort_AllSixOrderings(t *testing.T) {
	pool := startPostgres(t)
	fx := seedSearchFixture(t, pool)

	cases := []struct {
		sort string
		want []string
	}{
		// Case-insensitive: with a plain byte collation "Gamma News" would sort
		// before "beta shop" and "delta", which is not what an operator means
		// by "sort by name".
		{"name", []string{"Alpha Blog", "beta shop", "delta", "Gamma News"}},
		{"-name", []string{"Gamma News", "delta", "beta shop", "Alpha Blog"}},
		{"created_at", []string{"Alpha Blog", "beta shop", "Gamma News", "delta"}},
		{"-created_at", []string{"delta", "Gamma News", "beta shop", "Alpha Blog"}},
		// last_seen: delta oldest contact, then beta, then Alpha; the
		// never-seen Gamma News is LAST in both directions.
		{"last_seen", []string{"delta", "beta shop", "Alpha Blog", "Gamma News"}},
		{"-last_seen", []string{"Alpha Blog", "beta shop", "delta", "Gamma News"}},
	}
	for _, tc := range cases {
		t.Run(tc.sort, func(t *testing.T) {
			got := listNames(t, fx.svc, site.ListInput{TenantID: fx.tenant, Sort: tc.sort, Limit: 100})
			if !eqOrdered(got, tc.want) {
				t.Fatalf("sort=%q: got %v, want %v", tc.sort, got, tc.want)
			}
		})
	}

	t.Run("never-seen site is present in both directions", func(t *testing.T) {
		for _, s := range []string{"last_seen", "-last_seen"} {
			got := listNames(t, fx.svc, site.ListInput{TenantID: fx.tenant, Sort: s, Limit: 100})
			if len(got) != 4 {
				t.Fatalf("sort=%q returned %d sites, want 4: a null last_seen must not drop a site", s, len(got))
			}
			if got[len(got)-1] != "Gamma News" {
				t.Fatalf("sort=%q: last row = %q, want the never-seen site Gamma News", s, got[len(got)-1])
			}
		}
	})
}

// T5: absent sort is the historical ordering, identical to an explicit
// -created_at. Adding the parameter must not move a single row for a client
// that does not send it.
func TestSiteList_Sort_AbsentEqualsCreatedAtDesc(t *testing.T) {
	pool := startPostgres(t)
	fx := seedSearchFixture(t, pool)

	absent := listNames(t, fx.svc, site.ListInput{TenantID: fx.tenant, Limit: 100})
	explicit := listNames(t, fx.svc, site.ListInput{TenantID: fx.tenant, Sort: "-created_at", Limit: 100})
	if !eqOrdered(absent, explicit) {
		t.Fatalf("absent sort = %v, explicit -created_at = %v: must be identical", absent, explicit)
	}
	want := []string{"delta", "Gamma News", "beta shop", "Alpha Blog"}
	if !eqOrdered(absent, want) {
		t.Fatalf("absent sort = %v, want %v (newest first, the pre-change ordering)", absent, want)
	}
}

// tieFixture is a tenant whose sites deliberately COLLIDE on the sort keys:
// two share the name "Twin", two share a created_at. It is the only fixture
// that can tell a total ordering apart from a merely plausible one.
type tieFixture struct {
	svc                            *site.Service
	tenant                         uuid.UUID
	twinA, twinB, coevalA, coevalB uuid.UUID
}

func seedTieFixture(t *testing.T, pool *db.Pool) tieFixture {
	t.Helper()
	ctx := context.Background()
	siteSvc, _ := newSiteTagServices(pool)
	tenant := seedTenant(t, pool, "sitesort-ties-"+uuid.NewString()[:8])
	admin := connectAdmin(t, pool)
	t.Cleanup(admin.Close)

	sameInstant := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	mk := func(name, url string, created time.Time) uuid.UUID {
		s, err := siteSvc.Create(ctx, site.CreateInput{TenantID: tenant, URL: url, Name: name})
		if err != nil {
			t.Fatalf("create %s: %v", url, err)
		}
		if _, err := admin.Exec(ctx, `UPDATE sites SET created_at = $2 WHERE id = $1`, s.ID, created); err != nil {
			t.Fatalf("pin created_at: %v", err)
		}
		return s.ID
	}
	return tieFixture{
		svc:     siteSvc,
		tenant:  tenant,
		twinA:   mk("Twin", "https://twin-a.example.com", sameInstant.Add(-time.Minute)),
		twinB:   mk("Twin", "https://twin-b.example.com", sameInstant.Add(-2*time.Minute)),
		coevalA: mk("Coeval A", "https://coeval-a.example.com", sameInstant),
		coevalB: mk("Coeval B", "https://coeval-b.example.com", sameInstant),
	}
}

// T6 + R1: every ordering is TOTAL. Two sites share a name and two share a
// created_at; without a unique tiebreak their relative order is whatever the
// plan happens to produce, which lets LIMIT/OFFSET paging drop one row and
// repeat another. The tiebreak is id DESC, so this asserts both that repeated
// calls agree AND that they agree on the specific documented order.
func TestSiteList_Sort_TiesAreBrokenDeterministically(t *testing.T) {
	pool := startPostgres(t)
	tf := seedTieFixture(t, pool)
	siteSvc, tenant := tf.svc, tf.tenant
	twinA, twinB, coA, coB := tf.twinA, tf.twinB, tf.coevalA, tf.coevalB
	ctx := context.Background()

	listIDs := func(sort string) []uuid.UUID {
		got, err := siteSvc.List(ctx, site.ListInput{TenantID: tenant, Sort: sort, Limit: 100})
		if err != nil {
			t.Fatalf("List(sort=%q): %v", sort, err)
		}
		ids := make([]uuid.UUID, 0, len(got))
		for _, s := range got {
			ids = append(ids, s.ID)
		}
		return ids
	}
	idsEqual := func(a, b []uuid.UUID) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	// higherID reports the id that sorts FIRST under the id DESC tiebreak.
	// Postgres compares uuid byte-wise, which is what bytes.Compare does.
	higherID := func(x, y uuid.UUID) uuid.UUID {
		if bytes.Compare(x[:], y[:]) > 0 {
			return x
		}
		return y
	}
	lowerID := func(x, y uuid.UUID) uuid.UUID {
		if higherID(x, y) == x {
			return y
		}
		return x
	}

	for _, sort := range []string{"name", "-name", "created_at", "-created_at", "last_seen", "-last_seen"} {
		t.Run("stable/"+sort, func(t *testing.T) {
			first := listIDs(sort)
			for i := 0; i < 4; i++ {
				if again := listIDs(sort); !idsEqual(first, again) {
					t.Fatalf("sort=%q is not stable across calls: %v then %v", sort, first, again)
				}
			}
		})
	}

	t.Run("name ties break on id DESC", func(t *testing.T) {
		ids := listIDs("name")
		// "Coeval A", "Coeval B", then the two "Twin" rows.
		pos := map[uuid.UUID]int{}
		for i, id := range ids {
			pos[id] = i
		}
		if pos[higherID(twinA, twinB)] > pos[lowerID(twinA, twinB)] {
			t.Fatalf("same-name tie: expected the higher id first (id DESC), got order %v", ids)
		}
	})

	t.Run("created_at ties break on id DESC", func(t *testing.T) {
		ids := listIDs("created_at")
		pos := map[uuid.UUID]int{}
		for i, id := range ids {
			pos[id] = i
		}
		if pos[higherID(coA, coB)] > pos[lowerID(coA, coB)] {
			t.Fatalf("same-created_at tie: expected the higher id first (id DESC), got order %v", ids)
		}
	})
}

// T8: q narrows the OTHER filters rather than replacing them. Each assertion
// pairs a q that would match on its own with a filter that excludes it, so a
// server that ignored either half would fail.
func TestSiteList_Search_ComposesWithExistingFilters(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	fx := seedSearchFixture(t, pool)
	admin := connectAdmin(t, pool)
	t.Cleanup(admin.Close)

	t.Run("q AND tags", func(t *testing.T) {
		// "alpha" alone matches Alpha Blog; Alpha Blog carries prod, not staging.
		if got := listNames(t, fx.svc, site.ListInput{
			TenantID: fx.tenant, Query: "alpha", AnyTags: []string{"prod"}, Sort: "name", Limit: 100,
		}); !eqOrdered(got, []string{"Alpha Blog"}) {
			t.Fatalf("q=alpha + tags=[prod]: got %v, want [Alpha Blog]", got)
		}
		if got := listNames(t, fx.svc, site.ListInput{
			TenantID: fx.tenant, Query: "alpha", AnyTags: []string{"staging"}, Sort: "name", Limit: 100,
		}); len(got) != 0 {
			t.Fatalf("q=alpha + tags=[staging]: got %v, want [] (the tag filter must still apply)", got)
		}
		// "prod" alone (as a tag search) matches two sites; adding the tag
		// filter for staging must leave none of them.
		if got := listNames(t, fx.svc, site.ListInput{
			TenantID: fx.tenant, Query: "prod", Sort: "name", Limit: 100,
		}); !eqOrdered(got, []string{"Alpha Blog", "Gamma News"}) {
			t.Fatalf("q=prod: got %v, want [Alpha Blog Gamma News]", got)
		}
	})

	t.Run("q AND state", func(t *testing.T) {
		// Connect only Gamma News. q=example matches every site's url.
		if _, err := admin.Exec(ctx,
			`UPDATE sites SET connection_state = 'connected' WHERE id = $1`, fx.ids["Gamma News"]); err != nil {
			t.Fatalf("set connection_state: %v", err)
		}
		if got := listNames(t, fx.svc, site.ListInput{
			TenantID: fx.tenant, Query: "example", State: "connected", Sort: "name", Limit: 100,
		}); !eqOrdered(got, []string{"Gamma News"}) {
			t.Fatalf("q=example + state=connected: got %v, want [Gamma News]", got)
		}
		if got := listNames(t, fx.svc, site.ListInput{
			TenantID: fx.tenant, Query: "beta", State: "connected", Sort: "name", Limit: 100,
		}); len(got) != 0 {
			t.Fatalf("q=beta + state=connected: got %v, want [] (beta shop is not connected)", got)
		}
	})

	t.Run("q AND clientId", func(t *testing.T) {
		var clientID uuid.UUID
		if err := admin.QueryRow(ctx,
			`INSERT INTO clients (tenant_id, name) VALUES ($1, 'Acme Agency') RETURNING id`,
			fx.tenant).Scan(&clientID); err != nil {
			t.Fatalf("insert client: %v", err)
		}
		if _, err := admin.Exec(ctx,
			`UPDATE sites SET client_id = $2 WHERE id = $1`, fx.ids["delta"], clientID); err != nil {
			t.Fatalf("assign client: %v", err)
		}
		if got := listNames(t, fx.svc, site.ListInput{
			TenantID: fx.tenant, Query: "example", ClientID: &clientID, Sort: "name", Limit: 100,
		}); !eqOrdered(got, []string{"delta"}) {
			t.Fatalf("q=example + clientId: got %v, want [delta]", got)
		}
		if got := listNames(t, fx.svc, site.ListInput{
			TenantID: fx.tenant, Query: "alpha", ClientID: &clientID, Sort: "name", Limit: 100,
		}); len(got) != 0 {
			t.Fatalf("q=alpha + clientId: got %v, want [] (Alpha Blog is not in that client)", got)
		}
	})
}

// T9: paging over a filtered, ordered result is coherent. This is the defect
// class the whole change exists to remove: page 1 and page 2 must be disjoint,
// and their concatenation must be exactly the full ordered result. The search
// runs in the database, so page 2 contains real matches rather than "whatever
// was left of the first 50 rows".
func TestSiteList_Search_PaginationIsCoherent(t *testing.T) {
	pool := startPostgres(t)
	fx := seedSearchFixture(t, pool)

	// q=example matches THREE of the four sites: alpha.example.com,
	// gamma.example.org and delta.example.com. beta shop lives on
	// beta-store.test and must not appear on any page. Deliberately a strict
	// subset: a server that ignored q would page over all four and put a
	// non-matching site in front of the operator.
	full := listNames(t, fx.svc, site.ListInput{TenantID: fx.tenant, Query: "example", Sort: "name", Limit: 100})
	want := []string{"Alpha Blog", "delta", "Gamma News"}
	if !eqOrdered(full, want) {
		t.Fatalf("q=example returned %v, want %v (the filter must run in the database, not after the page)", full, want)
	}

	page1 := listNames(t, fx.svc, site.ListInput{TenantID: fx.tenant, Query: "example", Sort: "name", Limit: 2, Offset: 0})
	page2 := listNames(t, fx.svc, site.ListInput{TenantID: fx.tenant, Query: "example", Sort: "name", Limit: 2, Offset: 2})
	if len(page1) != 2 || len(page2) != 1 {
		t.Fatalf("pages = %v / %v, want 2 then 1 row", page1, page2)
	}
	for _, a := range page1 {
		for _, b := range page2 {
			if a == b {
				t.Fatalf("page1 %v and page2 %v overlap on %q", page1, page2, a)
			}
		}
	}
	union := append(append([]string{}, page1...), page2...)
	if !eqOrdered(union, full) {
		t.Fatalf("page1+page2 = %v, want the full ordered result %v", union, full)
	}
	for _, name := range union {
		if name == "beta shop" {
			t.Fatalf("q=example paged in %q, whose url is beta-store.test: pages must contain only matches", name)
		}
	}

	// The same coherence must hold when the sort key COLLIDES. This is the
	// case a missing tiebreak breaks: with two rows equal on the ordering key
	// and no unique final key, nothing stops the two page queries from
	// disagreeing about which of them comes first, dropping one row from the
	// result and repeating the other.
	tf := seedTieFixture(t, pool)
	for _, sort := range []string{"name", "-name", "created_at", "-created_at"} {
		t.Run("tied key/"+sort, func(t *testing.T) {
			all := listNames(t, tf.svc, site.ListInput{TenantID: tf.tenant, Sort: sort, Limit: 100})
			if len(all) != 4 {
				t.Fatalf("full list = %v, want 4 sites", all)
			}
			var paged []string
			for offset := int32(0); offset < 4; offset += 2 {
				paged = append(paged, listNames(t, tf.svc, site.ListInput{
					TenantID: tf.tenant, Sort: sort, Limit: 2, Offset: offset,
				})...)
			}
			if !eqOrdered(paged, all) {
				t.Fatalf("sort=%q paged in 2s = %v, want the full ordered result %v "+
					"(a dropped or repeated row means the ordering is not total)", sort, paged, all)
			}
		})
	}
}

// R5: the new parameters filter WITHIN the existing tenant/site scope and must
// not widen it. A site-scoped collaborator searching for a site outside their
// grant gets nothing, even when the search string clearly matches that site.
func TestSiteList_Search_DoesNotWidenSiteScope(t *testing.T) {
	pool := startPostgres(t)
	fx := seedSearchFixture(t, pool)

	scoped := domain.Principal{
		Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: fx.tenant,
		Role: "operator", Scope: domain.ScopeSite,
		AllowedSiteIDs: []uuid.UUID{fx.ids["Alpha Blog"]},
	}

	// Unfiltered: the scope alone already limits the collaborator to one site.
	if got := listNames(t, fx.svc, site.ListInput{
		TenantID: fx.tenant, Principal: scoped, Sort: "name", Limit: 100,
	}); !eqOrdered(got, []string{"Alpha Blog"}) {
		t.Fatalf("scoped list = %v, want [Alpha Blog]", got)
	}
	// Searching for a site outside the grant returns nothing, not that site.
	for _, q := range []string{"gamma", "beta-store", "staging", "example"} {
		got := listNames(t, fx.svc, site.ListInput{
			TenantID: fx.tenant, Principal: scoped, Query: q, Sort: "name", Limit: 100,
		})
		for _, name := range got {
			if name != "Alpha Blog" {
				t.Fatalf("q=%q under a site-scoped principal returned %q: search must not widen scope", q, name)
			}
		}
	}
	// Cross-tenant: another tenant's sites are never reachable by search.
	other := seedSearchFixture(t, pool)
	if got := listNames(t, fx.svc, site.ListInput{
		TenantID: other.tenant, Query: "alpha", Sort: "name", Limit: 100,
	}); len(got) != 1 {
		t.Fatalf("other tenant q=alpha returned %v, want exactly its own Alpha Blog", got)
	}
}
