package site

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
)

// TestBuildAvailableUpdatesFiltersAndSorts proves the per-site available-
// updates projection:
//   - filters to Components whose AvailableUpdate != nil (no NewVersion ⇒ drop)
//   - sorts plugins before themes; within each kind, active before inactive,
//     ties broken by slug
//   - attaches the optional CoreUpdate from the JSONB inventory
//   - stamps as_of from sites.components_updated_at (GH #553)
//   - GH #211: drops a same-version phantom advisory (available_update.new_version
//     == the component's own Version) from both Items and the item count, while
//     a legitimate newer advisory still surfaces, and an advisory on a component
//     with an EMPTY installed Version is kept (fail open — see wpversion.SameVersion)
func TestBuildAvailableUpdatesFiltersAndSorts(t *testing.T) {
	updatedAt := time.Date(2026, 5, 28, 10, 0, 0, 0, time.UTC)
	siteID := uuid.New()
	inventory := map[string]any{
		"plugins": []map[string]any{
			{"slug": "no-update", "name": "No Update", "version": "1.0", "active": true},
			{"slug": "wp-rocket", "name": "WP Rocket", "version": "3.16.1", "active": true,
				"available_update": map[string]any{"new_version": "3.16.2", "package": "https://example.com/wp-rocket.zip"}},
			{"slug": "akismet", "name": "Akismet", "version": "5.3.1", "active": false,
				"available_update": map[string]any{"new_version": "5.3.2"}},
			{"slug": "zoo", "name": "Zoo", "version": "1.0", "active": true,
				"available_update": map[string]any{"new_version": "1.1"}},
			// GH #211 phantom: new_version equals the installed Version
			// (observed with Kadence, e.g. "1.5.1 -> 1.5.1") — must be dropped.
			{"slug": "kadence", "name": "Kadence", "version": "1.5.1", "active": true,
				"available_update": map[string]any{"new_version": "1.5.1"}},
			// A "v"-prefixed phantom must also normalize-equal and drop.
			{"slug": "v-prefixed", "name": "V Prefixed", "version": "2.0.0", "active": true,
				"available_update": map[string]any{"new_version": "v2.0.0"}},
			// Fail-open: an empty installed Version must never suppress a
			// reported advisory (wpversion.SameVersion returns false when
			// either side is empty).
			{"slug": "zz-mystery", "name": "Mystery Plugin", "version": "", "active": true,
				"available_update": map[string]any{"new_version": "2.0"}},
		},
		"themes": []map[string]any{
			{"slug": "twentytwentyfour", "name": "Twenty Twenty-Four", "version": "1.0", "active": true,
				"available_update": map[string]any{"new_version": "1.1", "tested": "6.5", "requires_php": "7.4"}},
			{"slug": "old-theme", "name": "Old Theme", "version": "1.0", "active": false,
				"available_update": map[string]any{"new_version": "1.1"}},
		},
		"core_update": map[string]any{"new_version": "6.5.2", "current_version": "6.4.3"},
	}
	raw, err := json.Marshal(inventory)
	if err != nil {
		t.Fatalf("marshal inventory: %v", err)
	}
	// UpdatedAt is deliberately different from ComponentsUpdatedAt so this test
	// would catch a regression to the GH #553 fallback: as_of must come from
	// the latter, never the former.
	heartbeatAt := updatedAt.Add(5 * time.Minute)
	s := Site{ID: siteID, Components: raw, UpdatedAt: heartbeatAt, ComponentsUpdatedAt: &updatedAt}

	out := buildAvailableUpdates(s)
	if out.SiteID != siteID {
		t.Fatalf("SiteID mismatch: %v vs %v", out.SiteID, siteID)
	}
	if !out.AsOf.Set || out.AsOf.IsNull() || !out.AsOf.Value.Equal(updatedAt) {
		t.Fatalf("as_of not stamped: %+v", out.AsOf)
	}
	if !out.CoreUpdate.Set || out.CoreUpdate.IsNull() {
		t.Fatalf("core_update not surfaced: %+v", out.CoreUpdate)
	}
	if out.CoreUpdate.Value.NewVersion != "6.5.2" || out.CoreUpdate.Value.CurrentVersion != "6.4.3" {
		t.Fatalf("core_update wrong: %+v", out.CoreUpdate.Value)
	}
	// 4 plugins (no-update, kadence phantom, v-prefixed phantom dropped) + 2
	// themes = 6 items.
	if got := len(out.Items); got != 6 {
		t.Fatalf("filtered item count = %d, want 6: %+v", got, out.Items)
	}
	for _, it := range out.Items {
		if it.Slug == "kadence" {
			t.Fatalf("GH #211 phantom (same-version) advisory must be dropped: %+v", it)
		}
		if it.Slug == "v-prefixed" {
			t.Fatalf("GH #211 phantom (v-prefixed same-version) advisory must be dropped: %+v", it)
		}
	}
	// Expected sort: plugins (active before inactive, slug-sorted within each), then themes.
	//   1. plugin/wp-rocket  (active)
	//   2. plugin/zoo        (active)
	//   3. plugin/zz-mystery (active; fail-open empty-installed-version kept)
	//   4. plugin/akismet    (inactive)
	//   5. theme/twentytwentyfour (active)
	//   6. theme/old-theme   (inactive)
	want := []struct {
		typ  gen.SiteAvailableUpdatesItemsItemType
		slug string
	}{
		{gen.SiteAvailableUpdatesItemsItemTypePlugin, "wp-rocket"},
		{gen.SiteAvailableUpdatesItemsItemTypePlugin, "zoo"},
		{gen.SiteAvailableUpdatesItemsItemTypePlugin, "zz-mystery"},
		{gen.SiteAvailableUpdatesItemsItemTypePlugin, "akismet"},
		{gen.SiteAvailableUpdatesItemsItemTypeTheme, "twentytwentyfour"},
		{gen.SiteAvailableUpdatesItemsItemTypeTheme, "old-theme"},
	}
	for i, w := range want {
		got := out.Items[i]
		if got.Type != w.typ || got.Slug != w.slug {
			t.Fatalf("sort[%d] = (%s,%s), want (%s,%s)", i, got.Type, got.Slug, w.typ, w.slug)
		}
	}
	// WP Rocket carries the package URL; check the OptNilString round-trip. A
	// legitimate newer advisory must still surface despite the new same-version
	// guard.
	if !out.Items[0].Package.Set || out.Items[0].Package.Value != "https://example.com/wp-rocket.zip" {
		t.Fatalf("package URL not surfaced: %+v", out.Items[0].Package)
	}
	// zz-mystery: fail-open (empty installed Version) kept its advisory.
	if out.Items[2].Slug != "zz-mystery" || out.Items[2].NewVersion != "2.0" {
		t.Fatalf("fail-open (empty installed version) advisory not kept: %+v", out.Items[2])
	}
	// twentytwentyfour carries tested + requires_php.
	tt := out.Items[4]
	if !tt.Tested.Set || tt.Tested.Value != "6.5" || !tt.RequiresPhp.Set || tt.RequiresPhp.Value != "7.4" {
		t.Fatalf("optional advisory fields lost: %+v", tt)
	}
}

