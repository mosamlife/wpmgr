package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// Harness
//
// The router is built by calling TransportHandler.Register with the SAME
// argument shape server.go passes it (a gin.IRouter that is the root engine),
// so these proofs exercise the real mount rather than a hand-rolled route that
// only resembles it. A test that invented its own route would prove nothing
// about whether POST /mcp is reachable.
// ---------------------------------------------------------------------------

const testBearer = "connection-token-plaintext"

// newTransportRouter builds the real mount over a store, WITH A WORKING AUDIT
// RECORDER attached.
//
// The recorder is not optional scenery. A Service with no recorder now refuses
// every tool call and every refusal it cannot record (Service.requireRecorder,
// ADR-061 A10), so a router built without one answers -32603 to everything and
// every test below would be asserting against a surface that is refusing for a
// reason none of them are about. capturingRecorder succeeds and keeps what it
// was handed, which is also what lets a test assert a row was written rather
// than merely that the call succeeded.
func newTransportRouter(t *testing.T, store Store) *gin.Engine {
	t.Helper()
	r, _ := newTransportRouterWithAudit(t, store)
	return r
}

// newTransportRouterWithAudit is the same mount, handing back the recorder so a
// test can read the rows the request wrote.
func newTransportRouterWithAudit(t *testing.T, store Store) (*gin.Engine, *capturingRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	rec := &capturingRecorder{}
	svc := NewService(store).withAuditRecorder(rec)
	h := NewTransportHandler(svc, slog.New(slog.DiscardHandler), "test-version")
	h.Register(r)
	return r, rec
}

// liveGrantStore is a fakeStore whose credential resolves and whose grant is
// authorized, scoped to the given sites.
func liveGrantStore(siteIDs ...uuid.UUID) *fakeStore {
	tenantID := uuid.New()
	return &fakeStore{
		tokenOK: true,
		token: sqlc.GetMCPConnectionTokenByHashForLookupRow{
			ID:       uuid.New(),
			TenantID: tenantID,
		},
		recheckOK: true,
		recheck: sqlc.ReCheckMCPRequestAuthorizationInTenantTxRow{
			Authorized: true,
			GrantID:    uuid.New(),
			TokenID:    uuid.New(),
			// m127: the capability set is READ FROM THIS ROW, so a fixture that
			// omits it models a grant holding NO capability and every request
			// against it is refused. That is the honest default -- leaving the
			// field zero here and having the transport work anyway would mean
			// the service was still computing capabilities instead of reading
			// them.
			GrantCapabilities: []string{string(CapSitesRead)},
			GrantExpiresAt:    time.Now().UTC().Add(90 * 24 * time.Hour),
		},
		scopeSites: siteIDs,
	}
}

// post issues one JSON-RPC request. headers are applied verbatim, so a test
// can send NO protocol header at all -- which is the case the design says to
// expect most of.
func post(t *testing.T, r *gin.Engine, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, TransportPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testBearer)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decodeRPC(t *testing.T, w *httptest.ResponseRecorder) jsonrpcResponse {
	t.Helper()
	var resp jsonrpcResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not JSON-RPC: %v\nbody: %s", err, w.Body.String())
	}
	return resp
}

