package site

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// TestRecheckSiteURLRedirects502: when the site redirects its command address,
// Re-check keeps the 502 but answers code site_url_redirects with the target
// named in the message and in details, and records no heartbeat.
func TestRecheckSiteURLRedirects502(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	seen := time.Now().Add(-30 * time.Second)
	baseRepo := &recheckRepo{
		freshRepo: freshRepo{
			enrolled: true,
			lastSeen: &seen,
			site:     Site{URL: "https://example.com", ConnectionState: StateConnected},
		},
	}
	rechecker := &fakeRechecker{err: fmt.Errorf("wrapped: %w", &agentcmd.RedirectError{
		Command:          "metadata",
		Status:           301,
		From:             "https://example.com/wp-json/wpmgr/v1/command/metadata",
		To:               "https://www.example.com/wp-json/wpmgr/v1/command/metadata",
		SuggestedSiteURL: "https://www.example.com",
	})}
	connSvc := &fakeConnSvc{}

	svc := NewService(baseRepo, domain.NewValidator(), domain.SystemClock{})
	h := NewHandler(svc, nil, "")
	h.SetRechecker(rechecker)
	h.SetConnectionService(connSvc)

	rec := doRecheck(buildRecheckEngine(h, tenantID), siteID)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Code    string            `json:"code"`
		Message string            `json:"message"`
		Details map[string]string `json:"details"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if body.Code != "site_url_redirects" {
		t.Errorf("code = %q, want site_url_redirects", body.Code)
	}
	if !strings.Contains(body.Message, "https://www.example.com") || !strings.Contains(body.Message, "updates to https://www.example.com automatically") {
		t.Errorf("message %q should name the target and the remedy", body.Message)
	}
	want := map[string]string{
		"from":          "https://example.com",
		"to":            "https://www.example.com/wp-json/wpmgr/v1/command/metadata",
		"suggested_url": "https://www.example.com",
	}
	for k, v := range want {
		if body.Details[k] != v {
			t.Errorf("details[%q] = %q, want %q", k, body.Details[k], v)
		}
	}
	if connSvc.heartbeatCalls != 0 {
		t.Fatalf("RecordHeartbeat must NOT be called on a redirect, got %d", connSvc.heartbeatCalls)
	}
}
