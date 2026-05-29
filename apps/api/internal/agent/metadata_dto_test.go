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
