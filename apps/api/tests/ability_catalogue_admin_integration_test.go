package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/abilities"
	"github.com/mosamlife/wpmgr/apps/api/internal/admin"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

func abilityAdminEngine(t *testing.T, app *db.Pool, p domain.Principal) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ah := admin.NewHandler(nil, app)
	ah.SetAbilityRoutes(abilities.NewAdminHandler(abilities.NewAdminRepo(app)).RegisterAdmin)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	})
	ah.Register(engine.Group("/api/v1"))
	return engine
}

// TestAbilityCatalogueAdminGateAndActor: the catalogue routes are superadmin
// only; the actor is the session user, never a body field; an update merges
// omitted fields; entry_sha256 is the hash of the entry bytes the engine
// sends, reproduced from the stored row. Runs on the wpmgr_app pool.
func TestAbilityCatalogueAdminGateAndActor(t *testing.T) {
	ctx := context.Background()
	app := startPostgres(t)
	adm := connectAdmin(t, app)
	tenant := seedTenant(t, app, "abl-admin-"+uuid.NewString()[:8])

	regular := seedUserRow(t, adm, "regular-"+uuid.NewString()[:8]+"@example.com")
	root := seedUserRow(t, adm, "root-"+uuid.NewString()[:8]+"@example.com")
	other := seedUserRow(t, adm, "other-"+uuid.NewString()[:8]+"@example.com")
	markSuperadmin(t, adm, root)
	markSuperadmin(t, adm, other)
	pReg := domain.Principal{Type: domain.PrincipalUser, UserID: regular, TenantID: tenant, Role: "owner", Scope: domain.ScopeOrg}
	pRoot := domain.Principal{Type: domain.PrincipalUser, UserID: root, TenantID: tenant, Role: "owner", Scope: domain.ScopeOrg}

	name := "test-" + uuid.NewString()[:8] + "/read-thing"
	base := "/api/v1/admin/abilities/catalogue"
	body := `{"name":"` + name + `","source":"wpmgr","class":"read","status":"detect_only","enabled":false,` +
		`"approval_mode":"none","title":"A test read","description":"Reads a thing.","limits":{"b":2,"a":1}}`
	auditRows := func() int {
		var n int
		if err := adm.QueryRow(ctx, `SELECT count(*) FROM ability_catalogue_audit WHERE name = $1`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// A non-superadmin is refused on every route and writes nothing.
	engReg := abilityAdminEngine(t, app, pReg)
	for _, r := range []struct{ method, path, body string }{
		{http.MethodGet, base, ""},
		{http.MethodPost, base, body},
		{http.MethodPut, base + "/" + uuid.NewString(), body},
	} {
		if w := contentDo(engReg, r.method, r.path, r.body); w.Code != http.StatusForbidden {
			t.Fatalf("GATE LEAK: a non-superadmin got %d from %s %s: %s", w.Code, r.method, r.path, w.Body.String())
		}
	}
	if n := auditRows(); n != 0 {
		t.Fatalf("GATE LEAK: %d audit rows after refused writes", n)
	}

	engRoot := abilityAdminEngine(t, app, pRoot)
	// A body naming an actor is refused.
	spoofed := strings.TrimSuffix(body, "}") + `,"actor_user_id":"` + other.String() + `"}`
	if w := contentDo(engRoot, http.MethodPost, base, spoofed); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("ACTOR FROM BODY: answered %d, want 422: %s", w.Code, w.Body.String())
	}
	if n := auditRows(); n != 0 {
		t.Fatalf("ACTOR FROM BODY: %d audit rows after a refused body", n)
	}

	// Create.
	w := contentDo(engRoot, http.MethodPost, base, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create answered %d: %s", w.Code, w.Body.String())
	}
	var created struct {
		EntryID     string  `json:"entry_id"`
		Title       string  `json:"title"`
		Enabled     bool    `json:"enabled"`
		EntrySHA256 *string `json:"entry_sha256"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if created.EntrySHA256 == nil || len(*created.EntrySHA256) != 64 {
		t.Fatalf("no entry_sha256: %s", w.Body.String())
	}
	var actor uuid.UUID
	if err := adm.QueryRow(ctx, `SELECT actor_user_id FROM ability_catalogue_audit WHERE name = $1`, name).Scan(&actor); err != nil || actor != root {
		t.Fatalf("ACTOR FROM BODY: audit actor %s (err %v), want the session user %s", actor, err, root)
	}

	// Update with only `enabled`: every other field keeps its stored value,
	// and the hash is re-stamped over the merged entry.
	w = contentDo(engRoot, http.MethodPut, base+"/"+created.EntryID, `{"enabled":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update answered %d: %s", w.Code, w.Body.String())
	}
	var updated struct {
		Title       string  `json:"title"`
		Enabled     bool    `json:"enabled"`
		EntrySHA256 *string `json:"entry_sha256"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &updated)
	if !updated.Enabled || updated.Title != "A test read" || updated.EntrySHA256 == nil || *updated.EntrySHA256 == *created.EntrySHA256 {
		t.Fatalf("merge or re-stamp failed: %s", w.Body.String())
	}
	if n := auditRows(); n != 2 {
		t.Fatalf("audit rows = %d, want 2 (insert, update)", n)
	}

	// The stored hash is what the engine would send: SendableEntry over the
	// row read back as the app role accepts it.
	rows, err := abilities.NewAdminRepo(app).List(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.Name == name {
			found = true
			if _, sum, err := abilities.SendableEntry(r); err != nil || sum != *updated.EntrySHA256 {
				t.Fatalf("stored hash does not match the sendable bytes: %v", err)
			}
		}
	}
	if !found {
		t.Fatal("the entry is not listed")
	}

	// Renaming, and an unknown id, are refused.
	if w := contentDo(engRoot, http.MethodPut, base+"/"+created.EntryID, `{"name":"other/name"}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("rename answered %d, want 422: %s", w.Code, w.Body.String())
	}
	if w := contentDo(engRoot, http.MethodPut, base+"/"+uuid.NewString(), `{"enabled":true}`); w.Code != http.StatusNotFound {
		t.Fatalf("unknown id answered %d, want 404: %s", w.Code, w.Body.String())
	}
}
