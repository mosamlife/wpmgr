// connections_test.go: the unit half of the S16 list-and-revoke slice.
//
// WHAT THIS FILE CAN AND CANNOT PROVE, said up front because the gap is the
// interesting part. Everything here runs against fakeStore, so it proves the Go
// BRANCH STRUCTURE: which outcome maps to which status, which absence stays an
// absence, which principal is refused before the database is reached. It proves
// NOTHING about RLS, because a fake has no policies -- the site-scope policy and
// the revoke cascade are proven in
// apps/api/tests/adr064_s16_mcp_connections_integration_test.go, against the
// real schema as wpmgr_app. Every proof above the SQL layer on this stack runs
// against fakeStore and that gap is what let a P1 reach review, so the split is
// stated rather than assumed.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func ptrStr(s string) *string { return &s }

func tsAt(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// orgPrincipal is a full organisation member: not site constrained.
func orgPrincipal(tenantID uuid.UUID) domain.Principal {
	return domain.Principal{
		Type:     domain.PrincipalUser,
		UserID:   uuid.New(),
		TenantID: tenantID,
		Role:     "admin",
		Scope:    "org",
	}
}

// siteScopedPrincipal is an outside collaborator shared onto ONE site. This is
// the principal mcp_grants_site_scope_select exists to refuse.
func siteScopedPrincipal(tenantID uuid.UUID) domain.Principal {
	return domain.Principal{
		Type:           domain.PrincipalUser,
		UserID:         uuid.New(),
		TenantID:       tenantID,
		Role:           "admin", // admin ON THAT SITE, and still refused
		Scope:          domain.ScopeSite,
		AllowedSiteIDs: []uuid.UUID{uuid.New()},
	}
}

// ---------------------------------------------------------------------------
// PROOF 1 -- A FAILED LIST IS NOT AN EMPTY LIST.
//
// The single most repeated defect on this project (26+ instances). The service
// must return (nil, err) and the handler must answer non-2xx; neither may
// produce a body that decodes into a zero-length list.
// ---------------------------------------------------------------------------

func TestListConnectionsFailureIsNeverAnEmptyList(t *testing.T) {
	tenantID := uuid.New()
	boom := errors.New("connection refused: the database is down")
	store := &fakeStore{grantsErr: boom}
	svc := auditedService(store)

	got, err := svc.ListConnections(context.Background(), orgPrincipal(tenantID))
	if err == nil {
		t.Fatal("a failed read returned no error; the caller cannot tell it failed")
	}
	if !errors.Is(err, boom) {
		t.Errorf("the underlying failure was not wrapped through: got %v", err)
	}
	// The assertion that matters. A non-nil empty slice here is what becomes
	// "you have no connections" on screen.
	if got != nil {
		t.Fatalf("a failed read returned a non-nil slice of len %d. An empty list "+
			"is a CLAIM that the organisation has no connections, and this read "+
			"never happened", len(got))
	}
}

func TestListConnectionsHandlerAnswersNon2xxOnFailureWithNoConnectionsKey(t *testing.T) {
	tenantID := uuid.New()
	store := &fakeStore{grantsErr: errors.New("database is down")}
	eng := newConnectionsRouter(t, store, orgPrincipal(tenantID))

	w := httptest.NewRecorder()
	eng.ServeHTTP(w, httptest.NewRequest(http.MethodGet, ConnectionsPath, nil))

	if w.Code == http.StatusOK {
		t.Fatalf("a failed list answered 200. body: %s", w.Body.String())
	}

	// The envelope check is the second half and it is not redundant: a client
	// that ignores the status must still be unable to decode this body into a
	// list. The house error envelope has no `connections` key at all.
	var probe connectionListDTO
	if err := json.Unmarshal(w.Body.Bytes(), &probe); err == nil && probe.Connections != nil {
		t.Fatalf("the error body decoded into a connections list of len %d; a "+
			"client ignoring the status would render an empty fleet. body: %s",
			len(probe.Connections), w.Body.String())
	}
}

// The other half of the pair: a genuinely empty organisation MUST be
// distinguishable, and must serialise as [] rather than null. Without this the
// test above is satisfiable by refusing everything.
func TestListConnectionsEmptyOrgIsAnEmptyArrayNotNull(t *testing.T) {
	tenantID := uuid.New()
	store := &fakeStore{grants: nil} // no error, no rows
	eng := newConnectionsRouter(t, store, orgPrincipal(tenantID))

	w := httptest.NewRecorder()
	eng.ServeHTTP(w, httptest.NewRequest(http.MethodGet, ConnectionsPath, nil))

	if w.Code != http.StatusOK {
		t.Fatalf("an empty organisation answered %d, want 200. body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"connections":[]`) {
		t.Errorf("an empty list did not serialise as []; a null would force every "+
			"consumer to special-case a third state. body: %s", w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// PROOF 2 -- THE SITE-SCOPE GATE REFUSES OUT LOUD RATHER THAN RETURNING [].
//
// Constraint 2 of the brief. RLS refuses by returning zero rows with no error,
// which on this path is indistinguishable from an empty organisation -- so the
// service must refuse BEFORE the store is reached, and must SAY so.
// ---------------------------------------------------------------------------

func TestSiteScopedPrincipalIsRefusedAndNeverReachesTheStore(t *testing.T) {
	tenantID := uuid.New()

	t.Run("list", func(t *testing.T) {
		store := &fakeStore{}
		svc := auditedService(store)

		got, err := svc.ListConnections(context.Background(), siteScopedPrincipal(tenantID))
		if err == nil {
			t.Fatalf("a site-scoped collaborator listed %d connections without "+
				"being refused", len(got))
		}
		if got != nil {
			t.Errorf("the refusal came with a non-nil slice of len %d", len(got))
		}
		var de *domain.Error
		if !errors.As(err, &de) || de.Code != ErrCodeOrgScopeRequired {
			t.Fatalf("want a %s domain error, got %v", ErrCodeOrgScopeRequired, err)
		}
		if de.Kind != domain.KindForbidden {
			t.Errorf("the refusal is Kind %v, want Forbidden", de.Kind)
		}
		// The store must not have been consulted at all. If it had, an RLS
		// refusal downstream would have come back as zero rows and this
		// service would have reported an empty list.
		if log := store.callLog(); len(log) != 0 {
			t.Errorf("the store was called %v despite the principal being refused; "+
				"the refusal must precede the read", log)
		}
	})

	t.Run("revoke", func(t *testing.T) {
		store := &fakeStore{}
		svc := auditedService(store)

		_, err := svc.RevokeConnection(context.Background(),
			siteScopedPrincipal(tenantID), uuid.New())
		if err == nil {
			t.Fatal("a site-scoped collaborator revoked an organisation-wide credential")
		}
		var de *domain.Error
		if !errors.As(err, &de) || de.Code != ErrCodeOrgScopeRequired {
			t.Fatalf("want a %s domain error, got %v", ErrCodeOrgScopeRequired, err)
		}
		if log := store.callLog(); len(log) != 0 {
			t.Errorf("the store was called %v despite the principal being refused", log)
		}
	})
}

// The service hands the PRINCIPAL to the store, not a bare tenant id. That is
// what routes the query through db.RunTenantTx's site-scope dispatch; a
// signature taking a tenantID could only reach InTenantTx, leaving the
// RESTRICTIVE policies inert.
func TestStoreReceivesThePrincipalSoRunTenantTxCanDispatch(t *testing.T) {
	tenantID := uuid.New()
	p := orgPrincipal(tenantID)
	store := &fakeStore{}
	svc := auditedService(store)

	if _, err := svc.ListConnections(context.Background(), p); err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, err := svc.RevokeConnection(context.Background(), p, uuid.New()); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if len(store.listPrincipals) != 1 || store.listPrincipals[0].UserID != p.UserID {
		t.Errorf("ListGrants did not receive the calling principal: %+v", store.listPrincipals)
	}
	if len(store.revokePrincipals) != 1 || store.revokePrincipals[0].UserID != p.UserID {
		t.Errorf("RevokeGrantWithTokens did not receive the calling principal: %+v",
			store.revokePrincipals)
	}
}

// ---------------------------------------------------------------------------
// PROOF 3 -- REVOKE'S FOUR OUTCOMES, AND ONLY ONE IS A FAILURE.
//
// Constraint 4: "a revoke that matched zero rows must not return success". The
// discriminator is whether a ROW CAME BACK, never what the counts say -- a row
// of two zeroes is an idempotent success, and mapping it to 404 would report a
// correctly revoked credential as still live.
// ---------------------------------------------------------------------------

func TestRevokeOutcomeMapping(t *testing.T) {
	tenantID := uuid.New()

	cases := []struct {
		name       string
		row        sqlc.RevokeMCPGrantWithTokensInTenantTxRow
		storeErr   error
		wantStatus int
		wantCode   string
		// wantAlready is only read on a 200.
		wantAlready bool
		wantTokens  int64
	}{
		{
			name:       "no row at all means the grant is not visible: 404",
			storeErr:   pgx.ErrNoRows,
			wantStatus: http.StatusNotFound,
			wantCode:   ErrCodeConnectionNotFound,
		},
		{
			name:       "first revoke flips the grant and kills two tokens: 200",
			row:        sqlc.RevokeMCPGrantWithTokensInTenantTxRow{GrantsRevoked: 1, TokensRevoked: 2},
			wantStatus: http.StatusOK,
			wantTokens: 2,
		},
		{
			name: "half-revoked repair: grant already revoked, tokens were still live: 200",
			row:  sqlc.RevokeMCPGrantWithTokensInTenantTxRow{GrantsRevoked: 0, TokensRevoked: 3},
			// This is the state a security review actually observed in this
			// database. Re-running IS the repair, and it is a success.
			wantStatus:  http.StatusOK,
			wantAlready: true,
			wantTokens:  3,
		},
		{
			name: "already fully revoked: two zeroes is an IDEMPOTENT SUCCESS, not a 404",
			row:  sqlc.RevokeMCPGrantWithTokensInTenantTxRow{GrantsRevoked: 0, TokensRevoked: 0},
			// The trap. Two zeroes look like "nothing happened, therefore
			// something went wrong"; the row came back, which is what says the
			// grant is visible and the requested end state holds.
			wantStatus:  http.StatusOK,
			wantAlready: true,
			wantTokens:  0,
		},
		{
			name:       "an infra error is a 500 and never a quiet success",
			storeErr:   errors.New("connection reset by peer"),
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{revokeRow: tc.row, revokeErr: tc.storeErr}
			eng := newConnectionsRouter(t, store, orgPrincipal(tenantID))

			w := httptest.NewRecorder()
			eng.ServeHTTP(w, httptest.NewRequest(http.MethodPost,
				ConnectionRevokePathFor(uuid.NewString()), nil))

			if w.Code != tc.wantStatus {
				t.Fatalf("answered %d, want %d. body: %s", w.Code, tc.wantStatus, w.Body.String())
			}

			if tc.wantStatus != http.StatusOK {
				if tc.wantCode != "" {
					var env struct {
						Code string `json:"code"`
					}
					if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
						t.Fatalf("error body is not JSON: %v", err)
					}
					if env.Code != tc.wantCode {
						t.Errorf("code %q, want %q", env.Code, tc.wantCode)
					}
				}
				return
			}

			var got revokeResponseDTO
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("success body is not JSON: %v", err)
			}
			if got.Status != string(GrantStatusRevoked) {
				t.Errorf("status %q, want %q", got.Status, GrantStatusRevoked)
			}
			if got.AlreadyRevoked != tc.wantAlready {
				t.Errorf("already_revoked %v, want %v", got.AlreadyRevoked, tc.wantAlready)
			}
			// The token count is on the wire because "revoked, 0 tokens killed"
			// and "revoked, 3 tokens killed" are different facts to an operator.
			if got.TokensRevoked != tc.wantTokens {
				t.Errorf("tokens_revoked %d, want %d", got.TokensRevoked, tc.wantTokens)
			}
		})
	}
}

// A malformed id answers 404, identically to an id from another organisation and
// to one that never existed. Any other status here is an existence oracle.
func TestRevokeMalformedIDIs404NotAnOracle(t *testing.T) {
	store := &fakeStore{revokeErr: pgx.ErrNoRows}
	eng := newConnectionsRouter(t, store, orgPrincipal(uuid.New()))

	w := httptest.NewRecorder()
	eng.ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		ConnectionsPath+"/not-a-uuid/revoke", nil))

	if w.Code != http.StatusNotFound {
		t.Fatalf("a malformed connection id answered %d, want 404; a different "+
			"status for a malformed id than for an unknown one tells a caller "+
			"which ids are well-formed. body: %s", w.Code, w.Body.String())
	}
	// And it must not have reached the database at all.
	if log := store.callLog(); len(log) != 0 {
		t.Errorf("a malformed id reached the store: %v", log)
	}
}

