package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// ---------------------------------------------------------------------------
// fleet_updates_pending, proved THROUGH THE MOUNTED TRANSPORT WITH A REAL
// BEARER, because the claim being made is that an AI client can ask.
//
// Every assertion below is on an EXACT VALUE. "A plugin list came back" passes
// when the list belongs to the wrong site, which is the failure mode this whole
// file is about.
// ---------------------------------------------------------------------------

// updatesFixture is one site row with a known inventory document.
type updatesFixture struct {
	id         uuid.UUID
	name       string
	url        string
	components string
	collected  *time.Time
}

func (f updatesFixture) row() sqlc.Site {
	r := sqlc.Site{
		ID:              f.id,
		Name:            f.name,
		Url:             f.url,
		ConnectionState: "connected",
		HealthStatus:    "healthy",
		WpVersion:       "6.5.2",
		Components:      []byte(f.components),
	}
	if f.collected != nil {
		r.ComponentsUpdatedAt = pgtype.Timestamptz{Time: *f.collected, Valid: true}
	}
	return r
}

// callUpdatesPending drives tools/call over the real mount and returns the
// result text, failing the test on any JSON-RPC error.
func callUpdatesPending(t *testing.T, store Store) string {
	t.Helper()
	r := newTransportRouter(t, store)
	w := post(t, r, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":`+
		`{"name":"fleet_updates_pending","arguments":{}}}`, nil)
	// DECODED FROM THE RAW BODY, not through the package's own response type.
	// The claim under test is that a client reading the wire gets this, so the
	// wire is what is parsed.
	var envelope struct {
		Error  json.RawMessage `json:"error"`
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("response is not JSON-RPC: %v\nbody: %s", err, w.Body.String())
	}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		t.Fatalf("tools/call fleet_updates_pending errored: %s", envelope.Error)
	}
	if envelope.Result.IsError {
		t.Fatalf("tool reported isError: %s", w.Body.String())
	}
	if len(envelope.Result.Content) != 1 || envelope.Result.Content[0].Type != "text" {
		t.Fatalf("want exactly one text content block, got %+v", envelope.Result.Content)
	}
	return envelope.Result.Content[0].Text
}

// parseUpdatesPayload pulls the JSON half out of the result text.
func parseUpdatesPayload(t *testing.T, text string) updatesPayloadForTest {
	t.Helper()
	i := strings.Index(text, `{"sites":`)
	if i < 0 {
		t.Fatalf("no JSON payload in result text:\n%s", text)
	}
	var p updatesPayloadForTest
	if err := json.Unmarshal([]byte(text[i:]), &p); err != nil {
		t.Fatalf("payload is not JSON: %v\n%s", err, text[i:])
	}
	return p
}

// updatesPayloadForTest decodes what the wire actually carried, rather than
// re-using the producer's own struct. Sharing the struct would let a renamed
// JSON tag pass both halves of every assertion below.
type updatesPayloadForTest struct {
	Sites []struct {
		SiteID               string  `json:"site_id"`
		Name                 string  `json:"name"`
		URL                  string  `json:"url"`
		InventoryStatus      string  `json:"inventory_status"`
		InventoryCollectedAt *string `json:"inventory_collected_at"`
		PendingTotal         int     `json:"pending_total"`
		Summary              string  `json:"summary"`
		Core                 *struct {
			CurrentVersion string `json:"current_version"`
			NewVersion     string `json:"new_version"`
		} `json:"core_update"`
		Plugins []struct {
			Slug             string `json:"slug"`
			Name             string `json:"name"`
			Active           bool   `json:"active"`
			InstalledVersion string `json:"installed_version"`
			NewVersion       string `json:"new_version"`
		} `json:"plugins"`
		Themes []struct {
			Slug       string `json:"slug"`
			NewVersion string `json:"new_version"`
		} `json:"themes"`
	} `json:"sites"`
	Totals struct {
		SitesWithUpdates    int `json:"sites_with_updates"`
		SitesNeverCollected int `json:"sites_never_collected"`
		PendingPlugins      int `json:"pending_plugins"`
		PendingThemes       int `json:"pending_themes"`
		PendingCore         int `json:"pending_core"`
		PendingTotal        int `json:"pending_total"`
	} `json:"totals"`
	Envelope struct {
		Asked    int `json:"asked"`
		OK       int `json:"ok"`
		Refused  int `json:"refused"`
		Refusals []struct {
			SiteID string `json:"site_id"`
			Code   string `json:"code"`
		} `json:"refusals"`
	} `json:"envelope"`
}