// TestBuildAvailableUpdatesEmptyInventory proves the projection cleanly handles
// a site with no recorded inventory (e.g. brand-new site, agent not yet synced).
func TestBuildAvailableUpdatesEmptyInventory(t *testing.T) {
	siteID := uuid.New()
	out := buildAvailableUpdates(Site{ID: siteID})
	if out.SiteID != siteID {
		t.Fatalf("SiteID mismatch")
	}
	if len(out.Items) != 0 {
		t.Fatalf("expected empty items, got %d", len(out.Items))
	}
	if out.CoreUpdate.Set && !out.CoreUpdate.IsNull() {
		t.Fatalf("core_update should be unset on empty inventory: %+v", out.CoreUpdate)
	}
}

// GH #553 — as_of must report when the inventory was collected
// (components_updated_at), never when the site last said hello (updated_at,
// bumped every 60s by TouchSiteHeartbeat regardless of whether components
// ever changes). Three states, proved individually:

// TestBuildAvailableUpdatesAsOfFreshInventory: components_updated_at is set,
// so as_of must carry it.
func TestBuildAvailableUpdatesAsOfFreshInventory(t *testing.T) {
	siteID := uuid.New()
	collectedAt := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	s := Site{ID: siteID, ComponentsUpdatedAt: &collectedAt}

	out := buildAvailableUpdates(s)

	if !out.AsOf.Set || out.AsOf.IsNull() {
		t.Fatalf("as_of must be a set, non-null value when the inventory has been collected: %+v", out.AsOf)
	}
	if !out.AsOf.Value.Equal(collectedAt) {
		t.Fatalf("as_of = %v, want components_updated_at %v", out.AsOf.Value, collectedAt)
	}
}

// TestBuildAvailableUpdatesAsOfNeverCollected: components_updated_at is NULL
// (every pre-m121 row, and any site whose agent has never pushed metadata)
// while updated_at is fresh, because the connection heartbeat keeps bumping it
// on its own 60s cycle. as_of must surface the absence explicitly — not a zero
// time, not an omitted field, and above all not the heartbeat's timestamp. A
// fallback to UpdatedAt here IS the GH #553 defect, silently recreated.
func TestBuildAvailableUpdatesAsOfNeverCollected(t *testing.T) {
	siteID := uuid.New()
	heartbeatAt := time.Now().UTC() // a live site's fresh row mtime
	s := Site{ID: siteID, UpdatedAt: heartbeatAt, ComponentsUpdatedAt: nil}

	out := buildAvailableUpdates(s)

	if !out.AsOf.Set || !out.AsOf.IsNull() {
		t.Fatalf("as_of must be an explicit wire null when the inventory has never been collected: %+v", out.AsOf)
	}
	// The whole bug: the field must not carry the heartbeat time under any
	// name. IsNull() already implies a zeroed Value, but assert it by name
	// too, since this is the exact regression a UpdatedAt fallback reintroduces.
	if out.AsOf.Value.Equal(heartbeatAt) {
		t.Fatalf("as_of must not be sites.updated_at (the heartbeat time): got %v", out.AsOf.Value)
	}
}

