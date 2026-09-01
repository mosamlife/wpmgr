package site

import (
	"encoding/json"
	"testing"
)

// The read-side inventory predicates are now exported because the MCP read
// surface consumes them: an assistant answering "which of my sites need
// updates?" must count exactly what the dashboard counts. These tests assert
// the exact verdicts, not the shapes -- a test that only checked "a bool came
// back" would pass with the predicate inverted.

// TestActionableCoreUpdate_RejectsEmptyNewVersion is the regression for the bug
// this change fixes.
//
// buildAvailableUpdates and the updates_available count both used to ask
// `core != nil && !wpversion.SameVersion(current, new)` inline.
// wpversion.SameVersion FAILS OPEN -- it returns false when either side is
// blank -- so an advisory of {"current_version":"6.5","new_version":""} made
// that expression true, and the site was reported as having a core update
// available to no version at all. The operator saw a core update row with an
// empty target, and the count included it.
//
// It is the empty side that matters here, not the equal side: the equal case
// was always handled and has its own case below.
func TestActionableCoreUpdate_RejectsEmptyNewVersion(t *testing.T) {
	cu := &CoreUpdate{CurrentVersion: "6.5", NewVersion: ""}
	if ActionableCoreUpdate(cu) {
		t.Fatalf("ActionableCoreUpdate(%+v) = true, want false: an advisory naming no "+
			"target version is not an available update, and reporting it as one puts a "+
			"core update with an empty To column in front of the operator", cu)
	}
}

func TestActionableCoreUpdate_Verdicts(t *testing.T) {
	tests := []struct {
		name string
		cu   *CoreUpdate
		want bool
	}{
		{"nil advisory is not actionable", nil, false},
		{"real upgrade", &CoreUpdate{CurrentVersion: "6.5", NewVersion: "6.7.1"}, true},
		{"same-version phantom", &CoreUpdate{CurrentVersion: "6.7.1", NewVersion: "6.7.1"}, false},
		{"empty new_version", &CoreUpdate{CurrentVersion: "6.5", NewVersion: ""}, false},
		{
			// Unknown installed version keeps the advisory: we know a version is
			// on offer and we do not know what is installed, so withholding it
			// would assert "up to date" from ignorance.
			"unknown current keeps the advisory",
			&CoreUpdate{CurrentVersion: "", NewVersion: "6.7.1"},
			true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ActionableCoreUpdate(tc.cu); got != tc.want {
				t.Fatalf("ActionableCoreUpdate(%+v) = %v, want %v", tc.cu, got, tc.want)
			}
		})
	}
}

func TestActionableUpdate_Verdicts(t *testing.T) {
	tests := []struct {
		name string
		c    Component
		want bool
	}{
		{
			"real upgrade",
			Component{Slug: "woocommerce", Version: "8.9.1",
				AvailableUpdate: &AvailableUpdate{NewVersion: "8.9.4"}},
			true,
		},
		{
			"no advisory",
			Component{Slug: "woocommerce", Version: "8.9.1"},
			false,
		},
		{
			"advisory with empty new_version",
			Component{Slug: "woocommerce", Version: "8.9.1",
				AvailableUpdate: &AvailableUpdate{NewVersion: ""}},
			false,
		},
		{
			// GH #211.
			"same-version phantom",
			Component{Slug: "woocommerce", Version: "8.9.4",
				AvailableUpdate: &AvailableUpdate{NewVersion: "8.9.4"}},
			false,
		},
		{
			// The control plane must never offer its own agent as a selectable
			// update. The component still surfaces with its installed version;
			// only the advisory is withheld.
			"the agent's own plugin is never offered",
			Component{Slug: "fleet-agent-site-manager", Name: "WPMgr Agent", Version: "2.6.1",
				AvailableUpdate: &AvailableUpdate{NewVersion: "2.9.0"}},
			false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ActionableUpdate(tc.c); got != tc.want {
				t.Fatalf("ActionableUpdate(%+v) = %v, want %v", tc.c, got, tc.want)
			}
		})
	}
}

