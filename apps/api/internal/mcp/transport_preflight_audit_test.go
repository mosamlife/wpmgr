package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
)

// ---------------------------------------------------------------------------
// PROOF -- THE PREFLIGHT ADVERTISES EVERY VERB THE ROUTER ROUTES.
//
// The failure this guards is one the server cannot see. Every verb in
// methodNotAllowedVerbs is routed to a deliberately readable 405 that says
// "this method is not supported here" instead of gin's bare 404. A verb
// missing from the preflight never reaches that answer: the browser refuses
// its OWN request before it is sent, and the client surfaces an opaque network
// error indistinguishable from "the server is down". Our logs show nothing,
// because nothing arrived.
//
// So the two lists have to agree, and this asserts the agreement BY VALUE
// against methodNotAllowedVerbs rather than against a written-out list of
// verbs. A hard-coded list here would have to be edited in lockstep with the
// routing to stay meaningful -- which is the same drift the production code
// just stopped being able to have, reintroduced in the test that exists to
// catch it.
// ---------------------------------------------------------------------------

// allowedPreflightMethods parses the Access-Control-Allow-Methods header into a
// set. It parses rather than substring-matching: strings.Contains cannot tell
// an advertised verb from one that merely appears inside another token, and
// this assertion is the whole point of the test.
func allowedPreflightMethods(t *testing.T, h string) map[string]bool {
	t.Helper()
	set := map[string]bool{}
	for _, part := range strings.Split(h, ",") {
		if v := strings.TrimSpace(part); v != "" {
			set[v] = true
		}
	}
	if len(set) == 0 {
		t.Fatalf("Access-Control-Allow-Methods = %q parsed to no verbs at all", h)
	}
	return set
}

func TestTransport_PreflightAdvertisesEveryRoutedVerb(t *testing.T) {
	r := newTransportRouter(t, liveGrantStore(uuid.New()))

	req := httptest.NewRequest(http.MethodOptions, TransportPath, nil)
	req.Header.Set("Origin", "https://some-mcp-client.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight answered %d, want 204 — the rest of this test is vacuous", w.Code)
	}

	allowed := allowedPreflightMethods(t, w.Header().Get("Access-Control-Allow-Methods"))

	// POST is the verb that actually works, so it must be advertised.
	if !allowed[http.MethodPost] {
		t.Errorf("Access-Control-Allow-Methods = %v, missing POST — the endpoint is unusable", allowed)
	}

	// And every verb routed to a 405, BY VALUE against the routed list. This is
	// the assertion that catches a verb added to one list and not the other.
	if len(methodNotAllowedVerbs) == 0 {
		t.Fatal("methodNotAllowedVerbs is empty; this test would assert nothing")
	}
	for _, verb := range methodNotAllowedVerbs {
		if !allowed[verb] {
			t.Errorf("routed verb %s answers a readable 405 but is NOT advertised in "+
				"Access-Control-Allow-Methods (%v) — a browser blocks it before the 405 can be read",
				verb, allowed)
		}
	}
}

