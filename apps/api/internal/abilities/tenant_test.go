package abilities

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

type fakeReenableStore struct {
	calls      int
	p          domain.Principal
	entry      uuid.UUID
	superadmin bool
	done       bool
	err        error
}

func (f *fakeReenableStore) ReenableForTenant(_ context.Context, p domain.Principal, entryID uuid.UUID, sa bool) (bool, error) {
	f.calls++
	f.p, f.entry, f.superadmin = p, entryID, sa
	return f.done, f.err
}

type fakeSA struct {
	is  bool
	err error
}

func (f fakeSA) IsSuperadmin(context.Context, uuid.UUID) (bool, error) { return f.is, f.err }

func reenableRouter(store TenantReenableStore, sa SuperadminChecker, p *domain.Principal) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/api/v1", func(c *gin.Context) {
		if p != nil {
			c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), *p))
		}
		c.Next()
	})
	NewTenantHandler(store, sa).Register(g)
	return r
}

func doReenable(r *gin.Engine, entry string, ctype string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ai/abilities/"+entry+"/reenable", strings.NewReader(`{}`))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func userPrincipal(role string) domain.Principal {
	return domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: uuid.New(), Role: role}
}

func TestReenable_Authorization(t *testing.T) {
	entry := uuid.New()
	siteScoped := userPrincipal("admin")
	siteScoped.Scope = domain.ScopeSite
	siteScoped.AllowedSiteIDs = []uuid.UUID{uuid.New()}
	apiKey := domain.Principal{Type: domain.PrincipalAPIKey, APIKeyID: uuid.New(), TenantID: uuid.New(), Role: "owner"}
	for _, tc := range []struct {
		name   string
		p      domain.Principal
		sa     fakeSA
		status int
		wantSA bool
	}{
		{"owner", userPrincipal("owner"), fakeSA{}, http.StatusOK, false},
		{"admin", userPrincipal("admin"), fakeSA{}, http.StatusOK, false},
		{"operator refused", userPrincipal("operator"), fakeSA{}, http.StatusForbidden, false},
		{"viewer refused", userPrincipal("viewer"), fakeSA{}, http.StatusForbidden, false},
		{"site-scoped admin refused", siteScoped, fakeSA{}, http.StatusForbidden, false},
		{"api key refused", apiKey, fakeSA{is: true}, http.StatusForbidden, false},
		{"superadmin operator", userPrincipal("operator"), fakeSA{is: true}, http.StatusOK, true},
		{"superadmin lookup error fails closed", userPrincipal("viewer"), fakeSA{is: true, err: errors.New("db down")}, http.StatusForbidden, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeReenableStore{done: true}
			p := tc.p
			w := doReenable(reenableRouter(store, tc.sa, &p), entry.String(), "application/json")
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if tc.status != http.StatusOK {
				if store.calls != 0 {
					t.Fatalf("store reached on a refusal")
				}
				return
			}
			if store.calls != 1 || store.entry != entry || store.p.TenantID != p.TenantID || store.superadmin != tc.wantSA {
				t.Fatalf("store: %+v", store)
			}
			if !strings.Contains(w.Body.String(), `"reenabled":true`) {
				t.Fatalf("body: %s", w.Body.String())
			}
		})
	}
}

func TestReenable_NotOffIs404(t *testing.T) {
	p := userPrincipal("admin")
	w := doReenable(reenableRouter(&fakeReenableStore{done: false}, fakeSA{}, &p), uuid.NewString(), "application/json")
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "ability_not_disabled_for_account") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestReenable_BadInput(t *testing.T) {
	p := userPrincipal("admin")
	store := &fakeReenableStore{done: true}
	r := reenableRouter(store, fakeSA{}, &p)
	if w := doReenable(r, "not-a-uuid", "application/json"); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad uuid: %d %s", w.Code, w.Body.String())
	}
	if w := doReenable(r, uuid.NewString(), "text/plain"); w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("non-JSON body: %d %s", w.Code, w.Body.String())
	}
	if w := doReenable(reenableRouter(store, fakeSA{}, nil), uuid.NewString(), "application/json"); w.Code != http.StatusUnauthorized {
		t.Fatalf("no principal: %d", w.Code)
	}
	if store.calls != 0 {
		t.Fatalf("store reached: %d", store.calls)
	}
}
