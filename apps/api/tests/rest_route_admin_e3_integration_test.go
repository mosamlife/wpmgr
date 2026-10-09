package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/abilityrequest"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// TestE3RouteAdminAsAppRole: the REST route admin is superadmin only; a
// content edit stamps a new route hash (reproduced from the stored row) and a
// request approved under the old hash closes route_changed at dispatch; a
// disable closes one route_disabled. All on the wpmgr_app pool.
func TestE3RouteAdminAsAppRole(t *testing.T) {
	ctx := context.Background()
	w := newE3World(t)
	regular := seedUserRow(t, w.admin, "e3-regular-"+uuid.NewString()[:8]+"@example.test")
	root := seedUserRow(t, w.admin, "e3-root-"+uuid.NewString()[:8]+"@example.test")
	markSuperadmin(t, w.admin, root)
	pReg := domain.Principal{Type: domain.PrincipalUser, UserID: regular, TenantID: w.tenant, Role: "owner", Scope: domain.ScopeOrg}
	pRoot := domain.Principal{Type: domain.PrincipalUser, UserID: root, TenantID: w.tenant, Role: "owner", Scope: domain.ScopeOrg}
	base := "/api/v1/admin/abilities/rest-routes"

	engReg := abilityAdminEngine(t, w.pool, pReg)
	for _, r := range []struct{ method, path, body string }{
		{http.MethodGet, base, ""},
		{http.MethodPut, base + "/" + e3RouteID, `{"title":"Hijacked"}`},
	} {
		if rec := contentDo(engReg, r.method, r.path, r.body); rec.Code != http.StatusForbidden {
			t.Fatalf("GATE LEAK: a non-superadmin got %d from %s %s: %s", rec.Code, r.method, r.path, rec.Body.String())
		}
	}

	engRoot := abilityAdminEngine(t, w.pool, pRoot)
	rec := contentDo(engRoot, http.MethodGet, base, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Routes []struct {
			RouteID     string `json:"route_id"`
			HashCurrent bool   `json:"hash_current"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Routes) == 0 {
		t.Fatalf("list body: %v %s", err, rec.Body.String())
	}
	for _, r := range list.Routes {
		if !r.HashCurrent {
			t.Fatalf("route %s is not hash_current after the boot stamp", r.RouteID)
		}
	}

	// A content edit: the request approved before it closes route_changed.
	before := w.routeSum(t)
	a := w.askRetitle(t, "Spring sale A")
	w.approve(t, a)
	rec = contentDo(engRoot, http.MethodPut, base+"/"+e3RouteID, `{"title":"Change a page's title or excerpt (v2)"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body.String())
	}
	var edited struct {
		RouteSHA256 string `json:"route_sha256"`
		HashCurrent bool   `json:"hash_current"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &edited)
	if edited.RouteSHA256 == before || !edited.HashCurrent || w.routeSum(t) != edited.RouteSHA256 {
		t.Fatalf("edit did not stamp a new reproducible hash: before=%s after=%+v", before, edited)
	}
	w.dispatch(t, a)
	if state, _, notSent, _, _ := w.row(t, a); state != "not_sent" || notSent == nil || *notSent != abilityrequest.ReasonRouteChanged {
		t.Fatalf("request A: state=%s not_sent=%v, want not_sent/route_changed", state, notSent)
	}

	// A disable-only edit: the request approved before it closes route_disabled.
	b := w.askRetitle(t, "Spring sale B")
	w.approve(t, b)
	if rec := contentDo(engRoot, http.MethodPut, base+"/"+e3RouteID, `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body.String())
	}
	w.dispatch(t, b)
	if state, _, notSent, _, _ := w.row(t, b); state != "not_sent" || notSent == nil || *notSent != abilityrequest.ReasonRouteDisabled {
		t.Fatalf("request B: state=%s not_sent=%v, want not_sent/route_disabled", state, notSent)
	}
	if n := len(w.rest.sentWrites()); n != 0 {
		t.Fatalf("%d writes sent against an edited route", n)
	}

	// Refusals: a database CHECK names its constraint; an unknown route 404s;
	// an invalid output shape is refused before the write.
	rec = contentDo(engRoot, http.MethodPut, base+"/"+e3RouteID, `{"method":"GET"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a write route as GET: %d %s", rec.Code, rec.Body.String())
	}
	if rec := contentDo(engRoot, http.MethodPut, base+"/no-such-route", `{"title":"x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown route: %d %s", rec.Code, rec.Body.String())
	}
	if rec := contentDo(engRoot, http.MethodPut, base+"/"+e3RouteID, `{"output_fields":{"bogus":1}}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad output_fields: %d %s", rec.Code, rec.Body.String())
	}
	var audits int
	if err := w.admin.QueryRow(ctx, `SELECT count(*) FROM rest_route_catalogue_audit WHERE route_id = $1 AND actor_user_id = $2`, e3RouteID, root).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("route audit rows by the superadmin = %d (%v), want 2", audits, err)
	}
}