// ---------------------------------------------------------------------------
// THE SCOPE PROOF. It is first in the file and its leak assertions run before
// any count assertion, deliberately: a leak check sitting behind a row-count
// check is itself unproven, because the count fails first and the leak
// assertion never executes.
// ---------------------------------------------------------------------------

func TestFleetUpdatesPending_DoesNotLeakAnUnscopedSite(t *testing.T) {
	inScope := uuid.New()
	outOfScope := uuid.New()

	store := liveGrantStore(inScope) // the grant names ONLY inScope
	store.sites = []sqlc.Site{
		updatesFixture{
			id: inScope, name: "acme.com", url: "https://acme.com",
			components: `{"plugins":[{"slug":"in-scope-plugin","name":"In Scope Plugin",` +
				`"version":"1.0.0","active":true,"available_update":{"new_version":"1.1.0"}}]}`,
		}.row(),
		// THE FAKE STORE RETURNS THIS ROW ANYWAY, and that is the point of the
		// fixture rather than a flaw in it: the real query's site_ids predicate
		// and the RESTRICTIVE sites_site_scope policy both drop it, and this
		// test models the state where both have failed. It is the only state in
		// which layer 3 -- the in-Go SiteSet filter -- can be observed working.
		updatesFixture{
			id: outOfScope, name: "northgate-secret.co.uk", url: "https://northgate-secret.co.uk",
			components: `{"plugins":[{"slug":"out-of-scope-plugin","name":"Out Of Scope Plugin",` +
				`"version":"2.0.0","active":true,"available_update":{"new_version":"2.5.0"}}]}`,
		}.row(),
	}

	text := callUpdatesPending(t, store)

	// LEAK ASSERTIONS FIRST, over the RAW RESULT TEXT rather than the decoded
	// payload, so a leak into the instruction header or a truncation banner is
	// caught as well as one into the records.
	for _, secret := range []string{
		outOfScope.String(),
		"northgate-secret.co.uk",
		"out-of-scope-plugin",
		"Out Of Scope Plugin",
		"2.5.0",
	} {
		if strings.Contains(text, secret) {
			t.Fatalf("fleet_updates_pending disclosed %q, which belongs to a site outside the "+
				"connection's scope. Full result:\n%s", secret, text)
		}
	}

	p := parseUpdatesPayload(t, text)

	// LAYER 3 HIDES: the out-of-scope site is ABSENT, never refused. Naming it
	// in a refusal would disclose that it exists.
	if p.Envelope.Asked != 1 {
		t.Errorf("envelope.asked = %d, want 1 (the CALLER'S OWN scope cardinality, never the "+
			"tenant's site count)", p.Envelope.Asked)
	}
	if p.Envelope.OK != 1 || p.Envelope.Refused != 0 {
		t.Errorf("envelope ok/refused = %d/%d, want 1/0", p.Envelope.OK, p.Envelope.Refused)
	}
	for _, r := range p.Envelope.Refusals {
		if r.SiteID == outOfScope.String() {
			t.Fatalf("an out-of-scope site was REFUSED (%s) instead of being absent; a refusal "+
				"naming it discloses that it exists", r.Code)
		}
	}

	// And the in-scope site really did answer, so the absence above is scoping
	// rather than an empty result.
	if len(p.Sites) != 1 {
		t.Fatalf("want exactly 1 site, got %d", len(p.Sites))
	}
	if p.Sites[0].SiteID != inScope.String() {
		t.Errorf("site_id = %s, want the in-scope site %s", p.Sites[0].SiteID, inScope)
	}
	if len(p.Sites[0].Plugins) != 1 || p.Sites[0].Plugins[0].Slug != fenceSiteText("in-scope-plugin") {
		t.Errorf("plugins = %+v, want exactly [in-scope-plugin]", p.Sites[0].Plugins)
	}
}

// ---------------------------------------------------------------------------
// EXACT VALUES.
// ---------------------------------------------------------------------------

