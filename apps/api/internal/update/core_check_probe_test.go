package update

// core_check_probe_test.go: the post-update decision end to end, through the
// real signed agentcmd.Client and the real agentcmd.Probe against one fake
// site. After a WordPress core update, only a confirmed crash rolls core back:
// an HTTP 500 from the homepage, WordPress's own error screen, or an HTTP 500
// from the signed agent check. A healthy page keeps the update, and a gateway,
// unavailable or CDN error status leaves core in place and fails the task.

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
)

// checkSite answers the agent's signed command routes and the public homepage
// for one site, and records every rollback request body it receives.
type checkSite struct {
	mu         sync.Mutex
	item       agentcmd.ItemResult // the update command's one item result
	pingStatus int                 // 0 answers a healthy ping
	homeStatus int
	homeBody   string
	homeHeader map[string]string
	rollbacks  []string
}

func (s *checkSite) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/wp-json/wpmgr/v1/command/update", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		item := s.item
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(agentcmd.UpdateResponse{OK: true, Results: []agentcmd.ItemResult{item}})
	})
	mux.HandleFunc("/wp-json/wpmgr/v1/command/ping", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		status := s.pingStatus
		s.mu.Unlock()
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"agent_version":"0.61.161"}`)
	})
	mux.HandleFunc("/wp-json/wpmgr/v1/command/rollback", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.rollbacks = append(s.rollbacks, string(body))
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"restored_version":"7.0"}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		status, body, header := s.homeStatus, s.homeBody, s.homeHeader
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
	return mux
}

func (s *checkSite) rollbackBodies() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.rollbacks...)
}

// runSiteCheck applies item to the site through a worker wired with the real
// signed client and the real probe, and returns the task's one terminal state.
func runSiteCheck(t *testing.T, site *checkSite, task Task, item agentcmd.UpdateItem) FinishTaskInput {
	t.Helper()
	srv := httptest.NewServer(site.handler())
	defer srv.Close()

	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := agentcmd.NewSigner(base64.StdEncoding.EncodeToString(priv))
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	hc := httpclient.New(httpclient.Config{AllowPrivateNetworks: true, MaxRetries: 2, BackoffBase: time.Millisecond})
	repo := &probeFakeRepo{}
	w := NewWorker(repo, nil, agentcmd.NewClient(hc, signer), agentcmd.NewProbe(hc), nil, nil, nil, 5, 0)
	w.SetProbeRetryDelays([]time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond})

	if err := w.runApply(context.Background(), task, srv.URL, item); err != nil {
		t.Fatalf("runApply: %v", err)
	}
	return onlyFinish(t, repo)
}

// siteHealthyHomepage is a fully rendered, healthy homepage.
const siteHealthyHomepage = `<!DOCTYPE html>
<html lang="en-US">
<head><meta charset="UTF-8" /><title>Northwind Studio &#8211; Notes from a small web studio</title></head>
<body class="home blog">
<main id="main" class="site-main">
<article class="post-41 post type-post status-publish hentry">
<h2 class="entry-title"><a href="/2026/09/memory-limits/">How we fixed &#8220;Fatal error: Allowed memory size exhausted&#8221; on a client shop</a></h2>
<div class="entry-summary"><p>The checkout page stopped loading after a plugin update. Here is what the log said.</p></div>
</article>
</main>
</body>
</html>
`

// siteErrorScreen is the document WordPress's default wp_die() handler renders
// for its critical-error screen.
const siteErrorScreen = `<!DOCTYPE html>
<html lang="en-US">
<head>
	<meta http-equiv="Content-Type" content="text/html; charset=utf-8" />
	<title>WordPress &rsaquo; Error</title>
</head>
<body id="error-page">
	<div class="wp-die-message"><p>There has been a critical error on this website.</p></div></body>
