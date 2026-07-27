package site

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentplugin"
)

// The agent reports ITSELF in the plugin inventory it pushes, and WordPress
// advertises an available update for it like any other plugin. The control
// plane must never turn that advisory into something an operator can select:
// applying it is an in-process self-overwrite whose rollback cannot be
// delivered by the code being replaced. The agent upgrades itself over its own
// signed channel instead.
//
// The slugs below are the real inventory keys WordPress reports (the plugin
// file relative to the plugins directory), pinned literally so a rename of the
// shared constants cannot silently stop matching what a site actually sends.
const (
	agentSelfHostedKey = "wpmgr-agent/wpmgr-agent.php"
	agentDirectoryKey  = "fleet-agent-site-manager/fleet-agent-site-manager.php"
)

// TestSanitizeMetadataStripsAgentSelfUpdateAdvisory pins the write chokepoint:
// an inventory payload carrying an agent self-update advisory is persisted with
// the advisory REMOVED but the component itself intact, so the agent stays
// visible in the inventory with its installed version. Both distribution slugs
// are covered, as is a bare-directory key, while an ordinary plugin's advisory
// passes through untouched.
func TestSanitizeMetadataStripsAgentSelfUpdateAdvisory(t *testing.T) {
	m := sanitizeMetadata(Metadata{
		Plugins: []Component{
			{Slug: agentSelfHostedKey, Name: "Site Agent", Version: "0.61.88", Active: true,
				AvailableUpdate: &AvailableUpdate{NewVersion: "0.62.0", Package: "https://example.com/agent.zip"}},
			{Slug: agentDirectoryKey, Name: "Site Agent", Version: "0.61.88", Active: true,
				AvailableUpdate: &AvailableUpdate{NewVersion: "0.62.0"}},
			{Slug: "WPMGR-Agent", Name: "Site Agent (bare directory key)", Version: "0.61.88", Active: false,
				AvailableUpdate: &AvailableUpdate{NewVersion: "0.62.0"}},
			// Renamed install directory: the slug matches NEITHER shipped
			// distribution, so only the plugin-header name identifies it. A
			// host uploader that derives the folder from the release zip
			// produces exactly this shape. Without the name branch this row
			// keeps its advisory and becomes selectable, which is the whole
			// defect the name-aware match exists to close.
			{Slug: "wpmgr-agent-0.61.88/wpmgr-agent.php", Name: agentplugin.NameSelfHosted, Version: "0.61.88", Active: true,
				AvailableUpdate: &AvailableUpdate{NewVersion: "0.62.0"}},
			{Slug: "akismet/akismet.php", Name: "Akismet", Version: "5.3.1", Active: true,
				AvailableUpdate: &AvailableUpdate{NewVersion: "5.3.2"}},
			// The inverse: a third-party plugin whose name merely RESEMBLES
			// the agent's must keep its advisory. A guard that over-matches
			// here silently blocks a customer's updates.
			{Slug: "wpmgr-agent-pro/wpmgr-agent-pro.php", Name: "WPMgr Agent Pro", Version: "1.2.0", Active: true,
				AvailableUpdate: &AvailableUpdate{NewVersion: "1.3.0"}},
		},
	})

	if len(m.Plugins) != 6 {
		t.Fatalf("the agent must stay in the inventory: got %d plugins, want 6: %+v", len(m.Plugins), m.Plugins)
	}
	for i := 0; i < 4; i++ {
		got := m.Plugins[i]
		if got.AvailableUpdate != nil {
			t.Fatalf("plugin[%d] (%s): agent self-update advisory must be stripped, got %+v", i, got.Slug, got.AvailableUpdate)
		}
		if got.Version != "0.61.88" {
			t.Fatalf("plugin[%d] (%s): installed version must stay visible, got %q", i, got.Slug, got.Version)
		}
		if got.Name == "" {
			t.Fatalf("plugin[%d] (%s): component fields must be preserved", i, got.Slug)
		}
	}
	akismet := m.Plugins[4]
	if akismet.AvailableUpdate == nil || akismet.AvailableUpdate.NewVersion != "5.3.2" {
		t.Fatalf("an ordinary plugin's advisory must be untouched: %+v", akismet.AvailableUpdate)
	}
	lookalike := m.Plugins[5]
	if lookalike.AvailableUpdate == nil || lookalike.AvailableUpdate.NewVersion != "1.3.0" {
		t.Fatalf("a plugin whose name merely resembles the agent must keep its advisory: %+v", lookalike.AvailableUpdate)
	}
}

