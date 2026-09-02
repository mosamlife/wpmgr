package mcp

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/govcontext"
)

// ---------------------------------------------------------------------------
// A governed-context refusal is a DENIAL, and ADR-061 A10 lists denials:
// "every AI-originated read, proposal, approval, denial and execution".
//
// These two refusals happen AFTER the authorization gate has said yes, which is
// why they were not covered by the RecordToolDenied branch in authorizeCall:
// the gate passed, the tool ran, and the refusal came from resolving the
// operator's own context. The caller got a refused tool call and the ledger got
// nothing.
//
// EVERY ASSERTION BELOW READS THE EVENT THE RECORDER WAS HANDED, never "was a
// function called". A mock-call assertion here would prove that callTool
// reaches RecordToolDenied and nothing about what it reaches it with -- and the
// content is the whole value of the row, because the reason code is the ONE
// place context_unavailable's cause is written down at all (the wire says
// nothing about it, deliberately).
// ---------------------------------------------------------------------------

// contextRefusalRouter mounts the real transport over the real resolver, with a
// capturing recorder, so a test reads back exactly the row that would have been
// written. It is routerWithContext plus a handle on the recorder.
func contextRefusalRouter(t *testing.T, ctxStore govcontext.ContextStore) (*gin.Engine, *capturingRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	rec := &capturingRecorder{}
	svc := NewService(scopedStoreWithOneSite()).withAuditRecorder(rec).
		WithContextResolver(&govcontext.Resolver{Store: ctxStore})

	r := gin.New()
	NewTransportHandler(svc, slog.New(slog.DiscardHandler), "test-version").Register(r)
	return r, rec
}

// unresolvableContext is the store behind a context_unavailable refusal: the
// org read fails for an infra reason.
func unresolvableContext() *fakeContextStore {
	s := orgContextStore(operatorGuidance)
	s.orgErr = errContextStoreDown
	return s
}

// oversizedContext is the store behind a context_too_large refusal: the
// operator authored more than ModelInstructions will deliver.
func oversizedContext() *fakeContextStore {
	return orgContextStore(strings.Repeat("x", govcontext.MaxDeliverableInstructionBytes+1))
}

// TestContextRefusal_WritesTheDenialRow is the load-bearing pair. For each of
// the two codes: the caller is refused AND a durable denial event exists naming
// the tool and the reason.
//
// Both halves are asserted together on purpose. "The caller was refused" alone
// passes on the behaviour this test exists to reject, and "a row was written"
// alone would pass on a surface that recorded a denial and then answered.
func TestContextRefusal_WritesTheDenialRow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ctxStore *fakeContextStore
		reason   string
	}{
		{"context_unavailable", unresolvableContext(), ErrCodeContextUnavailable},
		{"context_too_large", oversizedContext(), govcontext.ErrCodeContextTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, rec := contextRefusalRouter(t, tc.ctxStore)
			w := post(t, r, sitesListCall, nil)
			body := w.Body.String()

			// The refusal still reaches the caller.
			resp := decodeRPC(t, w)
			if resp.Error == nil {
				t.Fatalf("a governed-context refusal returned a result: %s", body)
			}

			// AND the durable event exists. only() fails loudly on zero rows,
			// so "recorded nothing" cannot pass as "recorded correctly".
			e := rec.only(t, audit.ActionMCPToolDenied)
			if e.TargetID != ToolFleetSitesList {
				t.Errorf("recorded target = %q, want %q", e.TargetID, ToolFleetSitesList)
			}
			if got := e.Metadata["refusal_reason"]; got != tc.reason {
				t.Errorf("recorded refusal_reason = %v, want %q — the row is the only place "+
					"this cause is written down", got, tc.reason)
			}
			if e.ActorType != audit.ActorAssistant {
				t.Errorf("actor type = %q, want %q: a query on actor_id must return what a "+
					"connection did and what it was refused together", e.ActorType, audit.ActorAssistant)
			}
			// No mcp.tool.called row: a refusal is not a call, and a ledger
			// claiming a read that never happened is worse than a silent one.
			for _, ev := range rec.events {
				if ev.Action == audit.ActionMCPToolCalled {
					t.Errorf("a refused call also wrote %s", audit.ActionMCPToolCalled)
				}
			}
		})
	}
}

