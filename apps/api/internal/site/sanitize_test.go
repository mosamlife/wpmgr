package site

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestTruncateRunes(t *testing.T) {
	// Multi-byte rune must not be split: "é" is 2 bytes, 1 rune.
	if got := truncateRunes("éé", 1); got != "é" {
		t.Fatalf("truncateRunes(\"éé\", 1) = %q, want %q", got, "é")
	}
	if got := truncateRunes("abc", 10); got != "abc" {
		t.Fatalf("no truncation expected, got %q", got)
	}
	if got := truncateRunes("abc", 0); got != "" {
		t.Fatalf("truncate to 0 = %q, want empty", got)
	}
	// Result must remain valid UTF-8 (no broken sequences).
	long := strings.Repeat("世", 100)
	got := truncateRunes(long, 10)
	if []rune(got)[0] != '世' || len([]rune(got)) != 10 {
		t.Fatalf("rune-boundary truncation broken: %q", got)
	}
}

func TestSanitizeMetadataTruncatesScalars(t *testing.T) {
	m := sanitizeMetadata(Metadata{
		WPVersion:   strings.Repeat("a", 100),
		PHPVersion:  strings.Repeat("b", 100),
		ServerInfo:  strings.Repeat("c", 1000),
		ActiveTheme: strings.Repeat("d", 1000),
	})
	if len(m.WPVersion) != maxWPVersion {
		t.Fatalf("WPVersion len = %d, want %d", len(m.WPVersion), maxWPVersion)
	}
	if len(m.PHPVersion) != maxPHPVersion {
		t.Fatalf("PHPVersion len = %d, want %d", len(m.PHPVersion), maxPHPVersion)
	}
	if len(m.ServerInfo) != maxServerInfo {
		t.Fatalf("ServerInfo len = %d, want %d", len(m.ServerInfo), maxServerInfo)
	}
	if len(m.ActiveTheme) != maxActiveTheme {
		t.Fatalf("ActiveTheme len = %d, want %d", len(m.ActiveTheme), maxActiveTheme)
	}
}

func TestSanitizeMetadataComponents(t *testing.T) {
	m := sanitizeMetadata(Metadata{
		Plugins: []Component{
			{Slug: "akismet/akismet.php", Name: strings.Repeat("N", 500), Version: strings.Repeat("9", 100)},
			{Slug: "   ", Name: "empty-slug-drop-in"},     // dropped: empty after trim
			{Slug: "  hello/hello.php  ", Version: "1.0"}, // slug trimmed
		},
	})
	if len(m.Plugins) != 2 {
		t.Fatalf("expected 2 plugins after dropping empty-slug, got %d", len(m.Plugins))
	}
	if got := len([]rune(m.Plugins[0].Name)); got != maxComponentLen {
		t.Fatalf("name not truncated: %d runes", got)
	}
	if got := len([]rune(m.Plugins[0].Version)); got != maxVersionLen {
		t.Fatalf("version not truncated: %d runes", got)
	}
	if m.Plugins[1].Slug != "hello/hello.php" {
		t.Fatalf("slug not trimmed: %q", m.Plugins[1].Slug)
	}
}

func TestSanitizeMetadataCapsSliceSizes(t *testing.T) {
	plugins := make([]Component, 6000)
	for i := range plugins {
		plugins[i] = Component{Slug: "p", Version: "1"}
	}
	themes := make([]Component, 2000)
	for i := range themes {
		themes[i] = Component{Slug: "t", Version: "1"}
	}
	m := sanitizeMetadata(Metadata{Plugins: plugins, Themes: themes})
	if len(m.Plugins) != maxPlugins {
		t.Fatalf("plugins not capped: %d, want %d", len(m.Plugins), maxPlugins)
	}
	if len(m.Themes) != maxThemes {
		t.Fatalf("themes not capped: %d, want %d", len(m.Themes), maxThemes)
	}
}

// captureRepo records what UpdateMetadata persisted so the service test can
// assert the sanitized struct reached the repo.
type captureRepo struct {
	fakeRepo
	gotMeta       Metadata
	gotComponents []byte
}

func (r *captureRepo) UpdateMetadata(_ context.Context, tenantID, siteID uuid.UUID, m Metadata, components []byte) (Site, error) {
	r.gotMeta = m
	r.gotComponents = components
	return Site{ID: siteID, TenantID: tenantID}, nil
}

// TestApplyMetadataNeverRejectsOverLimitPayload proves the previously-422ing
// payload (100-char version, 6000 plugins, empty-slug entry) now succeeds and
// persists a sanitized struct.
func TestApplyMetadataNeverRejectsOverLimitPayload(t *testing.T) {
	repo := &captureRepo{}
	svc := newSvc(repo)

	plugins := make([]Component, 6000)
	for i := range plugins {
		plugins[i] = Component{Slug: "plugin", Version: "1.0"}
	}
	plugins[0] = Component{Slug: "woocommerce/woocommerce.php", Name: "WooCommerce", Version: strings.Repeat("9", 100)}
	plugins[1] = Component{Slug: "", Name: "drop-in.php"} // empty-slug drop-in

	tenant, siteID := uuid.New(), uuid.New()
	out, err := svc.ApplyMetadata(context.Background(), tenant, siteID, Metadata{
		WPVersion: strings.Repeat("x", 100),
		Plugins:   plugins,
	})
	if err != nil {
		t.Fatalf("ApplyMetadata returned error for over-limit payload: %v", err)
	}
	if out.ID != siteID {
		t.Fatalf("unexpected site returned")
	}
	if len(repo.gotMeta.WPVersion) != maxWPVersion {
		t.Fatalf("persisted WPVersion not truncated: %d", len(repo.gotMeta.WPVersion))
	}
	if len(repo.gotMeta.Plugins) != maxPlugins {
		t.Fatalf("persisted plugins not capped: %d", len(repo.gotMeta.Plugins))
	}
	if got := len([]rune(repo.gotMeta.Plugins[0].Version)); got != maxVersionLen {
		t.Fatalf("persisted version not truncated: %d", got)
	}
	for _, p := range repo.gotMeta.Plugins {
		if strings.TrimSpace(p.Slug) == "" {
			t.Fatalf("empty-slug component persisted")
		}
	}
	// Persisted JSONB must be well-formed.
	var comp map[string]any
	if err := json.Unmarshal(repo.gotComponents, &comp); err != nil {
		t.Fatalf("components JSON invalid: %v", err)
	}
}