func TestFleetUpdatesPending_ReportsExactPendingUpdates(t *testing.T) {
	siteA := uuid.New()
	siteB := uuid.New()
	collected := time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC)

	store := liveGrantStore(siteA, siteB)
	store.sites = []sqlc.Site{
		updatesFixture{
			id: siteA, name: "acme.com", url: "https://acme.com", collected: &collected,
			components: `{"plugins":[` +
				// Inactive, so it must sort AFTER the active one below.
				`{"slug":"akismet","name":"Akismet","version":"5.1","active":false,` +
				`"available_update":{"new_version":"5.3"}},` +
				`{"slug":"woocommerce","name":"WooCommerce","version":"8.1.0","active":true,` +
				`"available_update":{"new_version":"8.4.0"}},` +
				// No advisory at all: not pending.
				`{"slug":"hello-dolly","name":"Hello Dolly","version":"1.7.2","active":true}` +
				`],"themes":[` +
				`{"slug":"twentytwentyfour","name":"Twenty Twenty-Four","version":"1.0","active":true,` +
				`"available_update":{"new_version":"1.2"}}` +
				`],"core_update":{"current_version":"6.5.2","new_version":"6.6"}}`,
		}.row(),
		// Fully up to date: an inventory WAS collected and nothing is pending.
		updatesFixture{
			id: siteB, name: "quiet.example", url: "https://quiet.example", collected: &collected,
			components: `{"plugins":[{"slug":"akismet","name":"Akismet","version":"5.3","active":true}]}`,
		}.row(),
	}

	p := parseUpdatesPayload(t, callUpdatesPending(t, store))

	if len(p.Sites) != 2 {
		t.Fatalf("want 2 sites, got %d", len(p.Sites))
	}
	var a, b int
	for i, s := range p.Sites {
		switch s.SiteID {
		case siteA.String():
			a = i
		case siteB.String():
			b = i
		default:
			t.Fatalf("unexpected site_id %s", s.SiteID)
		}
	}

	// --- site A, exact ---
	got := p.Sites[a]
	if got.PendingTotal != 4 {
		t.Errorf("acme pending_total = %d, want 4 (2 plugins + 1 theme + core)", got.PendingTotal)
	}
	if got.InventoryStatus != inventoryCollected {
		t.Errorf("acme inventory_status = %q, want %q", got.InventoryStatus, inventoryCollected)
	}
	if got.InventoryCollectedAt == nil || *got.InventoryCollectedAt != "2026-08-30T09:00:00Z" {
		t.Errorf("acme inventory_collected_at = %v, want 2026-08-30T09:00:00Z", got.InventoryCollectedAt)
	}
	if len(got.Plugins) != 2 {
		t.Fatalf("acme plugins = %+v, want exactly 2 (hello-dolly has no advisory)", got.Plugins)
	}
	// ACTIVE BEFORE INACTIVE, matching the dashboard's own order.
	// EVERY EXPECTATION BELOW IS FENCED. Slugs, names and version strings all
	// come out of the site's inventory document, so each one carries the
	// site-supplied marker in the rendered result; see fence.go.
	if got.Plugins[0].Slug != fenceSiteText("woocommerce") || got.Plugins[1].Slug != fenceSiteText("akismet") {
		t.Errorf("plugin order = [%s %s], want [woocommerce akismet] (active before inactive)",
			got.Plugins[0].Slug, got.Plugins[1].Slug)
	}
	if got.Plugins[0].InstalledVersion != fenceSiteText("8.1.0") || got.Plugins[0].NewVersion != fenceSiteText("8.4.0") {
		t.Errorf("woocommerce = %s -> %s, want 8.1.0 -> 8.4.0",
			got.Plugins[0].InstalledVersion, got.Plugins[0].NewVersion)
	}
	if got.Plugins[0].Name != fenceSiteText("WooCommerce") || !got.Plugins[0].Active {
		t.Errorf("woocommerce name/active = %q/%v, want WooCommerce/true",
			got.Plugins[0].Name, got.Plugins[0].Active)
	}
	if got.Plugins[1].InstalledVersion != fenceSiteText("5.1") || got.Plugins[1].NewVersion != fenceSiteText("5.3") {
		t.Errorf("akismet = %s -> %s, want 5.1 -> 5.3",
			got.Plugins[1].InstalledVersion, got.Plugins[1].NewVersion)
	}
	if len(got.Themes) != 1 || got.Themes[0].Slug != fenceSiteText("twentytwentyfour") ||
		got.Themes[0].NewVersion != fenceSiteText("1.2") {
		t.Errorf("themes = %+v, want exactly [twentytwentyfour -> 1.2]", got.Themes)
	}
	if got.Core == nil {
		t.Fatal("acme core_update is null, want 6.5.2 -> 6.6")
	}
	if got.Core.CurrentVersion != fenceSiteText("6.5.2") || got.Core.NewVersion != fenceSiteText("6.6") {
		t.Errorf("core = %s -> %s, want 6.5.2 -> 6.6", got.Core.CurrentVersion, got.Core.NewVersion)
	}

	// --- site B: genuinely up to date, and it must say ZERO rather than being
	// omitted. A site dropped from the list because it has nothing pending is
	// indistinguishable from a site that was never read.
	if p.Sites[b].PendingTotal != 0 {
		t.Errorf("quiet.example pending_total = %d, want 0", p.Sites[b].PendingTotal)
	}
	if p.Sites[b].InventoryStatus != inventoryCollected {
		t.Errorf("quiet.example inventory_status = %q, want %q",
			p.Sites[b].InventoryStatus, inventoryCollected)
	}

	// --- fleet totals, exact ---
	if p.Totals.SitesWithUpdates != 1 {
		t.Errorf("totals.sites_with_updates = %d, want 1", p.Totals.SitesWithUpdates)
	}
	if p.Totals.PendingPlugins != 2 || p.Totals.PendingThemes != 1 || p.Totals.PendingCore != 1 {
		t.Errorf("totals plugins/themes/core = %d/%d/%d, want 2/1/1",
			p.Totals.PendingPlugins, p.Totals.PendingThemes, p.Totals.PendingCore)
	}
	if p.Totals.PendingTotal != 4 {
		t.Errorf("totals.pending_total = %d, want 4", p.Totals.PendingTotal)
	}
	if p.Totals.SitesNeverCollected != 0 {
		t.Errorf("totals.sites_never_collected = %d, want 0", p.Totals.SitesNeverCollected)
	}
	if p.Envelope.Asked != 2 || p.Envelope.OK != 2 || p.Envelope.Refused != 0 {
		t.Errorf("envelope = %d/%d/%d, want asked=2 ok=2 refused=0",
			p.Envelope.Asked, p.Envelope.OK, p.Envelope.Refused)
	}
}