func initBody(protocolVersion string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{
		"protocolVersion":%q,
		"clientInfo":{"name":"Claude Desktop","version":"1.2.3"}}}`, protocolVersion)
}

// ---------------------------------------------------------------------------
// PROOF 1 -- A CLIENT BELOW THE FLOOR IS REFUSED.
//
// The floor is a property of the approval flow, not a compatibility
// preference: revisions under 2025-03-26 drop fields the approval flow needs.
// So the refusal must be a refusal, and it must NAME BOTH NUMBERS so the user
// can act on it -- never a silent downgrade to the floor and never a quietly
// reduced surface.
// ---------------------------------------------------------------------------

func TestInitialize_BelowFloorIsRefusedNotDowngraded(t *testing.T) {
	// 2024-11-05 is the revision the design names explicitly as below the floor.
	for _, rev := range []string{"2024-11-05", "2024-01-01", "2025-03-25"} {
		t.Run(rev, func(t *testing.T) {
			r := newTransportRouter(t, liveGrantStore(uuid.New()))
			w := post(t, r, initBody(rev), nil)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("below-floor revision %s: got HTTP %d, want 400\nbody: %s",
					rev, w.Code, w.Body.String())
			}
			resp := decodeRPC(t, w)
			if resp.Error == nil {
				t.Fatalf("below-floor revision %s produced no JSON-RPC error: %s", rev, w.Body.String())
			}
			if resp.Error.Code != codeProtocolUnsupported {
				t.Errorf("error code = %d, want %d", resp.Error.Code, codeProtocolUnsupported)
			}

			// THE REFUSAL MUST NAME BOTH NUMBERS. A refusal that says only
			// "unsupported" cannot be acted on.
			if !strings.Contains(resp.Error.Message, ProtocolFloor) {
				t.Errorf("refusal does not name the floor %s: %q", ProtocolFloor, resp.Error.Message)
			}
			if !strings.Contains(resp.Error.Message, ProtocolTarget) {
				t.Errorf("refusal does not name the target %s: %q", ProtocolTarget, resp.Error.Message)
			}

			// AND IT MUST NOT HAVE SERVED THE REQUEST ANYWAY. A result here
			// would be the silent downgrade the floor exists to prevent.
			if resp.Result != nil {
				t.Errorf("a below-floor client was served a result: %#v", resp.Result)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// PROOF 2 -- THE THREE HEADER CASES ARE THREE DIFFERENT ANSWERS.
//
// Treating any two of them the same is a defect. Case 1 is the one that is
// easy to get wrong in the STRICT direction: no surveyed client documents this
// header, so header-less is the case to expect, and 400ing it rejects
// conforming clients.
// ---------------------------------------------------------------------------

func TestProtocolHeader_ThreeCasesThreeAnswers(t *testing.T) {
	cases := []struct {
		name        string
		header      string
		sendHeader  bool
		wantStatus  int
		wantOutcome NegotiationOutcome
	}{
		{
			name:        "absent assumes the floor and is NOT refused",
			sendHeader:  false,
			wantStatus:  http.StatusOK,
			wantOutcome: NegotiationAssumedFloor,
		},
		{
			name:        "present and supported is accepted",
			header:      ProtocolTarget,
			sendHeader:  true,
			wantStatus:  http.StatusOK,
			wantOutcome: NegotiationAccepted,
		},
		{
			name:        "present and below the floor is refused",
			header:      "2024-11-05",
			sendHeader:  true,
			wantStatus:  http.StatusBadRequest,
			wantOutcome: NegotiationBelowFloor,
		},
		{
			name:        "present and unparseable is refused",
			header:      "banana",
			sendHeader:  true,
			wantStatus:  http.StatusBadRequest,
			wantOutcome: NegotiationUnsupported,
		},
		{
			name:        "present and an unknown FUTURE revision is refused, not assumed fine",
			header:      "2099-01-01",
			sendHeader:  true,
			wantStatus:  http.StatusBadRequest,
			wantOutcome: NegotiationUnsupported,
		},
		{
			name:        "date-ish but not the revision form is unparseable, not compared",
			header:      "2025-3-26",
			sendHeader:  true,
			wantStatus:  http.StatusBadRequest,
			wantOutcome: NegotiationUnsupported,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The classifier itself.
			raw := ""
			if tc.sendHeader {
				raw = tc.header
			}
			neg := NegotiateProtocol(raw)
			if neg.Outcome != tc.wantOutcome {
				t.Errorf("NegotiateProtocol(%q).Outcome = %v, want %v", raw, neg.Outcome, tc.wantOutcome)
			}
			// An accepted or assumed negotiation must yield a version in the
			// window; a refusal must yield NO version, so a caller that
			// ignores Outcome cannot read a usable revision out of a refusal.
			if neg.Refused() && neg.Version != "" {
				t.Errorf("refused negotiation carries a version %q", neg.Version)
			}
			if !neg.Refused() && neg.Version == "" {
				t.Errorf("accepted negotiation carries no version")
			}

			// And the same three answers through the mounted route.
			r := newTransportRouter(t, liveGrantStore(uuid.New()))
			hdr := map[string]string{}
			if tc.sendHeader {
				hdr[ProtocolHeader] = tc.header
			}
			w := post(t, r, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, hdr)
			if w.Code != tc.wantStatus {
				t.Fatalf("HTTP %d, want %d\nbody: %s", w.Code, tc.wantStatus, w.Body.String())
			}
		})
	}
}

// TestProtocolHeader_AbsentIsNotABadRequest is PROOF 2's headline case stated
// on its own, because it is the one a stricter-looking implementation gets
// wrong while every other test still passes.
func TestProtocolHeader_AbsentIsNotABadRequest(t *testing.T) {
	r := newTransportRouter(t, liveGrantStore(uuid.New()))
	w := post(t, r, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)

	if w.Code == http.StatusBadRequest {
		t.Fatalf("a header-less request was rejected with 400; the specification says assume %s", ProtocolFloor)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d, want 200\nbody: %s", w.Code, w.Body.String())
	}
	if got := NegotiateProtocol("").Version; got != ProtocolFloor {
		t.Errorf("absent header negotiated %q, want the floor %q", got, ProtocolFloor)
	}
}

// ---------------------------------------------------------------------------
// PROOF 3 -- 401, NEVER 404.
//
// A 404 hides a routing failure behind something that looks like a deliberate
// refusal. The two facts are different: one is an outage, the other is
// expected. This asserts against the MOUNTED route, and asserts the
// distinction is real by showing an unmounted sibling path does 404.
// ---------------------------------------------------------------------------

func TestTransport_UnauthenticatedIs401Not404(t *testing.T) {
	r := newTransportRouter(t, &fakeStore{}) // tokenOK false: nothing resolves

	cases := []struct {
		name string
		auth string
	}{
		{"no Authorization header at all", ""},
		{"empty bearer", "Bearer "},
		{"wrong scheme", "Basic abc123"},
		{"unknown token", "Bearer not-a-real-token"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, TransportPath,
				strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code == http.StatusNotFound {
				t.Fatalf("unauthenticated request answered 404; it must be 401 so a routing "+
					"failure stays distinguishable from a refusal\nbody: %s", w.Body.String())
			}
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("HTTP %d, want 401\nbody: %s", w.Code, w.Body.String())
			}
			if got := w.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
				t.Errorf("401 does not name the scheme: WWW-Authenticate = %q", got)
			}
		})
	}

	// The 404 control: a path that genuinely is not mounted DOES 404, which is
	// what makes the assertion above meaningful rather than vacuous.
	req := httptest.NewRequest(http.MethodPost, "/mcp-not-mounted", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("control path returned %d, want 404 — the 401-not-404 assertion above is vacuous", w.Code)
	}
}

// TestTransport_NonPostVerbsAre405Not404 is the OTHER axis of the same
// invariant, and the one the original proof missed.
//
// The 404 control above uses a DIFFERENT PATH, which makes the POST assertion
// non-vacuous but says nothing about other verbs on the SAME path. GET is what
// `curl https://app.wpmgr.app/mcp` sends, and that URL is published to users:
// answering it "404 page not found" tells an operator the service is not
// deployed. GET and DELETE are also real parts of the Streamable HTTP
// transport, so a conforming client probes them.
func TestTransport_NonPostVerbsAre405Not404(t *testing.T) {
	r := newTransportRouter(t, liveGrantStore(uuid.New()))

	// OPTIONS IS DELIBERATELY ABSENT from this list. It used to be here, and
	// having it here was the defect: OPTIONS is the CORS preflight, and
	// answering it 405 made this endpoint unreachable from every
	// browser-hosted MCP client. Its behaviour is asserted by
	// TestTransport_PreflightIsAnsweredNot405 instead. Every verb below is
	// genuinely unsupported and 405 is genuinely the right answer for it.
	for _, verb := range []string{
		http.MethodGet, http.MethodHead, http.MethodPut,
		http.MethodPatch, http.MethodDelete,
	} {
		t.Run(verb, func(t *testing.T) {
			req := httptest.NewRequest(verb, TransportPath, nil)
			req.Header.Set("Authorization", "Bearer "+testBearer)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code == http.StatusNotFound {
				t.Fatalf("%s %s answered 404; the published endpoint must not read as "+
					"undeployed. Want 405.", verb, TransportPath)
			}
			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s answered %d, want 405", verb, TransportPath, w.Code)
			}
			if got := w.Header().Get("Allow"); got != http.MethodPost {
				t.Errorf("Allow = %q, want %q", got, http.MethodPost)
			}
			// HEAD carries no body by definition; every other verb must
			// answer in the JSON-RPC shape rather than gin's plain text, so
			// the wire contract holds for every probe.
			if verb != http.MethodHead {
				if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
					t.Errorf("Content-Type = %q, want JSON — a plain-text body breaks the wire contract", ct)
				}
				if strings.Contains(w.Body.String(), "404 page not found") {
					t.Error("the body is gin's default 404 text")
				}
			}
		})
	}

	// POST on the same path must still reach the handler and 401, so the 405s
	// above are not the router swallowing everything.
	req := httptest.NewRequest(http.MethodPost, TransportPath,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code == http.StatusMethodNotAllowed {
		t.Fatal("POST is being refused as method-not-allowed; the verb registration ate the real route")
	}
}

// TestTransport_OversizedBodyIsRefused proves the body cap exists and reports
// itself as a size problem rather than as malformed JSON.
func TestTransport_OversizedBodyIsRefused(t *testing.T) {
	r := newTransportRouter(t, liveGrantStore(uuid.New()))

	// Valid JSON, simply enormous: this must fail on SIZE, not on parsing, or
	// the caller is told to fix the wrong thing.
	huge := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"pad":"` +
		strings.Repeat("x", maxRequestBytes+1024) + `"}}`
	w := post(t, r, huge, nil)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body answered %d, want 413\nbody: %s", w.Code, w.Body.String())
	}

	// A body just under the cap is still served, so the cap does not reject
	// legitimate traffic.
	ok := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"pad":"` +
		strings.Repeat("x", 1024) + `"}}`
	w2 := post(t, r, ok, nil)
	if w2.Code != http.StatusOK {
		t.Fatalf("an in-budget body answered %d, want 200\nbody: %s", w2.Code, w2.Body.String())
	}
}

// ---------------------------------------------------------------------------
// PROOF 4 -- STALENESS IS HONEST.
//
// sites.components_updated_at is nullable with NO backfill precisely so that
// "we have never collected this" stays distinguishable from "collected at time
// T". A null must not render as a date, and must not fall back to updated_at:
// that fallback was removed deliberately because a 60s heartbeat bumps
// updated_at without touching components.
// ---------------------------------------------------------------------------

func siteRow(name string, componentsAt *time.Time) sqlc.Site {
	row := sqlc.Site{
		ID:              uuid.New(),
		Name:            name,
		Url:             "https://" + name + ".example",
		ConnectionState: "connected",
		HealthStatus:    "healthy",
		WpVersion:       "6.5.2",
		PhpVersion:      "8.2.14",
		AgentVersion:    "0.61.136",
		// updated_at is ALWAYS recent, which is exactly the trap: a fallback
		// to it would make a never-collected site look freshly inventoried.
		UpdatedAt: time.Now(),
		CreatedAt: time.Now(),
	}
	if componentsAt != nil {
		row.ComponentsUpdatedAt = pgtype.Timestamptz{Time: *componentsAt, Valid: true}
	}
	return row
}

