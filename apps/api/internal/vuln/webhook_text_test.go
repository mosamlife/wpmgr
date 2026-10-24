package vuln_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
	"github.com/mosamlife/wpmgr/apps/api/internal/uptime"
	"github.com/mosamlife/wpmgr/apps/api/internal/vuln"
)

type textRoundTripper func(*http.Request) (*http.Response, error)

func (f textRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestVulnWebhookText(t *testing.T) {
	tenantID, siteID, otherID := uuid.New(), uuid.New(), uuid.New()
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, siteName, component, want string
		batch                           bool
	}{
		{name: "single", siteName: "Example", component: "Example Plugin", want: "1 new vulnerability on 1 site: Example (https://example.com): Plugin: Example Plugin"},
		{name: "batch", siteName: "Example", component: "Example Plugin", batch: true, want: "3 new vulnerabilities on 2 sites: Example (https://example.com): Plugin: Example Plugin; and more"},
		{name: "site values", siteName: "Café @channel\n<!channel>", component: "[click](https://x) @all", want: "1 new vulnerability on 1 site: Café @\u2060channel ‹!channel› (https://example.com): Plugin: ［click］(https://x) @\u2060all"},
		{name: "rune caps", siteName: strings.Repeat("界", 201), component: strings.Repeat("é", 201), want: "1 new vulnerability on 1 site: " + strings.Repeat("界", 200) + " (https://example.com): " + "Plugin: " + strings.Repeat("é", 192)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			finding := claimedFinding(siteID, tc.siteName, vuln.KindPlugin, tc.component, vuln.SeverityCritical, now)
			finding.SiteURL = "https://example.com"
			finding.Finding.FixedVersion = "2.0"
			finding.Finding.CVE = "CVE-2026-1234"
			claimed := []vuln.ClaimedFinding{finding}
			legacyFinding := map[string]any{"component": "Plugin: " + tc.component, "installed_version": "1.0.0", "fixed_version": "2.0", "severity": vuln.SeverityCritical, "cve": "CVE-2026-1234"}
			legacySites := []any{map[string]any{"site_id": siteID.String(), "site_name": tc.siteName, "site_url": finding.SiteURL, "findings": []any{legacyFinding}}}
			if tc.batch {
				theme := claimedFinding(otherID, "Other", vuln.KindTheme, "Example Theme", vuln.SeverityHigh, now)
				core := claimedFinding(otherID, "Other", vuln.KindCore, "WordPress", vuln.SeverityLow, now)
				claimed = append(claimed, theme, core)
				legacySites = append(legacySites, map[string]any{"site_id": otherID.String(), "site_name": "Other", "site_url": "https://Other", "findings": []any{
					map[string]any{"component": "Theme: Example Theme", "installed_version": "1.0.0", "severity": vuln.SeverityHigh},
					map[string]any{"component": "WordPress Core", "installed_version": "1.0.0", "severity": vuln.SeverityLow},
				}})
			}
			payload := vuln.BuildVulnAlertWebhookPayload(tenantID, claimed)
			client := httpclient.New(httpclient.Config{})
			var body []byte
			var signature string
			client.HTTPClient().Transport = textRoundTripper(func(r *http.Request) (*http.Response, error) {
				var err error
				body, err = io.ReadAll(r.Body)
				signature = r.Header.Get("X-WPMgr-Signature")
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, err
			})
			d := uptime.NewDispatcher(nil, uptime.NewSSRFWebhookPoster(client), nil, nil)
			if err := d.PostSignedWebhook(context.Background(), "https://chat.example/hook", "secret", payload); err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			if got["text"] != tc.want {
				t.Errorf("text = %q, want %q", got["text"], tc.want)
			}
			mac := hmac.New(sha256.New, []byte("secret"))
			_, _ = mac.Write(body)
			if signature != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
				t.Error("signature does not cover exact transmitted body")
			}
			delete(got, "text")
			legacy := map[string]any{"event": vuln.VulnAlertEvent, "tenant_id": tenantID.String(), "new_count": float64(len(claimed)), "site_count": float64(len(legacySites)), "sites": legacySites, "fired_at": payload.FiredAt.Format(time.RFC3339Nano)}
			if !reflect.DeepEqual(got, legacy) {
				t.Errorf("existing fields changed:\ngot  %#v\nwant %#v", got, legacy)
			}
			withoutText, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			mac.Reset()
			_, _ = mac.Write(withoutText)
			if signature == "sha256="+hex.EncodeToString(mac.Sum(nil)) {
				t.Error("signature also validates without text")
			}
		})
	}
}
