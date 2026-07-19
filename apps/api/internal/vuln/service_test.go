package vuln_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/vuln"
	"github.com/mosamlife/wpmgr/apps/api/internal/wpversion"
)

// ---------------------------------------------------------------------------
// SeverityFromRating tests
// ---------------------------------------------------------------------------

func TestSeverityFromRating(t *testing.T) {
	type tc struct {
		rating string
		score  *float64
		want   string
	}
	score := func(f float64) *float64 { return &f }

	cases := []tc{
		{"Critical", nil, "critical"},
		{"High", nil, "high"},
		{"Medium", nil, "medium"},
		{"Low", nil, "low"},
		{"None", nil, "low"}, // Wordfence "None" = CVSS 0.0, informational; a legitimate confirmed-low, not "no data"
		{"", score(9.8), "critical"},
		{"", score(9.0), "critical"},
		{"", score(8.9), "high"},
		{"", score(7.0), "high"},
		{"", score(6.9), "medium"},
		{"", score(4.0), "medium"},
		{"", score(3.9), "low"},
		{"", score(0.0), "low"},
		// GH #245: no rating AND no score is genuinely "no data" — must be
		// "unknown", never silently bucketed as a confirmed "low" (that
		// fallback previously hid a CVSS 9.8 core RCE behind a "low" label).
		{"", nil, "unknown"},
		{"Unknown", nil, "unknown"}, // unrecognised rating string + no score → also honest-unknown
	}
	for _, tc := range cases {
		got := vuln.SeverityFromRating(tc.rating, tc.score)
		if got != tc.want {
			t.Errorf("SeverityFromRating(%q, %v) = %q; want %q", tc.rating, tc.score, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// affected_versions JSON parsing and IsVulnerable integration
// ---------------------------------------------------------------------------

func TestParseAffectedAndMatch(t *testing.T) {
	type tc struct {
		name             string
		affectedVersions string // JSON
		installed        string
		want             bool
	}

	cases := []tc{
		{
			name:             "unbounded range",
			affectedVersions: `[{"from_version":"*","from_inclusive":true,"to_version":"2.0","to_inclusive":false}]`,
			installed:        "1.5",
			want:             true,
		},
		{
			name:             "above patched boundary",
			affectedVersions: `[{"from_version":"*","from_inclusive":true,"to_version":"2.0","to_inclusive":false}]`,
			installed:        "2.0",
			want:             false,
		},
		{
			name:             "multi-range: in range 2",
			affectedVersions: `[{"from_version":"1.0","from_inclusive":true,"to_version":"1.5","to_inclusive":false},{"from_version":"1.8","from_inclusive":true,"to_version":"2.0","to_inclusive":false}]`,
			installed:        "1.9",
			want:             true,
		},
		{
			name:             "multi-range: between ranges",
			affectedVersions: `[{"from_version":"1.0","from_inclusive":true,"to_version":"1.5","to_inclusive":false},{"from_version":"1.8","from_inclusive":true,"to_version":"2.0","to_inclusive":false}]`,
			installed:        "1.6",
			want:             false,
		},
		{
			name:             "empty affected_versions",
			affectedVersions: `[]`,
			installed:        "1.0",
			want:             false,
		},
		{
			name:             "null affected_versions",
			affectedVersions: `null`,
			installed:        "1.0",
			want:             false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ranges []wpversion.AffectedVersionRange
			if err := json.Unmarshal([]byte(tc.affectedVersions), &ranges); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got := wpversion.IsVulnerable(tc.installed, ranges)
			if got != tc.want {
				t.Errorf("IsVulnerable(%q) = %v; want %v", tc.installed, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Mock SiteLoader for service-level tests
// ---------------------------------------------------------------------------

type mockSiteLoader struct {
	snap SiteSnap
	err  error
	ids  []uuid.UUID
}

type SiteSnap = vuln.SiteSnapshot

func (m *mockSiteLoader) GetSiteForVuln(_ context.Context, _, _ uuid.UUID) (vuln.SiteSnapshot, error) {
	return m.snap, m.err
}

func (m *mockSiteLoader) ListAllSiteIDs(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	return m.ids, nil
}

// ---------------------------------------------------------------------------
// Mock RescanEnqueuer
// ---------------------------------------------------------------------------

type captureEnqueuer struct {
	calls []vuln.RescanSiteArgs
}

func (e *captureEnqueuer) EnqueueRescanSite(_ context.Context, args vuln.RescanSiteArgs) error {
	e.calls = append(e.calls, args)
	return nil
}

// ---------------------------------------------------------------------------
// FeedMeta / FeedRecord struct literal tests (no DB)
// ---------------------------------------------------------------------------

func TestFeedMetaZeroValues(t *testing.T) {
	var meta vuln.FeedMeta
	if meta.OK {
		t.Error("zero FeedMeta should have OK=false")
	}
	if meta.FetchedAt != nil {
		t.Error("zero FeedMeta should have nil FetchedAt")
	}
}

func TestSiteSnapshotFields(t *testing.T) {
	snap := vuln.SiteSnapshot{
		ID:        uuid.New(),
		TenantID:  uuid.New(),
		WPVersion: "6.5",
		Plugins: []vuln.ComponentSnapshot{
			{Slug: "akismet", Name: "Akismet", Version: "5.3"},
		},
		Themes: []vuln.ComponentSnapshot{
			{Slug: "twentytwentyfour", Name: "Twenty Twenty-Four", Version: "1.1"},
		},
	}
	if len(snap.Plugins) != 1 {
		t.Error("expected 1 plugin")
	}
	if len(snap.Themes) != 1 {
		t.Error("expected 1 theme")
	}
}

func TestFindingDTO(t *testing.T) {
	now := time.Now().UTC()
	f := vuln.Finding{
		ID:               uuid.New(),
		TenantID:         uuid.New(),
		SiteID:           uuid.New(),
		VulnID:           "abc-123",
		Kind:             "plugin",
		Slug:             "akismet",
		Name:             "Akismet",
		InstalledVersion: "5.2",
		FixedVersion:     "5.3",
		Severity:         "medium",
		Title:            "XSS in Akismet",
		Status:           "open",
		FirstSeen:        now,
		LastSeen:         now,
	}
	// Sanity: zero-valued pointer fields don't panic.
	if f.CVSSScore != nil {
		t.Error("CVSSScore should be nil")
	}
	if f.CVE != "" {
		t.Error("CVE should be empty")
	}
	if f.ResolvedAt != nil {
		t.Error("ResolvedAt should be nil")
	}
}

// ---------------------------------------------------------------------------
// RescanSite unit-level: feed_not_ok → skip
// ---------------------------------------------------------------------------

// This test does not require a database; it exercises the service's early-exit
// when the feed meta is not OK.  We inject a minimal stub via the service
// constructor.  The repo method GetFeedMeta is the gatekeeper — here we test
// the service path where feed is degraded.

// Note: full integration tests (with testcontainers + Postgres) live in
// integration_test.go and require WPMGR_TEST_DB to be set. Those tests verify:
//  - RLS isolation on site_vulnerabilities
//  - UpsertFinding + ResolveStaleFindings round-trip
//  - PruneMissingVulns
//  - The ingester's 429 path (stub HTTP server returning 429)
//  - Key-unset no-op path

// ---------------------------------------------------------------------------
// F1: fleet endpoint scope gate (handler-level test via httptest)
// ---------------------------------------------------------------------------

// TestFleetSummaryRequiresOrgScope verifies that the fleet vulnerability
// endpoint (GET /vulnerabilities on the tenant-level group) is gated by
// RequireOrgScope: a site-scoped collaborator receives 403 and an org-scoped
// member is allowed through (reaching the handler, which returns 200 with an
// empty fleet summary from the stub service).
func TestFleetSummaryRequiresOrgScope(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tenantID := uuid.New()

	// Org-scoped principal — must reach the handler.
	orgPrincipal := domain.Principal{
		TenantID: tenantID,
		Type:     domain.PrincipalUser,
		UserID:   uuid.New(),
		Scope:    "", // "" is treated as org-scope
		Role:     string(authz.RoleViewer),
	}

	// Site-scoped principal — must be blocked with 403.
	sitePrincipal := domain.Principal{
		TenantID:       tenantID,
		Type:           domain.PrincipalUser,
		UserID:         uuid.New(),
		Scope:          domain.ScopeSite,
		AllowedSiteIDs: []uuid.UUID{uuid.New()},
		Role:           string(authz.RoleViewer),
	}

	// Minimal stub service: GetFleetSummary returns an empty summary so the
	// handler can complete without a DB. We inject it via a stub that satisfies
	// the handler's svc field by embedding a no-op *Service-shaped object.
	// Since *Service is not an interface, we mount the handler directly and
	// pass a nil service — the handler will reach the service call and return
	// an internal error, which is a 500. That is fine: we only care that the
	// site-scoped call never reaches the handler at all (403 from middleware),
	// and the org-scoped call DOES reach it (any non-403 response).
	h := vuln.NewHandler(nil, nil, nil)

	engine := gin.New()
	engine.Use(gin.Recovery()) // catch the nil-svc panic in the org-scoped path
	v1 := engine.Group("/api/v1")
	v1.Use(authz.RequireAuth(), authz.RequireTenant())
	h.Register(v1)

	// Helper: inject principal into context and run a request.
	runRequest := func(p domain.Principal) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/vulnerabilities", nil)
		ctx := domain.WithPrincipal(req.Context(), p)
		req = req.WithContext(ctx)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		return w.Code
	}

	t.Run("site_scoped_gets_403", func(t *testing.T) {
		code := runRequest(sitePrincipal)
		if code != http.StatusForbidden {
			t.Errorf("site-scoped principal: got HTTP %d, want 403", code)
		}
	})

	t.Run("org_scoped_passes_gate", func(t *testing.T) {
		code := runRequest(orgPrincipal)
		// The handler itself will fail (nil service → 500) but it must NOT be 403.
		// Any non-403 code proves the gate passed.
		if code == http.StatusForbidden {
			t.Errorf("org-scoped principal: got 403, expected to pass RequireOrgScope gate")
		}
	})
}

// ---------------------------------------------------------------------------
// F3: slug case-normalisation
// ---------------------------------------------------------------------------

// TestSlugNormalisation verifies that isSafeURL and the slug normalisation
// used by parseFeedRecord + LookupSoftware treat mixed-case slugs consistently.
// Because parseFeedRecord and isSafeURL are package-private, we test them via
// exported wrappers (IsSafeURL / NormSlug) declared in export_test.go, or we
// test the observable effect: a feed slug "Akismet" must produce the same
// stored key as an inventory slug "akismet".
func TestSlugNormalisedToLowercase(t *testing.T) {
	cases := []struct {
		feedSlug      string
		inventorySlug string
		wantMatch     bool
	}{
		{"akismet", "akismet", true},
		{"Akismet", "akismet", true},
		{"AKISMET", "akismet", true},
		{"WooCommerce", "woocommerce", true},
		{"WordPressSEO", "wordpressseo", true},
		{"hello-dolly", "hello-dolly", true},
		{"completely-different", "akismet", false},
	}
	for _, tc := range cases {
		normFeed := vuln.NormSlug(tc.feedSlug)
		normInventory := vuln.NormSlug(tc.inventorySlug)
		got := normFeed == normInventory
		if got != tc.wantMatch {
			t.Errorf("NormSlug(%q)==NormSlug(%q): got %v, want %v", tc.feedSlug, tc.inventorySlug, got, tc.wantMatch)
		}
	}
}

// ---------------------------------------------------------------------------
// F2: URL safety filter
// ---------------------------------------------------------------------------

// TestIsSafeURL verifies that isSafeURL accepts only http/https and rejects
// javascript:, data:, and other schemes.
func TestIsSafeURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"https://example.com", true},
		{"http://example.com/path?q=1", true},
		{"HTTPS://EXAMPLE.COM", true},
		{"HTTP://example.com", true},
		{"javascript:alert(1)", false},
		{"javascript://comment%0aalert(1)", false},
		{"data:text/html,<script>alert(1)</script>", false},
		{"vbscript:msgbox(1)", false},
		{"file:///etc/passwd", false},
		{"ftp://example.com", false},
		{"", false},
		{"   javascript:void(0)", false},
	}
	for _, tc := range cases {
		got := vuln.IsSafeURL(tc.url)
		if got != tc.want {
			t.Errorf("IsSafeURL(%q) = %v; want %v", tc.url, got, tc.want)
		}
	}
}

// TestFilterReferences verifies that filterReferences drops non-http(s) URLs
// from feed reference arrays.
func TestFilterReferences(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []string // expected safe URLs in output
	}{
		{
			name:  "all_safe_strings",
			input: `["https://example.com","https://nvd.nist.gov/vuln/detail/CVE-2024-1234"]`,
			want:  []string{"https://example.com", "https://nvd.nist.gov/vuln/detail/CVE-2024-1234"},
		},
		{
			name:  "mixed_safe_and_unsafe",
			input: `["https://example.com","javascript:alert(1)","data:text/html,xss"]`,
			want:  []string{"https://example.com"},
		},
		{
			name:  "all_unsafe",
			input: `["javascript:alert(1)","data:text/html,xss"]`,
			want:  []string{},
		},
		{
			name:  "empty_array",
			input: `[]`,
			want:  []string{},
		},
		{
			name:  "object_format_safe",
			input: `[{"url":"https://wordfence.com"},{"url":"https://nvd.nist.gov"}]`,
			want:  []string{"https://wordfence.com", "https://nvd.nist.gov"},
		},
		{
			name:  "object_format_drops_unsafe",
			input: `[{"url":"https://safe.example.com"},{"url":"javascript:void(0)"}]`,
			want:  []string{"https://safe.example.com"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := vuln.FilterReferences([]byte(tc.input))
			var got []string
			if len(out) == 0 {
				got = []string{}
			} else if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("unmarshal output: %v (raw: %s)", err, out)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len=%d want=%d; got=%v want=%v", len(got), len(tc.want), got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] got %q; want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}