// ---------------------------------------------------------------------------
// PROOF 4 -- THE FOUR PROTOCOL STATES STAY FOUR.
//
// Constraint 3: protocol_version NULL means "this client sent no protocol
// header", NOT "unknown version". And the DTO's zero value must never read as a
// version.
// ---------------------------------------------------------------------------

func TestClassifyStoredProtocolKeepsFourStatesApart(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name        string
		recordedAt  *time.Time
		stored      *string
		wantState   ClientProtocolState
		wantVersion string
	}{
		{
			name:      "never connected: recorded_at NULL",
			wantState: ClientProtocolNeverConnected,
		},
		{
			name:       "connected, sent no header: recorded_at set, version NULL",
			recordedAt: &now,
			wantState:  ClientProtocolAbsent,
		},
		{
			name:        "sent a revision we speak",
			recordedAt:  &now,
			stored:      ptrStr("2025-06-18"),
			wantState:   ClientProtocolRecognised,
			wantVersion: "2025-06-18",
		},
		{
			name:        "sent the floor, which we speak",
			recordedAt:  &now,
			stored:      ptrStr(ProtocolFloor),
			wantState:   ClientProtocolRecognised,
			wantVersion: ProtocolFloor,
		},
		{
			name:        "sent a revision below the floor",
			recordedAt:  &now,
			stored:      ptrStr("2024-11-05"),
			wantState:   ClientProtocolUnrecognised,
			wantVersion: "2024-11-05",
		},
		{
			name:        "sent an unknown future revision: refused, never assumed compatible",
			recordedAt:  &now,
			stored:      ptrStr("2099-01-01"),
			wantState:   ClientProtocolUnrecognised,
			wantVersion: "2099-01-01",
		},
		{
			name:        "sent garbage",
			recordedAt:  &now,
			stored:      ptrStr("banana"),
			wantState:   ClientProtocolUnrecognised,
			wantVersion: "banana",
		},
		{
			// The anomaly guard. NegotiateProtocol("") returns AssumedFloor with
			// Version=ProtocolFloor, so an unguarded pass-through would print
			// "2025-03-26" against a column that holds "". A stored empty string
			// is not an absent header -- absence is NULL.
			name:       "a non-NULL empty string is an anomaly, never the floor",
			recordedAt: &now,
			stored:     ptrStr(""),
			wantState:  ClientProtocolUnrecognised,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyStoredProtocol(tc.recordedAt, tc.stored)
			if got.State != tc.wantState {
				t.Fatalf("state %q, want %q", got.State, tc.wantState)
			}
			if got.Version != tc.wantVersion {
				t.Errorf("version %q, want %q", got.Version, tc.wantVersion)
			}
			// The load-bearing negative: neither absence state may carry a
			// version. This is the assertion that catches a future "helpful"
			// default of the floor onto an absent header.
			if tc.wantState == ClientProtocolAbsent || tc.wantState == ClientProtocolNeverConnected {
				if got.Version != "" {
					t.Errorf("state %q carries version %q; absence must carry no "+
						"version at all -- NULL means the client sent no header, "+
						"not that we do not know which one", got.State, got.Version)
				}
			}
		})
	}
}

