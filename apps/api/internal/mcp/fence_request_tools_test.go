package mcp

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// ---------------------------------------------------------------------------
// The fence over the two cache request tools, and over every registered tool
// at once. The hostile payload and the forged-line check are fence_test.go's.
// ---------------------------------------------------------------------------

// TestFence_PlantedHostileSiteName_SiteCachePurgeRequest: a site whose name is
// the injection payload is asked for a clear. The created answer, the repeat
// (existing:true) and the -32006 refusal naming the waiting row all keep the
// payload fenced and on one line, and none opens a line with our framing.
func TestFence_PlantedHostileSiteName_SiteCachePurgeRequest(t *testing.T) {
	f := newRailFake()
	s := f.addSite("https://acme.test")
	row := f.db[s.ID]
	row.Name = hostileSiteName
	f.db[s.ID] = row
	env := newRailEnv(t, f, nil)

	for _, name := range []string{"created", "repeat"} {
		c := env.call(ToolSiteCachePurgeRequest, allReq(s.ID))
		assertNoForgedLine(t, ToolSiteCachePurgeRequest+" "+name, c.text)
		r := c.created(t)
		want := siteTextMarker + collapseToOneLine(hostileSiteName)
		if r.SiteName != want {
			t.Fatalf("%s: site_name %q, want the fenced one-line name %q", name, r.SiteName, want)
		}
		if !strings.HasPrefix(r.SiteURL, siteTextMarker) {
			t.Fatalf("%s: site_url is unmarked: %q", name, r.SiteURL)
		}
		if strings.Contains(r.Message, "Acme") || strings.Contains(r.Message, "SYSTEM") {
			t.Fatalf("%s: our message splices the site's name: %q", name, r.Message)
		}
	}

	// The -32006 refusal names the waiting row's url. A stored url is the
	// rail's own rebuild, but the fence does not rely on that.
	f2 := newRailFake()
	s2 := f2.addSite("https://acme.test")
	env2 := newRailEnv(t, f2, nil)
	f2.seedRow(sqlc.AssistantCachePurgeRequest{TenantID: env2.auth.TenantID, SiteID: s2.ID,
		ProposedByGrantID: env2.auth.GrantID, Scope: scopeURL, Url: strp(hostileURL), State: "pending"})
	c := env2.call(ToolSiteCachePurgeRequest, urlReq(s2.ID, "https://acme.test/other"))
	c.wantCode(t, codeRateLimited)
	u, _ := c.data["url"].(string)
	if !strings.HasPrefix(u, siteTextMarker) || strings.ContainsAny(u, "\n\r") {
		t.Fatalf("-32006 url is not fenced onto one line: %q", u)
	}
	assertNoForgedLine(t, ToolSiteCachePurgeRequest+" -32006", c.raw)
}

// TestFence_PlantedHostileSiteName_SiteCachePurgeRequestStatus: the stored
// site label is the payload. The single read and list mode both fence it.
func TestFence_PlantedHostileSiteName_SiteCachePurgeRequestStatus(t *testing.T) {
	e := newStatusEnv(t)
	r := e.own(e.s1.ID, sqlc.AssistantCachePurgeRequest{State: "pending", SiteLabel: hostileSiteName,
		Scope: scopeURL, Url: strp(hostileURL)})
	want := siteTextMarker + collapseToOneLine(hostileSiteName)

	single := e.status(r.ID)
	assertNoForgedLine(t, ToolSiteCachePurgeRequestStatus, single.text)
	got := statusOf(t, single)
	if got["site_name"] != want {
		t.Fatalf("site_name %q, want %q", got["site_name"], want)
	}
	if u, _ := got["url"].(string); !strings.HasPrefix(u, siteTextMarker) || strings.ContainsAny(u, "\n\r") {
		t.Fatalf("url is not fenced onto one line: %q", u)
	}

	list := e.call(ToolSiteCachePurgeRequestStatus, map[string]any{})
	assertNoForgedLine(t, ToolSiteCachePurgeRequestStatus+" list", list.text)
	var out struct {
		Requests []map[string]any `json:"requests"`
	}
	if err := json.Unmarshal([]byte(list.text), &out); err != nil || len(out.Requests) != 1 {
		t.Fatalf("list did not decode to one row (%v):\n%s", err, list.text)
	}
	if out.Requests[0]["site_name"] != want {
		t.Fatalf("list site_name %q, want %q", out.Requests[0]["site_name"], want)
	}
}