// TestAgentSelfUpdateExcludedFromReadProjections covers the read side for an
// inventory persisted BEFORE the write-path guard existed (an older agent, or a
// replayed payload): the available-updates list, the updates_available count,
// and the full components inventory must all withhold the agent's advisory,
// while the agent's component row itself keeps surfacing with its version.
func TestAgentSelfUpdateExcludedFromReadProjections(t *testing.T) {
	inventory := map[string]any{
		"plugins": []map[string]any{
			{"slug": agentSelfHostedKey, "name": "Site Agent", "version": "0.61.88", "active": true,
				"available_update": map[string]any{"new_version": "0.62.0"}},
			// Renamed install directory, identifiable only by its plugin
			// header name. This row is what a host uploader deriving the
			// folder from the release zip produces, and it exercises the
			// name branch that the slug-only fixtures above never reach.
			{"slug": "wpmgr-agent-0.61.88/wpmgr-agent.php", "name": agentplugin.NameSelfHosted, "version": "0.61.88", "active": true,
				"available_update": map[string]any{"new_version": "0.62.0"}},
			{"slug": "akismet/akismet.php", "name": "Akismet", "version": "5.3.1", "active": true,
				"available_update": map[string]any{"new_version": "5.3.2"}},
		},
		"themes": []map[string]any{
			{"slug": "twentytwentyfour", "name": "Twenty Twenty-Four", "version": "1.0", "active": true},
		},
	}
	raw, err := json.Marshal(inventory)
	if err != nil {
		t.Fatalf("marshal inventory: %v", err)
	}
	s := Site{ID: uuid.New(), URL: "https://example.com", Components: raw}

	// 1. The available-updates projection offers only the ordinary plugin.
	out := buildAvailableUpdates(s)
	if len(out.Items) != 1 {
		t.Fatalf("available updates = %d items, want 1 (the agent must not be offered): %+v", len(out.Items), out.Items)
	}
	if out.Items[0].Slug != "akismet/akismet.php" {
		t.Fatalf("wrong item offered: %+v", out.Items[0])
	}

	// 2. The derived count matches the list: an item the operator can never
	// action must not inflate "N updates available".
	api := toAPI(s)
	if !api.UpdatesAvailable.Set || api.UpdatesAvailable.Value != 1 {
		t.Fatalf("updates_available = %+v, want 1", api.UpdatesAvailable)
	}

	// 3. The full inventory still lists the agent, with its version, minus the
	// advisory.
	if !api.Components.Set {
		t.Fatal("components inventory missing from the site projection")
	}
	var agent, akismet bool
	for _, c := range api.Components.Value.Plugins {
		switch c.Slug {
		case agentSelfHostedKey:
			agent = true
			if !c.Version.Set || c.Version.Value != "0.61.88" {
				t.Fatalf("the agent's installed version must stay visible: %+v", c.Version)
			}
			if c.AvailableUpdate.Set && !c.AvailableUpdate.IsNull() {
				t.Fatalf("the agent's advisory must be withheld from the inventory: %+v", c.AvailableUpdate)
			}
		case "akismet/akismet.php":
			akismet = true
			if !c.AvailableUpdate.Set || c.AvailableUpdate.IsNull() {
				t.Fatalf("an ordinary plugin's advisory must still surface: %+v", c.AvailableUpdate)
			}
		}
	}
	if !agent {
		t.Fatal("the agent must remain listed in the components inventory")
	}
	if !akismet {
		t.Fatal("the ordinary plugin disappeared from the components inventory")
	}
}
