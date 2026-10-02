package abilities

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// The agent's tests replay these files. They are regenerated, never
// hand-edited (WPMGR_WRITE_FIXTURES=1).
const (
	restReadRouteFixturePath  = "../../../agent/tests/fixtures/rest-call/read-wp-v2-pages-get.json"
	restWriteRouteFixturePath = "../../../agent/tests/fixtures/rest-call/write-wp-v2-pages-update-fields.json"
	restWriteFixturePath      = "../../../agent/tests/fixtures/ability-run/rest-write.json"
)

func strPtr(s string) *string { return &s }

// seededPagesGetRoute is wp-v2-pages-get as the m161 seed leaves it.
func seededPagesGetRoute() sqlc.RestRouteCatalogue {
	return sqlc.RestRouteCatalogue{
		RouteID: "wp-v2-pages-get", Method: "GET", Namespace: "wp/v2", Template: "/wp/v2/pages/{id}",
		CorePattern: `/wp/v2/pages/(?P<id>[\d]+)`,
		PathParams:  []byte(`{"id":{"type":"int","min":1,"max":9999999999,"required":true}}`),
		QueryKeys:   []byte(`{}`), PinnedQuery: []byte(`{"status":"publish","context":"view"}`), BodyKeys: []byte(`{}`),
		Class:        "read",
		OutputFields: []byte(`{"fields":{"id":"int","date_gmt":"string","modified_gmt":"string","slug":"string","status":"string","type":"string","link":"string","parent":"int","menu_order":"int","author":"int","title":{"fields":{"rendered":"string"}},"excerpt":{"fields":{"rendered":"string"}},"content":{"fields":{"rendered":"string"}}}}`),
		Snapshot:     "none", ArgRender: []byte(`{}`), EffectCopy: "none", Enabled: true,
		Title:       "Read a published page",
		Description: "Reads one published page: its title, excerpt, text, address and dates. Changes nothing.",
	}
}

// seededPagesUpdateFieldsRoute is wp-v2-pages-update-fields as the m161 seed
// leaves it.
func seededPagesUpdateFieldsRoute() sqlc.RestRouteCatalogue {
	return sqlc.RestRouteCatalogue{
		RouteID: "wp-v2-pages-update-fields", Method: "POST", Namespace: "wp/v2", Template: "/wp/v2/pages/{id}",
		CorePattern: `/wp/v2/pages/(?P<id>[\d]+)`,
		PathParams:  []byte(`{"id":{"type":"int","min":1,"max":9999999999,"required":true}}`),
		QueryKeys:   []byte(`{}`), PinnedQuery: []byte(`{}`),
		BodyKeys:     []byte(`{"title":{"type":"string","max_len":200},"excerpt":{"type":"string","max_len":1000}}`),
		Class:        "write",
		OutputFields: []byte(`{"fields":{"id":"int","modified_gmt":"string","status":"string","link":"string","title":{"fields":{"rendered":"string"}},"excerpt":{"fields":{"rendered":"string"}}}}`),
		Snapshot:     "post_fields", Target: []byte(`{"kind":"post","param":"id","post_type":"page"}`),
		ArgRender:          []byte(`{"title":{"label":"Title","kind":"text"},"excerpt":{"label":"Excerpt","kind":"text"}}`),
		OperatorPermission: strPtr("site.content.edit"), EffectCopy: "live", Enabled: true,
		Title:       "Change a page's title or excerpt",
		Description: "Changes the title or excerpt of one page. A published page changes immediately. Undo puts back the previous title and excerpt.",
	}
}

// seededRestWriteEntry is wpmgr/rest-write as the m161 seed leaves it.
func seededRestWriteEntry() sqlc.AbilityCatalogue {
	return sqlc.AbilityCatalogue{
		Name: "wpmgr/rest-write", Source: "wpmgr", Class: "write", Status: "admitted",
		Enabled: true, ApprovalMode: "per_call", PermissionMode: "principal",
		Title:              "Change a page's or post's title or excerpt",
		Description:        "Changes the title or excerpt of one page or post through the site's own WordPress API, after a person approves it. A published page changes immediately. Undo puts back the previous values.",
		OperatorPermission: strPtr("site.content.edit"), Snapshot: "post_fields", Preview: strPtr("structured"),
		EffectCopy: "live", ArgRender: []byte("{}"), Limits: []byte("{}"), Admission: []byte("{}"),
	}
}

type restWriteFixture struct {
	Note        string `json:"note"`
	SiteID      string `json:"site_id"`
	RequestID   string `json:"request_id"`
	Entry       string `json:"entry"`
	EntrySHA256 string `json:"entry_sha256"`
	Route       string `json:"route"`
	RouteSHA256 string `json:"route_sha256"`
	Input       string `json:"input"`
	Precheck    struct {
		P string `json:"p"`
	} `json:"precheck"`
	Write struct {
		Expected agentcmd.AbilityRunExpected `json:"expected"`
		P        string                      `json:"p"`
	} `json:"write"`
	Revert struct {
		P string `json:"p"`
	} `json:"revert"`
}