// A recorded-at of NULL wins over a set version. The branch order is the
// contract: this is what stops a never-connected grant reading as "sent no
// header".
func TestNeverConnectedWinsOverAStoredVersion(t *testing.T) {
	got := ClassifyStoredProtocol(nil, ptrStr("2025-06-18"))
	if got.State != ClientProtocolNeverConnected {
		t.Fatalf("state %q, want %q", got.State, ClientProtocolNeverConnected)
	}
}

// The DTO must not let the absence states carry a version key with a value.
func TestProtocolDTOEmitsNullVersionForBothAbsenceStates(t *testing.T) {
	for _, state := range []ClientProtocolState{ClientProtocolNeverConnected, ClientProtocolAbsent} {
		dto := toConnectionDTO(Connection{Protocol: ClientProtocol{State: state}})
		if dto.Protocol.Version != nil {
			t.Errorf("state %q serialised version %q; want null", state, *dto.Protocol.Version)
		}
		if dto.Protocol.State != string(state) {
			t.Errorf("state serialised as %q, want %q", dto.Protocol.State, state)
		}
	}
}

// ---------------------------------------------------------------------------
// PROOF 5 -- NULL last_used_at IS NOT A DATE.
//
// Constraint 4 again. The trap is a time.Time zero value, which serialises as
// 0001-01-01T00:00:00Z and renders as a real date in the year 1.
// ---------------------------------------------------------------------------