// TestTransport_EveryAdvertisedVerbIsRoutedOrPost is the CONVERSE, and it is
// what stops this being fixed by advertising everything.
//
// Access-Control-Allow-Methods is a claim about what the browser may attempt.
// Advertising a verb nothing routes would earn back the bare 404 the
// 405-not-404 rule exists to eliminate, one layer further out and harder to
// diagnose. Every advertised verb must therefore be POST or a routed 405 verb.
func TestTransport_EveryAdvertisedVerbIsRoutedOrPost(t *testing.T) {
	r := newTransportRouter(t, liveGrantStore(uuid.New()))

	req := httptest.NewRequest(http.MethodOptions, TransportPath, nil)
	req.Header.Set("Origin", "https://some-mcp-client.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	routed := map[string]bool{http.MethodPost: true}
	for _, verb := range methodNotAllowedVerbs {
		routed[verb] = true
	}

	for verb := range allowedPreflightMethods(t, w.Header().Get("Access-Control-Allow-Methods")) {
		if !routed[verb] {
			t.Errorf("Access-Control-Allow-Methods advertises %s, which nothing routes — "+
				"a browser would be invited to send a request that answers 404", verb)
		}
	}
}

// TestTransport_AdvertisedVerbsStillRefuse is the OVER-FIRE proof for the
// preflight change.
//
// Widening the advertisement must buy the caller exactly one thing: the right
// to SEND a request that will be honestly refused. It must not make any
// currently-refused request succeed. Every routed verb still answers 405 with
// its Allow header, and an unauthenticated POST is still 401 — asserted by
// value, not as "an error occurred".
func TestTransport_AdvertisedVerbsStillRefuse(t *testing.T) {
	r := newTransportRouter(t, liveGrantStore(uuid.New()))

	for _, verb := range methodNotAllowedVerbs {
		t.Run(verb+" still answers 405", func(t *testing.T) {
			req := httptest.NewRequest(verb, TransportPath, nil)
			req.Header.Set("Origin", "https://some-mcp-client.example")
			req.Header.Set("Authorization", "Bearer "+testBearer)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s answered %d, want 405 — the preflight change let a refused verb through\nbody: %s",
					verb, w.Code, w.Body.String())
			}
			if got := w.Header().Get("Allow"); !strings.Contains(got, http.MethodPost) {
				t.Errorf("%s 405 does not name the accepted verb: Allow = %q", verb, got)
			}
		})
	}

	t.Run("unauthenticated POST is still 401 by value", func(t *testing.T) {
		unauth := newTransportRouter(t, &fakeStore{}) // tokenOK false: nothing resolves
		req := httptest.NewRequest(http.MethodPost, TransportPath,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Origin", "https://some-mcp-client.example")
		w := httptest.NewRecorder()
		unauth.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("HTTP %d, want 401 — the preflight change weakened authentication\nbody: %s",
				w.Code, w.Body.String())
		}
		if got := w.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
			t.Errorf("401 does not name the scheme: WWW-Authenticate = %q", got)
		}
	})
}

// ---------------------------------------------------------------------------
// PROOF -- THE AUDIT GAP LINE CARRIES THE LOST ROW'S REASON AND ITS PHASE.
//
// When RecordProtocolDenied's append fails there is no durable row, and this
// log line is the ENTIRE surviving record of the refusal. It was being passed
// the phase in the reason slot, so it claimed refusal_reason=header, lost
// below_floor altogether, and recorded the phase nowhere. A fallback that
// carries the wrong half of the row it stands in for is worse than a
// mislabelled field: an operator reading it back learns a refusal happened for
// a reason that is not a reason.
//
// Both values are asserted BY VALUE and separately, and the test also asserts
// they are DISTINCT — which is what fails if the two arguments are ever
// transposed again.
// ---------------------------------------------------------------------------

// failingRecorder implements auditRecorder and refuses every append. It is the
// broken-audit condition the gap path exists for, without needing a broken
// Postgres to produce it.
type failingRecorder struct{ err error }

func (f *failingRecorder) Record(context.Context, audit.Event) (audit.Entry, error) {
	return audit.Entry{}, f.err
}

func (f *failingRecorder) RecordInTx(context.Context, pgx.Tx, audit.Event) (audit.Entry, error) {
	return audit.Entry{}, f.err
}

// gapRouter builds the real mount with a recorder that always fails and a
// logger that captures JSON, so the emitted gap line can be read back field by
// field.
func gapRouter(t *testing.T) (*gin.Engine, *bytes.Buffer) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	svc := NewService(liveGrantStore(uuid.New())).
		withAuditRecorder(&failingRecorder{err: errors.New("audit chain unavailable")})

	r := gin.New()
	NewTransportHandler(svc, log, "test-version").Register(r)
	return r, &buf
}

// findGapLine returns the single captured "mcp refusal audit write failed"
// record. It fails loudly when there is none: a test that quietly found no log
// line and passed would be exactly the "guard that finds nothing goes green"
// defect this suite is meant not to have.
func findGapLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["msg"] == "mcp refusal audit write failed" {
			found = append(found, rec)
		}
	}
	if len(found) == 0 {
		t.Fatalf("no audit-gap log line was emitted; the fallback did not run\ncaptured:\n%s", buf.String())
	}
	if len(found) > 1 {
		t.Fatalf("expected exactly 1 audit-gap line, got %d\ncaptured:\n%s", len(found), buf.String())
	}
	return found[0]
}

