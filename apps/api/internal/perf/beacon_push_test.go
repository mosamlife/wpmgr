package perf

// beacon_push_test.go — tests for the RUM beacon-key push gap (Deliverable A).
//
// Verified behavior:
//   - On first-enable (RumEnabled=true, BeaconKeySet=false): the agent push
//     payload carries a non-empty RumBeaconKey and a non-empty RumIngestURL.
//   - On a subsequent push (RumEnabled=true, BeaconKeySet=true): the agent
//     push payload has an empty RumBeaconKey (plaintext not re-sent).
//   - RumIngestURL is always set to cpBaseURL+"/rum/ingest" when RumEnabled.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// recordingAgent wraps the existing fakeAgent and records the most recent
// SyncPerfConfig request so tests can assert on the pushed payload.
type recordingAgent struct {
	fakeAgent
	mu          sync.Mutex
	lastPerfReq agentcmd.PerfConfigRequest
}

func (a *recordingAgent) SyncPerfConfig(_ context.Context, _ uuid.UUID, _ string, req agentcmd.PerfConfigRequest) (agentcmd.PerfConfigResult, error) {
	a.mu.Lock()
	a.lastPerfReq = req
	a.mu.Unlock()
	return agentcmd.PerfConfigResult{OK: true}, nil
}

// fakeBeaconKeyRepo is a minimal in-memory stub satisfying what
// toPerfConfigRequest / SetBeaconKeyRepo needs from *rum.BeaconKeyRepo.
// We can't use the real BeaconKeyRepo in unit tests (it needs a DB), so we
// wire SetBeaconKeyRepo with nil (keys won't be stored) and instead test the
// toPerfConfigRequest function directly — the one that builds the push payload.
//
// The correct end-to-end path through UpdateConfig requires a live DB; we
// therefore test the payload-building function directly and separately test
// the UpdateConfig flow via the service-level integration tests that use a live
// fakeRepo.

// ---------------------------------------------------------------------------
// toPerfConfigRequest payload tests (pure, no DB)
// ---------------------------------------------------------------------------

// TestToPerfConfigRequest_includesBeaconKeyOnFirstEnable verifies that when a
// freshBeaconKey is provided, the push payload carries it in RumBeaconKey.
func TestToPerfConfigRequest_includesBeaconKeyOnFirstEnable(t *testing.T) {
	cfg := Config{
		RumEnabled:    true,
		RumSampleRate: 0.5,
		BeaconKeySet:  false, // newly set — key was just generated
	}
	const freshKey = "aBcDeFgHiJkLmNoPqRsTuVwXyZ01234" // dummy plaintext
	const cpBase = "https://manage.example.com"

	req := toPerfConfigRequest(cfg, freshKey, cpBase)

	if req.RumBeaconKey != freshKey {
		t.Errorf("RumBeaconKey = %q, want %q", req.RumBeaconKey, freshKey)
	}
	if req.RumIngestURL != cpBase+"/rum/ingest" {
		t.Errorf("RumIngestURL = %q, want %q", req.RumIngestURL, cpBase+"/rum/ingest")
	}
	if !req.RumEnabled {
		t.Error("RumEnabled must be true when cfg.RumEnabled=true")
	}
	if req.RumSampleRate != 0.5 {
		t.Errorf("RumSampleRate = %g, want 0.5", req.RumSampleRate)
	}
}

// TestToPerfConfigRequest_omitsBeaconKeyOnSubsequentPush verifies that when
// freshBeaconKey is "" (unchanged key), the push carries an empty RumBeaconKey.
// The CP stores only the hash and cannot resend the plaintext on subsequent
// pushes; the agent must retain its own copy.
func TestToPerfConfigRequest_omitsBeaconKeyOnSubsequentPush(t *testing.T) {
	cfg := Config{
		RumEnabled:   true,
		BeaconKeySet: true, // already provisioned
	}
	req := toPerfConfigRequest(cfg, "" /*freshBeaconKey=unchanged*/, "https://cp.example.com")

	if req.RumBeaconKey != "" {
		t.Errorf("RumBeaconKey must be empty on unchanged push, got %q", req.RumBeaconKey)
	}
	// But IngestURL is still set because RumEnabled=true.
	if req.RumIngestURL == "" {
		t.Error("RumIngestURL must be set when RumEnabled=true, even on unchanged push")
	}
}