func TestListSites_NullComponentsUpdatedAtRendersNeverCollected(t *testing.T) {
	collected := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	rows := []sqlc.Site{
		siteRow("never", nil),
		siteRow("collected", &collected),
	}

	now := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	text, err := buildListSitesResult(rows, envFor(t, rows), now)
	if err != nil {
		t.Fatalf("buildListSitesResult: %v", err)
	}

	var payload struct {
		Sites []siteRecord `json:"sites"`
	}
	if err := json.Unmarshal([]byte(jsonPart(t, text)), &payload); err != nil {
		t.Fatalf("payload is not JSON: %v\n%s", err, text)
	}
	if len(payload.Sites) != 2 {
		t.Fatalf("got %d sites, want 2", len(payload.Sites))
	}

	never := payload.Sites[0]
	if never.InventoryStatus != inventoryNeverCollected {
		t.Errorf("never-collected site has status %q, want %q", never.InventoryStatus, inventoryNeverCollected)
	}
	if never.InventoryCollectedAt != nil {
		t.Errorf("a null components_updated_at was rendered as the date %q", *never.InventoryCollectedAt)
	}
	if never.InventoryAgeSeconds != nil {
		t.Errorf("a null components_updated_at was given the age %d; zero/any age reads as "+
			"'collected', which is the inversion of the truth", *never.InventoryAgeSeconds)
	}

	// The raw JSON must carry an explicit null, not an omitted key: an absent
	// key invites a reader to supply its own default.
	if !strings.Contains(text, `"inventory_collected_at":null`) {
		t.Errorf("never-collected site does not serialise an explicit null:\n%s", text)
	}

	got := payload.Sites[1]
	if got.InventoryStatus != inventoryCollected {
		t.Errorf("collected site has status %q, want %q", got.InventoryStatus, inventoryCollected)
	}
	if got.InventoryCollectedAt == nil || *got.InventoryCollectedAt != collected.Format(time.RFC3339) {
		t.Errorf("collected site rendered %v, want %s", got.InventoryCollectedAt, collected.Format(time.RFC3339))
	}
	if got.InventoryAgeSeconds == nil || *got.InventoryAgeSeconds != int64(now.Sub(collected).Seconds()) {
		t.Errorf("collected site age = %v, want %d", got.InventoryAgeSeconds, int64(now.Sub(collected).Seconds()))
	}

	// And the two must not be equal on any staleness field — if they were, the
	// distinction the nullable column exists for would be gone.
	if never.InventoryStatus == got.InventoryStatus {
		t.Error("never-collected and collected sites report the same inventory_status")
	}
}

// ---------------------------------------------------------------------------
// PROOF 5 -- TRUNCATION IS VISIBLE AND CUTS AT A RECORD BOUNDARY.
//
// A silently truncated result is a lie about the fleet. The payload here is
// sized to STRADDLE the cap: enough sites that the budget runs out partway
// through the list, so the cut lands between two records rather than at a
// convenient edge.
// ---------------------------------------------------------------------------