// TestContextRefusal_WireStillCarriesOnlyWhatItMayCarry is the not-over-fire
// half of the disclosure rule. Adding a row must not have moved detail onto the
// wire: context_too_large still names the operator's own numbers, and
// context_unavailable still names nothing of ours.
func TestContextRefusal_WireStillCarriesOnlyWhatItMayCarry(t *testing.T) {
	t.Run("too_large still names the size and the limit", func(t *testing.T) {
		r, _ := contextRefusalRouter(t, oversizedContext())
		got := decodeToolError(t, post(t, r, sitesListCall, nil).Body.String())
		if len(got.Data) == 0 {
			t.Fatalf("the actionable refusal lost its machine-readable detail: %+v", got)
		}
	})

	t.Run("unavailable still names nothing of ours", func(t *testing.T) {
		r, _ := contextRefusalRouter(t, unresolvableContext())
		got := decodeToolError(t, post(t, r, sitesListCall, nil).Body.String())
		if got.Message != genericToolFailure {
			t.Errorf("message = %q, want %q", got.Message, genericToolFailure)
		}
		if strings.Contains(got.Message, errContextStoreDown.Error()) || len(got.Data) != 0 {
			t.Errorf("internal state reached the wire: %+v", got)
		}
	})
}

// TestContextRefusal_UnrecordableRefusalIsNotASilentSuccess is the fail-closed
// arm. When the append itself fails while recording a context refusal, the call
// must not come back as a success, and it must not answer a second,
// differently-shaped error that hides the first.
//
// The assertion on genericToolFailure is what pins "no second confusing error":
// the answer is byte-identical to every other server-side fault, which is also
// why it is not a liveness oracle on the audit system.
func TestContextRefusal_UnrecordableRefusalIsNotASilentSuccess(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ctxStore *fakeContextStore
	}{
		{"context_unavailable", unresolvableContext()},
		{"context_too_large", oversizedContext()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			svc := NewService(scopedStoreWithOneSite()).
				withAuditRecorder(&failingRecorder{err: errors.New("audit chain unavailable")}).
				WithContextResolver(&govcontext.Resolver{Store: tc.ctxStore})
			r := gin.New()
			NewTransportHandler(svc, slog.New(slog.DiscardHandler), "test-version").Register(r)

			w := post(t, r, sitesListCall, nil)
			body := w.Body.String()

			resp := decodeRPC(t, w)
			if resp.Error == nil {
				t.Fatalf("an unrecordable refusal came back as a success: %s", body)
			}
			if resp.Error.Code != codeInternalError {
				t.Errorf("error code = %d, want %d (internal)", resp.Error.Code, codeInternalError)
			}
			if resp.Error.Message != genericToolFailure {
				t.Errorf("message = %q, want %q: an unrecordable refusal must not answer a "+
					"second, distinguishable error", resp.Error.Message, genericToolFailure)
			}
			for _, leak := range []string{"audit", ErrCodeAuditUnavailable} {
				if strings.Contains(strings.ToLower(body), strings.ToLower(leak)) {
					t.Errorf("the response names the audit system (%q), handing a caller a "+
						"liveness oracle\nbody: %s", leak, body)
				}
			}
		})
	}
}

// TestContextRefusal_HealthyCallStillWritesExactlyOneSuccessRow is the
// over-fire arm. A call that is NOT refused must be unaffected: the answer
// comes back and exactly one mcp.tool.called row is written -- not two, and no
// denial row alongside it.
func TestContextRefusal_HealthyCallStillWritesExactlyOneSuccessRow(t *testing.T) {
	r, rec := contextRefusalRouter(t, orgContextStore(operatorGuidance))

	w := post(t, r, sitesListCall, nil)
	body := w.Body.String()

	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d on a healthy call: %s", w.Code, body)
	}
	if !strings.Contains(body, jsonEscaped(t, operatorGuidance)) {
		t.Fatalf("the operator's context is missing from a healthy answer: %s", body)
	}

	// Exactly one, and it is the success action.
	if e := rec.only(t, audit.ActionMCPToolCalled); e.TargetID != ToolFleetSitesList {
		t.Errorf("recorded target = %q, want %q", e.TargetID, ToolFleetSitesList)
	}
	for _, ev := range rec.events {
		if ev.Action == audit.ActionMCPToolDenied {
			t.Fatalf("a successful call wrote a denial row as well; the new branch over-fires")
		}
	}
}