// TestParseInventory_RawMatchesSiteMethod pins the delegation. The Site methods
// and the raw functions must be the SAME decode: two decoders over one document
// are two answers to "what is installed on this site", and the MCP read tools
// take the raw path while the dashboard takes the method path.
func TestParseInventory_RawMatchesSiteMethod(t *testing.T) {
	raw := json.RawMessage(`{
	  "plugins":[{"slug":"woocommerce","name":"WooCommerce","version":"9.2.0","active":true,
	              "available_update":{"new_version":"9.3.0"}}],
	  "themes":[{"slug":"twentytwentyfour","name":"Twenty Twenty-Four","version":"1.2","active":true}],
	  "core_update":{"current_version":"6.7.1","new_version":"6.8"}
	}`)

	s := Site{Components: raw}

	sitePlugins, siteThemes := s.ParsedComponents()
	rawPlugins, rawThemes := ParseInventoryComponents(raw)

	// Exact values, not lengths. A length assertion passes when the decoder
	// returns the wrong site's plugins.
	if len(rawPlugins) != 1 || rawPlugins[0].Slug != "woocommerce" ||
		rawPlugins[0].Version != "9.2.0" || rawPlugins[0].AvailableUpdate == nil ||
		rawPlugins[0].AvailableUpdate.NewVersion != "9.3.0" {
		t.Fatalf("ParseInventoryComponents plugins = %+v, want one woocommerce 9.2.0 -> 9.3.0", rawPlugins)
	}
	if len(rawThemes) != 1 || rawThemes[0].Slug != "twentytwentyfour" || rawThemes[0].Version != "1.2" {
		t.Fatalf("ParseInventoryComponents themes = %+v, want one twentytwentyfour 1.2", rawThemes)
	}
	if len(sitePlugins) != len(rawPlugins) || sitePlugins[0].Slug != rawPlugins[0].Slug ||
		sitePlugins[0].Version != rawPlugins[0].Version {
		t.Fatalf("Site.ParsedComponents plugins %+v disagree with ParseInventoryComponents %+v",
			sitePlugins, rawPlugins)
	}
	if len(siteThemes) != len(rawThemes) || siteThemes[0].Slug != rawThemes[0].Slug {
		t.Fatalf("Site.ParsedComponents themes %+v disagree with ParseInventoryComponents %+v",
			siteThemes, rawThemes)
	}

	core := ParseInventoryCoreUpdate(raw)
	if core == nil || core.CurrentVersion != "6.7.1" || core.NewVersion != "6.8" {
		t.Fatalf("ParseInventoryCoreUpdate = %+v, want 6.7.1 -> 6.8", core)
	}
	siteCore := s.ParsedCoreUpdate()
	if siteCore == nil || *siteCore != *core {
		t.Fatalf("Site.ParsedCoreUpdate %+v disagrees with ParseInventoryCoreUpdate %+v", siteCore, core)
	}
}

// TestParseInventory_UnreadableYieldsNothingNotAnError pins that a malformed or
// absent document decodes to nothing rather than to an error or to a partial
// guess. The caller pairs this with the staleness stamp; the decoder must never
// be the thing that implies "up to date".
func TestParseInventory_UnreadableYieldsNothingNotAnError(t *testing.T) {
	for _, raw := range [][]byte{nil, {}, []byte(`not json`), []byte(`{"plugins":`)} {
		plugins, themes := ParseInventoryComponents(raw)
		if plugins != nil || themes != nil {
			t.Fatalf("ParseInventoryComponents(%q) = (%+v, %+v), want (nil, nil)", raw, plugins, themes)
		}
		if cu := ParseInventoryCoreUpdate(raw); cu != nil {
			t.Fatalf("ParseInventoryCoreUpdate(%q) = %+v, want nil", raw, cu)
		}
	}
}
