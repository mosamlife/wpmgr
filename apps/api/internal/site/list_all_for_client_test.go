package site

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// GH #970. A client report covered only the first page (50) of a client's
// sites. pagedClientRepo holds n sites for one client and serves List the way
// the real query does: ordered, then Limit/Offset applied.
type pagedClientRepo struct {
	fakeRepo
	clientID uuid.UUID
	sites    []Site
	calls    []ListInput
}

func newPagedClientRepo(n int) *pagedClientRepo {
	r := &pagedClientRepo{clientID: uuid.New()}
	for i := 0; i < n; i++ {
		r.sites = append(r.sites, Site{ID: uuid.New(), Name: "s"})
	}
	return r
}

func (r *pagedClientRepo) List(_ context.Context, in ListInput) ([]Site, error) {
	r.calls = append(r.calls, in)
	if in.ClientID == nil || *in.ClientID != r.clientID {
		return []Site{}, nil
	}
	lo := int(in.Offset)
	if lo > len(r.sites) {
		lo = len(r.sites)
	}
	hi := lo + int(in.Limit)
	if hi > len(r.sites) {
		hi = len(r.sites)
	}
	return append([]Site(nil), r.sites[lo:hi]...), nil
}

func TestListAllForClientReturnsEverySite(t *testing.T) {
	for _, n := range []int{0, 1, 50, 60, 200, 201, 450} {
		repo := newPagedClientRepo(n)
		svc := newSvc(repo)
		tenant := uuid.New()

		got, err := svc.ListAllForClient(context.Background(), tenant, repo.clientID)
		if err != nil {
			t.Fatalf("n=%d: ListAllForClient: %v", n, err)
		}
		if len(got) != n {
			t.Fatalf("n=%d: got %d sites, want all %d", n, len(got), n)
		}
		seen := map[uuid.UUID]bool{}
		for i, s := range got {
			if seen[s.ID] {
				t.Fatalf("n=%d: site %s returned twice", n, s.ID)
			}
			seen[s.ID] = true
			if s.ID != repo.sites[i].ID {
				t.Fatalf("n=%d: order changed at %d", n, i)
			}
		}
		for _, c := range repo.calls {
			if c.TenantID != tenant || c.ClientID == nil || *c.ClientID != repo.clientID {
				t.Fatalf("n=%d: a page was read without the caller's tenant and client: %+v", n, c)
			}
			if c.State != "" {
				t.Fatalf("n=%d: archived sites must stay out, as in List: state %q", n, c.State)
			}
		}
	}
}

// The 60-site client from the issue, against the path the report used before:
// List with no limit returns one page.
func TestListFirstPageIsWhyReportsStoppedAtFifty(t *testing.T) {
	repo := newPagedClientRepo(60)
	svc := newSvc(repo)
	clientID := repo.clientID
	first, err := svc.List(context.Background(), ListInput{TenantID: uuid.New(), ClientID: &clientID})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 50 {
		t.Fatalf("List's default page is %d, the test premise is 50", len(first))
	}
	all, err := svc.ListAllForClient(context.Background(), uuid.New(), clientID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 60 {
		t.Fatalf("ListAllForClient returned %d of 60 sites", len(all))
	}
}

func TestListAllForClientDropsDuplicatesAcrossPages(t *testing.T) {
	repo := newPagedClientRepo(250)
	// A site added between reads shifts the newest-first list down by one, so
	// page two starts with the last site of page one.
	shifted := &shiftingRepo{pagedClientRepo: repo}
	svc := newSvc(shifted)
	got, err := svc.ListAllForClient(context.Background(), uuid.New(), repo.clientID)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[uuid.UUID]bool{}
	for _, s := range got {
		if seen[s.ID] {
			t.Fatalf("site %s returned twice", s.ID)
		}
		seen[s.ID] = true
	}
	// The added site sorts first, onto the page already read, so this walk
	// does not see it; every site that existed throughout is there once.
	if len(got) != 250 {
		t.Fatalf("got %d sites, want the 250 that existed throughout", len(got))
	}
}

type shiftingRepo struct {
	*pagedClientRepo
	inserted bool
}

func (r *shiftingRepo) List(ctx context.Context, in ListInput) ([]Site, error) {
	if in.Offset > 0 && !r.inserted {
		r.inserted = true
		r.sites = append([]Site{{ID: uuid.New(), Name: "new"}}, r.sites...)
	}
	return r.pagedClientRepo.List(ctx, in)
}

func TestListAllForClientRequiresTenant(t *testing.T) {
	svc := newSvc(newPagedClientRepo(1))
	if _, err := svc.ListAllForClient(context.Background(), uuid.Nil, uuid.New()); err == nil {
		t.Fatal("a nil tenant must be refused")
	}
}
