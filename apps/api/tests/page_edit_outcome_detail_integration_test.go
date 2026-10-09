// BF-E: a page edit refused as a conflict says which conflict refused it, on
// the queue as the real list handler serves it, on the wpmgr_app pool. The
// rows are seeded through the shipped insert, dispatch and outcome
// statements (m169's helpers), the outcome carrying the site's detail as the
// outcome recording keeps it.
package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/abilityrequest"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// bfeQueueEngine mounts the ability request routes as the server does, for
// w's signed-in owner.
func bfeQueueEngine(w *e2World) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), w.person))
		c.Next()
	})
	abilityrequest.NewHandler(w.svc).Register(engine.Group("/api/v1"))
	return engine
}

// bfeRefused records a refused outcome with code and the site's detail on a
// sent page edit of post 42.
func bfeRefused(t *testing.T, w *e2World, seed, code, detail string) uuid.UUID {
	t.Helper()
	row := g2cSent(t, w, 42, seed)
	if n, err := m169Outcome(t, w.pool, w.tenant, row.ID, sqlc.RecordAbilityRequestOutcomeParams{
		Outcome: "refused", OutcomeCode: &code, SiteReportedText: &detail,
	}); err != nil || n != 1 {
		t.Fatalf("record refused %s: n=%d err=%v", seed, n, err)
	}
	return row.ID
}

// TestPageEditOutcomeDetailOnTheListAsAppRole: GET .../ai/ability-requests
// names the conflict a page edit was refused with (changed_since_read,
// editor_open, autosave_pending) and nothing else: a conflict whose detail
// is the site's own words, and another code carrying a detail word, list
// outcome_detail null.
func TestPageEditOutcomeDetailOnTheListAsAppRole(t *testing.T) {
	w := newE2World(t, true)
	want := map[uuid.UUID]any{}
	for i, d := range []string{"changed_since_read", "editor_open", "autosave_pending"} {
		want[bfeRefused(t, w, fmt.Sprintf("bfe-detail-%d", i), "conflict", d)] = d
	}
	want[bfeRefused(t, w, "bfe-detail-text", "conflict", "someone is editing this post right now")] = nil
	want[bfeRefused(t, w, "bfe-detail-other", "preview_changed", "editor_open")] = nil

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sites/"+w.site.String()+"/ai/ability-requests?limit=100", nil)
	bfeQueueEngine(w).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Requests []map[string]any `json:"requests"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	seen := 0
	for _, r := range body.Requests {
		id, err := uuid.Parse(fmt.Sprint(r["id"]))
		if err != nil {
			t.Fatalf("id %v: %v", r["id"], err)
		}
		exp, ok := want[id]
		if !ok {
			continue
		}
		seen++
		got, present := r["outcome_detail"]
		if !present || got != exp {
			t.Fatalf("request %s (code %v): outcome_detail %v (present %v), want %v", id, r["outcome_code"], got, present, exp)
		}
	}
	if seen != len(want) {
		t.Fatalf("listed %d of the %d refused edits", seen, len(want))
	}
}
