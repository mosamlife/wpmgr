package portal

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// The portal summary joins each report site to the live site list for its
// connection state and last backup. site.Service.List clamps one call to 200
// rows, so the summary's single call for 500 returned 200, and every site past
// them showed "disconnected" with no backup.

// pagedSites serves a fixed site list the way site.Service.List does: at most
// 200 rows a call (a larger Limit is clamped, not refused), from Offset, in a
// fixed order. It records every call.
type pagedSites struct {
	all   []site.Site
	calls []site.ListInput
	err   error
}

func (p *pagedSites) List(_ context.Context, in site.ListInput) ([]site.Site, error) {
	p.calls = append(p.calls, in)
	if p.err != nil {
		return nil, p.err
	}
	limit := int(in.Limit)
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	start := int(in.Offset)
	if start > len(p.all) {
		start = len(p.all)
	}
	end := start + limit
	if end > len(p.all) {
		end = len(p.all)
	}
	return append([]site.Site(nil), p.all[start:end]...), nil
}

func fakeSites(n int) []site.Site {
	out := make([]site.Site, n)
	for i := range out {
		out[i] = site.Site{ID: uuid.New(), Name: fmt.Sprintf("site-%03d", i), ConnectionState: "connected"}
	}
	return out
}

func testPrincipal() domain.Principal {
	return domain.Principal{
		TenantID:       uuid.New(),
		UserID:         uuid.New(),
		Scope:          "site",
		AllowedSiteIDs: []uuid.UUID{uuid.New()},
	}
}

func TestListAllSitesReadsEverySitePastTheFirstPage(t *testing.T) {
	for _, n := range []int{0, 1, 199, 200, 201, 450, 600} {
		t.Run(fmt.Sprintf("%d sites", n), func(t *testing.T) {
			src := &pagedSites{all: fakeSites(n)}
			p := testPrincipal()
			got, err := listAllSites(context.Background(), src, p.TenantID, p)
			if err != nil {
				t.Fatalf("listAllSites: %v", err)
			}
			if len(got) != n {
				t.Fatalf("read %d of %d sites", len(got), n)
			}
			seen := map[uuid.UUID]bool{}
			for _, s := range got {
				if seen[s.ID] {
					t.Fatalf("site %s read twice", s.ID)
				}
				seen[s.ID] = true
			}
			for _, s := range src.all {
				if !seen[s.ID] {
					t.Fatalf("site %s (%s) was never read", s.ID, s.Name)
				}
			}
		})
	}
}

// The premise: one call, however large its Limit, stops at 200. Without it the
// test above could pass against a list that was never clamped.
func TestPagedSitesFakeClampsOneCallAt200(t *testing.T) {
	src := &pagedSites{all: fakeSites(450)}
	rows, err := src.List(context.Background(), site.ListInput{Limit: 500})
	if err != nil || len(rows) != 200 {
		t.Fatalf("one call with Limit 500 returned %d rows (err %v), want 200", len(rows), err)
	}
}

// The scope is unchanged: every page is requested for the caller's tenant and
// with the caller's principal, which is what restricts the rows to the sites
// the principal may see.
func TestListAllSitesAsksEveryPageAsTheSamePrincipal(t *testing.T) {
	src := &pagedSites{all: fakeSites(450)}
	p := testPrincipal()
	if _, err := listAllSites(context.Background(), src, p.TenantID, p); err != nil {
		t.Fatal(err)
	}
	if len(src.calls) != 3 {
		t.Fatalf("%d List calls for 450 sites, want 3", len(src.calls))
	}
	for i, c := range src.calls {
		if c.Principal == nil {
			t.Fatalf("call %d carried no principal, so it would list the whole tenant", i)
		}
		if got, ok := c.Principal.(domain.Principal); !ok || got.UserID != p.UserID || got.Scope != p.Scope {
			t.Errorf("call %d carried principal %v, want the caller's", i, c.Principal)
		}
		if c.TenantID != p.TenantID {
			t.Errorf("call %d tenant %s, want %s", i, c.TenantID, p.TenantID)
		}
		if c.Limit > 200 {
			t.Errorf("call %d asked for %d rows; one call returns at most 200", i, c.Limit)
		}
		if want := int32(i) * 200; c.Offset != want {
			t.Errorf("call %d offset %d, want %d", i, c.Offset, want)
		}
		if c.ClientID != nil || c.State != "" || c.Query != "" || len(c.AnyTags) != 0 || len(c.AllTags) != 0 {
			t.Errorf("call %d added a filter the summary did not have: %+v", i, c)
		}
	}
}

func TestListAllSitesReturnsTheError(t *testing.T) {
	src := &pagedSites{all: fakeSites(10), err: fmt.Errorf("db down")}
	p := testPrincipal()
	if _, err := listAllSites(context.Background(), src, p.TenantID, p); err == nil {
		t.Fatal("a failed read returned no error, so the summary would show every site as disconnected")
	}
}
