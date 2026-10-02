// E3-G2, AI retitles a page through a reviewed REST route: the rest-write
// branch of site_ability_run over HTTP with a real bearer token, then
// approve and the dispatch worker through internal/abilityrequest's service,
// on the wpmgr_app pool with the real audit recorder and the m161 route
// catalogue stamped at "boot". A fake agent answers precheck, write and
// revert. The superuser connection only arranges state and reads rows.
//
// Run serially, on its own: go test -run TestE3 ./tests/ -count=1 -p 1
package tests

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/abilities"
	"github.com/mosamlife/wpmgr/apps/api/internal/abilityrequest"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/govcontext"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

const e3RouteID = "wp-v2-pages-update-fields"

// e3Agent answers rest-write calls with the agent's digests. Every value is
// ASCII hex or plain ASCII, where encoding/json and json_encode agree.
type e3Agent struct {
	mu     sync.Mutex
	writes []agentcmd.AbilityRunCall
	modes  []string
}

func (a *e3Agent) AbilityRun(_ context.Context, _ uuid.UUID, _ string, call agentcmd.AbilityRunCall) (agentcmd.AbilityRunResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.modes = append(a.modes, call.Mode)
	switch call.Mode {
	case agentcmd.AbilityRunModePrecheck:
		var in struct {
			RouteID string            `json:"route_id"`
			Body    map[string]string `json:"body"`
		}
		_ = json.Unmarshal(call.Input, &in)
		base := e2Hex([]byte("base-412"))
		pre := e2Hex(e2Arr(call.EntrySHA256, call.RouteSHA256, e2Hex(call.Input), base))
		tf, _ := json.Marshal(map[string]any{"id": 412, "post_type": "page", "status": "publish", "live": true,
			"title_before": "Old title", "excerpt_before": ""})
		ch, _ := json.Marshal([]map[string]string{{"key": "title", "before": "Old title", "after": in.Body["title"], "stored": in.Body["title"]}})
		undo := true
		return agentcmd.AbilityRunResponse{OK: true, Mode: "precheck", Valid: true, RouteID: in.RouteID,
			BaseFingerprint: base, PrecheckDigest: pre, TargetFacts: tf, Changes: ch, UndoExact: &undo}, nil
	case agentcmd.AbilityRunModeWrite:
		a.writes = append(a.writes, call)
		return agentcmd.AbilityRunResponse{OK: true, Outcome: "updated", Mode: "write", PostID: 412}, nil
	case agentcmd.AbilityRunModeRevert:
		return agentcmd.AbilityRunResponse{OK: true, Outcome: "reverted", Mode: "revert", PostID: 412}, nil
	}
	return agentcmd.AbilityRunResponse{}, &agentcmd.AbilityRunRefusal{Code: "bad_mode"}
}

func (a *e3Agent) sentWrites() []agentcmd.AbilityRunCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]agentcmd.AbilityRunCall(nil), a.writes...)
}

type e3World struct {
	*e2World
	rest *e3Agent
}

func newE3World(t *testing.T) *e3World {
	t.Helper()
	ctx := context.Background()
	pool := startPostgres(t)
	repo := mcp.NewRepo(pool)
	rec := audit.NewRecorder(pool, domain.SystemClock{})
	r := uuid.NewString()[:8]
	w := &e3World{e2World: &e2World{pool: pool, agent: &e2Agent{}}, rest: &e3Agent{}}
	w.tenant = seedTenant(t, pool, "e3-"+r)
	w.site = seedSite(t, pool, w.tenant, "https://e3-"+r+".test")
	w.admin = connectAdmin(t, pool)
	t.Cleanup(w.admin.Close)
	if _, err := w.admin.Exec(ctx, `UPDATE sites SET connection_state = 'connected', agent_version = $2,
		content_editing_enabled_at = now(), content_editing_principal_user_id = 7 WHERE tenant_id = $1`,
		w.tenant, agentcmd.MinAgentVersionForRestCall); err != nil {
		t.Fatalf("arrange site: %v", err)
	}
	m155Refresh(t, pool, w.tenant, w.site, mcp.AbilityRestWrite)
	if _, err := abilities.StampOwnEntryHashes(ctx, pool, slog.Default()); err != nil {
		t.Fatalf("stamp entries: %v", err)
	}
	if _, err := abilities.StampOwnRouteHashes(ctx, pool, slog.Default()); err != nil {
		t.Fatalf("stamp routes: %v", err)
	}
	user := seedUserRow(t, w.admin, "e3-"+r+"@example.test")
	seedMembershipRow(t, w.admin, user, w.tenant)
	w.person = domain.Principal{Type: domain.PrincipalUser, UserID: user, TenantID: w.tenant, Scope: domain.ScopeOrg, Role: "owner"}

	msvc := mcp.NewService(repo).WithAudit(rec).
		WithContextResolver(&govcontext.Resolver{Store: govcontext.NewRepo(pool)})
	if err := msvc.EnableAbilityTools(repo, w.rest, abilities.SendableEntry, "integration-secret"); err != nil {
		t.Fatalf("enable ability tools: %v", err)
	}
	msvc.SetRouteEncoder(abilities.SendableRoute)
	if err := msvc.EnableAbilityWrites(repo); err != nil {
		t.Fatalf("enable ability writes: %v", err)
	}
	if err := msvc.SetWriteToolsEnabled(true); err != nil {
		t.Fatalf("write tools: %v", err)
	}
	w.eng = mountLikeProduction(t, msvc, domain.Principal{TenantID: w.tenant, Scope: domain.ScopeOrg})
	w.bearer = e2Grant(t, repo, w.tenant, []uuid.UUID{w.site})

	w.svc = abilityrequest.NewService(pool, repo, msvc, rec, slog.Default())
	w.svc.SetWriteToolsEnabled(true)
	w.svc.SetSender(w.rest, abilities.SendableEntry, msvc)
	w.svc.SetRouteEncoder(abilities.SendableRoute)
	return w
}