func TestListSites_TruncationCutsAtRecordBoundaryWithAMarker(t *testing.T) {
	collected := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)

	// Build enough records to overrun recordByteBudget several times over, so
	// the budget is certain to run out mid-list.
	var rows []sqlc.Site
	for i := 0; i < 400; i++ {
		rows = append(rows, siteRow(fmt.Sprintf("site-%03d-with-a-deliberately-long-name-to-consume-budget", i), &collected))
	}

	now := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	text, err := buildListSitesResult(rows, envFor(t, rows), now)
	if err != nil {
		t.Fatalf("buildListSitesResult: %v", err)
	}

	raw := jsonPart(t, text)

	// 1. THE RESULT IS STILL VALID JSON. This is what "cut at a record
	//    boundary" buys: a mid-record cut would leave a truncated object and
	//    the model would get a parse error instead of a short list.
	var payload struct {
		Sites      []siteRecord   `json:"sites"`
		Truncation truncationInfo `json:"truncation"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("truncated payload is not valid JSON — the cut landed mid-record: %v", err)
	}

	// 2. IT ACTUALLY TRUNCATED, so the rest of this test is not vacuous.
	if len(payload.Sites) >= len(rows) {
		t.Fatalf("nothing was truncated (%d of %d returned); the cap is not being applied",
			len(payload.Sites), len(rows))
	}
	if len(payload.Sites) == 0 {
		t.Fatal("everything was truncated; the budget is too small to prove a boundary cut")
	}

	// 3. EVERY RETURNED RECORD IS WHOLE. The last one in particular must be a
	//    complete site, not a fragment.
	last := payload.Sites[len(payload.Sites)-1]
	if last.ID == "" || last.Name == "" || last.InventoryStatus == "" {
		t.Errorf("the final record is a fragment, not a whole record: %#v", last)
	}

	// 4. THE MARKER IS PRESENT AND MACHINE-READABLE.
	if !payload.Truncation.Truncated {
		t.Error("a truncated result reports truncated=false — it reads as complete")
	}
	if payload.Truncation.Returned != len(payload.Sites) {
		t.Errorf("truncation.returned = %d but %d sites present", payload.Truncation.Returned, len(payload.Sites))
	}
	if payload.Truncation.Available == nil || *payload.Truncation.Available != len(rows) {
		t.Errorf("truncation.available = %v, want %d", payload.Truncation.Available, len(rows))
	}

	// 5. THE MARKER IS ALSO AT THE HEAD, not only in the tail. The tail is
	//    what gets cut by a downstream context trim, so a marker that lives
	//    only at the end turns back into a silent truncation.
	head := text[:len(text)-len(raw)]
	if !strings.Contains(head, "INCOMPLETE RESULT") {
		t.Errorf("the truncation marker is not in the prepended header:\n%s", head)
	}

	// 6. THE BUDGET WAS ACTUALLY RESPECTED BY THE BYTES ACTUALLY EMITTED.
	//    The allowance over recordByteBudget is the JSON envelope (the "sites"
	//    and "truncation" keys and the truncation explanation), not slack in
	//    the record budget itself.
	const envelopeAllowance = 1024
	if len(raw) > recordByteBudget+envelopeAllowance {
		t.Errorf("emitted payload is %d bytes, over the %d-byte record budget (+%d envelope); "+
			"the cap is not governing the bytes actually sent", len(raw), recordByteBudget, envelopeAllowance)
	}
}

// TestListSites_UntruncatedResultDoesNotClaimTruncation is the over-fire half:
// a guard that reddens correct work gets switched off. A result that fits must
// NOT carry the marker.
func TestListSites_UntruncatedResultIsNotMarked(t *testing.T) {
	collected := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	rows := []sqlc.Site{siteRow("one", &collected), siteRow("two", nil)}

	text, err := buildListSitesResult(rows, envFor(t, rows), time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("buildListSitesResult: %v", err)
	}
	var payload struct {
		Sites      []siteRecord   `json:"sites"`
		Truncation truncationInfo `json:"truncation"`
	}
	if err := json.Unmarshal([]byte(jsonPart(t, text)), &payload); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if payload.Truncation.Truncated {
		t.Error("a complete result claims to be truncated")
	}
	if len(payload.Sites) != 2 {
		t.Errorf("got %d sites, want 2", len(payload.Sites))
	}
	if strings.Contains(text, "INCOMPLETE RESULT") {
		t.Error("a complete result carries the truncation banner")
	}
}

// envFor builds the envelope for a COMPLETE read of rows: every site the
// caller may see answered, nothing refused. It is the shape almost every
// renderer test wants, and building it through NewEnvelope rather than as a
// struct literal means these tests also exercise the balance check.
func envFor(t *testing.T, rows []sqlc.Site) Envelope {
	t.Helper()
	env, err := NewEnvelope(len(rows), len(rows), nil)
	if err != nil {
		t.Fatalf("NewEnvelope for %d complete rows: %v", len(rows), err)
	}
	return env
}

// TestListSites_UnreadInScopeSiteIsReportedAsPartial replaces the old
// TestListSites_PageBoundIsReportedAsTruncation, which asserted the behaviour
// that turned out to be the leak.
//
// THE OLD TEST PINNED A TENANT-CARDINALITY DISCLOSURE. It passed `more=true`
// — "this TENANT has rows beyond the page bound" — and required the rendered
// result to tell the caller so. For a connection scoped to a subset of the
// tenant that notice was false (the caller had received every site it may
// read) and disclosing (it revealed that the tenant holds more sites than the
// page bound). The value is no longer plumbed to the renderer at all.
//
// Incompleteness is now measured against the CALLER'S OWN SCOPE: an in-scope
// site that went unread is an explicit refusal, counted, with a banner. This
// asserts that, and asserts that the numbers balance without a residual.
func TestListSites_UnreadInScopeSiteIsReportedAsPartial(t *testing.T) {
	collected := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	rows := []sqlc.Site{siteRow("one", &collected)}

	// The caller may read two sites; only one came back.
	unread := uuid.New()
	env, err := NewEnvelope(2, 1, []Refusal{{
		SiteID: unread.String(),
		Code:   RefusalSiteUnread,
		Detail: "in scope but not returned by the bounded page",
	}})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}

	text, err := buildListSitesResult(rows, env, time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("buildListSitesResult: %v", err)
	}

	var payload struct {
		Envelope   Envelope       `json:"envelope"`
		Truncation truncationInfo `json:"truncation"`
	}
	if err := json.Unmarshal([]byte(jsonPart(t, text)), &payload); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}

	if payload.Envelope.Asked != 2 || payload.Envelope.OK != 1 || payload.Envelope.Refused != 1 {
		t.Errorf("envelope asked/ok/refused = %d/%d/%d, want 2/1/1",
			payload.Envelope.Asked, payload.Envelope.OK, payload.Envelope.Refused)
	}
	if payload.Envelope.OK+payload.Envelope.Refused != payload.Envelope.Asked {
		t.Error("envelope does not balance: a residual here is a site the caller cannot account for")
	}
	if len(payload.Envelope.Refusals) != 1 || payload.Envelope.Refusals[0].Code != RefusalSiteUnread {
		t.Errorf("refusals = %+v, want one site_unread", payload.Envelope.Refusals)
	}
	if !strings.Contains(text, "PARTIAL RESULT") {
		t.Error("a partial result does not carry the partial banner — it reads as complete")
	}
	// The byte cap was not hit, so the BYTE-CAP truncation flag must stay
	// false. A refusal is not a truncation and conflating them would make
	// each one unreadable.
	if payload.Truncation.Truncated {
		t.Error("truncation.truncated is set by a refusal; it must report the byte cap only")
	}
}

// TestListSites_BannerSurvivesInstructionClamp: the banner must NEVER be the
// thing the clamp drops.
//
// The clamp cuts from the tail, so appending the banner to the instructions
// put the "this result is incomplete" notice in the exact position that gets
// removed under budget pressure. It is prepended now, and this asserts the
// ordering rather than the arithmetic — a future instruction text that grows
// past the budget must not silently reintroduce the bug.
func TestListSites_BannerSurvivesInstructionClamp(t *testing.T) {
	collected := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	var rows []sqlc.Site
	for i := 0; i < 400; i++ {
		rows = append(rows, siteRow(fmt.Sprintf("site-%03d-with-a-deliberately-long-name-to-consume-budget", i), &collected))
	}

	text, err := buildListSitesResult(rows, envFor(t, rows), time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("buildListSitesResult: %v", err)
	}

	bannerAt := strings.Index(text, "INCOMPLETE RESULT")
	if bannerAt < 0 {
		t.Fatal("the truncation banner is absent entirely")
	}
	instructionsAt := strings.Index(text, "Fleet inventory, read-only")
	if instructionsAt < 0 {
		t.Fatal("the instructions are absent entirely")
	}
	// THE ORDERING IS THE ASSERTION: banner first, instructions after. If the
	// banner sorts after the instructions it is once again in the clamp's path.
	if bannerAt > instructionsAt {
		t.Errorf("the banner (at %d) comes AFTER the instructions (at %d); it is back in the "+
			"position the clamp cuts from", bannerAt, instructionsAt)
	}
	// And the clamp must apply to the instructions only, never to the banner.
	if strings.Contains(text[:bannerAt+len("INCOMPLETE RESULT")], "[instructions truncated]") {
		t.Error("the clamp marker appears before the banner")
	}
}

// TestListSites_OneOversizedRecordDoesNotBlockTheRest: a single huge record
// must not zero out the tool for that org.
//
// sites.name is tenant-controlled, so one very long name is enough. Because
// the list is ordered by name, a `break` would put that record in the same
// place on every call and suppress everything after it indefinitely.
func TestListSites_OneOversizedRecordDoesNotBlockTheRest(t *testing.T) {
	collected := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)

	// "a..." sorts first by name, and on its own exceeds the whole budget.
	oversized := siteRow("a"+strings.Repeat("x", recordByteBudget+512), &collected)
	small := siteRow("b-small-site", &collected)
	rows := []sqlc.Site{oversized, small}

	text, err := buildListSitesResult(rows, envFor(t, rows), time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("buildListSitesResult: %v", err)
	}
	var payload struct {
		Sites      []siteRecord   `json:"sites"`
		Truncation truncationInfo `json:"truncation"`
	}
	if err := json.Unmarshal([]byte(jsonPart(t, text)), &payload); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}

	if len(payload.Sites) != 1 {
		t.Fatalf("got %d sites, want 1 — the fitting site was suppressed by the oversized one",
			len(payload.Sites))
	}
	if payload.Sites[0].Name != "b-small-site" {
		t.Errorf("returned %q, want the small site", payload.Sites[0].Name)
	}
	// It is still truncation and must still be marked.
	if !payload.Truncation.Truncated {
		t.Error("a result that omitted a record reports truncated=false")
	}
	// The marker must say OMITTED, not "cut at" — the result is a subset, and
	// implying a prefix would tell the model the missing sites all sort after
	// the last one shown.
	if strings.Contains(payload.Truncation.Explanation, "cut at") {
		t.Errorf("the explanation implies a prefix: %q", payload.Truncation.Explanation)
	}
	if !strings.Contains(payload.Truncation.Explanation, "omitted") {
		t.Errorf("the explanation does not say records were omitted: %q", payload.Truncation.Explanation)
	}
}

// ---------------------------------------------------------------------------
// PROOF 6 -- AN EMPTY SITE SCOPE IS A NAMED REFUSAL, NEVER AN EMPTY SUCCESS.
//
// An empty result that reads as "nothing to do" is how a scoping bug becomes
// invisible, and an empty SiteSet must never widen into "all sites".
// ---------------------------------------------------------------------------

func TestToolsCall_EmptyScopeIsRefusedNotAnEmptyList(t *testing.T) {
	store := liveGrantStore() // scopeSites nil: the tag matched no site
	r := newTransportRouter(t, store)

	w := post(t, r, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"fleet_sites_list","arguments":{}}}`, nil)

	resp := decodeRPC(t, w)
	if resp.Error == nil {
		t.Fatalf("an empty site scope produced a SUCCESS: %s", w.Body.String())
	}
	if resp.Error.Code != codeScopeEmpty {
		t.Errorf("error code = %d, want %d (%s)", resp.Error.Code, codeScopeEmpty, ErrCodeScopeEmpty)
	}
	if strings.Contains(w.Body.String(), `"sites"`) {
		t.Error("the refusal body contains a sites list; it must not look like a result")
	}
	// It must not have read any sites either: an empty scope is refused before
	// the query, so a scoping bug cannot be masked by a tenant-wide read.
	for _, c := range store.calls {
		if c == "ListSitesForRead" {
			t.Error("an empty-scope request still read the sites table")
		}
	}
}