// TestToPerfConfigRequest_beaconKeyFieldAbsentFromWireOnUnchangedPush is a
// regression guard for the EnableCache push path (service.go — EnableCache
// calls toPerfConfigRequest(cfg, "", cpBaseURL)), which relies on
// `RumBeaconKey string json:"rum_beacon_key,omitempty"` (agentcmd/cache_
// contract.go) DROPPING the field entirely on the wire so the agent's local
// merge preserves whatever key it already has. Asserting only
// RumBeaconKey=="" (as the test above does) is not sufficient — a future
// change that removes `omitempty` or adds a custom MarshalJSON could still
// leave RumBeaconKey=="" in Go while emitting `"rum_beacon_key":""` on the
// wire, which a naive agent-side merge could misread as "the CP wants to
// clear the key." This test fails loudly on that regression by asserting the
// key is ABSENT from the marshaled JSON, not just empty.
func TestToPerfConfigRequest_beaconKeyFieldAbsentFromWireOnUnchangedPush(t *testing.T) {
	cfg := Config{RumEnabled: true, BeaconKeySet: true}
	req := toPerfConfigRequest(cfg, "" /*freshBeaconKey=unchanged*/, "https://cp.example.com")

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := wire["rum_beacon_key"]; present {
		t.Errorf("rum_beacon_key must be ABSENT from the wire payload on an unchanged push (omitempty regression), got: %s", string(data))
	}
}

// TestToPerfConfigRequest_cacheVariantWireNames verifies that the four page-
// cache variant lists marshal with the agent-side unprefixed keys
// (bypass_urls, bypass_cookies, include_queries, include_cookies), not the CP
// public/API prefixed names. This is a regression guard: if the wire names ever
// revert to cache_*, the agent will silently ignore the lists.
func TestToPerfConfigRequest_cacheVariantWireNames(t *testing.T) {
	cfg := Config{
		CacheBypassURLs:     []string{"/cart", "/checkout"},
		CacheBypassCookies:  []string{"woocommerce", "my_session"},
		CacheIncludeQueries: []string{"sort_dir", "filter_color"},
		CacheIncludeCookies: []string{"geo", "currency"},
	}

	req := toPerfConfigRequest(cfg, "", "")
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw := string(data)

	wantKeys := []string{"bypass_urls", "bypass_cookies", "include_queries", "include_cookies"}
	for _, key := range wantKeys {
		if !strings.Contains(raw, fmt.Sprintf("%q:", key)) {
			t.Errorf("payload missing expected key %q; got %s", key, raw)
		}
	}

	badKeys := []string{"cache_bypass_urls", "cache_bypass_cookies", "cache_include_queries", "cache_include_cookies"}
	for _, key := range badKeys {
		if strings.Contains(raw, fmt.Sprintf("%q:", key)) {
			t.Errorf("payload must not use CP public field name %q; got %s", key, raw)
		}
	}

	// Spot-check values are present so the test is not just key-name matching.
	for _, val := range []string{"/cart", "woocommerce", "sort_dir", "geo"} {
		if !strings.Contains(raw, fmt.Sprintf("%q", val)) {
			t.Errorf("payload missing expected value %q; got %s", val, raw)
		}
	}
}

// TestToPerfConfigRequest_ingestURLEmptyWhenRumDisabled verifies that the
// ingest URL is omitted when RumEnabled=false.
func TestToPerfConfigRequest_ingestURLEmptyWhenRumDisabled(t *testing.T) {
	cfg := Config{RumEnabled: false}
	req := toPerfConfigRequest(cfg, "", "https://cp.example.com")

	if req.RumIngestURL != "" {
		t.Errorf("RumIngestURL must be empty when RumEnabled=false, got %q", req.RumIngestURL)
	}
}

// TestToPerfConfigRequest_ingestURLTrailingSlashStripped verifies that a
// cpBaseURL with a trailing slash does not produce a double slash.
func TestToPerfConfigRequest_ingestURLTrailingSlashStripped(t *testing.T) {
	cfg := Config{RumEnabled: true}
	req := toPerfConfigRequest(cfg, "", "https://cp.example.com/")

	want := "https://cp.example.com/rum/ingest"
	if req.RumIngestURL != want {
		t.Errorf("RumIngestURL = %q, want %q", req.RumIngestURL, want)
	}
}

