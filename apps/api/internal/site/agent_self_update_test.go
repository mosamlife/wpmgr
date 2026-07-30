package site

import (
	"encoding/json"
	"strings"
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

// TestAgentSelfUpdateApplyRecordIsPersistedAndReadBack covers the OTHER
// agent_self_update payload: not the plugin-inventory advisory stripped above,
// but the agent's own account of what happened on the apply beat of its own
// upgrade.
//
// The two are easy to confuse and are opposites. The inventory advisory is a
// WordPress-supplied claim that an update is AVAILABLE, and it is dropped
// because acting on it would have the agent overwrite its running code. This
// record is the agent's report of an upgrade it ALREADY attempted, and it is
// kept because it is the only evidence of a failed apply that ever reaches the
// control plane: the apply runs in a cron request with no CP response to ride
// on.
func TestAgentSelfUpdateApplyRecordIsPersistedAndReadBack(t *testing.T) {
	m := sanitizeMetadata(Metadata{
		AgentVersion: "0.61.80",
		AgentSelfUpdate: &AgentSelfUpdateResult{
			Status:      "failed",
			FromVersion: "0.61.80",
			ToVersion:   "0.62.0",
			Detail:      "Upgrader threw: could not create directory",
			At:          1785000000,
			ApplyID:     "9f1c2e3a4b5d6e7f",
		},
	})
	if m.AgentSelfUpdate == nil {
		t.Fatal("the agent's account of its apply beat was dropped at the write chokepoint")
	}

	// Round-trip through the JSONB inventory exactly as ApplyMetadata stores it.
	raw, err := json.Marshal(map[string]any{
		"plugins":           []Component{},
		"themes":            []Component{},
		"agent_self_update": m.AgentSelfUpdate,
	})
	if err != nil {
		t.Fatalf("marshal inventory: %v", err)
	}
	got := Site{ID: uuid.New(), Components: raw}.ParsedAgentSelfUpdate()
	if got == nil {
		t.Fatal("the stored record did not read back")
	}
	if got.Status != "failed" || got.ToVersion != "0.62.0" {
		t.Fatalf("record did not round-trip: %+v", got)
	}
	if got.Detail != "Upgrader threw: could not create directory" {
		t.Fatalf("the agent's own reason is the whole value of the record, got %q", got.Detail)
	}
	if got.At != 1785000000 {
		t.Fatalf("at = %d, want the agent's stamp", got.At)
	}
	// The apply id is what lets the confirmation worker tell THIS run's own
	// apply apart from an unrelated version movement (see
	// update.agentApplyAttributed). Dropping it anywhere on this path silently
	// defeats that check.
	if got.ApplyID != "9f1c2e3a4b5d6e7f" {
		t.Fatalf("apply_id did not round-trip: got %q", got.ApplyID)
	}
}

// TestAgentSelfUpdateApplyRecordIsBoundedAndOptional: the record is agent-
// supplied telemetry like everything else here, so it is bounded on arrival
// rather than trusted, and its absence is never an error.
func TestAgentSelfUpdateApplyRecordIsBoundedAndOptional(t *testing.T) {
	t.Run("absent stays absent", func(t *testing.T) {
		if got := sanitizeMetadata(Metadata{AgentVersion: "0.61.80"}).AgentSelfUpdate; got != nil {
			t.Fatalf("want no record for an agent that sent none, got %+v", got)
		}
	})

	t.Run("a record with no status says nothing and is dropped", func(t *testing.T) {
		got := sanitizeMetadata(Metadata{
			AgentSelfUpdate: &AgentSelfUpdateResult{Status: "   ", ToVersion: "0.62.0"},
		}).AgentSelfUpdate
		if got != nil {
			t.Fatalf("want the empty shell dropped, got %+v", got)
		}
	})

	t.Run("oversized fields are truncated, not rejected", func(t *testing.T) {
		got := sanitizeMetadata(Metadata{
			AgentSelfUpdate: &AgentSelfUpdateResult{
				Status:      strings.Repeat("s", 200),
				FromVersion: strings.Repeat("1", 200),
				ToVersion:   strings.Repeat("2", 200),
				Detail:      strings.Repeat("d", 4000),
				At:          -1,
				ApplyID:     strings.Repeat("a", 200),
			},
		}).AgentSelfUpdate
		if got == nil {
			t.Fatal("an oversized record must be bounded, never dropped: it may be the only account of a failure")
		}
		if len(got.Status) != maxSelfUpdateStatus {
			t.Fatalf("status len = %d, want %d", len(got.Status), maxSelfUpdateStatus)
		}
		if len(got.Detail) != maxSelfUpdateDetail {
			t.Fatalf("detail len = %d, want %d", len(got.Detail), maxSelfUpdateDetail)
		}
		if len(got.FromVersion) != maxVersionLen || len(got.ToVersion) != maxVersionLen {
			t.Fatalf("versions not bounded: %+v", got)
		}
		if got.At != 0 {
			t.Fatalf("a negative timestamp must normalize to unset, got %d", got.At)
		}
		if len(got.ApplyID) != maxSelfUpdateApplyID {
			t.Fatalf("apply_id len = %d, want %d", len(got.ApplyID), maxSelfUpdateApplyID)
		}
	})

	t.Run("a site with no inventory reads back nothing", func(t *testing.T) {
		for name, s := range map[string]Site{
			"empty":       {},
			"malformed":   {Components: []byte("{")},
			"no such key": {Components: []byte(`{"plugins":[],"themes":[]}`)},
			"no status":   {Components: []byte(`{"agent_self_update":{"to_version":"0.62.0"}}`)},
		} {
			if got := s.ParsedAgentSelfUpdate(); got != nil {
				t.Fatalf("%s: want nil, got %+v", name, got)
			}
		}
	})
}