func TestNullTimestampsSerialiseAsNullAndNeverAsTheZeroYear(t *testing.T) {
	grant := sqlc.McpGrant{
		ID:            uuid.New(),
		Name:          "Claude Desktop",
		Status:        string(GrantStatusActive),
		SiteScopeMode: string(SiteScopeModeAll),
		CreatedAt:     time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC),
		// LastUsedAt, RevokedAt and ClientIdentityRecordedAt all left invalid:
		// a fresh grant nothing has connected to yet.
	}

	dto := toConnectionDTO(connectionFromGrant(grant))
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)

	if !strings.Contains(body, `"last_used_at":null`) {
		t.Errorf("a never-used connection did not serialise last_used_at as null. body: %s", body)
	}
	if !strings.Contains(body, `"revoked_at":null`) {
		t.Errorf("a live connection did not serialise revoked_at as null. body: %s", body)
	}
	// The specific trap, named so a failure explains itself.
	if strings.Contains(body, "0001-01-01") {
		t.Errorf("a NULL timestamp serialised as the Go zero time, which renders "+
			"as a real date in the year 1. body: %s", body)
	}
	// And the keys must be PRESENT, not omitted: a missing key is a third state
	// the consumer has to guess at.
	for _, key := range []string{`"last_used_at"`, `"revoked_at"`,
		`"reported_client_name"`, `"reported_client_version"`} {
		if !strings.Contains(body, key) {
			t.Errorf("key %s was omitted entirely; an explicit null says "+
				"'none' out loud, an absent key says nothing. body: %s", key, body)
		}
	}
}