func TestTransport_AuditGapLogsReasonAndPhaseSeparately(t *testing.T) {
	cases := []struct {
		name       string
		header     string // MCP-Protocol-Version, "" to omit
		body       string
		wantReason string
		wantPhase  string
	}{
		{
			// Below the floor, refused at the per-request header.
			name:       "below floor at the header",
			header:     "2024-11-05",
			body:       `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
			wantReason: "below_floor",
			wantPhase:  "header",
		},
		{
			// No header (so the header negotiation does not refuse), an
			// unparseable revision in initialize's params.
			name:       "unsupported at the initialize params",
			header:     "",
			body:       initBody("banana"),
			wantReason: "unsupported",
			wantPhase:  "initialize_params",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, buf := gapRouter(t)

			req := httptest.NewRequest(http.MethodPost, TransportPath, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+testBearer)
			if tc.header != "" {
				req.Header.Set(ProtocolHeader, tc.header)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			// The refusal itself is UNCHANGED by the append failing. That is
			// the documented decision in auditGap, and if it ever stops being
			// true the rest of this test is measuring the wrong thing.
			if w.Code != http.StatusBadRequest {
				t.Fatalf("HTTP %d, want 400 — the refusal changed when the audit append failed\nbody: %s",
					w.Code, w.Body.String())
			}

			rec := findGapLine(t, buf)

			if got, _ := rec["audit_gap"].(bool); !got {
				t.Errorf("audit_gap = %v, want true — the alertable field is what makes this findable", rec["audit_gap"])
			}

			gotReason, _ := rec["refusal_reason"].(string)
			if gotReason != tc.wantReason {
				t.Errorf("refusal_reason = %q, want %q — the lost row's REASON is not in the fallback",
					gotReason, tc.wantReason)
			}

			gotPhase, _ := rec["phase"].(string)
			if gotPhase != tc.wantPhase {
				t.Errorf("phase = %q, want %q — the lost row's PHASE is recorded under its own key or nowhere",
					gotPhase, tc.wantPhase)
			}

			// The transposition check. reason and phase answer different
			// questions, and passing one where the other belongs is the exact
			// defect this test was written for.
			if gotReason == gotPhase {
				t.Errorf("refusal_reason and phase are both %q — the two arguments are transposed or duplicated",
					gotReason)
			}

			// The fallback must agree with the row it stands in for. Deriving
			// the expectation from the same function the durable row uses is
			// what keeps the two from drifting apart later.
			neg := NegotiateProtocol(tc.header)
			if tc.wantPhase == "initialize_params" {
				neg = NegotiateProtocol("banana")
			}
			if want := neg.RefusalReason(); gotReason != want {
				t.Errorf("refusal_reason = %q but Negotiation.RefusalReason() = %q — "+
					"the fallback describes a different refusal from the audit row", gotReason, want)
			}
		})
	}
}

// TestTransport_AuditGapOmitsAnEmptyPhase keeps the tool-denial call site
// honest: a refusal with no phase must not log phase="", because an alert or a
// query keyed on the field would then match a blank and report a phase that
// does not exist.
func TestTransport_AuditGapOmitsAnEmptyPhase(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	h := NewTransportHandler(NewService(&fakeStore{}), log, "test-version")

	h.auditGap(context.Background(),
		AuthorizedRequest{TenantID: uuid.New(), GrantID: uuid.New()},
		"mcp.tool.denied", "sites_list", "unregistered", "", errors.New("boom"))

	rec := findGapLine(t, &buf)

	if got, ok := rec["phase"]; ok {
		t.Errorf("phase = %v, want the key ABSENT for a refusal that has no phase", got)
	}
	if got, _ := rec["refusal_reason"].(string); got != "unregistered" {
		t.Errorf("refusal_reason = %q, want %q", got, "unregistered")
	}
}
