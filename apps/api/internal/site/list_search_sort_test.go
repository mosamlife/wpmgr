// list_search_sort_test.go (GH #349). Unit coverage for the ?q= / ?sort=
// contract of GET /api/v1/sites at the service and handler boundary: the
// accept-set for sort, the 422 an unrecognised sort produces (T4), and the
// fact that the handler hands both parameters through to the repo alongside
// (not instead of) the existing filters.
//
// The SQL semantics of q and sort (which rows match, in what order, with what
// tiebreak, and where nulls land) are proved against a real Postgres in
// tests/site_list_search_sort_integration_test.go. Ordering cannot be
// meaningfully faked in memory.
package site

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// listRecordingRepo records the ListInput the service passed down, so a test
// can assert what the service normalised before the query ran.
type listRecordingRepo struct {
	fakeRepo
	calls int
	last  ListInput
}

func (r *listRecordingRepo) List(ctx context.Context, in ListInput) ([]Site, error) {
	r.calls++
	r.last = in
	return r.fakeRepo.List(ctx, in)
}

func TestParseListSortAcceptSet(t *testing.T) {
	t.Run("empty is the historical default", func(t *testing.T) {
		for _, raw := range []string{"", "   ", "\t"} {
			got, err := ParseListSort(raw)
			if err != nil {
				t.Fatalf("ParseListSort(%q): unexpected error %v", raw, err)
			}
			if got != DefaultListSort {
				t.Fatalf("ParseListSort(%q) = %q, want %q", raw, got, DefaultListSort)
			}
		}
		if DefaultListSort != SortCreatedDesc {
			t.Fatalf("default sort = %q, want %q (the ordering /sites has always had)",
				DefaultListSort, SortCreatedDesc)
		}
	})

	t.Run("the six documented values are accepted", func(t *testing.T) {
		for _, raw := range []string{"name", "-name", "created_at", "-created_at", "last_seen", "-last_seen"} {
			got, err := ParseListSort(raw)
			if err != nil {
				t.Fatalf("ParseListSort(%q): unexpected error %v", raw, err)
			}
			if string(got) != raw {
				t.Fatalf("ParseListSort(%q) = %q, want %q", raw, got, raw)
			}
		}
	})

	t.Run("anything else is a validation error, never a fallback", func(t *testing.T) {
		// Near-misses matter more than nonsense here: each of these is a value
		// a client could plausibly send, and each must be REJECTED rather than
		// quietly served as -created_at.
		for _, raw := range []string{
			"id",              // a real column, not an offered sort
			"Name",            // wrong case
			"+name",           // ascending spelled the other way
			"name asc",        // SQL-ish
			"created_at DESC", // SQL-ish
			"-last_seen_at",   // column name instead of the wire name
			"url",
			"name; drop table sites",
		} {
			got, err := ParseListSort(raw)
			if err == nil {
				t.Fatalf("ParseListSort(%q) = %q, want a validation error", raw, got)
			}
			de, ok := domain.AsDomain(err)
			if !ok || de.Kind != domain.KindValidation {
				t.Fatalf("ParseListSort(%q): want KindValidation, got %v", raw, err)
			}
			if de.Code != "invalid_sort" {
				t.Fatalf("ParseListSort(%q): code = %q, want invalid_sort", raw, de.Code)
			}
		}
	})
}

// TestServiceListRejectsUnknownSort is T4 at the service boundary, and also
// asserts the rejection happens BEFORE the repo is touched: an invalid request
// must not reach the database at all.
func TestServiceListRejectsUnknownSort(t *testing.T) {
	repo := &listRecordingRepo{}
	svc := newSvc(repo)

	_, err := svc.List(context.Background(), ListInput{TenantID: uuid.New(), Sort: "bogus"})
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindValidation {
		t.Fatalf("want a KindValidation error, got %v", err)
	}
	if got := domain.HTTPStatus(err); got != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", got)
	}
	if repo.calls != 0 {
		t.Fatalf("repo.List called %d times, want 0 (validate before querying)", repo.calls)
	}
}

// TestServiceListNormalisesSortAndQuery pins what the service hands the repo:
// an absent sort becomes the explicit default (so the repo never has to guess),
// and a whitespace-only q becomes "no search" rather than a filter that would
// match nothing.
func TestServiceListNormalisesSortAndQuery(t *testing.T) {
	cases := []struct {
		name      string
		in        ListInput
		wantSort  string
		wantQuery string
	}{
		{"absent sort defaults", ListInput{}, string(DefaultListSort), ""},
		{"explicit sort survives", ListInput{Sort: "name"}, "name", ""},
		{"query is trimmed", ListInput{Query: "  acme  "}, string(DefaultListSort), "acme"},
		{"whitespace-only query is no search", ListInput{Query: "   "}, string(DefaultListSort), ""},
		{"inner whitespace is preserved", ListInput{Query: " acme  client "}, string(DefaultListSort), "acme  client"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &listRecordingRepo{}
			in := tc.in
			in.TenantID = uuid.New()
			if _, err := newSvc(repo).List(context.Background(), in); err != nil {
				t.Fatalf("List: %v", err)
			}
			if repo.last.Sort != tc.wantSort {
				t.Fatalf("repo got sort %q, want %q", repo.last.Sort, tc.wantSort)
			}
			if repo.last.Query != tc.wantQuery {
				t.Fatalf("repo got query %q, want %q", repo.last.Query, tc.wantQuery)
			}
		})
	}
}

