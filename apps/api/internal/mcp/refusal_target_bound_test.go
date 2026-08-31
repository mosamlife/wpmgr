package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
)

// ---------------------------------------------------------------------------
// PROOF -- A REFUSAL CANNOT PUT UNBOUNDED ATTACKER BYTES THROUGH THE CHAIN.
//
// Both refusal rows record a value the CALLER chose: the tool name as spelled,
// and the protocol revision as sent. Both arrive in the JSON body, so both are
// bounded only by maxRequestBytes (256 KiB). Every audit append takes the
// per-tenant advisory lock and hashes the row into the chain, so an unbounded
// target_id lets a caller who can use NOTHING on this surface still drive the
// one serialised write path on that tenant's ledger -- being denied is not a
// barrier to writing, which is exactly what makes the refusal row the reachable
// case.
//
// These tests assert by value on the event the recorder actually received.
// ---------------------------------------------------------------------------

// capturingRecorder records every event it is handed and succeeds, so a test
// can read back precisely what would have been written.
type capturingRecorder struct {
	mu     sync.Mutex
	events []audit.Event
}

func (c *capturingRecorder) Record(_ context.Context, e audit.Event) (audit.Entry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
	return audit.Entry{}, nil
}

func (c *capturingRecorder) RecordInTx(_ context.Context, _ pgx.Tx, e audit.Event) (audit.Entry, error) {
	return c.Record(context.Background(), e)
}

// only returns the single captured event of the given action, failing loudly
// when there is none — a test that found no row and passed would be the
// "guard that finds nothing goes green" defect.
func (c *capturingRecorder) only(t *testing.T, action string) audit.Event {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()

	var found []audit.Event
	for _, e := range c.events {
		if e.Action == action {
			found = append(found, e)
		}
	}
	if len(found) == 0 {
		t.Fatalf("no %s row was recorded; the refusal did not audit and this test asserts nothing", action)
	}
	if len(found) > 1 {
		t.Fatalf("expected exactly 1 %s row, got %d", action, len(found))
	}
	return found[0]
}

func capturingRouter(t *testing.T) (*gin.Engine, *capturingRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	rec := &capturingRecorder{}
	svc := NewService(liveGrantStore(uuid.New())).withAuditRecorder(rec)

	r := gin.New()
	NewTransportHandler(svc, slog.New(slog.DiscardHandler), "test-version").Register(r)
	return r, rec
}

func TestRefusalAudit_BoundsAnAttackerChosenTarget(t *testing.T) {
	// Big enough to matter, well inside the 256 KiB body limit so the request
	// is ACCEPTED and actually reaches the refusal — a 413 would prove nothing.
	const oversized = 64 * 1024

	cases := []struct {
		name   string
		action string
		body   func(payload string) string
		header string
	}{
		{
			name:   "tool name",
			action: audit.ActionMCPToolDenied,
			body: func(p string) string {
				return fmt.Sprintf(
					`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":{}}}`, p)
			},
		},
		{
			name:   "initialize protocol revision",
			action: audit.ActionMCPProtocolDenied,
			body:   func(p string) string { return initBody(p) },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, rec := capturingRouter(t)
			payload := strings.Repeat("a", oversized)

			req := httptest.NewRequest(http.MethodPost, TransportPath, strings.NewReader(tc.body(payload)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+testBearer)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			// The request must have been ACCEPTED and refused on the merits,
			// not rejected for size. If this is a 413 the rest is vacuous.
			if w.Code == http.StatusRequestEntityTooLarge {
				t.Fatalf("request was refused as too large; the refusal path was never reached")
			}

			e := rec.only(t, tc.action)

			if len(e.TargetID) > audit.MaxTargetIDLen {
				t.Errorf("target_id is %d bytes, want <= %d — a refused caller can drive %d bytes "+
					"through the per-tenant audit lock", len(e.TargetID), audit.MaxTargetIDLen, len(e.TargetID))
			}
			if !utf8.ValidString(e.TargetID) {
				t.Error("target_id is not valid UTF-8; Postgres would reject this row")
			}
			if !strings.HasPrefix(payload, e.TargetID) {
				t.Error("target_id is not a prefix of what the caller sent; the evidence is wrong, not just short")
			}

			// The row must SAY it is a prefix, by value, or an auditor reads
			// the shortened string as the name actually sent.
			if got, _ := e.Metadata["target_truncated"].(bool); !got {
				t.Errorf("metadata.target_truncated = %v, want true", e.Metadata["target_truncated"])
			}
			if got, _ := e.Metadata["target_original_len"].(int); got != oversized {
				t.Errorf("metadata.target_original_len = %v, want %d", e.Metadata["target_original_len"], oversized)
			}
		})
	}
}

// TestRefusalAudit_DoesNotTruncateAnOrdinaryTarget is the OVER-FIRE proof.
//
// The bound must be invisible to every real refusal. A normal mistyped tool
// name and a normal unsupported revision are recorded verbatim, with neither
// truncation key present — a bound that quietly rewrote ordinary evidence
// would be worse than the problem it fixes.
func TestRefusalAudit_DoesNotTruncateAnOrdinaryTarget(t *testing.T) {
	cases := []struct {
		name   string
		action string
		body   string
		want   string
	}{
		{
			name:   "a mistyped tool name is recorded verbatim",
			action: audit.ActionMCPToolDenied,
			body:   `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_sitez","arguments":{}}}`,
			want:   "list_sitez",
		},
		{
			name:   "an unsupported revision is recorded verbatim",
			action: audit.ActionMCPProtocolDenied,
			body:   initBody("2019-01-01"),
			want:   "2019-01-01",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, rec := capturingRouter(t)

			req := httptest.NewRequest(http.MethodPost, TransportPath, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+testBearer)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			e := rec.only(t, tc.action)

			if e.TargetID != tc.want {
				t.Errorf("target_id = %q, want %q — the bound rewrote an ordinary value", e.TargetID, tc.want)
			}
			if _, ok := e.Metadata["target_truncated"]; ok {
				t.Error("metadata.target_truncated is present on an untruncated row; " +
					"an auditor would think evidence was dropped when none was")
			}
			if _, ok := e.Metadata["target_original_len"]; ok {
				t.Error("metadata.target_original_len is present on an untruncated row")
			}
		})
	}
}