// TestFleetUpdatesPending_AgreesWithTheDashboardsPredicates is the proof that
// the assistant will not contradict the screen.
//
// Both fixtures below are advisories the dashboard DROPS. If this tool counted
// them, an operator would read "2 updates pending" from the assistant and see
// "up to date" on the site page, and only one of those two gets noticed.
func TestFleetUpdatesPending_AgreesWithTheDashboardsPredicates(t *testing.T) {
	siteID := uuid.New()
	store := liveGrantStore(siteID)
	store.sites = []sqlc.Site{
		updatesFixture{
			id: siteID, name: "acme.com", url: "https://acme.com",
			components: `{"plugins":[` +
				// GH #211 phantom: the advisory names the version already installed.
				`{"slug":"phantom-plugin","name":"Phantom","version":"3.0.0","active":true,` +
				`"available_update":{"new_version":"3.0.0"}},` +
				// An advisory with no target version at all.
				`{"slug":"empty-advisory","name":"Empty","version":"1.0","active":true,` +
				`"available_update":{"new_version":""}},` +
				// The agent's own plugin, which is never offered as an update.
				`{"slug":"fleet-agent-site-manager","name":"Fleet Agent Site Manager",` +
				`"version":"2.6.1","active":true,"available_update":{"new_version":"2.9.0"}},` +
				// One real one, so a tool that returned nothing at all cannot pass.
				`{"slug":"woocommerce","name":"WooCommerce","version":"8.1.0","active":true,` +
				`"available_update":{"new_version":"8.4.0"}}` +
				`],"core_update":{"current_version":"6.6","new_version":"6.6"}}`,
		}.row(),
	}

	text := callUpdatesPending(t, store)
	p := parseUpdatesPayload(t, text)

	if len(p.Sites) != 1 {
		t.Fatalf("want 1 site, got %d", len(p.Sites))
	}
	got := p.Sites[0]
	if got.PendingTotal != 1 {
		t.Errorf("pending_total = %d, want 1 -- only woocommerce is actionable. Got plugins %+v, "+
			"core %+v", got.PendingTotal, got.Plugins, got.Core)
	}
	if len(got.Plugins) != 1 || got.Plugins[0].Slug != fenceSiteText("woocommerce") {
		t.Fatalf("plugins = %+v, want exactly [woocommerce]", got.Plugins)
	}
	// Named, so the failure says WHICH rule broke rather than only that a count
	// moved.
	for _, dropped := range []string{"phantom-plugin", "empty-advisory", "fleet-agent-site-manager"} {
		if strings.Contains(text, dropped) {
			t.Errorf("%q was offered as a pending update; the dashboard drops it, so the "+
				"assistant now contradicts the screen", dropped)
		}
	}
	if got.Core != nil {
		t.Errorf("core_update = %+v, want null: an advisory naming the installed version is a "+
			"phantom for core exactly as it is for a component", got.Core)
	}
}

