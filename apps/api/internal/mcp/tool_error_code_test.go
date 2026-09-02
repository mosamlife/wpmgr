package mcp

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/govcontext"
)

// errContextStoreDown is the infra failure behind a context_unavailable
// refusal. Its text is asserted ABSENT from the wire below.
var errContextStoreDown = errors.New("dial tcp 10.0.0.4:5432: connect: connection refused")

// wireError is the JSON-RPC error object AS IT ARRIVES, decoded from response
// bytes rather than read off a Go value.
//
// THE DECODE IS THE POINT. The defect class here is the wire shape disagreeing
// with the type that produced it: a domain error carrying one code and one
// message, arriving under a different code with a different message. An
// assertion on a struct field upstream of json.Marshal cannot see that, which
// is how this mapping shipped the same defect twice.
type wireError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func decodeToolError(t *testing.T, body string) wireError {
	t.Helper()
	var resp struct {
		Error *wireError `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode JSON-RPC response: %v\nbody: %s", err, body)
	}
	if resp.Error == nil {
		t.Fatalf("expected a JSON-RPC error, got a result: %s", body)
	}
	return *resp.Error
}

// toolErrorWire runs one error through the production mapping and returns the
// wire object, marshalled and re-decoded so the assertion sees bytes.
func toolErrorWire(t *testing.T, err error) wireError {
	t.Helper()
	h := NewTransportHandler(NewService(&fakeStore{}), slog.New(slog.DiscardHandler), "test-version")
	enc, merr := json.Marshal(h.toolError(json.RawMessage(`1`), err))
	if merr != nil {
		t.Fatalf("marshal response: %v", merr)
	}
	return decodeToolError(t, string(enc))
}

// TestToolError_ServerSideFailuresAreNotBlamedOnTheCallersParams pins the code
// half of the mapping for both governed-context refusals, end to end through
// the mounted route.
//
// -32602 IS "INVALID PARAMS", AND NEITHER OF THESE IS ABOUT THE CALLER'S
// PARAMS. A client told its arguments were wrong does the only sensible thing
// and retries with different arguments, forever, against a server-side fault
// its arguments cannot reach.
func TestToolError_ServerSideFailuresAreNotBlamedOnTheCallersParams(t *testing.T) {
	unavailable := orgContextStore(operatorGuidance)
	unavailable.orgErr = errContextStoreDown

	for _, tc := range []struct {
		name     string
		ctxStore *fakeContextStore
	}{
		{"context_unavailable", unavailable},
		{"context_too_large", orgContextStore(
			strings.Repeat("x", govcontext.MaxDeliverableInstructionBytes+1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := post(t, routerWithContext(t, scopedStoreWithOneSite(), tc.ctxStore), sitesListCall, nil)
			got := decodeToolError(t, w.Body.String())
			if got.Code == codeInvalidParams {
				t.Errorf("a server-side failure arrived as %d (invalid params), telling the client its "+
					"arguments were wrong when they were not; want %d (internal): %+v",
					codeInvalidParams, codeInternalError, got)
			}
			if got.Code != codeInternalError {
				t.Errorf("wire code = %d, want %d (internal)", got.Code, codeInternalError)
			}
		})
	}
}

// TestToolError_ContextTooLargeCarriesTheSizeAndTheLimit is the message half
// for the code whose content is the OPERATOR'S OWN. The size and the limit are
// the operator's own text measured against a constant we publish anyway, so
// naming them crosses no trust boundary — and a number the caller can act on
// is the whole value of the refusal.
func TestToolError_ContextTooLargeCarriesTheSizeAndTheLimit(t *testing.T) {
	oversized := strings.Repeat("x", govcontext.MaxDeliverableInstructionBytes+1)
	w := post(t, routerWithContext(t, scopedStoreWithOneSite(), orgContextStore(oversized)), sitesListCall, nil)
	got := decodeToolError(t, w.Body.String())

	limit := strconv.Itoa(govcontext.MaxDeliverableInstructionBytes)
	if !strings.Contains(got.Message, limit) {
		t.Errorf("the refusal does not name the limit the operator must write under: %+v", got)
	}
	if len(got.Data) == 0 {
		t.Fatalf("the refusal carries no machine-readable size: %+v", got)
	}
	var data map[string]any
	if err := json.Unmarshal(got.Data, &data); err != nil {
		t.Fatalf("decode error data: %v", err)
	}
	if _, ok := data["instruction_bytes"]; !ok {
		t.Errorf("error data does not carry the ACTUAL size, only the limit: %v", data)
	}
	if _, ok := data["budget_bytes"]; !ok {
		t.Errorf("error data does not carry the limit: %v", data)
	}
	if data["code"] != govcontext.ErrCodeContextTooLarge {
		t.Errorf("error data code = %v, want %q", data["code"], govcontext.ErrCodeContextTooLarge)
	}
}

// TestToolError_ContextUnavailableCarriesNoInternalDetail is the message half
// for the code whose content is OURS.
//
// The fork this test and the one above encode is not "how bad is it" but WHOSE
// INFORMATION THE MESSAGE CARRIES. A missing resolver or a failed read is our
// internal state, and putting it on the wire hands any token holder a polled
// read on it in English — the same argument the audit-unavailable branch
// makes. So this one is generic, byte-identical to every other internal
// failure.
func TestToolError_ContextUnavailableCarriesNoInternalDetail(t *testing.T) {
	ctxStore := orgContextStore(operatorGuidance)
	ctxStore.orgErr = errContextStoreDown

	w := post(t, routerWithContext(t, scopedStoreWithOneSite(), ctxStore), sitesListCall, nil)
	got := decodeToolError(t, w.Body.String())

	for _, leak := range []string{
		"context",                   // the subsystem that failed
		"organisation",              // what it was resolving
		"resolver",                  // the wiring
		"5432",                      // the cause, verbatim
		errContextStoreDown.Error(), // the whole of it
	} {
		if strings.Contains(strings.ToLower(got.Message), strings.ToLower(leak)) {
			t.Errorf("the refusal message names our internal state (%q), handing a third-party client "+
				"an English read on it: %+v", leak, got)
		}
	}
	if len(got.Data) != 0 {
		t.Errorf("the refusal carries structured internal detail: %s", got.Data)
	}
	// Byte-identical to every other internal failure, so the message is not
	// itself the oracle it was stopped from being.
	if got.Message != genericToolFailure {
		t.Errorf("message = %q, want %q", got.Message, genericToolFailure)
	}
}

// TestToolError_UnrecognisedServerCodeDefaultsToInternal is the fail-closed
// proof, and the one that stops the NEXT occurrence rather than this one.
//
// mcp_capability_unmapped is a real code, documented at its definition in
// policy.go as "an internal misconfiguration, not a caller error". Before this
// change it took the unsafe fallthrough exactly as the two context codes did —
// a third live instance of the same defect, fixed here by inverting the
// default rather than by adding a third string to a list.
func TestToolError_UnrecognisedServerCodeDefaultsToInternal(t *testing.T) {
	got := toolErrorWire(t, domain.Forbidden(ErrCodeCapabilityUnmapped,
		"scope mcp:sites:read confers no capability"))
	if got.Code != codeInternalError {
		t.Errorf("an unenumerated server-side code arrived as %d, not %d: the mapping still defaults "+
			"to blaming the caller", got.Code, codeInternalError)
	}
	if got.Message != genericToolFailure {
		t.Errorf("message = %q, want %q", got.Message, genericToolFailure)
	}
}

// TestToolError_CallerFaultsStillNameThemselves is the not-over-fire half.
// Inverting the default must not turn a genuine caller mistake into an opaque
// internal error: a model that guessed a tool name has to be told the name was
// wrong, or it cannot correct on the next round trip.
func TestToolError_CallerFaultsStillNameThemselves(t *testing.T) {
	got := toolErrorWire(t, domain.Forbidden(ErrCodeToolNotAvailable,
		`no tool named "fleet_sites_lst" exists; see tools/list`))
	if got.Code != codeInvalidParams {
		t.Errorf("a caller-caused refusal arrived as %d, want %d: a client cannot correct what it is "+
			"not told is its own mistake", got.Code, codeInvalidParams)
	}
	if !strings.Contains(got.Message, "fleet_sites_lst") {
		t.Errorf("the refusal does not name the tool the caller guessed: %+v", got)
	}
}