func (w *e3World) askRetitle(t *testing.T, title string) uuid.UUID {
	t.Helper()
	res := cpeCall(t, w.eng, w.bearer, mcp.ToolSiteAbilityRun, map[string]any{
		"site_id": w.site.String(), "name": mcp.AbilityRestWrite,
		"input": map[string]any{"route_id": e3RouteID, "path": map[string]any{"id": 412}, "body": map[string]any{"title": title}},
	}).wantOK(t, "ask to retitle a page")
	if res["state"] != "waiting_for_approval" {
		t.Fatalf("state = %v, want waiting_for_approval", res["state"])
	}
	id, err := uuid.Parse(res["request_id"].(string))
	if err != nil {
		t.Fatalf("request_id: %v", err)
	}
	return id
}

func (w *e3World) approve(t *testing.T, id uuid.UUID) {
	t.Helper()
	if _, err := w.svc.Approve(context.Background(), w.person, w.site, id, w.digest(t, id)); err != nil {
		t.Fatalf("approve: %v", err)
	}
}

func (w *e3World) routeSum(t *testing.T) string {
	t.Helper()
	var sum *string
	if err := w.admin.QueryRow(context.Background(), `SELECT route_sha256 FROM rest_route_catalogue WHERE route_id = $1`, e3RouteID).Scan(&sum); err != nil || sum == nil {
		t.Fatalf("read route hash: %v (stamped=%v)", err, sum != nil)
	}
	return *sum
}

// TestE3RestWriteApproveDispatchAppliedAsAppRole: the request records the
// route and its hash and a card that says the page is published immediately;
// the worker sends the approved route bytes with the base fingerprint; the
// outcome is applied with the undo window open.
func TestE3RestWriteApproveDispatchAppliedAsAppRole(t *testing.T) {
	w := newE3World(t)
	id := w.askRetitle(t, "Spring sale 2026")
	var routeID, routeSum *string
	var card []byte
	var target *int64
	if err := w.admin.QueryRow(context.Background(),
		`SELECT route_id, route_sha256, card_facts, target_post_id FROM assistant_ability_requests WHERE id = $1`, id).
		Scan(&routeID, &routeSum, &card, &target); err != nil {
		t.Fatalf("read request: %v", err)
	}
	if routeID == nil || *routeID != e3RouteID || routeSum == nil || *routeSum != w.routeSum(t) || target == nil || *target != 412 {
		t.Fatalf("request facts: route=%v sum=%v target=%v", routeID, routeSum, target)
	}
	var cf map[string]any
	if json.Unmarshal(card, &cf) != nil || cf["effect_label"] != "Published immediately" || cf["live"] != true {
		t.Fatalf("card_facts: %s", card)
	}
	w.approve(t, id)
	w.dispatch(t, id)
	writes := w.rest.sentWrites()
	if len(writes) != 1 {
		t.Fatalf("writes sent = %d, want 1", len(writes))
	}
	if writes[0].RouteSHA256 != *routeSum || e2Hex(writes[0].Route) != *routeSum ||
		writes[0].Expected == nil || writes[0].Expected.BaseFingerprint == "" || writes[0].Expected.PreviewDigest != "" {
		t.Fatalf("write not bound to the approved route: %+v", writes[0])
	}
	var state string
	var outcome, undo *string
	if err := w.admin.QueryRow(context.Background(),
		`SELECT state, outcome, undo_state FROM assistant_ability_requests WHERE id = $1`, id).Scan(&state, &outcome, &undo); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if state != "done" || outcome == nil || *outcome != "applied" || undo == nil || *undo != "available" {
		t.Fatalf("row after dispatch: state=%s outcome=%v undo=%v", state, outcome, undo)
	}
}