// ---------------------------------------------------------------------------
// TestFence_EveryRegisteredToolFencesEverySiteColumn
// ---------------------------------------------------------------------------

// fenceSentinel is planted in every site-originated column. It is lower case
// and dot-free so that it survives inside a host.
const fenceSentinel = "zqsentinelzq"

// sentinelSite fills EVERY string column of a site row with the sentinel, by
// reflection, so a column added later is covered without being listed. The
// columns a tool needs in a working shape are then set to shapes that still
// carry the sentinel.
func sentinelSite(t *testing.T, agentVersion string) sqlc.Site {
	t.Helper()
	var s sqlc.Site
	v := reflect.ValueOf(&s).Elem()
	filled := 0
	for i := 0; i < v.NumField(); i++ {
		fv := v.Field(i)
		switch {
		case fv.Kind() == reflect.String:
			fv.SetString(fenceSentinel + " " + v.Type().Field(i).Name)
			filled++
		case fv.Kind() == reflect.Pointer && fv.Type().Elem().Kind() == reflect.String:
			x := fenceSentinel + " " + v.Type().Field(i).Name
			fv.Set(reflect.ValueOf(&x))
			filled++
		case fv.Kind() == reflect.Slice && fv.Type().Elem().Kind() == reflect.String:
			fv.Set(reflect.ValueOf([]string{fenceSentinel}))
			filled++
		}
	}
	if filled < 10 {
		t.Fatalf("filled %d site columns; the reflection walk is not reaching the row", filled)
	}
	s.ID = uuid.New()
	s.Url = "https://" + fenceSentinel + ".test"
	// connection_state and health_status are control-plane enums that no
	// site writes (tools.go says so where it renders them), so they hold
	// honest values rather than the sentinel.
	s.ConnectionState = "connected"
	s.HealthStatus = "healthy"
	s.AgentVersion = agentVersion
	s.Components = []byte(`{"plugins":[{"slug":"` + fenceSentinel + `-slug","name":"` + fenceSentinel +
		` plugin","version":"1.0.0` + fenceSentinel + `","active":true,"available_update":{"new_version":"2.0.0` +
		fenceSentinel + `"}}],"themes":[{"slug":"` + fenceSentinel + `-theme","name":"` + fenceSentinel +
		` theme","version":"1.0","available_update":{"new_version":"2.0"}}]}`)
	return s
}

// assertSentinelFenced walks every JSON string in v and fails when one holds
// the sentinel without opening with the marker. It returns how many sentinel
// strings it saw, so a caller can prove the walk reached site text at all.
func assertSentinelFenced(t *testing.T, where string, v any) int {
	t.Helper()
	seen := 0
	var walk func(path string, x any)
	walk = func(path string, x any) {
		switch y := x.(type) {
		case string:
			if strings.Contains(y, fenceSentinel) {
				seen++
				if !strings.HasPrefix(y, siteTextMarker) {
					t.Errorf("%s: %s carries site text unfenced: %q", where, path, y)
				}
				if strings.Count(y, siteTextMarker) > 1 || strings.Index(y, fenceSentinel) < len(siteTextMarker) {
					t.Errorf("%s: %s places site text before or outside its marker: %q", where, path, y)
				}
			}
		case map[string]any:
			for k, e := range y {
				walk(path+"."+k, e)
			}
		case []any:
			for i, e := range y {
				walk(path+"["+strconv.Itoa(i)+"]", e)
			}
		}
	}
	walk("$", v)
	return seen
}