// TestToPerfConfigRequest_cdnWireAndFileTypeMapping verifies that the CDN
// rewrite config is marshaled with the agent-side wire names and enum values,
// and that provider/credential fields never leak into the agent payload.
func TestToPerfConfigRequest_cdnWireAndFileTypeMapping(t *testing.T) {
	cases := []struct {
		name       string
		fileTypes  string
		wantWire   string
		wantMapped string
	}{
		{"all", "all", "cdn", "all"},
		{"images maps to image", "images", "cdn", "image"},
		{"css_js maps to css_js_font", "css_js", "cdn", "css_js_font"},
		{"empty defaults to all", "", "cdn", "all"},
		{"unrecognized defaults to all", "unknown", "cdn", "all"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{
				CDNEnabled:   true,
				CDNURL:       "https://cdn.example.com",
				CDNFileTypes: tc.fileTypes,
				CDNProvider:  "bunny",
			}
			req := toPerfConfigRequest(cfg, "", "")

			if !req.CDNEnabled {
				t.Error("CDNEnabled must be true")
			}
			if req.CDNURL != cfg.CDNURL {
				t.Errorf("CDNURL = %q, want %q", req.CDNURL, cfg.CDNURL)
			}
			if req.CDNFileType != tc.wantMapped {
				t.Errorf("CDNFileType = %q, want %q", req.CDNFileType, tc.wantMapped)
			}

			data, err := json.Marshal(req)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			raw := string(data)

			// The enabled flag must marshal under the agent key.
			if !strings.Contains(raw, fmt.Sprintf("%q:", tc.wantWire)) {
				t.Errorf("payload missing expected key %q; got %s", tc.wantWire, raw)
			}
			// The CP public key must NOT appear.
			if strings.Contains(raw, `"cdn_enabled"`) {
				t.Errorf("payload must not use CP public field name \"cdn_enabled\"; got %s", raw)
			}
			// The mapped file-type value must be present.
			if !strings.Contains(raw, fmt.Sprintf("%q", tc.wantMapped)) {
				t.Errorf("payload missing expected file-type value %q; got %s", tc.wantMapped, raw)
			}
			// Provider and credential-shaped fields must be absent.
			for _, bad := range []string{"cdn_provider", "api_token", "zone_id", "zone", "cdn_credentials"} {
				if strings.Contains(raw, fmt.Sprintf("%q:", bad)) {
					t.Errorf("payload must not contain credential/provider field %q; got %s", bad, raw)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// UpdateConfig integration (uses fakeRepo + recordingAgent + nil beaconRepo)
// ---------------------------------------------------------------------------

// TestUpdateConfig_recordingAgentReceivesRumFields verifies that when
// UpdateConfig is called with RumEnabled=true on a new site (BeaconKeySet=false
// in the stored config), the agent push contains RumEnabled=true and a
// non-empty RumIngestURL.
//
// NOTE: The freshBeaconKey field will be empty because SetBeaconKeyRepo is not
// wired in this test (the real BeaconKeyRepo requires a live DB). The
// beacon-key generation path is tested separately via the pure function tests
// above (TestToPerfConfigRequest_includesBeaconKeyOnFirstEnable). The
// integration path (SetBeaconKeyRepo wired, no key exists) is exercised by the
// e2e tests against a live DB. Here we validate the agent push shape.
func TestUpdateConfig_recordingAgentReceivesRumFields(t *testing.T) {
	repo := &fakeRepo{
		config:      Config{RumEnabled: false, BeaconKeySet: false},
		configFound: true,
	}
	agent := &recordingAgent{}
	svc := NewService(repo, nil, &fakeEvents{}, nil)
	svc.SetAgentClient(agent, &fakeSites{url: "https://example.com"})
	svc.cpBaseURL = "https://manage.example.com"

	in := UpdateConfigInput{Config: Config{
		TenantID:      uuid.New(),
		SiteID:        uuid.New(),
		RumEnabled:    true,
		RumSampleRate: 1.0,
	}}

	_, err := svc.UpdateConfig(context.Background(), in.Config.TenantID, in.Config.SiteID, in)
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}

	agent.mu.Lock()
	got := agent.lastPerfReq
	agent.mu.Unlock()

	if !got.RumEnabled {
		t.Error("push payload: RumEnabled must be true")
	}
	if got.RumIngestURL != "https://manage.example.com/rum/ingest" {
		t.Errorf("push payload: RumIngestURL = %q, want %q", got.RumIngestURL, "https://manage.example.com/rum/ingest")
	}
}