// TestE3RouteChanged (route W1, four layers): a route edited, edited out of
// band, or disabled between approval and dispatch sends nothing and closes
// not_sent. Layer 1 is the dispatch query's route_hash_current and
// route_enabled; layer 2 the worker's reproduction of the approved bytes; layer
// 3 the reservation's WHERE; layer 4 the agent's own hash check.
func TestE3RouteChanged(t *testing.T) {
	cases := []struct {
		name   string
		mutate string
		want   string
	}{
		// An admin edit moves both the row and its stored hash.
		{"edited", `UPDATE rest_route_catalogue SET title = title || ' (edited)', route_sha256 = $2 WHERE route_id = $1`, abilityrequest.ReasonRouteChanged},
		// An edit outside the admin write keeps the stored hash: only the
		// worker's reproduction of the bytes can see it.
		{"edited_out_of_band", `UPDATE rest_route_catalogue SET title = title || ' (sneaky)' WHERE route_id = $1 AND $2 <> ''`, abilityrequest.ReasonRouteChanged},
		{"disabled", `UPDATE rest_route_catalogue SET enabled = false WHERE route_id = $1 AND $2 <> ''`, abilityrequest.ReasonRouteDisabled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newE3World(t)
			id := w.askRetitle(t, "Spring sale "+tc.name)
			w.approve(t, id)
			if _, err := w.admin.Exec(context.Background(), tc.mutate, e3RouteID, strings.Repeat("c", 64)); err != nil {
				t.Fatalf("change route: %v", err)
			}
			w.dispatch(t, id)
			if n := len(w.rest.sentWrites()); n != 0 {
				t.Fatalf("%d writes sent against a changed route", n)
			}
			state, _, notSent, _, _ := w.row(t, id)
			if state != "not_sent" || notSent == nil || *notSent != tc.want {
				t.Fatalf("row: state=%s not_sent_reason=%v, want not_sent/%s", state, notSent, tc.want)
			}
		})
	}
}

// TestE3StaleCardDigestApproveRefusedAsAppRole: an approve presenting a digest
// other than the one over the stored card is refused and approves nothing.
func TestE3StaleCardDigestApproveRefusedAsAppRole(t *testing.T) {
	w := newE3World(t)
	id := w.askRetitle(t, "Spring sale stale")
	_, err := w.svc.Approve(context.Background(), w.person, w.site, id, strings.Repeat("0", 64))
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != abilityrequest.CodeRequestChanged {
		t.Fatalf("stale digest: got %v, want %s", err, abilityrequest.CodeRequestChanged)
	}
	if state, _, _, _, _ := w.row(t, id); state != "pending" {
		t.Fatalf("state after a refused approve = %s, want pending", state)
	}
}

// TestE3UnstampedRouteFailsClosedAsAppRole: a route whose hash is NULL is
// not offered, so a write naming it is refused before the site is asked.
func TestE3UnstampedRouteFailsClosedAsAppRole(t *testing.T) {
	w := newE3World(t)
	if _, err := w.admin.Exec(context.Background(), `UPDATE rest_route_catalogue SET route_sha256 = NULL WHERE route_id = $1`, e3RouteID); err != nil {
		t.Fatalf("unstamp: %v", err)
	}
	cpeCall(t, w.eng, w.bearer, mcp.ToolSiteAbilityRun, map[string]any{
		"site_id": w.site.String(), "name": mcp.AbilityRestWrite,
		"input": map[string]any{"route_id": e3RouteID, "path": map[string]any{"id": 412}, "body": map[string]any{"title": "x"}},
	})
	w.rest.mu.Lock()
	defer w.rest.mu.Unlock()
	for _, m := range w.rest.modes {
		if m == agentcmd.AbilityRunModePrecheck {
			t.Fatal("a precheck was sent for an unstamped route")
		}
	}
}