// ---------------------------------------------------------------------------
// PROOF 6b -- AN UNTICKED CAPABILITY REFUSES ON THE WIRE, WITH ITS OWN CODE.
//
// The D1 ruling in one wire assertion: the tool is listed, calling it answers
// -32005 and not -32004, and the data says which capability is missing and that
// retrying will not help.
//
// IT GOES THROUGH h.authorizeCall, the same function the router now calls
// before the rate-limit gate on the tools/call path (1A-11 merge): the
// capability check runs first specifically so it cannot be preempted by a
// rate-limit refusal, and that is what this test exercises. The
// AuthorizedRequest is built here rather than authenticated, because the
// capability axis is not stored per-grant yet (m124 DECISION 1) and every
// bearer the fake store mints therefore holds mcp.sites.read. The DB-side
// proof of the same refusal lives in
// apps/api/tests/adr064_s7_mcp_tool_registry_rls_test.go, as wpmgr_app.
// ---------------------------------------------------------------------------

func TestToolsCall_UntickedCapabilityRefusesWithItsOwnCode(t *testing.T) {
	// A REAL Service, not nil, AND a real recorder. authorizeCall records the
	// refusal (RecordToolDenied), which dereferences the service; a nil
	// *Service panics on the refusal path -- the path this test exists to walk.
	// The recorder used to be omitted, on the since-reversed basis that an
	// unaudited Service records nothing and carries on: it now refuses, and the
	// refusal it produces is -32603, not the -32005 this test is about.
	h := NewTransportHandler(NewService(&fakeStore{}).withAuditRecorder(&capturingRecorder{}),
		slog.New(slog.NewTextHandler(io.Discard, nil)), "test")

	// Holds NO capability, but a non-empty site scope -- so nothing below can
	// be attributed to the site axis, which this change did not touch.
	//
	// The ORG CEILING is the production one (authWith resolves
	// OrgDefaultCapabilities), so this is the middle case of the ruling: the
	// organisation has this capability enabled and THIS GRANT does not hold it.
	// That is the case that must still be listed. A connection whose ceiling
	// excluded the capability is a different case and is proven separately in
	// registry_test.go.
	auth := authWith(CapabilitySet{}, uuid.New())

	// The tool is LISTED to this connection: the refusal below is a refusal,
	// not a consequence of an empty surface.
	if got := VisibleTools(auth); !sameToolNames(got, registryToolNames(auth)) {
		t.Fatalf("tools/list hid a tool from a connection lacking its capability: %+v", got)
	}

	_, _, resp, refused := h.authorizeCall(context.Background(), auth, jsonrpcRequest{
		ID:     json.RawMessage(`61`),
		Method: "tools/call",
		Params: json.RawMessage(`{"name":"fleet_sites_list","arguments":{}}`),
	})

	if !refused {
		t.Fatal("a connection holding no capability was not refused")
	}
	if resp.Error == nil {
		t.Fatalf("a connection holding no capability got a SUCCESS: %+v", resp)
	}
	// BY VALUE, and against the code it must NOT be: -32004 tells a model to
	// ask tools/list for a tool tools/list already showed it, which is the
	// contradiction that produces another call rather than a report to the user.
	if resp.Error.Code != codeCapabilityNotGranted {
		t.Fatalf("error code = %d, want %d (%s)",
			resp.Error.Code, codeCapabilityNotGranted, ErrCodeCapabilityNotGranted)
	}
	if resp.Error.Code == codeToolNotAvailable {
		t.Fatal("the capability refusal answered the uniform not-available code")
	}
	if !strings.Contains(resp.Error.Message, string(CapSitesRead)) {
		t.Fatalf("the wire message does not name the missing capability: %q", resp.Error.Message)
	}

	var data struct {
		Code               string   `json:"code"`
		Tool               string   `json:"tool"`
		RequiredCapability string   `json:"required_capability"`
		HeldCapabilities   []string `json:"held_capabilities"`
		Retryable          *bool    `json:"retryable"`
	}
	if err := json.Unmarshal(resp.Error.Data, &data); err != nil {
		t.Fatalf("decode error data %s: %v", resp.Error.Data, err)
	}
	if data.Code != ErrCodeCapabilityNotGranted {
		t.Errorf("data.code = %q, want %q", data.Code, ErrCodeCapabilityNotGranted)
	}
	if data.Tool != ToolFleetSitesList {
		t.Errorf("data.tool = %q, want %q", data.Tool, ToolFleetSitesList)
	}
	if data.RequiredCapability != string(CapSitesRead) {
		t.Errorf("data.required_capability = %q, want %q", data.RequiredCapability, CapSitesRead)
	}
	if len(data.HeldCapabilities) != 0 {
		t.Errorf("data.held_capabilities = %v for a connection holding none", data.HeldCapabilities)
	}
	// A MISSING retryable IS A FAILURE, not a pass. A client that has to infer
	// retryability infers "try again", which is the loop this refusal exists to
	// prevent.
	if data.Retryable == nil {
		t.Fatal("the error data carries no retryable field")
	}
	if *data.Retryable {
		t.Error("data.retryable = true on a permanent refusal")
	}
}

