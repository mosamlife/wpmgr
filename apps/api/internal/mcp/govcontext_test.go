package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/govcontext"
)

// noOperatorContext is what the older result-rendering tests pass for the
// governed-context parameter: an organisation that has authored nothing. It is
// a named constant rather than a bare "" so those call sites say WHICH state
// they are in — "this org wrote no context", not "this argument is unset".
// buildListSitesResult/buildUpdatesPendingResult are byte-for-byte unchanged
// in that state, which is why every one of those assertions still holds.
const noOperatorContext = ""

// fakeContextStore is a govcontext.ContextStore whose org read can be made to
// fail on demand. It is deliberately the STORE and not the resolver: a fake
// resolver would let this file assert about a second implementation of
// resolution, which is the exact thing ADR-064 Decision 8 forbids and which
// this change exists to avoid. Faking the store means every test below runs
// the real Resolve.
type fakeContextStore struct {
	org    govcontext.Snapshot
	orgOK  bool
	orgErr error

	// siteCalls counts LatestSiteSnapshot calls. A fleet-wide resolution must
	// never read a site row; this counter is what proves that rather than
	// assuming it.
	siteCalls int
}

func (f *fakeContextStore) LatestOrgSnapshot(context.Context, uuid.UUID) (govcontext.Snapshot, bool, error) {
	if f.orgErr != nil {
		return govcontext.Snapshot{}, false, f.orgErr
	}
	return f.org, f.orgOK, nil
}

func (f *fakeContextStore) LatestSiteSnapshot(context.Context, uuid.UUID, uuid.UUID) (govcontext.Snapshot, bool, error) {
	f.siteCalls++
	return govcontext.Snapshot{}, false, nil
}

// operatorGuidance is the sentence an operator types into the editor. Every
// test below asserts on THIS string, because the question the feature answers
// is "does what the operator wrote reach the model", and nothing weaker.
const operatorGuidance = "Never recommend deactivating a plugin without naming the site it is on."

// forbiddenTool is the restriction half of the same question. A deny-list that
// does not arrive is the half with teeth.
const forbiddenTool = "site_delete"

func orgContextStore(guidance string) *fakeContextStore {
	return &fakeContextStore{
		orgOK: true,
		org: govcontext.Snapshot{
			Restrictions: govcontext.RestrictionSet{ForbiddenTools: []string{forbiddenTool}},
			Guidance:     govcontext.GuidanceSet{BrandVoice: guidance},
		},
	}
}

// scopedStoreWithOneSite is liveGrantStore plus a single in-scope site, the
// minimum a fleet tool needs in order to produce an answer at all — which is
// what the refusal tests must have, or they would pass on an empty fleet.
func scopedStoreWithOneSite() *fakeStore {
	allowed := uuid.New()
	store := liveGrantStore(allowed)
	row := siteRow("in-scope", nil)
	row.ID = allowed
	store.sites = append(store.sites[:0], row)
	return store
}

// emptyContextResolver is the real resolver over an organisation that has
// authored nothing. Every pre-existing tool test mounts it, because a Service
// with NO resolver now refuses (see Service.context), and a test suite whose
// calls are all refused for an unrelated reason proves nothing about what it
// was written to prove — the same argument the toolcall-limit helper already
// makes about the audit recorder.
//
// It is the empty org rather than a stub returning "": with nothing authored,
// withOperatorContext adds no bytes, so every one of those tests asserts on a
// byte-for-byte unchanged result.
func emptyContextResolver() *govcontext.Resolver {
	return &govcontext.Resolver{Store: &fakeContextStore{}}
}

// routerWithContext mounts the real transport over store, with the real
// resolver over ctxStore. It is newTransportRouterWithAudit plus the resolver,
// kept separate so the existing transport tests keep their exact wiring.
func routerWithContext(t *testing.T, store Store, ctxStore govcontext.ContextStore) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	svc := NewService(store).withAuditRecorder(&capturingRecorder{}).
		WithContextResolver(&govcontext.Resolver{Store: ctxStore})
	NewTransportHandler(svc, slog.New(slog.DiscardHandler), "test-version").Register(r)
	return r
}

