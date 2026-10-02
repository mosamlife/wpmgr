package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

func restWriteRoute() sqlc.RestRouteCatalogue {
	perm := "site.content.edit"
	return sqlc.RestRouteCatalogue{
		RouteID: "wp-v2-pages-update-fields", Method: "POST", Namespace: "wp/v2", Template: "/wp/v2/pages/{id}",
		CorePattern: `/wp/v2/pages/(?P<id>[\d]+)`,
		PathParams:  []byte(`{"id":{"type":"int","min":1,"max":9999999999,"required":true}}`),
		QueryKeys:   []byte(`{}`), PinnedQuery: []byte(`{}`),
		BodyKeys:     []byte(`{"title":{"type":"string","max_len":200},"excerpt":{"type":"string","max_len":1000}}`),
		Class:        "write",
		OutputFields: []byte(`{"fields":{"id":"int","status":"string","title":{"fields":{"rendered":"string"}}}}`),
		Snapshot:     "post_fields", Target: []byte(`{"kind":"post","param":"id","post_type":"page"}`),
		ArgRender:          []byte(`{"title":{"label":"Title","kind":"text"},"excerpt":{"label":"Excerpt","kind":"text"}}`),
		OperatorPermission: &perm, EffectCopy: "live", Enabled: true, Title: "Change a page's title or excerpt",
	}
}

func restReadRoute() sqlc.RestRouteCatalogue {
	return sqlc.RestRouteCatalogue{
		RouteID: "wp-v2-pages-list", Method: "GET", Namespace: "wp/v2", Template: "/wp/v2/pages",
		CorePattern: "/wp/v2/pages", PathParams: []byte(`{}`),
		QueryKeys:    []byte(`{"per_page":{"type":"int","min":1,"max":50},"search":{"type":"string","max_len":64},"include":{"type":"int_list","max_items":50},"orderby":{"type":"enum","values":["date","title"]}}`),
		PinnedQuery:  []byte(`{"status":"publish","context":"view"}`),
		BodyKeys:     []byte(`{}`), Class: "read",
		OutputFields: []byte(`{"items":{"fields":{"id":"int","link":"string","title":{"fields":{"rendered":"string"}}}}}`),
		Snapshot:     "none", ArgRender: []byte(`{}`), EffectCopy: "none", Enabled: true, Title: "List published pages",
	}
}