func TestPresentTimestampSurvivesAsADate(t *testing.T) {
	used := time.Date(2026, 8, 29, 14, 30, 0, 0, time.UTC)
	dto := toConnectionDTO(connectionFromGrant(sqlc.McpGrant{
		ID:         uuid.New(),
		Status:     string(GrantStatusActive),
		CreatedAt:  used,
		LastUsedAt: tsAt(used),
	}))
	if dto.LastUsedAt == nil {
		t.Fatal("a used connection serialised last_used_at as null")
	}
	if !strings.HasPrefix(*dto.LastUsedAt, "2026-08-29T14:30:00") {
		t.Errorf("last_used_at %q does not carry the stored instant", *dto.LastUsedAt)
	}
}

// ---------------------------------------------------------------------------
// PROOF 6 -- REVOKED GRANTS ARE LISTED, AND STATUS IS READ NOT INFERRED.
//
// m124 Decision 2 keeps the revoked row so last_used_at and revoked_at stay
// readable. Filtering them out here would hide exactly the record an operator
// reviews after revoking.
// ---------------------------------------------------------------------------

func TestListIncludesRevokedGrantsAndReportsStoredStatus(t *testing.T) {
	revokedAt := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	store := &fakeStore{grants: []sqlc.McpGrant{
		{
			ID: uuid.New(), Name: "live", Status: string(GrantStatusActive),
			SiteScopeMode: string(SiteScopeModeAll), CreatedAt: revokedAt,
		},
		{
			ID: uuid.New(), Name: "dead", Status: string(GrantStatusRevoked),
			SiteScopeMode: string(SiteScopeModeAll), CreatedAt: revokedAt,
			RevokedAt: tsAt(revokedAt), LastUsedAt: tsAt(revokedAt),
		},
	}}
	svc := auditedService(store)

	got, err := svc.ListConnections(context.Background(), orgPrincipal(uuid.New()))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d connections, want 2; a revoked grant was filtered out and "+
			"its last_used_at record went with it", len(got))
	}
	if got[1].Status != GrantStatusRevoked {
		t.Errorf("the revoked grant reports status %q", got[1].Status)
	}
	if got[1].RevokedAt == nil {
		t.Error("the revoked grant lost its revoked_at")
	}
	if got[0].Status != GrantStatusActive {
		t.Errorf("the live grant reports status %q", got[0].Status)
	}
	// Status is READ, never inferred from RevokedAt being nil.
	if got[0].RevokedAt != nil {
		t.Error("the live grant carries a revoked_at")
	}
}