// jsonEscaped is the form a string takes INSIDE a JSON-RPC response body, and
// every assertion that searches the raw body for text containing quotes must
// go through it.
//
// This is not a nicety. The first version of the fallback assertion below
// searched the body for listSitesInstructions verbatim; that constant contains
// `"never_collected"`, which appears in the response as `\"never_collected\"`,
// so the substring never matched and the assertion could not fire — it was
// discovered only because the planted fallback mutation reddened the SECOND
// assertion instead of the first. An assertion that cannot fail is worse than
// no assertion, because it reads as protection.
func jsonEscaped(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("json-escape %q: %v", s, err)
	}
	return string(b[1 : len(b)-1]) // drop the surrounding quotes
}

const sitesListCall = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fleet_sites_list","arguments":{}}}`
const updatesCall = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fleet_updates_pending","arguments":{}}}`

// TestFleetTools_OperatorContextReachesTheModel is the whole point of this
// change, asserted end to end through the mounted route: an operator writes a
// sentence in WPMgr, and the model's tool result contains that sentence.
//
// Before this change it did not. internal/mcp did not import govcontext at
// all, so the two compiled-in instruction constants were the entire header and
// an organisation's authored context reached nothing.
func TestFleetTools_OperatorContextReachesTheModel(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"fleet_sites_list", sitesListCall},
		{"fleet_updates_pending", updatesCall},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctxStore := orgContextStore(operatorGuidance)
			w := post(t, routerWithContext(t, scopedStoreWithOneSite(), ctxStore), tc.body, nil)

			if w.Code != http.StatusOK {
				t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if !strings.Contains(body, operatorGuidance) {
				t.Errorf("the operator's context never reached the model.\nwrote:  %q\nresult: %s",
					operatorGuidance, body)
			}
			if !strings.Contains(body, forbiddenTool) {
				t.Errorf("the operator's forbidden-tools list never reached the model: %s", body)
			}
			// FLEET-WIDE MEANS ORGANISATION SCOPE. No site row may be read for
			// a question about the whole fleet: there is no single site whose
			// overrides could apply, and reading one would hand the model one
			// site's policy as though it governed all of them.
			if ctxStore.siteCalls != 0 {
				t.Errorf("a fleet-wide tool read %d site context row(s); it must resolve at organisation scope only",
					ctxStore.siteCalls)
			}
		})
	}
}

// TestFleetTools_RefuseWhenOperatorContextCannotLoad is the fallback proof.
//
// THE MUTATION THIS EXISTS TO CATCH is the one this project has shipped
// repeatedly: on a load failure, quietly carry on with the compiled-in
// default. The assertions are ordered so a fallback trips the FIRST one — the
// header text — rather than being caught incidentally by a later check.
func TestFleetTools_RefuseWhenOperatorContextCannotLoad(t *testing.T) {
	ctxStore := orgContextStore(operatorGuidance)
	ctxStore.orgErr = errors.New("context store unreachable")

	w := post(t, routerWithContext(t, scopedStoreWithOneSite(), ctxStore), sitesListCall, nil)
	body := w.Body.String()

	// FIRST, AND DELIBERATELY FIRST: the compiled-in instructions must not
	// have been served in place of the operator's. This is the assertion the
	// fallback mutation trips, and its message names what was dropped.
	if strings.Contains(body, jsonEscaped(t, listSitesInstructions)) {
		t.Errorf("the tool answered with its HARDCODED instructions after the operator context failed to load.\n"+
			"The instructions silently dropped were this organisation's own: guidance %q and forbidden tool %q.\n"+
			"An operator cannot tell this answer from a governed one.\nresult: %s",
			operatorGuidance, forbiddenTool, body)
	}
	// SECOND: no fleet answer at all. A refusal that still lists the sites is
	// not a refusal.
	if strings.Contains(body, "in-scope") {
		t.Errorf("the fleet was answered despite unresolvable operator context: %s", body)
	}
}

