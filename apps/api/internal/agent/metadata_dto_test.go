package agent

import (
	"encoding/json"
	"testing"
)

// TestMetadataDTOOldShapeStillDecodes proves OLD agents (no available_update,
// no core_update) still decode cleanly: this is the load-bearing tolerance
// guarantee of Track B — control plane MUST accept telemetry from agents that
// pre-date the Updates feature.
func TestMetadataDTOOldShapeStillDecodes(t *testing.T) {
	old := []byte(`{
		"wp_version":"6.4.3",
		"php_version":"8.2",
		"active_theme":"twentytwentyfour",
		"plugins":[
			{"slug":"akismet/akismet.php","name":"Akismet","version":"5.3.1","active":true},
			{"slug":"hello.php","name":"Hello Dolly","version":"1.7.2","active":false}
		],
		"themes":[
			{"slug":"twentytwentyfour","name":"Twenty Twenty-Four","version":"1.0","active":true}
		]
	}`)
	var dto metadataDTO
	if err := json.Unmarshal(old, &dto); err != nil {
		t.Fatalf("OLD agent shape must decode without error, got %v", err)
	}
	m := dto.toMetadata()
	if m.WPVersion != "6.4.3" || m.PHPVersion != "8.2" {
		t.Fatalf("scalars not decoded: %+v", m)
	}
	if len(m.Plugins) != 2 || len(m.Themes) != 1 {
		t.Fatalf("components not decoded: %+v", m)
	}
	if m.Plugins[0].AvailableUpdate != nil {
		t.Fatalf("OLD shape MUST yield nil AvailableUpdate; got %+v", m.Plugins[0].AvailableUpdate)
	}
	if m.CoreUpdate != nil {
		t.Fatalf("OLD shape MUST yield nil CoreUpdate; got %+v", m.CoreUpdate)
	}
}

// TestMetadataDTONewShapeDecodes proves the NEW Track A payload (with
// available_update + core_update) decodes into the corresponding fields.
func TestMetadataDTONewShapeDecodes(t *testing.T) {
	body := []byte(`{
		"wp_version":"6.4.3",
		"plugins":[
			{"slug":"wp-rocket","name":"WP Rocket","version":"3.16.1","active":true,
			 "available_update":{"new_version":"3.16.2","package":"https://example.com/wp-rocket.zip","tested":"6.5","requires_php":"7.4"}},
			{"slug":"akismet","name":"Akismet","version":"5.3.1","active":true,"available_update":null}
		],
		"themes":[
			{"slug":"twentytwentyfour","version":"1.0","active":true,
			 "available_update":{"new_version":"1.1"}}
		],
		"core_update":{"new_version":"6.5.2","current_version":"6.4.3"}
	}`)
	var dto metadataDTO
	if err := json.Unmarshal(body, &dto); err != nil {
		t.Fatalf("NEW agent shape must decode without error, got %v", err)
	}
	m := dto.toMetadata()
	if len(m.Plugins) != 2 {
		t.Fatalf("plugins decoded wrong: %+v", m.Plugins)
	}
	if m.Plugins[0].AvailableUpdate == nil || m.Plugins[0].AvailableUpdate.NewVersion != "3.16.2" {
		t.Fatalf("plugin AvailableUpdate not decoded: %+v", m.Plugins[0].AvailableUpdate)
	}
	if m.Plugins[0].AvailableUpdate.Tested != "6.5" || m.Plugins[0].AvailableUpdate.RequiresPHP != "7.4" {
		t.Fatalf("optional advisory fields not decoded: %+v", m.Plugins[0].AvailableUpdate)
	}
	if m.Plugins[1].AvailableUpdate != nil {
		t.Fatalf("explicit null available_update must yield nil; got %+v", m.Plugins[1].AvailableUpdate)
	}
	if len(m.Themes) != 1 || m.Themes[0].AvailableUpdate == nil || m.Themes[0].AvailableUpdate.NewVersion != "1.1" {
		t.Fatalf("theme AvailableUpdate not decoded: %+v", m.Themes)
	}
	if m.CoreUpdate == nil || m.CoreUpdate.NewVersion != "6.5.2" || m.CoreUpdate.CurrentVersion != "6.4.3" {
		t.Fatalf("core_update not decoded: %+v", m.CoreUpdate)
	}
}