// TestToolsCall_HeldCapabilityIsUnaffected is the over-fire control. The SAME
// call, from a connection that holds the capability and a site, must reach the
// tool -- so the refusal above is the capability gate and not a broken fixture.
func TestToolsCall_HeldCapabilityIsUnaffected(t *testing.T) {
	store := liveGrantStore(uuid.New())
	r := newTransportRouter(t, store)

	w := post(t, r, `{"jsonrpc":"2.0","id":62,"method":"tools/call","params":{"name":"fleet_sites_list","arguments":{}}}`, nil)

	resp := decodeRPC(t, w)
	if resp.Error != nil {
		t.Fatalf("a connection HOLDING the capability was refused %d: %s",
			resp.Error.Code, w.Body.String())
	}
	// The sites payload is JSON nested inside the JSON-RPC text content, so it
	// arrives escaped. Matching the unescaped form would never fire.
	if !strings.Contains(w.Body.String(), `\"sites\":`) {
		t.Fatalf("the granted call returned no sites payload: %s", w.Body.String())
	}
	// And its tools/list descriptor carries no capability notice.
	w = post(t, r, `{"jsonrpc":"2.0","id":63,"method":"tools/list","params":{}}`, nil)
	if strings.Contains(w.Body.String(), "NOT AVAILABLE TO THIS CONNECTION") {
		t.Fatalf("a connection holding the capability was told the tool is unavailable: %s",
			w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// PROOF 7 -- THE CONNECT RECORD KEEPS HEADER ABSENCE AS ABSENCE.
//
// Decision 10: a stamped client_identity_recorded_at with a NULL
// protocol_version is "connected and sent no header", which is a compatibility
// signal an operator needs. Defaulting it to the negotiated floor would
// manufacture a header the client never sent.
// ---------------------------------------------------------------------------

func TestInitialize_RecordsClientIdentityAndHeaderAbsence(t *testing.T) {
	t.Run("header absent is persisted as NULL", func(t *testing.T) {
		store := liveGrantStore(uuid.New())
		r := newTransportRouter(t, store)

		w := post(t, r, initBody(ProtocolTarget), nil) // no MCP-Protocol-Version
		if w.Code != http.StatusOK {
			t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
		}
		if len(store.identityCalls) != 1 {
			t.Fatalf("got %d identity records, want 1", len(store.identityCalls))
		}
		rec := store.identityCalls[0]
		if rec.ProtocolVersion != nil {
			t.Errorf("an ABSENT header was persisted as %q; absence must stay NULL", *rec.ProtocolVersion)
		}
		if rec.ProtocolVersion == nil && strings.Contains(w.Body.String(), `"protocolVersion":""`) {
			t.Error("the negotiated version leaked as empty")
		}
		if rec.Name != "Claude Desktop" || rec.Version != "1.2.3" {
			t.Errorf("client identity = %q/%q, want Claude Desktop/1.2.3", rec.Name, rec.Version)
		}
	})

	t.Run("header present is persisted verbatim", func(t *testing.T) {
		store := liveGrantStore(uuid.New())
		r := newTransportRouter(t, store)

		w := post(t, r, initBody(ProtocolTarget), map[string]string{ProtocolHeader: ProtocolTarget})
		if w.Code != http.StatusOK {
			t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
		}
		rec := store.identityCalls[0]
		if rec.ProtocolVersion == nil {
			t.Fatal("a PRESENT header was persisted as NULL; the two absences are now indistinguishable")
		}
		if *rec.ProtocolVersion != ProtocolTarget {
			t.Errorf("persisted %q, want %q", *rec.ProtocolVersion, ProtocolTarget)
		}
	})
}

// TestInitialize_FailedConnectRecordIsNotSwallowed: a failed write must refuse
// the session, not proceed with an unattributable connection.
func TestInitialize_FailedConnectRecordRefusesTheSession(t *testing.T) {
	store := liveGrantStore(uuid.New())
	store.identityErr = fmt.Errorf("database is down")
	r := newTransportRouter(t, store)

	w := post(t, r, initBody(ProtocolTarget), nil)
	resp := decodeRPC(t, w)
	if resp.Error == nil {
		t.Fatalf("a failed connect record was swallowed and the session succeeded: %s", w.Body.String())
	}
	if resp.Error.Code != codeInternalError {
		t.Errorf("error code = %d, want %d", resp.Error.Code, codeInternalError)
	}
	if strings.Contains(resp.Error.Message, "database is down") {
		t.Error("the internal error message leaked to the client")
	}
}

// ---------------------------------------------------------------------------
// PROOF 8 -- tools/list IS TOOLS ONLY, AND THE SURFACE IS READ-ONLY.
// ---------------------------------------------------------------------------

func TestToolsList_IsToolsOnlyAndReadOnly(t *testing.T) {
	r := newTransportRouter(t, liveGrantStore(uuid.New()))
	w := post(t, r, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Result struct {
			Tools []ToolDescriptor `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("tools/list is not JSON: %v", err)
	}
	// THE LISTING IS COMPARED AGAINST THE REGISTRY, not against a hard-coded
	// count. The point of this assertion is that tools/list over the real
	// mounted transport returns nothing the closed literal never declared --
	// an extra name here is a tool reachable over HTTP that no reviewed diff
	// added.
	//
	// IT ASSERTS CONTAINMENT, NOT EQUALITY, AND THE ASYMMETRY IS DELIBERATE.
	// This test drives HTTP and so holds no AuthorizedRequest, which means it
	// cannot know this fixture's org ceiling -- and the ceiling is what decides
	// how many tools SHOULD be listed. Demanding equality against the whole
	// registry would therefore go red the day a tool outside the read ceiling
	// lands, on a listing that was correct. Containment cannot over-fire that
	// way and still catches the thing this test exists for. The ceiling-aware
	// equality check lives in the proofs that do hold an auth.
	registered := map[string]bool{}
	for _, d := range Tools() {
		registered[d.Name] = true
	}
	for _, d := range resp.Result.Tools {
		if !registered[d.Name] {
			t.Fatalf("tools/list offered %q, which is not in the closed registry", d.Name)
		}
	}
	if len(resp.Result.Tools) == 0 {
		t.Fatal("tools/list returned nothing, so every assertion here is vacuous")
	}
	if !sameToolNames(resp.Result.Tools, []string{ToolFleetSitesList, ToolFleetUpdatesPending}) {
		t.Fatalf("tools = %#v, want both Tier 0 fleet reads for a default-ceiling connection",
			resp.Result.Tools)
	}
	// EVERY tool must carry a schema, not just the first.
	for _, d := range resp.Result.Tools {
		if len(d.InputSchema) == 0 {
			t.Errorf("tool %q carries no inputSchema, so a model cannot call it without guessing", d.Name)
		}
	}

	// Resources and prompts are refused BY NAME, not answered with an empty
	// list: "this server has no resources" is a different and false statement
	// from "this server does not implement resources".
	for _, method := range []string{"resources/list", "prompts/list"} {
		w := post(t, r, fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":%q}`, method), nil)
		resp := decodeRPC(t, w)
		if resp.Error == nil {
			t.Errorf("%s returned a result; Phase 1 exposes tools only", method)
			continue
		}
		if resp.Error.Code != codeMethodNotFound {
			t.Errorf("%s error code = %d, want %d", method, resp.Error.Code, codeMethodNotFound)
		}
	}
}

// TestToolsCall_ReadPathReturnsStampedSites is the happy path end to end
// through the mounted route, so the tool is proven reachable and not merely
// unit-tested.
func TestToolsCall_ReadPathReturnsStampedSites(t *testing.T) {
	allowed := uuid.New()
	store := liveGrantStore(allowed)

	collected := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	inScope := siteRow("in-scope", &collected)
	inScope.ID = allowed
	outOfScope := siteRow("out-of-scope", &collected) // a different, unscoped id
	store.sites = []sqlc.Site{inScope, outOfScope}

	r := newTransportRouter(t, store)
	w := post(t, r, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"fleet_sites_list","arguments":{}}}`, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "in-scope") {
		t.Errorf("the scoped site is missing from the result: %s", body)
	}
	// THE SITE SET IS A FILTER, NOT DECORATION. A site outside the resolved set
	// must not appear even though the tenant read returned it.
	if strings.Contains(body, "out-of-scope") {
		t.Error("a site outside the grant's resolved site set was returned")
	}
}

// TestToolsCall_SiteReadIsDispatchedSiteConstrained is the unit-speed guard on
// the SECOND RLS layer, and it is the counterpart of
// TestAuthenticateHandsTheChokepointAnUnscopedPrincipal in oauth_test.go: that
// one asserts the bootstrap read is NOT site-constrained, this one asserts the
// site read IS.
//
// WHY IT IS NEEDED AT ALL, when the layer-3 test above already passes. Swapping
// connectionScopedPrincipal for bootstrapTenantPrincipal at the call site
// compiles, changes no rendered output, and leaves every other test in this
// package green -- because the Go filter in ListSitesForModel still hides
// out-of-scope rows. What it silently removes is the database layer:
// db.RunTenantTx routes an org-scoped principal to InTenantTx, app.site_scope
// is never set, and the RESTRICTIVE sites_site_scope policy (m19) evaluates its
// first disjunct to true for every row. Nothing observable at this layer
// changes. This test is what goes red.
func TestToolsCall_SiteReadIsDispatchedSiteConstrained(t *testing.T) {
	allowed := uuid.New()
	store := liveGrantStore(allowed)

	collected := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	inScope := siteRow("in-scope", &collected)
	inScope.ID = allowed
	store.sites = []sqlc.Site{inScope}

	r := newTransportRouter(t, store)
	w := post(t, r, `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"fleet_sites_list","arguments":{}}}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
	}

	store.mu.Lock()
	principals := append([]domain.Principal(nil), store.sitePrincipals...)
	store.mu.Unlock()

	if len(principals) != 1 {
		t.Fatalf("ListSitesForRead was called %d times, want exactly 1", len(principals))
	}
	p := principals[0]

	// THE ASSERTION THAT NAMES THE LEAK, FIRST. An unconstrained principal here
	// means the site read ran with app.site_scope unset and the m19 policy
	// inert, leaving the Go filter as the only gate on the site axis.
	if !p.IsSiteConstrained() {
		t.Fatalf("RLS LAYER LOST: the assistant's site read was dispatched with an "+
			"UNCONSTRAINED principal (scope=%q, %d allowed site ids). RunTenantTx routes that "+
			"to InTenantTx, app.site_scope is never set, and the RESTRICTIVE sites_site_scope "+
			"policy is inert -- the database enforces nothing and the Go filter is the only "+
			"gate. Build the principal with connectionScopedPrincipal, not "+
			"bootstrapTenantPrincipal: the ADR-061 A14 exception is the scope BOOTSTRAP and "+
			"does not reach this call site, which runs after the allowlist is resolved",
			p.Scope, len(p.AllowedSiteIDs))
	}

	// The allowlist must be the connection's resolved scope, not something
	// wider that merely happens to be non-empty.
	if len(p.AllowedSiteIDs) != 1 || p.AllowedSiteIDs[0] != allowed {
		t.Fatalf("the site read's allowlist = %v, want exactly the resolved scope [%s]",
			p.AllowedSiteIDs, allowed)
	}
	if p.TenantID != store.token.TenantID {
		t.Fatalf("the site read ran under tenant %s, want %s", p.TenantID, store.token.TenantID)
	}
}