// TestFleetTools_RefuseWhenOperatorContextIsOverBudget is the budget-refusal
// proof. An organisation row larger than the assistant surface can carry —
// which any row authored before the write-time ceiling may be — must stop the
// call, not be clipped to fit.
func TestFleetTools_RefuseWhenOperatorContextIsOverBudget(t *testing.T) {
	// One byte over. Using exactly "one byte over" is what makes this a
	// boundary test rather than a "very big string" test.
	oversized := strings.Repeat("x", govcontext.MaxDeliverableInstructionBytes+1)
	ctxStore := orgContextStore(oversized)

	w := post(t, routerWithContext(t, scopedStoreWithOneSite(), ctxStore), sitesListCall, nil)
	body := w.Body.String()

	// FIRST: nothing was clipped into the answer. Any run of the operator's
	// text appearing here means something truncated instead of refusing.
	if strings.Contains(body, strings.Repeat("x", 64)) {
		t.Errorf("an over-budget operator context was CLIPPED into the result instead of refusing the call. "+
			"Operator instructions are never truncated (ADR-064 Decision 14): %s", body)
	}
	// SECOND: no answer at all. Deliberately not an assertion about the
	// hardcoded instructions here — under a clipping mutation those are
	// legitimately present alongside the clipped context, so an assertion on
	// them would fire with a message that misnames the defect.
	if strings.Contains(body, "in-scope") {
		t.Errorf("the fleet was answered with the operator's context undeliverable: %s", body)
	}
}

// TestFleetTools_NilResolverRefuses covers the wiring bug: a Service built
// without WithContextResolver refuses every fleet tool call, for the same
// reason requireRecorder refuses without an audit recorder. A deploy that
// forgot the resolver would otherwise serve a fleet that looks governed and is
// not.
func TestFleetTools_NilResolverRefuses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Everything the surface needs EXCEPT the resolver, so the refusal below
	// can only be about the resolver.
	svc := NewService(scopedStoreWithOneSite()).withAuditRecorder(&capturingRecorder{})
	NewTransportHandler(svc, slog.New(slog.DiscardHandler), "test-version").Register(r)

	w := post(t, r, sitesListCall, nil)
	if strings.Contains(w.Body.String(), "in-scope") {
		t.Errorf("a Service with no context resolver answered a fleet question: %s", w.Body.String())
	}
}

// TestOperatorContextIsByteIdenticalToThePreview is the assertion the
// effective-context preview screen was built for and, until now, could not
// support: the operator sees the exact bytes the model is handed.
//
// IT ASSERTS BYTES, NOT "BOTH CALL RESOLVE". Two independent entry points are
// driven — Service.GetEffectiveContext (the function the preview route calls,
// govcontext/handler.go) and Service.operatorContext (the function the fleet
// tools call) — and their rendered output is compared byte for byte. The
// preview side additionally goes through a JSON round trip of exactly the
// fields the preview response publishes, so a field the preview drops on the
// wire cannot pass here. govcontext's own preview_test.go closes the last
// link, asserting that the HTTP response body is byte-identical to that DTO.
//
// The preview is site-scoped and the fleet tools are organisation-scoped, so
// the site here has NO site-level context: with layer 3 empty, the two
// resolutions must render identically, and that is the claim under test — the
// org half of a site preview is what a fleet-wide caller receives.
func TestOperatorContextIsByteIdenticalToThePreview(t *testing.T) {
	tenantID := uuid.New()
	siteID := uuid.New()
	resolver := &govcontext.Resolver{Store: orgContextStore(operatorGuidance)}

	// --- what the MODEL gets, on its production path -------------------------
	modelText, err := (&Service{}).withContextResolver(resolver).
		operatorContext(context.Background(), AuthorizedRequest{TenantID: tenantID})
	if err != nil {
		t.Fatalf("operatorContext: %v", err)
	}
	if modelText == "" {
		t.Fatal("operatorContext returned empty text for an organisation that authored context")
	}

	// --- what the PREVIEW SCREEN shows, on its production path ---------------
	rc, err := govcontext.NewService(nil, nil, resolver).
		GetEffectiveContext(context.Background(), tenantID, siteID)
	if err != nil {
		t.Fatalf("GetEffectiveContext: %v", err)
	}
	encoded, err := json.Marshal(rc)
	if err != nil {
		t.Fatalf("marshal preview context: %v", err)
	}
	var decoded govcontext.ResolvedContext
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode preview context: %v", err)
	}
	previewText := decoded.InstructionText()

	if previewText != modelText {
		t.Errorf("the preview and the model do not receive the same bytes.\npreview: %q\nmodel:   %q",
			previewText, modelText)
	}

	// And the text is actually IN the result the model reads, not merely
	// computed alongside it.
	result, err := buildListSitesResult(nil, envFor(t, nil), time.Unix(0, 0).UTC(), modelText)
	if err != nil {
		t.Fatalf("buildListSitesResult: %v", err)
	}
	if !strings.Contains(result, previewText) {
		t.Errorf("the previewed text is not present in the tool result the model receives:\n%s", result)
	}
}
