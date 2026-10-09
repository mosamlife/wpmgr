package update

// worker_forbidden_test.go: GH #679. A site that answers the signed update
// command with HTTP 403 fails the task with copy that says what to do, chosen
// by WHO refused: a firewall or security rule in front of the agent, or the
// WPMgr agent itself with one of its refusal codes. The raw response stays in
// the task's error log and never reaches the detail.
//
// These go through a REAL agentcmd.Client against an httptest server, so the
// error the worker classifies is the one production builds (postRaw ->
// newCommandError), not a hand-built struct.

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
)

// reporterFirewallPage is the shape of the page the #679 reporter's host
// served: a web-server or host-firewall 403, not anything WordPress produced.
const reporterFirewallPage = `<!DOCTYPE HTML PUBLIC "-//IETF//DTD HTML 2.0//EN">
<html><head>
<title>403 Forbidden</title>
</head><body>
<h1>Forbidden</h1>
<p>Request forbidden by administrative rules.</p>
</body></html>`

// forbiddenAgentClient builds a real agentcmd.Client whose every command is
// answered by a server that returns 403 with body.
func forbiddenAgentClient(t *testing.T, contentType, body string) (*agentcmd.Client, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	signer, err := agentcmd.NewSigner(base64.StdEncoding.EncodeToString(priv))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	hc := httpclient.New(httpclient.Config{AllowPrivateNetworks: true}) // test-only: loopback target
	return agentcmd.NewClient(hc, signer), srv.URL
}

func TestRunApply_Forbidden403_ExplainsWhoRefusedAndWhatToDo(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
		// wantIn must all appear in the detail.
		wantIn []string
		// siteText must never appear in the detail.
		siteText []string
	}{
		{
			name:        "host firewall HTML page (the reporter's first error)",
			contentType: "text/html",
			body:        reporterFirewallPage,
			wantIn:      []string{"Update not started.", "firewall or security rule", "/wp-json/wpmgr/v1/"},
			siteText:    []string{"administrative rules", "<html", "<h1>"},
		},
		{
			name:        "agent refusal: site clock behind",
			contentType: "application/json",
			body:        `{"code":"wpmgr_token_skew","message":"Forbidden.","data":{"status":403}}`,
			wantIn:      []string{"Update not started.", "wpmgr_token_skew", "server clock", "sync the server time"},
			siteText:    []string{"Forbidden."},
		},
		{
			name:        "agent refusal: the reporter's second error",
			contentType: "application/json",
			body:        `{"code":"wpmgr_invalid_token","message":"Forbidden.","data":{"status":403}}`,
			wantIn:      []string{"Update not started.", "wpmgr_invalid_token", "server clock", "reconnect the site"},
			siteText:    []string{"Forbidden."},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, siteURL := forbiddenAgentClient(t, tc.contentType, tc.body)
			repo := &probeFakeRepo{}
			w := newApplyTestWorker(repo, client, &panicProber{t: t})

			if err := w.runApply(context.Background(), testTask(), siteURL, updateItem()); err != nil {
				t.Fatalf("runApply() = %v; a 403 is a terminal failure, not a retry", err)
			}
			if len(repo.finished) != 1 {
				t.Fatalf("expected exactly one terminal finish, got %d: %+v", len(repo.finished), repo.finished)
			}
			got := repo.finished[0]
			if got.Status != TaskFailed {
				t.Errorf("status = %q, want %q", got.Status, TaskFailed)
			}
			if got.Detail == "update command failed" {
				t.Fatalf("detail is still the generic %q, with no next step", got.Detail)
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(got.Detail, want) {
					t.Errorf("detail %q does not contain %q", got.Detail, want)
				}
			}
			for _, leak := range tc.siteText {
				if strings.Contains(got.Detail, leak) {
					t.Errorf("detail %q carries site-supplied text %q", got.Detail, leak)
				}
			}
			if !strings.Contains(got.Error, "status 403 body=") {
				t.Errorf("error = %q, want the raw agent error kept in the task's error log", got.Error)
			}
		})
	}
}

// TestRunDry_Forbidden403_ExplainsWhoRefusedAndWhatToDo is runDry's mirror:
// the same classifier, with "Dry run" as the action.
func TestRunDry_Forbidden403_ExplainsWhoRefusedAndWhatToDo(t *testing.T) {
	client, siteURL := forbiddenAgentClient(t, "text/html", reporterFirewallPage)
	repo := &probeFakeRepo{}
	w := newApplyTestWorker(repo, client, &panicProber{t: t})

	if err := w.runDry(context.Background(), testTask(), siteURL, updateItem()); err != nil {
		t.Fatalf("runDry() = %v; a 403 is a terminal failure, not a retry", err)
	}
	if len(repo.finished) != 1 {
		t.Fatalf("expected exactly one terminal finish, got %d: %+v", len(repo.finished), repo.finished)
	}
	got := repo.finished[0]
	if got.Status != TaskFailed {
		t.Errorf("status = %q, want %q", got.Status, TaskFailed)
	}
	if !strings.HasPrefix(got.Detail, "Dry run not started.") || !strings.Contains(got.Detail, "firewall or security rule") {
		t.Errorf("detail = %q, want the dry-run firewall copy", got.Detail)
	}
	if strings.Contains(got.Detail, "administrative rules") {
		t.Errorf("detail %q carries the site's page text", got.Detail)
	}
	if !strings.Contains(got.Error, "status 403 body=") {
		t.Errorf("error = %q, want the raw agent error kept", got.Error)
	}
}

// TestRunApply_Non403CommandError_KeepsGenericDetail is the over-fire guard:
// a status other than 403 is not this classifier's to describe.
func TestRunApply_Non403CommandError_KeepsGenericDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"rest_no_route","message":"No route was found matching the URL and request method.","data":{"status":404}}`))
	}))
	t.Cleanup(srv.Close)
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	signer, err := agentcmd.NewSigner(base64.StdEncoding.EncodeToString(priv))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	client := agentcmd.NewClient(httpclient.New(httpclient.Config{AllowPrivateNetworks: true}), signer)

	repo := &probeFakeRepo{}
	w := newApplyTestWorker(repo, client, &panicProber{t: t})
	if err := w.runApply(context.Background(), testTask(), srv.URL, updateItem()); err != nil {
		t.Fatalf("runApply() = %v", err)
	}
	if len(repo.finished) != 1 || repo.finished[0].Detail != "update command failed" {
		t.Fatalf("a 404 must keep the generic detail, got %+v", repo.finished)
	}
}