// TestFleetUpdatesPending_NeverCollectedIsNotZero pins the distinction
// sites.components_updated_at is nullable-with-no-backfill in order to keep.
func TestFleetUpdatesPending_NeverCollectedIsNotZero(t *testing.T) {
	never := uuid.New()
	store := liveGrantStore(never)
	store.sites = []sqlc.Site{
		// No collected stamp, and no inventory document either: we have never
		// looked at this site.
		updatesFixture{id: never, name: "unknown.example", url: "https://unknown.example",
			components: `{}`}.row(),
	}

	text := callUpdatesPending(t, store)
	p := parseUpdatesPayload(t, text)

	if len(p.Sites) != 1 {
		t.Fatalf("want 1 site, got %d", len(p.Sites))
	}
	if p.Sites[0].InventoryStatus != inventoryNeverCollected {
		t.Errorf("inventory_status = %q, want %q", p.Sites[0].InventoryStatus, inventoryNeverCollected)
	}
	if p.Sites[0].InventoryCollectedAt != nil {
		t.Errorf("inventory_collected_at = %v, want JSON null -- a substitute date here reports "+
			"an observation that was never taken", *p.Sites[0].InventoryCollectedAt)
	}
	// THE COUNT THAT MATTERS: never-collected is reported SEPARATELY and is not
	// folded into the up-to-date sites.
	if p.Totals.SitesNeverCollected != 1 {
		t.Errorf("totals.sites_never_collected = %d, want 1", p.Totals.SitesNeverCollected)
	}
	if p.Totals.SitesWithUpdates != 0 {
		t.Errorf("totals.sites_with_updates = %d, want 0", p.Totals.SitesWithUpdates)
	}
	// And the model is TOLD, in the prepended text, not left to infer it.
	if !strings.Contains(text, "never_collected") {
		t.Error("the instructions never mention never_collected, so a model reading pending_total 0 " +
			"has nothing telling it we simply have not looked")
	}
}

// TestFleetUpdatesPending_EmptyScopeRefuses: an empty resolved scope is a
// REFUSAL and never an empty list, which would be indistinguishable from a
// healthy organisation owning nothing.
func TestFleetUpdatesPending_EmptyScopeRefuses(t *testing.T) {
	store := liveGrantStore() // no sites in the grant
	store.sites = []sqlc.Site{
		updatesFixture{id: uuid.New(), name: "somebody-elses.com", url: "https://somebody-elses.com",
			components: `{"plugins":[{"slug":"leaky","version":"1.0","active":true,` +
				`"available_update":{"new_version":"2.0"}}]}`}.row(),
	}

	r := newTransportRouter(t, store)
	w := post(t, r, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":`+
		`{"name":"fleet_updates_pending","arguments":{}}}`, nil)

	// LEAK FIRST, again: an empty-scope call must not disclose the row the fake
	// store would have handed back.
	if strings.Contains(w.Body.String(), "somebody-elses.com") || strings.Contains(w.Body.String(), "leaky") {
		t.Fatalf("an empty-scope refusal disclosed a site: %s", w.Body.String())
	}

	body := w.Body.String()
	if !strings.Contains(body, ErrCodeScopeEmpty) {
		t.Fatalf("want a %s refusal, got: %s", ErrCodeScopeEmpty, body)
	}
	if strings.Contains(body, `"sites":[]`) {
		t.Fatalf("an empty scope returned an empty LIST rather than refusing: %s", body)
	}
}

// TestFleetUpdatesPending_UnreadInScopeSiteIsRefusedNotDropped: a site the
// caller may see and the page did not return is accounted for by name, so
// ok+refused still balances against the caller's own scope.
func TestFleetUpdatesPending_UnreadInScopeSiteIsRefusedNotDropped(t *testing.T) {
	present := uuid.New()
	missing := uuid.New()

	store := liveGrantStore(present, missing)
	store.sites = []sqlc.Site{
		updatesFixture{id: present, name: "acme.com", url: "https://acme.com",
			components: `{}`}.row(),
		// `missing` is in scope and is deliberately NOT in the store's page.
	}

	p := parseUpdatesPayload(t, callUpdatesPending(t, store))

	if p.Envelope.Asked != 2 || p.Envelope.OK != 1 || p.Envelope.Refused != 1 {
		t.Fatalf("envelope = asked %d ok %d refused %d, want 2/1/1",
			p.Envelope.Asked, p.Envelope.OK, p.Envelope.Refused)
	}
	if len(p.Envelope.Refusals) != 1 {
		t.Fatalf("want exactly 1 refusal, got %+v", p.Envelope.Refusals)
	}
	if p.Envelope.Refusals[0].SiteID != missing.String() {
		t.Errorf("refusal names %s, want the unread in-scope site %s",
			p.Envelope.Refusals[0].SiteID, missing)
	}
	if p.Envelope.Refusals[0].Code != RefusalSiteUnread.String() {
		t.Errorf("refusal code = %q, want %q", p.Envelope.Refusals[0].Code, RefusalSiteUnread)
	}
}