</html>
`

// siteMaintenanceScreen is the same wp_die() document carrying WordPress's
// maintenance-mode message, which WordPress serves with HTTP 503.
const siteMaintenanceScreen = `<!DOCTYPE html>
<html lang="en-US">
<head><meta http-equiv="Content-Type" content="text/html; charset=utf-8" /><title>Maintenance</title></head>
<body id="error-page">
	<div class="wp-die-message">Briefly unavailable for scheduled maintenance. Check back in a minute.</div></body>
</html>
`

func requireCoreRolledBack(t *testing.T, site *checkSite, got FinishTaskInput) {
	t.Helper()
	bodies := site.rollbackBodies()
	if len(bodies) != 1 {
		t.Fatalf("rollback requests = %d (%q), want exactly 1 after a confirmed crash", len(bodies), bodies)
	}
	if !strings.Contains(bodies[0], `"allow_core_downgrade":true`) {
		t.Errorf("rollback request %s: a core rollback must carry allow_core_downgrade", bodies[0])
	}
	if got.Status != TaskRolledBack {
		t.Errorf("status = %q (detail %q), want %q", got.Status, got.Detail, TaskRolledBack)
	}
}

func requireCoreLeftInPlace(t *testing.T, site *checkSite, got FinishTaskInput, wantErrSub string) {
	t.Helper()
	if bodies := site.rollbackBodies(); len(bodies) != 0 {
		t.Fatalf("rollback requests = %q, want none: only a confirmed crash rolls core back", bodies)
	}
	if got.Status != TaskFailed {
		t.Fatalf("status = %q (detail %q), want %q", got.Status, got.Detail, TaskFailed)
	}
	if got.Detail != coreLeftAsIsDetail {
		t.Errorf("detail = %q, want %q", got.Detail, coreLeftAsIsDetail)
	}
	if got.FromVersion != "7.0" || got.ToVersion != "7.1" {
		t.Errorf("versions = %q -> %q, want 7.0 -> 7.1 (core stays on the new version)", got.FromVersion, got.ToVersion)
	}
	if !strings.Contains(got.Error, wantErrSub) {
		t.Errorf("error = %q, want it to contain %q", got.Error, wantErrSub)
	}
}

// TestCoreUpdateCheck_HealthyHomepage_KeepsTheUpdate: a healthy page is never
// treated as a crash, so the core update stands and nothing is rolled back.
func TestCoreUpdateCheck_HealthyHomepage_KeepsTheUpdate(t *testing.T) {
	site := &checkSite{item: coreUpdated(), homeStatus: http.StatusOK, homeBody: siteHealthyHomepage}
	got := runSiteCheck(t, site, coreTask(), coreItem())

	if bodies := site.rollbackBodies(); len(bodies) != 0 {
		t.Fatalf("rollback requests = %q, want none for a healthy site", bodies)
	}
	if got.Status != TaskSucceeded {
		t.Fatalf("status = %q (detail %q, error %q), want %q", got.Status, got.Detail, got.Error, TaskSucceeded)
	}
	if got.ToVersion != "7.1" {
		t.Errorf("to_version = %q, want 7.1", got.ToVersion)
	}
}

// TestCoreUpdateCheck_ConfirmedCrash_RollsBackCore: an HTTP 500 homepage, or
// WordPress's own error screen on a page that otherwise reports success,
// rolls core back with the downgrade flag.
func TestCoreUpdateCheck_ConfirmedCrash_RollsBackCore(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"homepage answers 500", http.StatusInternalServerError, ""},
		{"homepage shows the error screen", http.StatusOK, siteErrorScreen},
		{"error screen after page output", http.StatusOK, siteHealthyHomepage + siteErrorScreen},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			site := &checkSite{item: coreUpdated(), homeStatus: tc.status, homeBody: tc.body}
			requireCoreRolledBack(t, site, runSiteCheck(t, site, coreTask(), coreItem()))
		})
	}
}

// TestCoreUpdateCheck_GatewayOrUnavailableStatus_LeavesCoreInPlace: a 502,
// 503, 504 or CDN 52x homepage counts as a timeout, not a crash. Core stays on
// the new version and the task fails with the status in its error.
func TestCoreUpdateCheck_GatewayOrUnavailableStatus_LeavesCoreInPlace(t *testing.T) {
	cases := []struct {
		status int
		body   string
		header map[string]string
	}{
		{http.StatusBadGateway, "Bad Gateway", nil},
		{http.StatusServiceUnavailable, siteMaintenanceScreen, map[string]string{"Retry-After": "600"}},
		{http.StatusGatewayTimeout, "Gateway Timeout", nil},
		{520, "Web server is returning an unknown error", map[string]string{"Cf-Cache-Status": "DYNAMIC"}},
		{521, "Web server is down", nil},
		{522, "Connection timed out", nil},
		{523, "Origin is unreachable", nil},
		{524, "A timeout occurred", nil},
		{525, "SSL handshake failed", nil},
		{526, "Invalid SSL certificate", nil},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("homepage %d", tc.status), func(t *testing.T) {
			site := &checkSite{item: coreUpdated(), homeStatus: tc.status, homeBody: tc.body, homeHeader: tc.header}
			got := runSiteCheck(t, site, coreTask(), coreItem())
			requireCoreLeftInPlace(t, site, got, fmt.Sprintf("status=%d", tc.status))
		})
	}
}

// TestCoreUpdateCheck_SignedCheckStatus: the signed agent check confirms a
// crash only with HTTP 500. A gateway or unavailable status from it on every
// attempt fails the task and leaves core in place.
func TestCoreUpdateCheck_SignedCheckStatus(t *testing.T) {
	t.Run("signed check answers 500", func(t *testing.T) {
		site := &checkSite{item: coreUpdated(), pingStatus: http.StatusInternalServerError, homeStatus: http.StatusOK, homeBody: siteHealthyHomepage}
		requireCoreRolledBack(t, site, runSiteCheck(t, site, coreTask(), coreItem()))
	})
	for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 522} {
		t.Run(fmt.Sprintf("signed check answers %d", status), func(t *testing.T) {
			site := &checkSite{item: coreUpdated(), pingStatus: status, homeStatus: http.StatusOK, homeBody: siteHealthyHomepage}
			got := runSiteCheck(t, site, coreTask(), coreItem())
			requireCoreLeftInPlace(t, site, got, "agent reachability check failed")
		})
	}
}

// TestPluginUpdateCheck_UnavailableStatus_StillRollsBackThePlugin: plugin and
// theme updates keep their rollback after a failed check, with no downgrade
// flag, whatever the failing status.
func TestPluginUpdateCheck_UnavailableStatus_StillRollsBackThePlugin(t *testing.T) {
	plugin := agentcmd.ItemResult{Type: TargetPlugin, Slug: "suremail", FromVersion: "1.9.9", ToVersion: "2.0.0", Status: agentcmd.ItemSucceeded, SnapshotID: "snap-1"}
	cases := []struct {
		name string
		site *checkSite
	}{
		{"homepage answers 503", &checkSite{item: plugin, homeStatus: http.StatusServiceUnavailable, homeBody: siteMaintenanceScreen}},
		{"signed check answers 503", &checkSite{item: plugin, pingStatus: http.StatusServiceUnavailable, homeStatus: http.StatusOK, homeBody: siteHealthyHomepage}},
		{"homepage shows the error screen", &checkSite{item: plugin, homeStatus: http.StatusOK, homeBody: siteErrorScreen}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runSiteCheck(t, tc.site, testTask(), updateItem())
			bodies := tc.site.rollbackBodies()
			if len(bodies) != 1 {
				t.Fatalf("rollback requests = %d (%q), want 1 (plugin behaviour unchanged)", len(bodies), bodies)
			}
			if strings.Contains(bodies[0], "allow_core_downgrade") {
				t.Errorf("plugin rollback request %s carries the core downgrade flag", bodies[0])
			}
			if got.Status != TaskRolledBack {
				t.Errorf("status = %q (detail %q), want %q", got.Status, got.Detail, TaskRolledBack)
			}
		})
	}
}
