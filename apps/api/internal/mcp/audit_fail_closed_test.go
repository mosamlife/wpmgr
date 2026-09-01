package mcp

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// ADR-061 A10, fail-closed: no tool answer is served whose audit record was
// lost.
//
// WHAT THESE PIN, AND WHY THE ASSERTION IS ABOUT THE BODY RATHER THAN THE CODE.
// The property is not "an unrecordable call returns an error" -- that would
// still hold if the handler serialised the tool's text and then bolted an error
// field beside it, and it would hold under a future refactor that answers the
// client first and records in a defer. The property is that the DATA THE READ
// RETURNED does not reach the client, so every test below asserts on the
// absence of the site names from the response bytes. A test that only checked
// the JSON-RPC code would go green on a surface that leaks the answer inside
// the failure.
//
// The recorder is a fake rather than a broken Postgres because that is the only
// way the failure branch is reachable at unit speed; the same property is
// re-proven against a real database, as the wpmgr_app role, in the integration
// package.
// ---------------------------------------------------------------------------

// failClosedStore is a live grant over two sites with distinctive names, so a
// leak of the tool's answer into an error response is greppable.
func failClosedStore(t *testing.T) (*fakeStore, uuid.UUID) {
	t.Helper()
	allowed := uuid.New()
	store := liveGrantStore(allowed)
	collected := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	row := siteRow(failClosedSiteName, &collected)
	row.ID = allowed
	store.sites = []sqlc.ListSitesRow{row}
	return store, allowed
}

// The name is deliberately not a substring of any JSON key, error message or
// protocol constant, so `strings.Contains` over the whole body is a sound leak
// check rather than one that trips over the envelope.
const failClosedSiteName = "zzz-canary-site"

// auditedService is NewService plus the recorder production always wires. It
// exists because "no recorder" stopped being a supported configuration: an
// unaudited Service refuses to approve, mint, revoke or serve a tool call, so a
// test that builds one is testing a shape that never ships. Tests which are
// ABOUT the unaudited case call NewService directly and assert the refusal --
// see TestToolCall_ARecorderlessServiceRefusesRatherThanServing.
func auditedService(store Store) *Service {
	return NewService(store).withAuditRecorder(&capturingRecorder{})
}

// routerWithRecorder mounts the real route over a store with the given
// recorder, so a test can swap a failing recorder for a working one without
// changing anything else about the request.
func routerWithRecorder(t *testing.T, store Store, rec auditRecorder) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	svc := NewService(store)
	if rec != nil {
		svc = svc.withAuditRecorder(rec)
	}
	NewTransportHandler(svc, slog.New(slog.DiscardHandler), "test-version").Register(r)
	return r
}

const toolCallBody = `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"list_sites","arguments":{}}}`

// TestToolCall_AnswerIsWithheldWhenTheAuditAppendFails is the red-first half:
// the append fails, and the tool's answer must not reach the client.
func TestToolCall_AnswerIsWithheldWhenTheAuditAppendFails(t *testing.T) {
	store, _ := failClosedStore(t)
	r := routerWithRecorder(t, store, &failingRecorder{err: errors.New("audit chain unavailable")})

	w := post(t, r, toolCallBody, nil)
	body := w.Body.String()

	// THE LOAD-BEARING ASSERTION. The read ran -- the store returned the row --
	// and the answer was discarded because the row recording it could not be
	// committed.
	if strings.Contains(body, failClosedSiteName) {
		t.Fatalf("the tool's answer was served despite an audit append that failed; "+
			"this is the fail-open behaviour A10 forbids\nbody: %s", body)
	}

	resp := decodeRPC(t, w)
	if resp.Error == nil {
		t.Fatalf("an unrecordable tool call returned a result: %s", body)
	}
	if resp.Error.Code != codeInternalError {
		t.Errorf("error code = %d, want %d (internal) — an unrecordable call must not be "+
			"reported as the caller's fault", resp.Error.Code, codeInternalError)
	}
	// The wire must not say WHICH failure this was: that is a poll on whether
	// the audit system is up. See toolError's ErrCodeAuditUnavailable branch.
	for _, leak := range []string{"audit", ErrCodeAuditUnavailable} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(leak)) {
			t.Errorf("the response names the audit system (%q), handing a caller a "+
				"liveness oracle\nbody: %s", leak, body)
		}
	}
}

// TestToolCall_HealthyAuditStillAnswersAndWritesExactlyOneRow is the restore
// half, and the over-fire check: the same request, the same store, a recorder
// that works. The answer comes back AND exactly one mcp.tool.called row is
// written -- not zero (the fail-closed change did not quietly stop recording)
// and not two (the record is not written on both sides of the gate).
func TestToolCall_HealthyAuditStillAnswersAndWritesExactlyOneRow(t *testing.T) {
	store, _ := failClosedStore(t)
	rec := &capturingRecorder{}
	r := routerWithRecorder(t, store, rec)

	w := post(t, r, toolCallBody, nil)
	body := w.Body.String()

	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d on a healthy audit path: %s", w.Code, body)
	}
	if !strings.Contains(body, failClosedSiteName) {
		t.Fatalf("the tool's answer is missing on a healthy audit path: %s", body)
	}

	// only() fails loudly when there is no row, so "recorded nothing" cannot
	// pass as "recorded correctly".
	e := rec.only(t, audit.ActionMCPToolCalled)
	if e.TargetID != ToolListSites {
		t.Errorf("recorded target = %q, want %q", e.TargetID, ToolListSites)
	}
}

