package uptime

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
)

type textRoundTripper func(*http.Request) (*http.Response, error)

func (f textRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWebhookTextEvents(t *testing.T) {
	tenantID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	siteID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		event, want                             string
		kind                                    AlertKind
		security, aggregate, updated, recovered bool
	}{
		{event: "uptime.down", kind: AlertDown, want: "Example (https://example.com) is down: HTTP 503"},
		{event: "uptime.recovery", kind: AlertRecovery, want: "✅ Example (https://example.com) is back up"},
		{event: "uptime.app_down", kind: AlertAppDown, want: "Example (https://example.com) application is down: the application-health check returned HTTP 500 (PHP itself returned an error)"},
		{event: "uptime.app_recovery", kind: AlertAppRecovery, want: "✅ Example (https://example.com) application is back up"},
		{event: "uptime.app_down_aggregate", aggregate: true, want: "Application health issue: 3 of 5 sites are down"},
		{event: "uptime.app_down_aggregate_update", aggregate: true, updated: true, want: "Application health issue worsened: 3 of 5 sites are down"},
		{event: "uptime.app_recovery_aggregate", aggregate: true, recovered: true, want: "✅ Fleet application health recovered: 3 of 5 sites are down"},
		{event: "security.user.created", security: true, want: "Security event on Example (https://example.com): Administrator created"},
		{event: "security.file_integrity", security: true, want: "Security event on Example (https://example.com): Administrator created"},
	}
	for _, tc := range cases {
		t.Run(tc.event, func(t *testing.T) {
			client := httpclient.New(httpclient.Config{})
			var body []byte
			var signature string
			client.HTTPClient().Transport = textRoundTripper(func(r *http.Request) (*http.Response, error) {
				var err error
				body, err = io.ReadAll(r.Body)
				signature = r.Header.Get("X-WPMgr-Signature")
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, err
			})
			d := NewDispatcher(nil, NewSSRFWebhookPoster(client), nil, nil)
			cfg := AlertConfig{WebhookURL: "https://chat.example/hook", WebhookSecret: "secret"}
			legacy := map[string]any{
				"event": tc.event, "tenant_id": tenantID.String(), "site_id": siteID.String(),
				"site_name": "Example", "site_url": "https://example.com", "fired_at": now.Format(time.RFC3339),
			}
			switch {
			case tc.aggregate:
				legacy["site_id"], legacy["site_url"] = "", ""
				delete(legacy, "site_name")
				legacy["error"] = "3/5 alert-eligible sites app-down"
				legacy["app_down_count"], legacy["app_eligible_count"] = float64(3), float64(5)
				legacy["app_suppressed_sites"] = []any{"Raw @all\nsite"}
				d.FireAppAggregate(context.Background(), cfg, AppAggregateAlert{TenantID: tenantID, DownCount: 3, EligibleCount: 5, SuppressedSites: []string{"Raw @all\nsite"}, FiredAt: now, Recovered: tc.recovered, Updated: tc.updated})
			case tc.security:
				legacy["error"] = "Administrator created"
				d.FireSecurityEvent(context.Background(), cfg, SecurityEvent{TenantID: tenantID, SiteID: siteID, SiteName: "Example", SiteURL: "https://example.com", EventType: strings.TrimPrefix(tc.event, "security."), Summary: "Administrator created", FiredAt: now})
			default:
				alert := Alert{TenantID: tenantID, SiteID: siteID, Kind: tc.kind, SiteName: "Example", SiteURL: "https://example.com", FiredAt: now}
				if tc.kind == AlertDown {
					alert.HTTPStatus = 503
					legacy["http_status"] = float64(503)
				}
				if tc.kind == AlertAppDown {
					alert.Error = AppProbeReasonRest5xx
					legacy["error"] = AppProbeReasonRest5xx
				}
				d.Fire(context.Background(), cfg, alert)
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

func TestWebhookTextSiteValues(t *testing.T) {
	cases := []struct {
		name, url, detail, want string
		security                bool
	}{
		{name: "@channel <!channel> [click](https://x)", url: "https://x/@here\n", detail: "a\n@all", want: "@\u2060channel ‹!channel› ［click］(https://x) (https://x/@\u2060here) is down: a @\u2060all"},
		{url: "https://example.com", want: "https://example.com is down"},
		{want: "22222222-2222-2222-2222-222222222222 is down"},
		{name: strings.Repeat("é", 201), want: strings.Repeat("é", 200) + " is down"},
		{name: "Example", detail: "<!channel> [click](https://x)\r\n@someone", security: true, want: "Security event on Example: ‹!channel› ［click］(https://x) @\u2060someone"},
		{name: "Example", url: strings.Repeat("é", 201), detail: strings.Repeat("界", 201), want: "Example (" + strings.Repeat("é", 200) + ") is down: " + strings.Repeat("界", 200)},
		{name: "Example", detail: "wp_fatal_error", want: "Example is down: WordPress critical error"},
		{name: "Example", detail: "wp_db_error", want: "Example is down: database connection error"},
		{name: "Example", security: true, want: "Security event on Example: user.created"},
		{name: "&#64;channel", want: "&\u2060#64;channel is down"},
	}
	for i, tc := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			poster := &textPayloadCapture{}
			d := NewDispatcher(nil, poster, nil, nil)
			cfg := AlertConfig{WebhookURL: "https://chat.example/hook"}
			siteID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
			if tc.security {
				d.FireSecurityEvent(context.Background(), cfg, SecurityEvent{SiteID: siteID, SiteName: tc.name, SiteURL: tc.url, Summary: tc.detail, EventType: "user.created"})
			} else {
				d.Fire(context.Background(), cfg, Alert{SiteID: siteID, Kind: AlertDown, SiteName: tc.name, SiteURL: tc.url, Error: tc.detail})
			}
			var got map[string]any
			if err := json.Unmarshal(poster.body, &got); err != nil {
				t.Fatal(err)
			}
			if got["text"] != tc.want {
				t.Errorf("text = %q, want %q", got["text"], tc.want)
			}
			if got["site_url"] != tc.url {
				t.Error("site_url changed")
			}
			if tc.name != "" && got["site_name"] != tc.name {
				t.Error("site_name changed")
			}
			if tc.detail != "" && got["error"] != tc.detail {
				t.Error("error changed")
			}
		})
	}
}

type textPayloadCapture struct{ body []byte }

func (p *textPayloadCapture) Post(_ context.Context, _, _ string, payload any) (err error) {
	p.body, err = json.Marshal(payload)
	return err
}