// TestFleetUpdateTotalsCoverOnlyTheKeptRecords is the unit half of the
// truncation reasoning: a roll-up that counted records the byte cap removed
// would describe a list the caller cannot see, and the only way to notice would
// be to re-add the per-site numbers and find they disagree.
func TestFleetUpdateTotalsCoverOnlyTheKeptRecords(t *testing.T) {
	now := time.Now().UTC()
	rows := make([]sqlc.Site, 0, 40)
	for i := 0; i < 40; i++ {
		// A long name so each record is large; 40 of them cross recordByteBudget.
		rows = append(rows, updatesFixture{
			id:   uuid.New(),
			name: fmt.Sprintf("site-%02d-%s", i, strings.Repeat("padding", 120)),
			url:  "https://example.test",
			components: `{"plugins":[{"slug":"woocommerce","name":"WooCommerce","version":"8.1.0",` +
				`"active":true,"available_update":{"new_version":"8.4.0"}}]}`,
		}.row())
	}
	env, err := NewEnvelope(len(rows), len(rows), nil)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}

	text, err := buildUpdatesPendingResult(rows, env, now, noOperatorContext)
	if err != nil {
		t.Fatalf("buildUpdatesPendingResult: %v", err)
	}
	p := parseUpdatesPayload(t, text)

	if len(p.Sites) >= len(rows) {
		t.Fatalf("the fixture did not cross the byte cap (%d of %d returned), so this proof "+
			"checks nothing", len(p.Sites), len(rows))
	}
	if p.Totals.PendingTotal != len(p.Sites) {
		t.Errorf("totals.pending_total = %d over a list of %d sites each with 1 pending update: "+
			"the roll-up covers records the byte cap removed", p.Totals.PendingTotal, len(p.Sites))
	}
	if p.Totals.SitesWithUpdates != len(p.Sites) {
		t.Errorf("totals.sites_with_updates = %d, want %d", p.Totals.SitesWithUpdates, len(p.Sites))
	}
	if !strings.Contains(text, "INCOMPLETE RESULT") {
		t.Error("a truncated result carried no banner, so it reads as complete")
	}
}

// TestFleetUpdatesPending_SummaryCarriesTheAgeInProse closes the gap the
// structured age leaves open: inventory_age_seconds is a field a model may
// simply not read, and a pending_total read without it looks like a current
// fact. The age has to travel in the same string as the count.
func TestFleetUpdatesPending_SummaryCarriesTheAgeInProse(t *testing.T) {
	old := uuid.New()
	recent := uuid.New()
	now := time.Now().UTC()
	eightMonths := now.Add(-243 * 24 * time.Hour)
	twoHours := now.Add(-2 * time.Hour)

	store := liveGrantStore(old, recent)
	store.sites = []sqlc.Site{
		updatesFixture{
			id: old, name: "forgotten.example", url: "https://forgotten.example",
			collected: &eightMonths,
			components: `{"plugins":[{"slug":"woocommerce","name":"WooCommerce","version":"8.1.0",` +
				`"active":true,"available_update":{"new_version":"8.4.0"}}]}`,
		}.row(),
		updatesFixture{
			id: recent, name: "current.example", url: "https://current.example",
			collected:  &twoHours,
			components: `{"plugins":[{"slug":"akismet","name":"Akismet","version":"5.3","active":true}]}`,
		}.row(),
	}

	text := callUpdatesPending(t, store)
	p := parseUpdatesPayload(t, text)

	byName := map[string]string{}
	for _, s := range p.Sites {
		byName[s.Name] = s.Summary
	}

	// KEYED ON THE FENCED NAME, because that is what the record now carries.
	oldSummary := byName[fenceSiteText("forgotten.example")]
	// THE AGE IS IN THE SENTENCE, not only in a neighbouring key.
	if !strings.Contains(oldSummary, "months ago") {
		t.Errorf("the summary for an eight-month-old inventory does not carry its age in prose: %q",
			oldSummary)
	}
	// And the count is in the SAME string, so a reader cannot take one without
	// the other.
	if !strings.Contains(oldSummary, "1 update outstanding") {
		t.Errorf("the summary does not carry the count alongside the age: %q", oldSummary)
	}

	recentSummary := byName[fenceSiteText("current.example")]
	if !strings.Contains(recentSummary, "2 hours ago") {
		t.Errorf("the summary for a two-hour-old inventory does not carry its age: %q", recentSummary)
	}
	if !strings.Contains(recentSummary, "no updates outstanding") {
		t.Errorf("an up-to-date site's summary does not say so: %q", recentSummary)
	}

	// THE NO-JUDGEMENT RULE, PINNED. Rendering any of these would be choosing a
	// freshness window by the back door -- the owner ruled that the age is
	// reported and the reader judges it.
	for _, verdict := range []string{"stale", "outdated", "out of date", "too old", "fresh", "needs refresh"} {
		if strings.Contains(strings.ToLower(text), verdict) {
			t.Errorf("the result renders the verdict %q. Reporting an age is the ruling; judging "+
				"it picks a freshness window nobody chose. Full text:\n%s", verdict, text)
		}
	}
}