// ---------------------------------------------------------------------------
// PROOF 7 -- THE UNVERIFIED CLIENT CLAIM IS NEVER DEFAULTED TO THE OPERATOR'S.
// ---------------------------------------------------------------------------

func TestReportedClientIsNeverDefaultedToTheOperatorName(t *testing.T) {
	c := connectionFromGrant(sqlc.McpGrant{
		ID:     uuid.New(),
		Name:   "Sohil's laptop", // the OPERATOR's name for the connection
		Status: string(GrantStatusActive),
		// ClientName left NULL: the client has reported nothing.
	})
	if c.ReportedClientName != nil {
		t.Fatalf("a client that reported no name came back as %q; the operator's "+
			"label and the client's claim are different assertions and must not "+
			"be substituted for one another", *c.ReportedClientName)
	}

	c2 := connectionFromGrant(sqlc.McpGrant{
		ID: uuid.New(), Name: "Sohil's laptop", Status: string(GrantStatusActive),
		ClientName: ptrStr("Claude Desktop"), ClientVersion: ptrStr("1.2.3"),
	})
	if c2.ReportedClientName == nil || *c2.ReportedClientName != "Claude Desktop" {
		t.Error("the client's reported name was lost")
	}
	if c2.Name != "Sohil's laptop" {
		t.Error("the operator's name was overwritten by the client's claim")
	}
}

// ---------------------------------------------------------------------------
// PROOF 8 -- THE ROUTES ARE MOUNTED, AND A WRONG VERB IS 405 NOT 404.
//
// A 404 reads as "not deployed", which is exactly how the S6b-2 blocker
// presented and cost a debugging session.
// ---------------------------------------------------------------------------