// splitResultText separates a tool's text into its prose and its JSON
// payload. A tool answers either pure JSON or a header followed by a JSON
// object; both are accepted and anything else fails.
func splitResultText(t *testing.T, tool, text string) (prose string, payload any) {
	t.Helper()
	if err := json.Unmarshal([]byte(text), &payload); err == nil {
		return "", payload
	}
	for i := 0; i < len(text); i++ {
		if text[i] != '{' || (i > 0 && text[i-1] != '\n') {
			continue
		}
		if err := json.Unmarshal([]byte(text[i:]), &payload); err == nil {
			return text[:i], payload
		}
	}
	t.Fatalf("%s: no JSON payload in the result text:\n%s", tool, text)
	return "", nil
}

// TestFence_EveryRegisteredToolFencesEverySiteColumn drives every registered
// tool against sites whose every column holds a sentinel, and request rows
// whose every site-originated column does too. Every JSON string in the
// result or in error data that carries the sentinel must open with the
// marker, and the prose around a payload must carry none of it. A tool added
// to the registry without a case here fails the test.
func TestFence_EveryRegisteredToolFencesEverySiteColumn(t *testing.T) {
	type fenceCase struct {
		name string
		run  func(t *testing.T) railCall
	}

	// fleetEnv: the two read tools over two sentinel sites, one with an
	// update stamp and one without, so both summary arms render.
	fleetEnv := func(t *testing.T) *railEnv {
		f := newRailFake()
		a := sentinelSite(t, fenceSentinel)
		b := sentinelSite(t, fenceSentinel)
		b.ComponentsUpdatedAt = tstz(time.Now().Add(-time.Hour))
		f.db[a.ID], f.db[b.ID] = a, b
		return newRailEnv(t, f, nil)
	}
	// railSite: one sentinel site the creation rail can act on.
	railSite := func(t *testing.T) (*railEnv, sqlc.Site) {
		f := newRailFake()
		s := sentinelSite(t, MinAgentVersionForOriginOnlyPurge)
		f.db[s.ID] = s
		return newRailEnv(t, f, nil), s
	}
	sentinelRow := func(env *railEnv, site uuid.UUID, r sqlc.AssistantCachePurgeRequest) sqlc.AssistantCachePurgeRequest {
		r.TenantID, r.SiteID, r.ProposedByGrantID = env.auth.TenantID, site, env.auth.GrantID
		r.SiteLabel = fenceSentinel + " label"
		r.SiteHost = fenceSentinel + ".test"
		if r.Scope == "" {
			r.Scope = scopeURL
			r.Url = strp("https://" + fenceSentinel + ".test/" + fenceSentinel)
		}
		return env.f.seedRow(r)
	}

	// abilityEnv: one sentinel site whose inventory carries the sentinel in
	// an unreviewed name, its label and description, and whose agent answers
	// a read with the sentinel.
	abilityEnv := func(t *testing.T) (*railEnv, sqlc.Site) {
		env, s := railSite(t)
		s.AgentVersion = agentcmd.MinAgentVersionForAbilityEngine
		env.f.db[s.ID] = s
		label, desc := fenceSentinel+" label", fenceSentinel+" description"
		store := &fakeAbilityStore{
			run: &sqlc.SiteAbilityInventoryRun{SiteID: s.ID, CheckedAt: time.Now(), SnapshotID: uuid.New()},
			rows: []sqlc.SiteAbilityInventory{
				{SiteID: s.ID, Name: "wpmgr/site-facts", OwnerKind: "plugin"},
				{SiteID: s.ID, Name: fenceSentinel + "/tool", OwnerKind: "unknown", SiteLabel: &label, SiteDescription: &desc},
			},
			cat: []sqlc.AbilityCatalogue{catalogueRow("wpmgr/site-facts", "wpmgr", "read")},
		}
		agent := &fakeAbilityAgent{out: `{"wp_version":"` + fenceSentinel + `","active_theme":{"template":"` + fenceSentinel +
			`"},"active_plugins":["` + fenceSentinel + `"]}`}
		if err := env.svc.EnableAbilityTools(store, agent, testEntryEncoder, "s"); err != nil {
			t.Fatal(err)
		}
		return env, s
	}

	cases := map[string][]fenceCase{
		ToolSiteAbilitiesDiscover: {{"site and unreviewed name", func(t *testing.T) railCall {
			env, s := abilityEnv(t)
			return env.call(ToolSiteAbilitiesDiscover, map[string]any{"site_id": s.ID.String()})
		}}},
		ToolSiteAbilityDescribe: {{"from_the_site", func(t *testing.T) railCall {
			env, s := abilityEnv(t)
			return env.call(ToolSiteAbilityDescribe, map[string]any{"site_id": s.ID.String(), "name": fenceSentinel + "/tool"})
		}}},
		ToolSiteAbilityRun: {{"read output", func(t *testing.T) railCall {
			env, s := abilityEnv(t)
			return env.call(ToolSiteAbilityRun, map[string]any{"site_id": s.ID.String(), "name": "wpmgr/site-facts"})
		}}},
		ToolSiteAbilityRequestStatus: {{"-32003 echoing the model's request_id", func(t *testing.T) railCall {
			env, _ := abilityEnv(t)
			return env.call(ToolSiteAbilityRequestStatus, map[string]any{"request_id": fenceSentinel})
		}}},
		ToolFleetSitesList: {{"list", func(t *testing.T) railCall {
			return fleetEnv(t).call(ToolFleetSitesList, map[string]any{})
		}}},
		ToolFleetUpdatesPending: {{"list", func(t *testing.T) railCall {
			return fleetEnv(t).call(ToolFleetUpdatesPending, map[string]any{})
		}}},
		ToolSiteCachePurgeRequest: {
			{"created, scope all", func(t *testing.T) railCall {
				env, s := railSite(t)
				return env.call(ToolSiteCachePurgeRequest, allReq(s.ID))
			}},
			{"created, scope url", func(t *testing.T) railCall {
				env, s := railSite(t)
				return env.call(ToolSiteCachePurgeRequest, urlReq(s.ID, "https://"+fenceSentinel+".test/"+fenceSentinel))
			}},
			{"-32006 naming the waiting row", func(t *testing.T) railCall {
				env, s := railSite(t)
				sentinelRow(env, s.ID, sqlc.AssistantCachePurgeRequest{State: "pending"})
				return env.call(ToolSiteCachePurgeRequest, allReq(s.ID))
			}},
			{"-32003 echoing the model's url", func(t *testing.T) railCall {
				env, s := railSite(t)
				return env.call(ToolSiteCachePurgeRequest, urlReq(s.ID, "https://other.test/"+fenceSentinel))
			}},
		},
		ToolSiteCachePurgeRequestStatus: {
			{"{request_id}", func(t *testing.T) railCall {
				env, s := railSite(t)
				r := sentinelRow(env, s.ID, sqlc.AssistantCachePurgeRequest{State: "pending"})
				return env.call(ToolSiteCachePurgeRequestStatus, map[string]any{"request_id": r.ID.String()})
			}},
			{"{} list mode", func(t *testing.T) railCall {
				env, s := railSite(t)
				sentinelRow(env, s.ID, sqlc.AssistantCachePurgeRequest{State: "pending"})
				return env.call(ToolSiteCachePurgeRequestStatus, map[string]any{})
			}},
		},
	}

	for _, p := range RegistryPolicies() {
		cs, ok := cases[p.Name]
		if !ok {
			t.Errorf("registered tool %q has no case here: add one that plants the sentinel in every "+
				"site column it reads", p.Name)
			continue
		}
		delete(cases, p.Name)
		for _, c := range cs {
			t.Run(p.Name+" "+c.name, func(t *testing.T) {
				call := c.run(t)
				seen := 0
				if call.resp.Error != nil {
					var data any
					if len(call.resp.Error.Data) > 0 {
						if err := json.Unmarshal(call.resp.Error.Data, &data); err != nil {
							t.Fatalf("error data is not JSON: %v", err)
						}
					}
					if strings.Contains(call.resp.Error.Message, fenceSentinel) {
						t.Errorf("error message carries site text: %q", call.resp.Error.Message)
					}
					seen = assertSentinelFenced(t, p.Name+" error data", data)
				} else {
					prose, payload := splitResultText(t, p.Name, call.text)
					if strings.Contains(prose, fenceSentinel) {
						t.Errorf("the prose around the payload carries site text:\n%s", prose)
					}
					seen = assertSentinelFenced(t, p.Name+" result", payload)
				}
				if seen == 0 {
					t.Fatalf("no sentinel reached the answer, so this case proves nothing:\n%s", call.raw)
				}
			})
		}
	}
	for name := range cases {
		t.Errorf("a case names %q, which is not registered", name)
	}
}