// TestNotification_ToolsCallWithNoIdDoesNotExecuteTheTool.
//
// A JSON-RPC request with no id is a notification and takes no response body.
// The transport used to DISPATCH FIRST and apply that rule afterwards, so an
// id-less tools/call ran the tool and then answered 202 with an empty body: the
// caller triggered the work and saw nothing of it.
//
// Harmless while every tool is a read. The moment a write tool lands it is a
// fire-and-forget invocation channel -- an effect with no answer and no record
// the caller ever sees, which is the shape the proposal machinery exists to
// prevent. The assertion that matters is therefore NOT the status code, which
// was always 202; it is that the store was never touched.
func TestNotification_ToolsCallWithNoIdDoesNotExecuteTheTool(t *testing.T) {
	allowed := uuid.New()
	store := liveGrantStore(allowed)
	row := siteRow("should-never-be-read", nil)
	row.ID = allowed
	store.sites = []sqlc.Site{row}

	r := newTransportRouter(t, store)
	w := post(t, r, `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"fleet_sites_list","arguments":{}}}`, nil)

	if w.Code != http.StatusAccepted {
		t.Fatalf("HTTP %d, want 202: %s", w.Code, w.Body.String())
	}
	if body := strings.TrimSpace(w.Body.String()); body != "" {
		t.Errorf("a notification returned a body: %q", body)
	}

	// THE REAL ASSERTION. ListSitesForRead must never have been called.
	for _, c := range store.callLog() {
		if c == "ListSitesForRead" {
			t.Fatalf("an id-less tools/call EXECUTED the tool; the call log is %v", store.callLog())
		}
	}

	// Positive control: the same call WITH an id does execute, so the proof
	// above is the notification rule and not a broken fixture.
	w2 := post(t, r, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fleet_sites_list","arguments":{}}}`, nil)
	if w2.Code != http.StatusOK {
		t.Fatalf("the id-carrying control got HTTP %d: %s", w2.Code, w2.Body.String())
	}
	var executed bool
	for _, c := range store.callLog() {
		if c == "ListSitesForRead" {
			executed = true
		}
	}
	if !executed {
		t.Fatal("the id-carrying control did not execute the tool either; the fixture is broken")
	}
}

// TestActivityStamp_OnEveryToolCallAndNotOnKeepalives pins the decision GH #605
// turns on: WHICH methods count as "used".
//
// The stamp is what makes mcp_grants.last_used_at real, and last_used_at is the
// input to the idle-expiry deadline. Both directions are asserted because each
// alone is a different bug:
//
//   - tools/list and tools/call MUST stamp. If they do not, an actively used
//     connection reads as never used and idle-expires -- the fleet outage m127
//     DECISION 4 describes.
//   - ping and notifications/initialized MUST NOT. They are keepalives a stuck
//     background process emits forever; letting them refresh the deadline makes
//     "unused for 30 days" unfalsifiable and the feature decorative.
//
// initialize does not stamp either: RecordConnect already records that a client
// connected (client_identity_recorded_at), which is the separate fact.
func TestActivityStamp_OnEveryToolCallAndNotOnKeepalives(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantStamp bool
	}{
		{"tools/list stamps", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, true},
		{"tools/call stamps",
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fleet_sites_list","arguments":{}}}`, true},
		{"ping does not stamp", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, false},
		{"initialize does not stamp",
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"c","version":"1"}}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := liveGrantStore(uuid.New())
			r := newTransportRouter(t, store)

			w := post(t, r, tc.body, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
			}

			got := len(store.touchCalls) > 0
			if got != tc.wantStamp {
				t.Fatalf("%s: stamped=%v want %v (%d TouchActivity calls)",
					tc.name, got, tc.wantStamp, len(store.touchCalls))
			}
			if !tc.wantStamp {
				return
			}
			// The ids matter as much as the count: a stamp on the wrong grant
			// keeps the wrong connection alive.
			c := store.touchCalls[0]
			if c.TenantID != store.token.TenantID {
				t.Errorf("stamped tenant %s, want %s", c.TenantID, store.token.TenantID)
			}
			if c.GrantID != store.recheck.GrantID {
				t.Errorf("stamped grant %s, want %s", c.GrantID, store.recheck.GrantID)
			}
			if c.TokenID != store.recheck.TokenID {
				t.Errorf("stamped token %s, want %s", c.TokenID, store.recheck.TokenID)
			}
		})
	}
}