func TestCheckRestInput(t *testing.T) {
	w, r := restWriteRoute(), restReadRoute()
	// A row that (against m161's CHECK) lists a forbidden key: the code list
	// refuses it anyway.
	badRow := restReadRoute()
	badRow.QueryKeys = []byte(`{"status":{"type":"enum","values":["draft"]}}`)
	cases := []struct {
		name  string
		route sqlc.RestRouteCatalogue
		input string
		ok    bool
	}{
		{"write ok", w, `{"route_id":"wp-v2-pages-update-fields","path":{"id":412},"body":{"title":"Spring sale"}}`, true},
		{"read ok", r, `{"route_id":"wp-v2-pages-list","query":{"per_page":10,"include":[1,2],"orderby":"title"}}`, true},
		{"route_id names another route", w, `{"route_id":"wp-v2-posts-update-fields","path":{"id":412},"body":{"title":"x"}}`, false},
		{"raw route key", w, `{"route_id":"wp-v2-pages-update-fields","route":"/wp/v2/users","path":{"id":412},"body":{"title":"x"}}`, false},
		{"forbidden status", r, `{"route_id":"wp-v2-pages-list","query":{"status":"draft"}}`, false},
		{"forbidden status listed by a bad row", badRow, `{"route_id":"wp-v2-pages-list","query":{"status":"draft"}}`, false},
		{"forbidden _method", w, `{"route_id":"wp-v2-pages-update-fields","path":{"id":412},"query":{"_method":"DELETE"},"body":{"title":"x"}}`, false},
		{"unlisted body key", w, `{"route_id":"wp-v2-pages-update-fields","path":{"id":412},"body":{"content":"x"}}`, false},
		{"path id as string", w, `{"route_id":"wp-v2-pages-update-fields","path":{"id":"412"},"body":{"title":"x"}}`, false},
		{"path id missing", w, `{"route_id":"wp-v2-pages-update-fields","body":{"title":"x"}}`, false},
		{"empty title", w, `{"route_id":"wp-v2-pages-update-fields","path":{"id":412},"body":{"title":"  "}}`, false},
		{"no body", w, `{"route_id":"wp-v2-pages-update-fields","path":{"id":412}}`, false},
		{"title too long", w, `{"route_id":"wp-v2-pages-update-fields","path":{"id":412},"body":{"title":"` + strings.Repeat("a", 201) + `"}}`, false},
		{"bidi control", w, `{"route_id":"wp-v2-pages-update-fields","path":{"id":412},"body":{"title":"a‮b"}}`, false},
		{"enum outside", r, `{"route_id":"wp-v2-pages-list","query":{"orderby":"rand"}}`, false},
		{"newline in query", r, `{"route_id":"wp-v2-pages-list","query":{"search":"a\nb"}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := checkRestInput([]byte(tc.input), tc.route)
			if ok != tc.ok {
				t.Fatalf("checkRestInput(%s) = %v, want %v", tc.input, ok, tc.ok)
			}
		})
	}
}

// restPrecheckFixture is an agent precheck answer for the write route, with
// digests computed the agent's way.
func restPrecheckFixture(t *testing.T, entrySum string, rt sendableRoute, input []byte) agentcmd.AbilityRunResponse {
	t.Helper()
	base := sha256Hex([]byte("base"))
	undo := true
	tf, _ := json.Marshal(restTargetFacts{ID: 412, PostType: "page", Status: "publish", Live: true,
		TitleBefore: "Old <b>title</b>", ExcerptBefore: "Old excerpt"})
	ch, _ := json.Marshal([]restChange{{Key: "title", Before: "Old <b>title</b>", After: "Spring sale", Stored: "Spring sale"}})
	return agentcmd.AbilityRunResponse{
		OK: true, Mode: "precheck", Valid: true, RouteID: rt.row.RouteID, BaseFingerprint: base,
		PrecheckDigest: restPrecheckDigest(entrySum, rt.sum, input, base), TargetFacts: tf, Changes: ch, UndoExact: &undo,
	}
}

func TestVerifyRestWritePrecheck(t *testing.T) {
	rt := sendableRoute{row: restWriteRoute(), sum: sha256Hex([]byte("route"))}
	entrySum := sha256Hex([]byte("entry"))
	input := []byte(`{"route_id":"wp-v2-pages-update-fields","path":{"id":412},"body":{"title":"Spring sale"}}`)
	in, ok := checkRestInput(input, rt.row)
	if !ok {
		t.Fatal("input refused")
	}
	good := restPrecheckFixture(t, entrySum, rt, input)
	if _, ok := verifyRestWritePrecheck(good, entrySum, rt, input, in); !ok {
		t.Fatal("a correct precheck was refused")
	}
	mut := map[string]func(*agentcmd.AbilityRunResponse){
		"digest over another route": func(r *agentcmd.AbilityRunResponse) {
			r.PrecheckDigest = restPrecheckDigest(entrySum, sha256Hex([]byte("other")), input, r.BaseFingerprint)
		},
		"another route id":  func(r *agentcmd.AbilityRunResponse) { r.RouteID = "wp-v2-posts-update-fields" },
		"another post":      func(r *agentcmd.AbilityRunResponse) { r.TargetFacts = []byte(strings.Replace(string(r.TargetFacts), "412", "413", 1)) },
		"another post type": func(r *agentcmd.AbilityRunResponse) { r.TargetFacts = []byte(strings.Replace(string(r.TargetFacts), `"page"`, `"post"`, 1)) },
		"live without publish": func(r *agentcmd.AbilityRunResponse) {
			r.TargetFacts = []byte(strings.Replace(string(r.TargetFacts), `"publish"`, `"draft"`, 1))
		},
		"change value differs": func(r *agentcmd.AbilityRunResponse) {
			r.Changes = []byte(strings.Replace(string(r.Changes), `"after":"Spring sale"`, `"after":"Spring sales"`, 1))
		},
		"extra target member": func(r *agentcmd.AbilityRunResponse) {
			r.TargetFacts = []byte(strings.Replace(string(r.TargetFacts), "{", `{"x":1,`, 1))
		},
		"no undo_exact": func(r *agentcmd.AbilityRunResponse) { r.UndoExact = nil },
		"not valid":     func(r *agentcmd.AbilityRunResponse) { r.Valid = false },
	}
	for name, f := range mut {
		t.Run(name, func(t *testing.T) {
			r := restPrecheckFixture(t, entrySum, rt, input)
			f(&r)
			if _, ok := verifyRestWritePrecheck(r, entrySum, rt, input, in); ok {
				t.Fatal("a tampered precheck was accepted")
			}
		})
	}
}

// TestRestCardFacts: ruling 5 copy, site strings only in from_the_site, and
// the undo note when undo is not exact.
func TestRestCardFacts(t *testing.T) {
	pc := checkedRestPrecheck{
		target:    restTargetFacts{ID: 412, PostType: "page", Status: "publish", Live: true, TitleBefore: "Old‮ title"},
		changes:   []restChange{{Key: "title", Before: "Old‮ title", After: "Spring sale"}},
		undoExact: true,
	}
	f := buildRestCardFacts(restWriteRoute(), pc)
	if f.EffectLabel != restEffectLive || !f.Live || f.UndoNote != nil {
		t.Fatalf("live page card: %+v", f)
	}
	if strings.ContainsRune(f.Target.FromTheSite.TitleBefore, '‮') || strings.ContainsRune(f.Changes[0].FromTheSite.Before, '‮') {
		t.Fatal("a bidi control from the site reached the card")
	}
	if f.Changes[0].Label != "Title" || f.Changes[0].After != "Spring sale" {
		t.Fatalf("change: %+v", f.Changes[0])
	}
	pc.target.Live, pc.target.Status, pc.undoExact = false, "draft", false
	f = buildRestCardFacts(restWriteRoute(), pc)
	if f.EffectLabel != restEffectNotLive || f.UndoNote == nil || *f.UndoNote != restUndoNotExact {
		t.Fatalf("draft, inexact undo card: %+v", f)
	}
}

// TestRestWriteDigestBindsCardFacts: the presented digest covers the exact
// card_facts bytes, the route hash and the input, so an approve presenting
// the digest of a stale card is refused by the approve compare.
func TestRestWriteDigestBindsCardFacts(t *testing.T) {
	perm := "site.content.edit"
	e := &sqlc.AbilityCatalogue{EntryID: uuid.New(), Name: AbilityRestWrite, OperatorPermission: &perm, Snapshot: "post_fields", EffectCopy: "live"}
	auth := AuthorizedRequest{GrantID: uuid.New(), GrantName: "Laptop"}
	row := sqlc.Site{ID: uuid.New(), Name: "Shop"}
	rt := sendableRoute{row: restWriteRoute(), sum: sha256Hex([]byte("route"))}
	pc := checkedRestPrecheck{precheckDigest: sha256Hex([]byte("p")), baseFingerprint: sha256Hex([]byte("b")),
		target: restTargetFacts{ID: 412, PostType: "page"}}
	input := []byte(`{"route_id":"wp-v2-pages-update-fields","path":{"id":412},"body":{"title":"Spring sale"}}`)
	card := []byte(`{"changes":[{"after":"Spring sale"}]}`)
	f := abilityRequestFacts{nonce: strings.Repeat("a", 64), expiresAt: time.Unix(0, 0).UTC()}
	base := restWriteDigest(auth, row, e, "e", rt, input, card, pc, f)
	if restWriteDigest(auth, row, e, "e", rt, input, card, pc, f) != base {
		t.Fatal("the digest is not deterministic")
	}
	stale := []byte(`{"changes":[{"after":"Spring sale!"}]}`)
	if restWriteDigest(auth, row, e, "e", rt, input, stale, pc, f) == base {
		t.Fatal("a stale card_facts produced the same digest")
	}
	rt2 := rt
	rt2.sum = sha256Hex([]byte("route2"))
	if restWriteDigest(auth, row, e, "e", rt2, input, card, pc, f) == base {
		t.Fatal("another route hash produced the same digest")
	}
}

// TestRestReadOutputDropsUnlistedFields: a key the route's output_fields do
// not list never reaches the answer, and every string is fenced.
func TestRestReadOutputDropsUnlistedFields(t *testing.T) {
	raw := json.RawMessage(`[{"id":7,"link":"https://x.test/a","password":"hunter2","guid":{"raw":"secret"},` +
		`"title":{"rendered":"Hi","raw":"Hi raw"}}]`)
	out, truncated := fenceRouteOutput(restReadRoute().OutputFields, raw, 1<<16)
	if truncated {
		t.Fatal("truncated")
	}
	s := string(out)
	for _, leak := range []string{"hunter2", "password", "guid", "secret", "Hi raw"} {
		if strings.Contains(s, leak) {
			t.Fatalf("unlisted %q reached the output: %s", leak, s)
		}
	}
	if !strings.Contains(s, siteTextMarker+"Hi") || !strings.Contains(s, `"id":7`) {
		t.Fatalf("listed fields missing or unfenced: %s", s)
	}
	if out, _ := fenceRouteOutput([]byte(`{"bogus":1}`), raw, 1<<16); string(out) != "null" {
		t.Fatalf("an invalid shape returned output: %s", out)
	}
}

// TestClassifyRestFloor: both REST entries are gated on the rest-call agent
// floor.
func TestClassifyRestFloor(t *testing.T) {
	for _, name := range []string{AbilityRestRead, AbilityRestWrite} {
		perm := "site.content.edit"
		sum := strings.Repeat("a", 64)
		e := sqlc.AbilityCatalogue{Name: name, Source: "wpmgr", Class: restClassOf(name), Status: "admitted", Enabled: true,
			ApprovalMode: "none", EntrySha256: &sum}
		if name == AbilityRestWrite {
			e.ApprovalMode, e.Snapshot, e.OperatorPermission = "per_call", "post_fields", &perm
		}
		inv := sqlc.SiteAbilityInventory{Name: name}
		old := classify(name, []sqlc.AbilityCatalogue{e}, &inv, "0.61.157", "7.1")
		if old.runnable || old.reason == nil || *old.reason != notRunnableAgentOutdated {
			t.Fatalf("%s on 0.61.157: runnable=%v reason=%v", name, old.runnable, old.reason)
		}
		cur := classify(name, []sqlc.AbilityCatalogue{e}, &inv, agentcmd.MinAgentVersionForRestCall, "7.1")
		if !cur.runnable {
			t.Fatalf("%s on %s: not runnable, reason=%v", name, agentcmd.MinAgentVersionForRestCall, *cur.reason)
		}
	}
}