// TestBuildAvailableUpdatesAsOfStaleInventory is the failure scenario from GH
// #553 itself: components_updated_at is old (the agent's WP-Cron died months
// ago) while updated_at is seconds old (the connection heartbeat never
// stopped). as_of must report the old collection time, not the fresh
// heartbeat — the case a fallback to UpdatedAt would silently get wrong.
func TestBuildAvailableUpdatesAsOfStaleInventory(t *testing.T) {
	siteID := uuid.New()
	staleCollectedAt := time.Now().UTC().AddDate(0, -3, 0) // 3 months stale
	freshHeartbeatAt := time.Now().UTC()                   // seconds old
	s := Site{ID: siteID, UpdatedAt: freshHeartbeatAt, ComponentsUpdatedAt: &staleCollectedAt}

	out := buildAvailableUpdates(s)

	if !out.AsOf.Set || out.AsOf.IsNull() {
		t.Fatalf("as_of must be a set, non-null value for a stale-but-collected inventory: %+v", out.AsOf)
	}
	if !out.AsOf.Value.Equal(staleCollectedAt) {
		t.Fatalf("as_of = %v, want the stale components_updated_at %v", out.AsOf.Value, staleCollectedAt)
	}
	if out.AsOf.Value.Equal(freshHeartbeatAt) {
		t.Fatalf("as_of must not report the fresh heartbeat time (updated_at) for a stale inventory")
	}
}

// TestApplyMetadataPersistsAvailableUpdate proves the new advisory fields
// round-trip through ApplyMetadata into the JSONB inventory and back through
// the gen.Site projection. Track C uses this end-to-end shape.
func TestApplyMetadataPersistsAvailableUpdate(t *testing.T) {
	repo := &captureRepo{}
	svc := newSvc(repo)

	tenantID, siteID := uuid.New(), uuid.New()
	in := Metadata{
		WPVersion: "6.4.3",
		Plugins: []Component{
			{Slug: "wp-rocket", Name: "WP Rocket", Version: "3.16.1", Active: true,
				AvailableUpdate: &AvailableUpdate{NewVersion: "3.16.2", Package: "https://example.com/p.zip", Tested: "6.5", RequiresPHP: "7.4"}},
		},
		Themes:     []Component{{Slug: "tt4", Name: "TT4", Version: "1.0", Active: true}},
		CoreUpdate: &CoreUpdate{NewVersion: "6.5.2", CurrentVersion: "6.4.3"},
	}
	if _, err := svc.ApplyMetadata(context.Background(), tenantID, siteID, in); err != nil {
		t.Fatalf("ApplyMetadata: %v", err)
	}
	// The persisted JSONB must carry available_update + core_update verbatim.
	var got struct {
		Plugins []struct {
			Slug            string           `json:"slug"`
			AvailableUpdate *AvailableUpdate `json:"available_update"`
		} `json:"plugins"`
		CoreUpdate *CoreUpdate `json:"core_update"`
	}
	if err := json.Unmarshal(repo.gotComponents, &got); err != nil {
		t.Fatalf("components JSON invalid: %v", err)
	}
	if len(got.Plugins) != 1 || got.Plugins[0].AvailableUpdate == nil ||
		got.Plugins[0].AvailableUpdate.NewVersion != "3.16.2" {
		t.Fatalf("AvailableUpdate not persisted: %+v", got.Plugins)
	}
	if got.CoreUpdate == nil || got.CoreUpdate.NewVersion != "6.5.2" {
		t.Fatalf("CoreUpdate not persisted: %+v", got.CoreUpdate)
	}
}

// TestSanitizeMetadataDropsEmptyCoreUpdate proves a CoreUpdate whose new_version
// is empty (after trimming) is dropped to nil so the persisted JSONB does not
// carry an empty advisory.
func TestSanitizeMetadataDropsEmptyCoreUpdate(t *testing.T) {
	m := sanitizeMetadata(Metadata{
		CoreUpdate: &CoreUpdate{NewVersion: "   ", CurrentVersion: "6.4.3"},
	})
	if m.CoreUpdate != nil {
		t.Fatalf("empty CoreUpdate must be dropped, got %+v", m.CoreUpdate)
	}
}

// TestSanitizeMetadataDropsEmptyAvailableUpdate proves a Component carrying an
// AvailableUpdate with empty new_version yields a Component without the
// advisory after sanitize.
func TestSanitizeMetadataDropsEmptyAvailableUpdate(t *testing.T) {
	m := sanitizeMetadata(Metadata{
		Plugins: []Component{
			{Slug: "p", Version: "1.0", AvailableUpdate: &AvailableUpdate{NewVersion: ""}},
		},
	})
	if len(m.Plugins) != 1 {
		t.Fatalf("plugin must survive even with empty advisory, got %d", len(m.Plugins))
	}
	if m.Plugins[0].AvailableUpdate != nil {
		t.Fatalf("empty-new-version advisory must be dropped: %+v", m.Plugins[0].AvailableUpdate)
	}
}