// TestActivityStamp_FailureRefusesTheRequestAndDoesNotRunTheTool is the other
// half of the decision: the stamp is NOT best-effort.
//
// A stamp that fails silently leaves the connection's idle clock running while
// it is being used, and the cost lands N days later as an expiry nobody can
// explain. So the request is refused -- with the INTERNAL code, never an auth
// or tool-availability code, because nothing is wrong with the caller's token
// or its capability set and telling it otherwise sends an operator rotating a
// credential that was never the problem.
func TestActivityStamp_FailureRefusesTheRequestAndDoesNotRunTheTool(t *testing.T) {
	store := liveGrantStore(uuid.New())
	store.touchErr = errors.New("simulated: the stamp could not be written")
	r := newTransportRouter(t, store)

	w := post(t, r,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fleet_sites_list","arguments":{}}}`, nil)

	var resp struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
		Result any `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if resp.Error.Code != codeInternalError {
		t.Fatalf("error code = %d, want %d (internal): %s",
			resp.Error.Code, codeInternalError, w.Body.String())
	}
	if resp.Result != nil {
		t.Fatalf("a refused request still carried a result: %s", w.Body.String())
	}

	// AND THE TOOL DID NOT RUN. Serving the read anyway would mean the caller
	// got its answer while the record that it asked says otherwise -- the stamp
	// runs before the tool precisely so an unrecordable request is not served.
	for _, c := range store.callLog() {
		if c == "ListSitesForRead" {
			t.Fatal("the tool executed despite the activity stamp failing")
		}
	}
}

// jsonPart returns the JSON payload half of a tool result, i.e. everything
// from the first '{' that begins the payload object. The instructions are
// PREPENDED, so the payload is the tail.
func jsonPart(t *testing.T, text string) string {
	t.Helper()
	i := strings.Index(text, "{")
	if i < 0 {
		t.Fatalf("no JSON payload found in tool result:\n%s", text)
	}
	return text[i:]
}

var _ = io.Discard

// ---------------------------------------------------------------------------
// CORS preflight
// ---------------------------------------------------------------------------

// TestTransport_PreflightIsAnsweredNot405 is the regression for the defect this
// file's 405 list used to contain.
//
// The failure it guards is not cosmetic. Our discovery documents set
// Access-Control-Allow-Origin: * so that a browser-hosted MCP client may read
// them, and they advertise this endpoint's URL. If the preflight is refused,
// that client reads the invitation and then cannot open the door: the browser
// never issues the POST, and the user sees an opaque network error.
//
// A preflight is not avoidable. Authorization alone forces one, and so does
// Content-Type: application/json.
func TestTransport_PreflightIsAnsweredNot405(t *testing.T) {
	r := newTransportRouter(t, liveGrantStore(uuid.New()))

	// A REALISTIC browser preflight: no Authorization (the browser strips it),
	// and the two Access-Control-Request-* headers a browser actually sends.
	req := httptest.NewRequest(http.MethodOptions, TransportPath, nil)
	req.Header.Set("Origin", "https://some-mcp-client.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type,mcp-protocol-version")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code == http.StatusMethodNotAllowed {
		t.Fatalf("preflight answered 405; the endpoint is unreachable from any browser client")
	}
	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight answered %d, want 204\nbody: %s", w.Code, w.Body.String())
	}

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, "*")
	}

	// Allow-Credentials must be ABSENT. Setting it true is the change that
	// would turn "*" into a real hole, and it is also incompatible with "*".
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want it absent — "+
			"credentialed CORS on a bearer endpoint is a hole", got)
	}

	// Every method the transport specifies for this endpoint, by value, DERIVED
	// from the routed list rather than written out. The 405 verbs are included
	// ON PURPOSE even though none of them succeeds: omitting one makes the
	// browser block the request before our honest 405 can be read, turning a
	// clear refusal into an opaque network error. A written-out list here would
	// drift from the routing exactly as the production copy did.
	//
	// TestTransport_PreflightAdvertisesEveryRoutedVerb covers this properly,
	// including the converse; this is the same claim kept local to the test
	// that already reads the whole preflight response.
	methods := w.Header().Get("Access-Control-Allow-Methods")
	for _, m := range append([]string{http.MethodPost}, methodNotAllowedVerbs...) {
		if !allowedPreflightMethods(t, methods)[m] {
			t.Errorf("Access-Control-Allow-Methods = %q, missing %s", methods, m)
		}
	}

	// Every header the specification says a client may send. Missing any one
	// of these fails the whole real request, not just that header.
	allowed := strings.ToLower(w.Header().Get("Access-Control-Allow-Headers"))
	for _, hdr := range []string{
		"authorization",
		"content-type",
		"accept",
		strings.ToLower(ProtocolHeader),
		"mcp-session-id",
		"last-event-id",
	} {
		if !strings.Contains(allowed, hdr) {
			t.Errorf("Access-Control-Allow-Headers = %q, missing %q", allowed, hdr)
		}
	}
}

// TestTransport_PreflightDoesNotWeakenTheBearerRequirement is the OVER-FIRE
// check, and it is the one that matters.
//
// A preflight that succeeds buys the caller exactly one thing: the right to
// SEND the real request. It must buy nothing else. If answering OPTIONS had
// made any currently-refused request start succeeding, that is a hole, and
// this test is what would catch it.
//
// Every assertion is BY VALUE -- the HTTP status and the JSON-RPC code -- never
// "an error occurred". Two tests in this package were caught passing for the
// wrong reason, one because a different policy was doing the refusing.
func TestTransport_PreflightDoesNotWeakenTheBearerRequirement(t *testing.T) {
	r := newTransportRouter(t, &fakeStore{}) // tokenOK false: nothing resolves

	// Send the preflight first, exactly as a browser would, so the sequence
	// under test is the real one and not a POST in isolation.
	pre := httptest.NewRequest(http.MethodOptions, TransportPath, nil)
	pre.Header.Set("Origin", "https://some-mcp-client.example")
	pre.Header.Set("Access-Control-Request-Method", http.MethodPost)
	pw := httptest.NewRecorder()
	r.ServeHTTP(pw, pre)
	if pw.Code != http.StatusNoContent {
		t.Fatalf("preflight answered %d, want 204 — the rest of this test is vacuous", pw.Code)
	}

	t.Run("unauthenticated POST is still 401 by value", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, TransportPath,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Origin", "https://some-mcp-client.example")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("HTTP %d, want 401 — the preflight let an unauthenticated POST through\nbody: %s",
				w.Code, w.Body.String())
		}
		resp := decodeRPC(t, w)
		if resp.Error == nil {
			t.Fatal("401 carried no JSON-RPC error object")
		}
		if resp.Error.Code != codeInvalidRequest {
			t.Errorf("JSON-RPC code = %d, want %d", resp.Error.Code, codeInvalidRequest)
		}
		if got := w.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
			t.Errorf("401 does not name the scheme: WWW-Authenticate = %q", got)
		}
	})

	t.Run("a bad token after a preflight is still 401 by value", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, TransportPath,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Origin", "https://some-mcp-client.example")
		req.Header.Set("Authorization", "Bearer not-a-real-token")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("HTTP %d, want 401\nbody: %s", w.Code, w.Body.String())
		}
		resp := decodeRPC(t, w)
		if resp.Error == nil || resp.Error.Code != codeInvalidRequest {
			t.Fatalf("want JSON-RPC code %d, got %+v", codeInvalidRequest, resp.Error)
		}
	})

	t.Run("GET is still 405 by value", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, TransportPath, nil)
		req.Header.Set("Origin", "https://some-mcp-client.example")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET answered %d, want 405 — the preflight change opened a stream", w.Code)
		}
		if got := w.Header().Get("Allow"); got != http.MethodPost {
			t.Errorf("Allow = %q, want %q", got, http.MethodPost)
		}
	})

	t.Run("DELETE is still 405 by value", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, TransportPath, nil)
		req.Header.Set("Origin", "https://some-mcp-client.example")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("DELETE answered %d, want 405 — the preflight change made sessions terminable", w.Code)
		}
	})
}

// TestTransport_ActualResponsesAreReadableCrossOrigin covers the half of this
// fix that a preflight-only change would have missed.
//
// The preflight only authorises the request to be SENT. Without
// Access-Control-Allow-Origin on the ACTUAL response the browser still refuses
// to hand it to the client, and the client sees a network error rather than
// our answer. An unreadable 401 sends the caller hunting for an outage instead
// of at their token.
func TestTransport_ActualResponsesAreReadableCrossOrigin(t *testing.T) {
	r := newTransportRouter(t, &fakeStore{})

	t.Run("the 401 is readable", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, TransportPath,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Origin", "https://some-mcp-client.example")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("HTTP %d, want 401", w.Code)
		}
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("Access-Control-Allow-Origin on the 401 = %q, want %q — "+
				"the browser will not expose this refusal to the client", got, "*")
		}
		// WWW-Authenticate is useless to a browser client unless exposed.
		exposed := strings.ToLower(w.Header().Get("Access-Control-Expose-Headers"))
		if !strings.Contains(exposed, "www-authenticate") {
			t.Errorf("Access-Control-Expose-Headers = %q, missing www-authenticate — "+
				"the client can see 401 but not how to authenticate", exposed)
		}
	})

	t.Run("the 405 is readable", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, TransportPath, nil)
		req.Header.Set("Origin", "https://some-mcp-client.example")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("HTTP %d, want 405", w.Code)
		}
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("Access-Control-Allow-Origin on the 405 = %q, want %q — "+
				"the client sees an opaque CORS error instead of the refusal", got, "*")
		}
	})
}
