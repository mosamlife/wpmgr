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

// TestPageEditOutsideChangeOnTheListAsAppRole: GET .../ai/ability-requests
// names the one kind of thing a page edit's save changed outside the page
// (outside_change), from the agent's detail as the outcome recording keeps
// it, and nothing else: a detail naming two kinds, a label naming none, the
// site's own words, and the kit sentence under another code all list
// outside_change null.
func TestPageEditOutsideChangeOnTheListAsAppRole(t *testing.T) {
	w := newE2World(t, true)
	const scope = "the save wrote outside the page: "
	want := map[uuid.UUID]any{}
	for i, c := range []struct {
		code, detail string
		want         any
	}{
		{"side_effect_detected", "the active kit changed during the save", "active_kit"},
		{"side_effect_detected", scope + "other_post_meta_written, other_post_written", "other_posts"},
		{"side_effect_detected", scope + "term_changed", "terms"},
		{"side_effect_detected", scope + "option_written", "site_settings"},
		{"side_effect_detected", scope + "role_changed, user_changed", "users"},
		{"side_effect_detected", scope + "option_written, other_post_written", nil},
		{"side_effect_detected", scope + "too_many_revisions", nil},
		{"side_effect_detected", "a plugin rewrote the homepage", nil},
		{"builder_save_refused", "the active kit changed during the save", nil},
	} {
		want[bfeRefused(t, w, fmt.Sprintf("bfe-outside-%d", i), c.code, c.detail)] = c.want
	}

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
		got, present := r["outside_change"]
		if !present || got != exp {
			t.Fatalf("request %s (code %v): outside_change %v (present %v), want %v", id, r["outcome_code"], got, present, exp)
		}
	}
	if seen != len(want) {
		t.Fatalf("listed %d of the %d refused edits", seen, len(want))
	}
}
