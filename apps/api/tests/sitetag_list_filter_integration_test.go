// sitetag_list_filter_integration_test.go — GH #230 "rich tags" (m100)
// GET /api/v1/sites list-filter semantics: any (tags && ...) vs all
// (tags @> ...) vs the legacy single ?tag= alias vs no filter at all,
// combined with the existing ?state= filter. Requires Docker; skips when
// unavailable (via startPostgres).
package tests

import (
	"context"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

func siteNames(t *testing.T, sites []site.Site) []string {
	t.Helper()
	out := make([]string, 0, len(sites))
	for _, s := range sites {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}

func TestSiteList_TagFilters_AnyAllLegacyNone(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	siteSvc, _ := newSiteTagServices(pool)
	tenant := seedTenant(t, pool, "sitetag-listfilter-"+uuid.NewString()[:8])

	mk := func(name string, tags []string) {
		s, err := siteSvc.Create(ctx, site.CreateInput{TenantID: tenant, URL: "https://" + name + ".example.com", Name: name})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if len(tags) > 0 {
			if _, err := siteSvc.SetTags(ctx, site.SetTagsInput{TenantID: tenant, SiteID: s.ID, Tags: tags}); err != nil {
				t.Fatalf("SetTags %s: %v", name, err)
			}
		}
	}
	mk("only-prod", []string{"prod"})
	mk("only-staging", []string{"staging"})
	mk("prod-and-eu", []string{"prod", "eu"})
	mk("no-tags", nil)

	list := func(in site.ListInput) []string {
		in.TenantID = tenant
		in.Limit = 100
		got, err := siteSvc.List(ctx, in)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		return siteNames(t, got)
	}

	t.Run("any_single_tag_matches_prod_bearers", func(t *testing.T) {
		got := list(site.ListInput{AnyTags: []string{"prod"}})
		want := []string{"only-prod", "prod-and-eu"}
		if !equalStrSlices(got, want) {
			t.Fatalf("any=[prod]: got %v, want %v", got, want)
		}
	})

	t.Run("any_multi_tag_is_union", func(t *testing.T) {
		got := list(site.ListInput{AnyTags: []string{"prod", "staging"}})
		want := []string{"only-prod", "only-staging", "prod-and-eu"}
		if !equalStrSlices(got, want) {
			t.Fatalf("any=[prod,staging]: got %v, want %v", got, want)
		}
	})

	t.Run("all_requires_every_tag", func(t *testing.T) {
		got := list(site.ListInput{AllTags: []string{"prod", "eu"}})
		want := []string{"prod-and-eu"}
		if !equalStrSlices(got, want) {
			t.Fatalf("all=[prod,eu]: got %v, want %v", got, want)
		}
		// A single-element AllTags must behave like a membership filter too.
		got2 := list(site.ListInput{AllTags: []string{"prod"}})
		want2 := []string{"only-prod", "prod-and-eu"}
		if !equalStrSlices(got2, want2) {
			t.Fatalf("all=[prod]: got %v, want %v", got2, want2)
		}
	})

	t.Run("legacy_alias_is_any_semantics", func(t *testing.T) {
		// The handler maps legacy ?tag=x to AnyTags=[x]; exercise the same
		// repo-level input the handler would build.
		got := list(site.ListInput{AnyTags: []string{"staging"}})
		want := []string{"only-staging"}
		if !equalStrSlices(got, want) {
			t.Fatalf("legacy tag=staging (any=[staging]): got %v, want %v", got, want)
		}
	})

	t.Run("no_filter_returns_everything", func(t *testing.T) {
		got := list(site.ListInput{})
		want := []string{"no-tags", "only-prod", "only-staging", "prod-and-eu"}
		if !equalStrSlices(got, want) {
			t.Fatalf("no filter: got %v, want %v", got, want)
		}
	})

	t.Run("combined_with_state_filter", func(t *testing.T) {
		// Archive one of the prod-tagged sites directly (admin bypass — no
		// public connection-lifecycle transition needed for this fixture).
		// The default (no ?state=) list must hide it even though it still
		// carries the tag; ?state=archived must surface it, still filtered
		// by tag.
		var onlyProdID uuid.UUID
		for _, s := range mustListRaw(t, siteSvc, ctx, tenant) {
			if s.Name == "only-prod" {
				onlyProdID = s.ID
			}
		}
		if onlyProdID == uuid.Nil {
			t.Fatal("could not resolve only-prod site id")
		}
		admin := connectAdmin(t, pool)
		defer admin.Close()
		if _, err := admin.Exec(ctx, `UPDATE sites SET connection_state = 'archived' WHERE id = $1`, onlyProdID); err != nil {
			t.Fatalf("archive only-prod: %v", err)
		}

		gotDefault := list(site.ListInput{AnyTags: []string{"prod"}})
		wantDefault := []string{"prod-and-eu"}
		if !equalStrSlices(gotDefault, wantDefault) {
			t.Fatalf("any=[prod] (default, archived hidden): got %v, want %v", gotDefault, wantDefault)
		}

		gotArchived := list(site.ListInput{AnyTags: []string{"prod"}, State: "archived"})
		wantArchived := []string{"only-prod"}
		if !equalStrSlices(gotArchived, wantArchived) {
			t.Fatalf("any=[prod] state=archived: got %v, want %v", gotArchived, wantArchived)
		}
	})
}

func mustListRaw(t *testing.T, svc *site.Service, ctx context.Context, tenant uuid.UUID) []site.Site {
	t.Helper()
	got, err := svc.List(ctx, site.ListInput{TenantID: tenant, Limit: 100})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return got
}