// TestAvailableUpdateDTOJSONRoundtrip verifies the wire-level JSON tags for the
// per-item advisory: clients on Track C consume these.
func TestAvailableUpdateDTOJSONRoundtrip(t *testing.T) {
	in := availableUpdateDTO{NewVersion: "1.2.3"}
	pkg := flexString("https://example.com/x.zip")
	tested := flexString("6.5")
	requires := flexString("7.4")
	in.Package = &pkg
	in.Tested = &tested
	in.RequiresPHP = &requires
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out availableUpdateDTO
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(out.NewVersion) != "1.2.3" || out.Package == nil || string(*out.Package) != "https://example.com/x.zip" {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
	if out.Tested == nil || string(*out.Tested) != "6.5" || out.RequiresPHP == nil || string(*out.RequiresPHP) != "7.4" {
		t.Fatalf("optional fields lost on round-trip: %+v", out)
	}
}

// TestCoreUpdateDTOJSONRoundtrip verifies the core update wire tags.
func TestCoreUpdateDTOJSONRoundtrip(t *testing.T) {
	in := coreUpdateDTO{NewVersion: "6.5.2", CurrentVersion: "6.4.3"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out coreUpdateDTO
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(out.NewVersion) != "6.5.2" || string(out.CurrentVersion) != "6.4.3" {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
}

// TestMetadataDTOTolerantOnFlexFields proves the new optional fields don't
// regress the existing flexString/flexBool tolerance: a numeric tested field,
// a stringified bool active, and a missing requires_php must all be accepted.
func TestMetadataDTOTolerantOnFlexFields(t *testing.T) {
	body := []byte(`{
		"plugins":[{"slug":"p","name":"P","version":"1","active":"true",
			"available_update":{"new_version":"2","tested":6.5}}]
	}`)
	var dto metadataDTO
	if err := json.Unmarshal(body, &dto); err != nil {
		t.Fatalf("tolerant decode failed: %v", err)
	}
	m := dto.toMetadata()
	if len(m.Plugins) != 1 || !m.Plugins[0].Active {
		t.Fatalf("flexBool active not coerced from string: %+v", m.Plugins[0])
	}
	if m.Plugins[0].AvailableUpdate == nil || m.Plugins[0].AvailableUpdate.Tested != "6.5" {
		t.Fatalf("flexString tested not coerced from number: %+v", m.Plugins[0].AvailableUpdate)
	}
}

// TestMetadataDTODropsSameVersionAvailableUpdate proves GH #211's first CP
// checkpoint: toMetadata drops a component's available_update (and the
// top-level core_update) when new_version normalizes equal to the reported
// installed version — the WordPress update-transient quirk observed with
// Kadence ("1.5.1 -> 1.5.1"). A legitimate newer advisory on a sibling
// component in the same payload must still decode.
func TestMetadataDTODropsSameVersionAvailableUpdate(t *testing.T) {
	body := []byte(`{
		"wp_version":"6.4.3",
		"plugins":[
			{"slug":"kadence","name":"Kadence","version":"1.5.1","active":true,
			 "available_update":{"new_version":"1.5.1"}},
			{"slug":"wp-rocket","name":"WP Rocket","version":"3.16.1","active":true,
			 "available_update":{"new_version":"3.16.2"}}
		],
		"core_update":{"new_version":"6.4.3","current_version":"6.4.3"}
	}`)
	var dto metadataDTO
	if err := json.Unmarshal(body, &dto); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	m := dto.toMetadata()
	if len(m.Plugins) != 2 {
		t.Fatalf("both components must survive (only the advisory is dropped): %+v", m.Plugins)
	}
	if m.Plugins[0].Slug != "kadence" || m.Plugins[0].AvailableUpdate != nil {
		t.Fatalf("same-version phantom advisory must be dropped: %+v", m.Plugins[0])
	}
	if m.Plugins[1].Slug != "wp-rocket" || m.Plugins[1].AvailableUpdate == nil ||
		m.Plugins[1].AvailableUpdate.NewVersion != "3.16.2" {
		t.Fatalf("legitimate newer advisory must still decode: %+v", m.Plugins[1])
	}
	if m.CoreUpdate != nil {
		t.Fatalf("same-version phantom core_update must be dropped: %+v", m.CoreUpdate)
	}
}

// TestMetadataDTODecodesTheAgentSelfUpdateAdvisory pins the wire shape of the
// agent's own account of its last self-update apply beat, exactly as
// class-metadata-command.php builds it.
//
// The apply runs in a cron request that has no control-plane response to ride
// on, so this advisory is the ONLY way a failed or expired apply is ever heard
// about. It rides the ordinary metadata push, which means it has to decode as
// tolerantly as everything else here and stay absent for every agent that
// predates it.
func TestMetadataDTODecodesTheAgentSelfUpdateAdvisory(t *testing.T) {
	body := []byte(`{
		"wp_version":"6.4.3",
		"agent_version":"0.61.80",
		"plugins":[],
		"themes":[],
		"agent_self_update":{
			"status":"failed",
			"from_version":"0.61.80",
			"to_version":"0.62.0",
			"detail":"Upgrader threw: could not create directory",
			"at":1785000000,
			"apply_id":"9f1c2e3a4b5d6e7f"
		}
	}`)
	var dto metadataDTO
	if err := json.Unmarshal(body, &dto); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	m := dto.toMetadata()
	if m.AgentSelfUpdate == nil {
		t.Fatal("the agent's account of its apply beat was dropped")
	}
	got := *m.AgentSelfUpdate
	if got.Status != "failed" || got.FromVersion != "0.61.80" || got.ToVersion != "0.62.0" {
		t.Fatalf("advisory not decoded: %+v", got)
	}
	if got.Detail != "Upgrader threw: could not create directory" {
		t.Fatalf("the agent's own reason is the whole value of the record, got %q", got.Detail)
	}
	if got.At != 1785000000 {
		t.Fatalf("at = %d, want the agent's stamp", got.At)
	}
	if got.ApplyID != "9f1c2e3a4b5d6e7f" {
		t.Fatalf("apply_id = %q: dropping this field silently defeats every attribution check downstream", got.ApplyID)
	}
}

// TestMetadataDTOToleratesAnAbsentOrEmptyAgentSelfUpdateAdvisory: the record is
// additive. An agent that never staged a self-update, and every agent that
// predates the channel, send nothing, and a record with no status says nothing
// and must not be persisted as an empty shell.
func TestMetadataDTOToleratesAnAbsentOrEmptyAgentSelfUpdateAdvisory(t *testing.T) {
	for name, body := range map[string]string{
		"absent":       `{"wp_version":"6.4.3","plugins":[],"themes":[]}`,
		"null":         `{"wp_version":"6.4.3","agent_self_update":null}`,
		"empty object": `{"wp_version":"6.4.3","agent_self_update":{}}`,
		"blank status": `{"wp_version":"6.4.3","agent_self_update":{"status":"  ","to_version":"0.62.0"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var dto metadataDTO
			if err := json.Unmarshal([]byte(body), &dto); err != nil {
				t.Fatalf("decode failed: %v", err)
			}
			m := dto.toMetadata()
			if m.AgentSelfUpdate != nil {
				t.Fatalf("want no advisory, got %+v", *m.AgentSelfUpdate)
			}
			if m.WPVersion != "6.4.3" {
				t.Fatalf("the rest of the push must still decode: %+v", m)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// GH #350 — the site's WordPress role registry
// ---------------------------------------------------------------------------

// TestMetadataDTORolesDecode proves the agent's reported role registry reaches
// the site domain with slug AND localized display name. FAILS against the
// pre-change decoder, which had no Roles field and dropped `roles` entirely.
func TestMetadataDTORolesDecode(t *testing.T) {
	body := []byte(`{
		"wp_version":"6.8",
		"roles":[
			{"slug":"administrator","name":"Amministratore"},
			{"slug":"shop_manager","name":"Gestore negozio"},
			{"slug":"customer","name":"Cliente"}
		]
	}`)
	var dto metadataDTO
	if err := json.Unmarshal(body, &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	m := dto.toMetadata()
	if len(m.Roles) != 3 {
		t.Fatalf("want 3 roles, got %d (%+v)", len(m.Roles), m.Roles)
	}
	if m.Roles[1].Slug != "shop_manager" || m.Roles[1].Name != "Gestore negozio" {
		t.Fatalf("custom role not carried through: %+v", m.Roles[1])
	}
	if m.Roles[0].Name != "Amministratore" {
		t.Fatalf("localized name must survive verbatim, got %q", m.Roles[0].Name)
	}
}

// TestMetadataDTORolesAbsentMeansUnknown proves an agent that predates role
// reporting yields NIL roles, not an empty slice. The distinction is the whole
// point: nil means "this site has not told us", which the dashboard says out
// loud instead of quietly offering only the five default roles.
func TestMetadataDTORolesAbsentMeansUnknown(t *testing.T) {
	var dto metadataDTO
	if err := json.Unmarshal([]byte(`{"wp_version":"6.8"}`), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m := dto.toMetadata(); m.Roles != nil {
		t.Fatalf("absent roles must decode as nil, got %+v", m.Roles)
	}
}

// TestMetadataDTORolesTolerateJunk proves a slugless entry is dropped (the slug
// is the only part the policy can enforce against) and a nameless one falls
// back to its slug so the dashboard always has something to render.
func TestMetadataDTORolesTolerateJunk(t *testing.T) {
	body := []byte(`{"roles":[
		{"slug":"","name":"No slug at all"},
		{"slug":"  shop_manager  ","name":"  Gestore negozio  "},
		{"slug":"nameless"}
	]}`)
	var dto metadataDTO
	if err := json.Unmarshal(body, &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	m := dto.toMetadata()
	if len(m.Roles) != 2 {
		t.Fatalf("want 2 usable roles, got %+v", m.Roles)
	}
	if m.Roles[0].Slug != "shop_manager" || m.Roles[0].Name != "Gestore negozio" {
		t.Fatalf("whitespace not trimmed: %+v", m.Roles[0])
	}
	if m.Roles[1].Slug != "nameless" || m.Roles[1].Name != "nameless" {
		t.Fatalf("nameless role must fall back to its slug: %+v", m.Roles[1])
	}
}