// TestFleetUpdatesPending_NeverCollectedSaysWeHaveNotLookedInWords: the absence
// must read as a finding, because a reader filling in an absence fills it in
// with the reassuring assumption.
func TestFleetUpdatesPending_NeverCollectedSaysWeHaveNotLookedInWords(t *testing.T) {
	never := uuid.New()
	store := liveGrantStore(never)
	store.sites = []sqlc.Site{
		updatesFixture{id: never, name: "unknown.example", url: "https://unknown.example",
			components: `{}`}.row(),
	}

	p := parseUpdatesPayload(t, callUpdatesPending(t, store))
	if len(p.Sites) != 1 {
		t.Fatalf("want 1 site, got %d", len(p.Sites))
	}
	s := p.Sites[0].Summary
	for _, want := range []string{"has ever been collected", "we do not know", "have not looked"} {
		if !strings.Contains(s, want) {
			t.Errorf("the never_collected summary does not say %q in words: %q", want, s)
		}
	}
	// It must NOT read as a clean bill of health.
	if strings.Contains(s, "no updates outstanding") {
		t.Errorf("a site we have never looked at is described as having no updates outstanding: %q", s)
	}
}

// TestFleetUpdatesPending_UndatedInventoryWithAdvisoriesDoesNotClaimZero is the
// security reviewer's probe A, adopted as a regression test.
//
// THE STATE IS REACHABLE BY CONSTRUCTION. m121 added components_updated_at NULL
// with no backfill and UpdateSiteMetadata is its only writer, so every site
// enrolled before 2026-08-23 that has not pushed metadata since has a POPULATED
// components document and no stamp. Those are disproportionately the neglected
// sites, which carry the most outstanding updates.
//
// The defect it caught: updatesSummary branched on InventoryStatus alone, so
// this site was told "the zero counts below mean we have not looked" while
// pending_total said 3. A false zero in the one field built so the count cannot
// be dropped without its caveat.
//
// MY OWN never_collected FIXTURE MISSED IT because it used components '{}'.
// A fixture that cannot reach the state cannot test it.
func TestFleetUpdatesPending_UndatedInventoryWithAdvisoriesDoesNotClaimZero(t *testing.T) {
	id := uuid.New()
	store := liveGrantStore(id)
	store.sites = []sqlc.Site{
		updatesFixture{
			id: id, name: "legacy.example", url: "https://legacy.example",
			// collected is nil: components_updated_at IS NULL (a pre-m121 row).
			components: `{"plugins":[` +
				`{"slug":"woocommerce","name":"WooCommerce","version":"8.1.0","active":true,` +
				`"available_update":{"new_version":"8.4.0"}},` +
				`{"slug":"akismet","name":"Akismet","version":"5.1","active":true,` +
				`"available_update":{"new_version":"5.3"}}` +
				`],"core_update":{"current_version":"6.5.2","new_version":"6.6"}}`,
		}.row(),
	}

	text := callUpdatesPending(t, store)
	p := parseUpdatesPayload(t, text)
	if len(p.Sites) != 1 {
		t.Fatalf("want 1 site, got %d", len(p.Sites))
	}
	got := p.Sites[0]

	if got.PendingTotal != 3 {
		t.Fatalf("pending_total = %d, want 3 (2 plugins + core); the fixture no longer reaches "+
			"the state under test", got.PendingTotal)
	}
	if got.InventoryStatus != inventoryNeverCollected {
		t.Fatalf("inventory_status = %q, want %q; the fixture no longer reaches the state under test",
			got.InventoryStatus, inventoryNeverCollected)
	}

	// THE DEFECT ITSELF: the summary must not assert a zero it does not have.
	if strings.Contains(got.Summary, "The zero counts below") {
		t.Errorf("the summary asserts a zero count while pending_total=%d and %d plugins are "+
			"listed:\n%s", got.PendingTotal, len(got.Plugins), got.Summary)
	}
	// And it must carry both facts: the real count, and that its age is unknown.
	if !strings.Contains(got.Summary, "3 updates outstanding") {
		t.Errorf("the summary drops the real count: %q", got.Summary)
	}
	for _, want := range []string{"no record of when", "lower bound"} {
		if !strings.Contains(got.Summary, want) {
			t.Errorf("the summary does not say %q, so the count reads as dated: %q", want, got.Summary)
		}
	}

	// THE TOTALS OVERLAP, AND THE HEADER MUST SAY SO.
	//
	// The reviewer's probe asserted the two totals could never both be
	// non-zero, which encoded the instruction header's original claim that they
	// are mutually exclusive. That claim was FALSE for exactly this site, so the
	// header was corrected rather than the counts suppressed -- throwing away a
	// true fact to protect a sentence would be the worse trade. This assertion
	// pins the replacement contract in both halves.
	if p.Totals.SitesWithUpdates != 1 || p.Totals.SitesNeverCollected != 1 {
		t.Errorf("totals sites_with_updates/sites_never_collected = %d/%d, want 1/1: this site "+
			"genuinely is both", p.Totals.SitesWithUpdates, p.Totals.SitesNeverCollected)
	}
	if !strings.Contains(text, "OVERLAPS") {
		t.Error("the instructions do not tell the model the two totals overlap, so a model that " +
			"adds them double-counts this site")
	}
	if strings.Contains(text, "never merge them with the up-to-date ones") {
		t.Error("the instructions still claim the two totals are mutually exclusive, which is " +
			"false for an undated inventory carrying advisories")
	}
}