// TestServiceListNulByteQueryIsEmptyNotAnError covers a real 500 found while
// building this: a NUL byte in ?q= reached the driver and failed the whole
// statement with "invalid byte sequence for encoding UTF8", so a client-
// supplied search string produced a server error. No Postgres text value can
// contain a NUL, so such a search matches nothing by definition; the answer is
// an empty list, the same as any other search that matches nothing, and the
// database is never asked.
func TestServiceListNulByteQueryIsEmptyNotAnError(t *testing.T) {
	for _, q := range []string{"\x00", "a\x00b", "acme\x00"} {
		repo := &listRecordingRepo{}
		got, err := newSvc(repo).List(context.Background(), ListInput{TenantID: uuid.New(), Query: q})
		if err != nil {
			t.Fatalf("q=%q: got error %v, want an empty list", q, err)
		}
		if len(got) != 0 {
			t.Fatalf("q=%q: got %d sites, want 0", q, len(got))
		}
		if repo.calls != 0 {
			t.Fatalf("q=%q: repo.List called %d times, want 0 (an unmatchable search needs no query)", q, repo.calls)
		}
	}
}

// buildListEngine mounts the real list handler behind a tenant/principal
// injector, the same shape RequireAuth+RequireTenant produce in production.
func buildListEngine(h *Handler, tenantID uuid.UUID) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		ctx := domain.WithTenantID(c.Request.Context(), tenantID)
		ctx = domain.WithPrincipal(ctx, domain.Principal{
			Type:     domain.PrincipalUser,
			UserID:   uuid.New(),
			TenantID: tenantID,
		})
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	r.GET("/api/v1/sites", h.list)
	return r
}

func doList(r *gin.Engine, query string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sites"+query, nil))
	return rec
}

// TestListHandlerUnknownSortIs422 is T4 over the wire.
func TestListHandlerUnknownSortIs422(t *testing.T) {
	repo := &listRecordingRepo{}
	h := NewHandler(newSvc(repo), nil, "")
	rec := doList(buildListEngine(h, uuid.New()), "?sort=bogus")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Code != "invalid_sort" {
		t.Fatalf("error code = %q, want invalid_sort", body.Code)
	}
}

// TestListHandlerThreadsQueryAndSort proves the two new params reach the query
// layer AND that they are additive: the tag, state and client filters that
// were already on the request are all still present in the same ListInput.
// This is the T8 wiring half; T8's row-level truth is in the integration test.
func TestListHandlerThreadsQueryAndSort(t *testing.T) {
	clientID := uuid.New()
	repo := &listRecordingRepo{}
	h := NewHandler(newSvc(repo), nil, "")
	rec := doList(buildListEngine(h, uuid.New()),
		"?q=%20acme%20&sort=-last_seen&tags=prod&tags=eu&tags_match=all&state=connected&clientId="+clientID.String())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := repo.last
	if got.Query != "acme" {
		t.Fatalf("query = %q, want %q (trimmed)", got.Query, "acme")
	}
	if got.Sort != "-last_seen" {
		t.Fatalf("sort = %q, want -last_seen", got.Sort)
	}
	if len(got.AllTags) != 2 || got.AllTags[0] != "prod" || got.AllTags[1] != "eu" {
		t.Fatalf("all_tags = %v, want [prod eu]", got.AllTags)
	}
	if got.AnyTags != nil {
		t.Fatalf("any_tags = %v, want nil (tags_match=all)", got.AnyTags)
	}
	if got.State != "connected" {
		t.Fatalf("state = %q, want connected", got.State)
	}
	if got.ClientID == nil || *got.ClientID != clientID {
		t.Fatalf("client_id = %v, want %v", got.ClientID, clientID)
	}
}

// TestListHandlerAbsentSortIsDefault is the wiring half of T5: sending no sort
// must produce exactly the ordering the endpoint had before this change.
func TestListHandlerAbsentSortIsDefault(t *testing.T) {
	repo := &listRecordingRepo{}
	h := NewHandler(newSvc(repo), nil, "")
	if rec := doList(buildListEngine(h, uuid.New()), ""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if repo.last.Sort != string(SortCreatedDesc) {
		t.Fatalf("sort = %q, want %q", repo.last.Sort, SortCreatedDesc)
	}
	if repo.last.Query != "" {
		t.Fatalf("query = %q, want empty", repo.last.Query)
	}
}