// TestRequestTools_NeverReturnTheDigestNonceGrantLabelOrSetupClient: the
// values only a person's approval may see never reach the model, from any
// answer either request tool gives.
func TestRequestTools_NeverReturnTheDigestNonceGrantLabelOrSetupClient(t *testing.T) {
	secrets := map[string]string{
		"presented_digest": "digestzq0123456789abcdef",
		"digest_nonce":     "noncezq0123456789abcdef",
		"grant_label":      "grantlabelzq-laptop",
		"setup_client":     "setupclientzq-desktop",
	}
	f := newRailFake()
	s := f.addSite("https://x.test")
	env := newRailEnv(t, f, nil)
	row := f.seedRow(sqlc.AssistantCachePurgeRequest{TenantID: env.auth.TenantID, SiteID: s.ID,
		ProposedByGrantID: env.auth.GrantID, Scope: scopeAll, SiteLabel: "Shop", SiteHost: "x.test",
		State: "pending", PresentedDigest: secrets["presented_digest"], DigestNonce: secrets["digest_nonce"],
		GrantLabel: secrets["grant_label"], SetupClient: strp(secrets["setup_client"])})

	answers := map[string]railCall{
		"repeat (existing:true)": env.call(ToolSiteCachePurgeRequest, allReq(s.ID)),
		"-32006":                 env.call(ToolSiteCachePurgeRequest, urlReq(s.ID, "https://x.test/a")),
		"status":                 env.call(ToolSiteCachePurgeRequestStatus, map[string]any{"request_id": row.ID.String()}),
		"status list":            env.call(ToolSiteCachePurgeRequestStatus, map[string]any{}),
	}
	if !answers["repeat (existing:true)"].created(t).Existing {
		t.Fatal("the repeat did not read the seeded row, so its answer proves nothing")
	}
	answers["-32006"].wantCode(t, codeRateLimited)

	// A fresh creation: its own digest and nonce are generated, so they are
	// read back from the row and searched for.
	f2 := newRailFake()
	s2 := f2.addSite("https://y.test")
	env2 := newRailEnv(t, f2, nil)
	env2.auth.GrantName = secrets["grant_label"]
	answers["created"] = env2.call(ToolSiteCachePurgeRequest, allReq(s2.ID))
	created := f2.rowsFor(env2.auth.GrantID)
	if len(created) != 1 || created[0].PresentedDigest == "" || created[0].DigestNonce == "" {
		t.Fatalf("the creation stored no digest or nonce to look for: %+v", created)
	}

	for name, c := range answers {
		if c.raw == "" {
			t.Fatalf("%s: empty answer", name)
		}
		for field, v := range secrets {
			if strings.Contains(c.raw, v) {
				t.Errorf("%s returns %s (%q):\n%s", name, field, v, c.raw)
			}
		}
		for _, key := range []string{"presented_digest", "digest_nonce", "grant_label", "setup_client"} {
			if strings.Contains(c.raw, `"`+key+`"`) {
				t.Errorf("%s carries a %q key:\n%s", name, key, c.raw)
			}
		}
	}
	for _, v := range []string{created[0].PresentedDigest, created[0].DigestNonce} {
		if strings.Contains(answers["created"].raw, v) {
			t.Errorf("the created answer returns a stored secret %q:\n%s", v, answers["created"].raw)
		}
	}
}