// TestFleetUpdatesPending_FutureDatedStampIsNotRenderedAsFresh is the reviewer's
// probe B, adopted. The arm existed and behaved correctly but had no committed
// test, so nothing would have caught its removal.
func TestFleetUpdatesPending_FutureDatedStampIsNotRenderedAsFresh(t *testing.T) {
	id := uuid.New()
	future := time.Now().UTC().Add(48 * time.Hour)
	store := liveGrantStore(id)
	store.sites = []sqlc.Site{
		updatesFixture{
			id: id, name: "skewed.example", url: "https://skewed.example",
			collected: &future,
			components: `{"plugins":[{"slug":"woocommerce","name":"WooCommerce","version":"8.1.0",` +
				`"active":true,"available_update":{"new_version":"8.4.0"}}]}`,
		}.row(),
	}

	text := callUpdatesPending(t, store)
	p := parseUpdatesPayload(t, text)
	s := p.Sites[0].Summary

	if !strings.Contains(s, "clock problem") {
		t.Errorf("the future-stamp arm does not name the cause: %q", s)
	}
	// A negative age must never reach the sentence.
	if strings.Contains(s, "-1 ") || strings.Contains(s, "ago, 2026") && strings.Contains(s, "-") &&
		strings.Contains(s, "seconds ago") {
		t.Errorf("a negative age leaked into the sentence: %q", s)
	}
	// The INSTANT is still reported: withholding the age is not a reason to
	// withhold the observation.
	if p.Sites[0].InventoryCollectedAt == nil {
		t.Error("the future stamp was withheld entirely; the instant should still be reported")
	}
	for _, verdict := range []string{"stale", "outdated", "out of date", "too old", "fresh", "needs refresh"} {
		if strings.Contains(strings.ToLower(text), verdict) {
			t.Errorf("the future-stamp result renders the verdict %q", verdict)
		}
	}
}

// TestHumanAgeSelectsAUnitAndJudgesNothing is the unit half. Each case pins the
// spelling; the loop after it pins the property that matters.
func TestHumanAgeSelectsAUnitAndJudgesNothing(t *testing.T) {
	cases := []struct {
		seconds int64
		want    string
	}{
		{30, "less than a minute ago"},
		{60, "1 minute ago"},
		{600, "10 minutes ago"},
		{3600, "1 hour ago"},
		{7200, "2 hours ago"},
		{86400, "1 day ago"},
		{86400 * 9, "9 days ago"},
		{86400 * 243, "8 months ago"},
		{86400 * 800, "2 years ago"},
	}
	for _, c := range cases {
		if got := humanAge(c.seconds); got != c.want {
			t.Errorf("humanAge(%d) = %q, want %q", c.seconds, got, c.want)
		}
	}
	for _, c := range cases {
		got := strings.ToLower(humanAge(c.seconds))
		for _, verdict := range []string{"stale", "old", "recent", "fresh"} {
			if strings.Contains(got, verdict) {
				t.Errorf("humanAge(%d) = %q, which judges the age instead of stating it",
					c.seconds, got)
			}
		}
	}
}