func TestConnectionRoutesAreMountedAndWrongVerbsAre405(t *testing.T) {
	eng := newConnectionsRouter(t, &fakeStore{}, orgPrincipal(uuid.New()))
	id := uuid.NewString()

	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, ConnectionsPath, http.StatusOK},
		{http.MethodPost, ConnectionRevokePathFor(id), http.StatusOK},
		{http.MethodDelete, ConnectionsPath, http.StatusMethodNotAllowed},
		{http.MethodPut, ConnectionsPath, http.StatusMethodNotAllowed},
		{http.MethodGet, ConnectionRevokePathFor(id), http.StatusMethodNotAllowed},
		{http.MethodDelete, ConnectionRevokePathFor(id), http.StatusMethodNotAllowed},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			eng.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			if w.Code != tc.want {
				t.Fatalf("answered %d, want %d. body: %s", w.Code, tc.want, w.Body.String())
			}
			if tc.want == http.StatusMethodNotAllowed && w.Header().Get("Allow") == "" {
				t.Error("a 405 carried no Allow header, so it does not say which verb to use")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Router fixture
// ---------------------------------------------------------------------------

// newConnectionsRouter mounts RegisterConnections the way server.New does:
// on a group carrying a principal, which is what RequireAuth + RequireTenant
// guarantee by the time the handler runs.
//
// THE PERMISSION MIDDLEWARE IS REAL HERE, not stubbed. RegisterConnections
// installs authz.RequirePermission itself, so these tests exercise the actual
// gate; the principal fixtures carry role "admin", which
// authz.minRoleFor[PermAPIKeyRead] requires.
func newConnectionsRouter(t *testing.T, store Store, p domain.Principal) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	eng := gin.New()
	g := eng.Group(APIV1Prefix)
	g.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	})
	NewHandler(auditedService(store)).RegisterConnections(g)
	return eng
}

// ---------------------------------------------------------------------------
// PROOF 8 -- GH #652. THE LISTED CAPABILITY SET IS THE STORED ONE, EXACTLY,
// FOR A GRANT HOLDING MORE THAN THE DEFAULT.
//
// Until the capability vocabulary widened to eight strings every grant held
// exactly {mcp.sites.read}, so an omitted field here was indistinguishable
// from a correct one -- there was only ever one answer. This grant carries
// three, in a deliberately non-alphabetical, non-insertion-order column value,
// so a test that happened to pass on a coincidental ordering would not be
// mistaken for one that reads the column.
//
// This runs through the MOUNTED HANDLER, not connectionFromGrant directly, so
// it also proves toConnectionDTO does not drop the field on the way to JSON.
// ---------------------------------------------------------------------------

func TestListedCapabilitiesAreTheStoredSetExactlyForANonDefaultGrant(t *testing.T) {
	tenantID := uuid.New()
	stored := []string{
		string(CapPerformanceRead),
		string(CapSitesRead),
		string(CapUptimeRead),
	}
	store := &fakeStore{grants: []sqlc.McpGrant{
		{
			ID:            uuid.New(),
			Name:          "ci runner",
			Status:        string(GrantStatusActive),
			SiteScopeMode: string(SiteScopeModeAll),
			Capabilities:  stored,
		},
	}}
	eng := newConnectionsRouter(t, store, orgPrincipal(tenantID))

	w := httptest.NewRecorder()
	eng.ServeHTTP(w, httptest.NewRequest(http.MethodGet, ConnectionsPath, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list answered %d, want 200. body: %s", w.Code, w.Body.String())
	}

	var got connectionListDTO
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("list body did not decode: %v. body: %s", err, w.Body.String())
	}
	if len(got.Connections) != 1 {
		t.Fatalf("got %d connections, want 1", len(got.Connections))
	}

	gotCaps := got.Connections[0].Capabilities
	if len(gotCaps) != len(stored) {
		t.Fatalf("listed %d capabilities %v, want the %d stored ones %v",
			len(gotCaps), gotCaps, len(stored), stored)
	}
	want := map[string]bool{}
	for _, c := range stored {
		want[c] = true
	}
	for _, c := range gotCaps {
		if !want[c] {
			t.Errorf("listed capability %q was never in the stored set %v", c, stored)
		}
		delete(want, c)
	}
	if len(want) != 0 {
		remaining := make([]string, 0, len(want))
		for c := range want {
			remaining = append(remaining, c)
		}
		t.Errorf("stored capabilities %v never made it to the wire", remaining)
	}
}