func buildRestWriteFixture(t *testing.T) []byte {
	t.Helper()
	entry, sum, err := EntryBytes(seededRestWriteEntry())
	if err != nil {
		t.Fatal(err)
	}
	route, routeSum, err := RouteBytes(seededPagesUpdateFieldsRoute())
	if err != nil {
		t.Fatal(err)
	}
	input := []byte(`{"route_id":"wp-v2-pages-update-fields","path":{"id":412},"query":{},"body":{"title":"Café & croissants 🥐","excerpt":"Prix: 5€ — naïve 日本語"}}`)
	reqID := uuid.MustParse("11111111-2222-4333-8444-666666666666")
	f := restWriteFixture{
		Note:        "Generated by Go (internal/abilities/restcall_fixture_test.go). Regenerate with WPMGR_WRITE_FIXTURES=1. The expected digest and fingerprint are fixed stand-ins.",
		SiteID:      "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		RequestID:   reqID.String(),
		Entry:       string(entry),
		EntrySHA256: sum,
		Route:       string(route),
		RouteSHA256: routeSum,
		Input:       string(input),
	}
	build := func(c agentcmd.AbilityRunCall) string {
		c.RequestID, c.Entry, c.EntrySHA256 = reqID, entry, sum
		p, _, err := agentcmd.BuildAbilityRunParams(c)
		if err != nil {
			t.Fatal(err)
		}
		return string(p)
	}
	f.Precheck.P = build(agentcmd.AbilityRunCall{Mode: agentcmd.AbilityRunModePrecheck, Input: input, Route: route, RouteSHA256: routeSum})
	f.Write.Expected = agentcmd.AbilityRunExpected{
		PrecheckDigest:  agentcmd.SHA256Hex([]byte("precheck-digest-stand-in")),
		BaseFingerprint: agentcmd.SHA256Hex([]byte("base-fingerprint-stand-in")),
	}
	f.Write.P = build(agentcmd.AbilityRunCall{Mode: agentcmd.AbilityRunModeWrite, Input: input, Route: route, RouteSHA256: routeSum, Expected: &f.Write.Expected})
	f.Revert.P = build(agentcmd.AbilityRunCall{Mode: agentcmd.AbilityRunModeRevert})

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(f); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func routeFixtureBytes(t *testing.T, r sqlc.RestRouteCatalogue) []byte {
	t.Helper()
	b, _, err := RouteBytes(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func checkOrWriteFixture(t *testing.T, path string, want []byte) {
	t.Helper()
	if os.Getenv("WPMGR_WRITE_FIXTURES") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v; regenerate with WPMGR_WRITE_FIXTURES=1", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from what Go produces now; regenerate with WPMGR_WRITE_FIXTURES=1", path)
	}
}

// TestRestCallRouteFixtures: the route row bytes the agent's RestCallTest
// reads are exactly RouteBytes of the m161 seed rows.
func TestRestCallRouteFixtures(t *testing.T) {
	checkOrWriteFixture(t, restReadRouteFixturePath, routeFixtureBytes(t, seededPagesGetRoute()))
	checkOrWriteFixture(t, restWriteRouteFixturePath, routeFixtureBytes(t, seededPagesUpdateFieldsRoute()))
}

// TestRestWriteFixture: the full precheck/write/revert p texts for
// wpmgr/rest-write, which a PHPUnit replay hashes and decodes.
func TestRestWriteFixture(t *testing.T) {
	checkOrWriteFixture(t, restWriteFixturePath, buildRestWriteFixture(t))
}

// TestRouteBytesStable: the hash depends on the value, not on how Postgres
// spelled the jsonb (jsonb reorders keys).
func TestRouteBytesStable(t *testing.T) {
	a := seededPagesGetRoute()
	b := seededPagesGetRoute()
	b.PinnedQuery = []byte(`{"context": "view", "status": "publish"}`)
	b.PathParams = []byte(`{"id": {"max": 9999999999, "min": 1, "type": "int", "required": true}}`)
	_, sa, _ := RouteBytes(a)
	_, sb, _ := RouteBytes(b)
	if sa != sb {
		t.Fatalf("route hash moved with jsonb key order: %s vs %s", sa, sb)
	}
	b.Title = "Read a published page!"
	if _, sc, _ := RouteBytes(b); sc == sa {
		t.Fatal("a title edit must move the route hash")
	}
}

// TestSendableRouteFailsClosed: a NULL hash and a stale hash both refuse.
func TestSendableRouteFailsClosed(t *testing.T) {
	r := seededPagesUpdateFieldsRoute()
	if _, _, err := SendableRoute(r); !errors.Is(err, ErrRouteUnstamped) {
		t.Fatalf("NULL route_sha256: got %v, want ErrRouteUnstamped", err)
	}
	_, sum, _ := RouteBytes(r)
	r.RouteSha256 = &sum
	if _, got, err := SendableRoute(r); err != nil || got != sum {
		t.Fatalf("stamped route: %v %s", err, got)
	}
	r.Enabled = false
	if _, _, err := SendableRoute(r); !errors.Is(err, ErrRouteChanged) {
		t.Fatalf("edited route: got %v, want ErrRouteChanged", err)
	}
}