func TestApplyMetadataHappyPath(t *testing.T) {
	repo := &captureRepo{}
	svc := newSvc(repo)
	_, err := svc.ApplyMetadata(context.Background(), uuid.New(), uuid.New(), Metadata{
		WPVersion:  "6.5.2",
		PHPVersion: "8.3.0",
		ServerInfo: "nginx/1.25.3",
		Multisite:  true,
		Plugins:    []Component{{Slug: "akismet/akismet.php", Name: "Akismet", Version: "5.3", Active: true}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repo.gotMeta.WPVersion != "6.5.2" || !repo.gotMeta.Multisite {
		t.Fatalf("metadata not persisted as-is: %+v", repo.gotMeta)
	}
}

// TestApplyMetadataDropsSameVersionAvailableUpdate proves GH #211: an
// AvailableUpdate whose new_version normalizes equal to the component's own
// installed Version (e.g. Kadence's observed "1.5.1 -> 1.5.1") is never
// persisted — sanitizeComponents is the universal write chokepoint for
// agent-pushed metadata, so this covers every write path (ApplyAgentMetadata,
// the recheck handler, etc). The component itself still survives (only the
// phantom advisory is dropped), and a legitimate newer advisory on a sibling
// component in the same payload is unaffected. A same-version CoreUpdate
// advisory is dropped the same way (sanitizeCoreUpdate).
func TestApplyMetadataDropsSameVersionAvailableUpdate(t *testing.T) {
	repo := &captureRepo{}
	svc := newSvc(repo)

	_, err := svc.ApplyMetadata(context.Background(), uuid.New(), uuid.New(), Metadata{
		Plugins: []Component{
			{Slug: "kadence", Name: "Kadence", Version: "1.5.1", Active: true,
				AvailableUpdate: &AvailableUpdate{NewVersion: "1.5.1"}}, // GH #211 phantom
			{Slug: "wp-rocket", Name: "WP Rocket", Version: "3.16.1", Active: true,
				AvailableUpdate: &AvailableUpdate{NewVersion: "3.16.2"}}, // legitimate
		},
		CoreUpdate: &CoreUpdate{NewVersion: "6.4.3", CurrentVersion: "6.4.3"}, // phantom core advisory
	})
	if err != nil {
		t.Fatalf("ApplyMetadata: %v", err)
	}
	if len(repo.gotMeta.Plugins) != 2 {
		t.Fatalf("both components must survive (only the advisory is dropped): got %d", len(repo.gotMeta.Plugins))
	}
	if repo.gotMeta.Plugins[0].Slug != "kadence" || repo.gotMeta.Plugins[0].AvailableUpdate != nil {
		t.Fatalf("same-version advisory must not be persisted: %+v", repo.gotMeta.Plugins[0])
	}
	if repo.gotMeta.Plugins[1].Slug != "wp-rocket" || repo.gotMeta.Plugins[1].AvailableUpdate == nil ||
		repo.gotMeta.Plugins[1].AvailableUpdate.NewVersion != "3.16.2" {
		t.Fatalf("legitimate newer advisory must still be persisted: %+v", repo.gotMeta.Plugins[1])
	}
	if repo.gotMeta.CoreUpdate != nil {
		t.Fatalf("same-version core-update advisory must not be persisted: %+v", repo.gotMeta.CoreUpdate)
	}

	// The persisted JSONB itself must not carry the phantom advisory either.
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
	if got.Plugins[0].AvailableUpdate != nil {
		t.Fatalf("persisted JSONB must not carry the phantom advisory: %+v", got.Plugins[0])
	}
	if got.CoreUpdate != nil {
		t.Fatalf("persisted JSONB must not carry the phantom core advisory: %+v", got.CoreUpdate)
	}
}

// TestApplyMetadataKeepsAvailableUpdateWhenInstalledVersionEmpty proves the
// fail-open rule: a component reporting an empty installed Version keeps its
// advisory (never suppressed) — wpversion.SameVersion returns false whenever
// either side is empty, so an unknown/unreported installed version can never
// hide a real update.
func TestApplyMetadataKeepsAvailableUpdateWhenInstalledVersionEmpty(t *testing.T) {
	repo := &captureRepo{}
	svc := newSvc(repo)

	_, err := svc.ApplyMetadata(context.Background(), uuid.New(), uuid.New(), Metadata{
		Plugins: []Component{
			{Slug: "mystery", Version: "", Active: true,
				AvailableUpdate: &AvailableUpdate{NewVersion: "2.0"}},
		},
	})
	if err != nil {
		t.Fatalf("ApplyMetadata: %v", err)
	}
	if len(repo.gotMeta.Plugins) != 1 || repo.gotMeta.Plugins[0].AvailableUpdate == nil ||
		repo.gotMeta.Plugins[0].AvailableUpdate.NewVersion != "2.0" {
		t.Fatalf("fail-open: advisory on an empty-installed-version component must be kept: %+v", repo.gotMeta.Plugins)
	}
}