// TestToolCall_ARecorderlessServiceRefusesRatherThanServing pins item 3: a
// Service built without WithAudit cannot record, so it cannot answer.
//
// This used to be a SUPPORTED configuration -- the audit field's doc called it
// "simply does not audit" and every unit test in this package relied on it --
// and it is the exact shape of this codebase's signature defect: a surface that
// presents as working while the guarantee it advertises is switched off. A
// wiring mistake now fails at the first request instead of at the first audit.
func TestToolCall_ARecorderlessServiceRefusesRatherThanServing(t *testing.T) {
	store, _ := failClosedStore(t)
	r := routerWithRecorder(t, store, nil) // no WithAudit, no fake: nil recorder

	w := post(t, r, toolCallBody, nil)
	body := w.Body.String()

	if strings.Contains(body, failClosedSiteName) {
		t.Fatalf("an UNAUDITED service served a tool answer; every call it handles would "+
			"be an AI action with no record\nbody: %s", body)
	}
	resp := decodeRPC(t, w)
	if resp.Error == nil {
		t.Fatalf("an unaudited service returned a result: %s", body)
	}
	if resp.Error.Code != codeInternalError {
		t.Errorf("error code = %d, want %d (internal)", resp.Error.Code, codeInternalError)
	}
}

// TestRecordToolCall_PropagatesTheAppendFailure keeps the service-level
// contract separate from the transport's use of it. If this returned nil on a
// failed append, callTool's gate would be unreachable and every test above
// would still pass for the wrong reason.
func TestRecordToolCall_PropagatesTheAppendFailure(t *testing.T) {
	boom := errors.New("audit chain unavailable")
	svc := NewService(&fakeStore{}).withAuditRecorder(&failingRecorder{err: boom})

	auth := authWith(CapabilitySet{}, uuid.New())
	err := svc.RecordToolCall(t.Context(), auth, ToolListSites, "read")
	if err == nil {
		t.Fatal("RecordToolCall swallowed a failed append; the fail-closed gate above it " +
			"can never fire")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want it to wrap the append failure", err)
	}
}

// TestRecordToolCall_RecorderlessIsAnErrorNotASkip is the service-level half of
// item 3, for the same reason as the test above.
func TestRecordToolCall_RecorderlessIsAnErrorNotASkip(t *testing.T) {
	svc := NewService(&fakeStore{})

	auth := authWith(CapabilitySet{}, uuid.New())
	if err := svc.RecordToolCall(t.Context(), auth, ToolListSites, "read"); err == nil {
		t.Fatal("a Service with no recorder reported success for a row it did not write")
	}
}

// TestMint_RESTWireDoesNotCarryTheAuditMessage answers, by execution, what the
// REST surface does with an audit append that fails inside RecordInTx.
//
// THE THREE RecordInTx SITES DO NOT GO THROUGH auditFailure. Approve
// (service.go), RevokeConnection (service.go) and MintConnection (mint.go)
// return the recorder's own error under fmt.Errorf("...%w"), so what reaches
// httpx.Error is internal/audit's own typed domain error -- audit_insert_failed,
// audit_lock_failed, and the rest -- not this package's
// ErrCodeAuditUnavailable. domain.AsDomain uses errors.As, so the %w wrapper is
// transparent and the audit error IS what the mapping sees.
//
// That is the same starting position as the JSON-RPC leak, and this test exists
// because the two mappings do NOT behave the same way and the difference is the
// entire answer to the question. httpx.Error suppresses de.Message for
// domain.KindInternal, which every audit error is; it copies de.Code
// unconditionally. So the human-readable "failed to append audit entry" cannot
// reach a REST client, and the machine-readable code can.
//
// The assertion is written as "the message must not appear" rather than "the
// body must equal X" on purpose: the code being visible is a judged, accepted
// outcome (see the report on finding #4) and pinning the whole envelope would
// turn a deliberate decision into a tripwire that fires when someone revisits
// it. The message leaking is not judged -- it is the defect -- so that is what
// is pinned.
func TestMint_RESTWireDoesNotCarryTheAuditMessage(t *testing.T) {
	// The REAL shape internal/audit returns from a refused INSERT: a typed
	// domain error of Kind internal. errors.New would take a different branch
	// in httpx.Error and prove nothing about the case that actually happens --
	// which is precisely how the -32602 defect survived the unit suite.
	realAppendFailure := domain.Internal("audit_insert_failed", "failed to append audit entry")

	tenant := uuid.New()
	gin.SetMode(gin.TestMode)
	eng := gin.New()
	g := eng.Group(APIV1Prefix)
	g.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), orgPrincipal(tenant)))
		c.Next()
	})
	svc := NewService(&fakeStore{}).withAuditRecorder(&failingRecorder{err: realAppendFailure})
	NewHandler(svc).RegisterConnections(g)

	var h http.Handler = eng
	status, body := postMint(t, h, map[string]any{"name": "audit-leak-probe", "site_scope_mode": "all"})

	raw, _ := json.Marshal(body)
	t.Logf("REST mint with a failing audit append: HTTP %d body=%s", status, raw)

	if status == http.StatusCreated {
		t.Fatal("the mint SUCCEEDED while its audit append failed; the RecordInTx " +
			"callback is not propagating and the credential exists unrecorded")
	}
	if strings.Contains(string(raw), "failed to append audit entry") {
		t.Errorf("the audit system's own message reached the REST wire: %s", raw)
	}
	if msg, _ := body["message"].(string); msg != "internal server error" {
		t.Errorf("message = %q, want the generic %q — httpx.Error's KindInternal "+
			"guard is what suppresses the audit detail, and this pins it",
			msg, "internal server error")
	}
}
