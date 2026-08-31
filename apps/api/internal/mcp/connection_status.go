package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// ---------------------------------------------------------------------------
// CONNECTION STATUS -- the read the add-connection wizard polls for Steps 8
// and 9 (wireframes S29, 2026-08-24 generation).
//
// ONE ENDPOINT FOR BOTH STEPS, AND THAT IS A DESIGN DECISION RATHER THAN A
// CONVENIENCE. Steps 8 and 9 are two questions about ONE grant row, and the
// wizard shows them stacked on one page. Two endpoints would mean two polls
// against the same row at two instants, which makes an INCONSISTENT PAIR
// representable: Step 9 answering "it read your fleet" while Step 8 still says
// "waiting for the client" is a state that cannot occur in reality (nothing can
// call a tool before it initializes) but is trivially producible by two reads
// straddling a handshake. The wizard would then have to reconcile them, and the
// reconciliation would live in the browser where nobody would test it. One
// handler, one snapshot: the pair is consistent by construction.
//
// POLLING, NOT SSE, AND ALSO A DECISION. The wireframe says "this page updates
// itself", which is a requirement about the USER's experience and not about the
// transport. Against SSE: ogen cannot model text/event-stream (ogen.yaml sets
// ignore_not_implemented), so a stream needs a hand-written transport, and the
// only hand-written streaming transport in this package is the very file
// another agent is completing in parallel. A stream would also hold a
// connection open per wizard tab for a page whose whole lifetime is the two or
// three minutes an operator spends pasting a config into a client. A poll costs
// one indexed primary-key read per interval and depends on nothing that is
// still moving. PollAfterMs is returned by the server so the interval is a
// server-side decision the client cannot drift from.
//
// WHAT THIS ENDPOINT WILL NOT DO: it will not synthesise a verdict its columns
// cannot support. Every state below is a fact read off a stored column, and the
// two places where the data genuinely cannot answer the wireframe are named
// explicitly in the types (HandshakeRefusal, FirstCallIndeterminate) rather than
// being rounded to the nearest state that renders nicely. An absence rendered
// as a fact is this feature's recurring defect and the reason those two fields
// exist.
// ---------------------------------------------------------------------------

// statusPollAfterMs is the interval the wizard is told to wait between polls.
//
// Two seconds, because the human on the other side has just been told to start
// a client and is watching a spinner: slower feels broken, and faster buys
// nothing because the event being waited on is a person alt-tabbing to a
// terminal. It is returned in the body rather than hard-coded in the browser so
// that changing it is a control-plane deploy and not a dashboard release.
const statusPollAfterMs = 2000

// firstCallScanLimit bounds how many mcp.tool.called rows this endpoint reads
// while looking for THIS grant's first one.
//
// A BOUND IS REQUIRED AND ITS BEING HIT IS NOT A FAILURE -- see
// FirstCallIndeterminate. The available generated query filters by action
// prefix and not by actor, so the oldest tool call for THIS connection can sit
// behind an arbitrary number of tool calls belonging to the organisation's
// OTHER connections. Reading a page and reporting "no call yet" when the page
// filled with other grants' rows would be an absence manufactured out of a
// pagination limit, which is precisely the defect class this file is written
// against. So the scan is bounded, and the bound being reached without a match
// is reported as its own third state.
const firstCallScanLimit = 200

// ---------------------------------------------------------------------------
// Step 8 -- the handshake
// ---------------------------------------------------------------------------

// HandshakeState is which of the wireframe's Step 8 frames a grant is in.
//
// THE FIRST VALUE IS A NOT-YET AND EVERY OTHER VALUE IS A FACT ABOUT SOMETHING
// THAT HAPPENED. Nothing here is a failure, and that is not an oversight: see
// HandshakeRefusal for the one Step 8 frame this server cannot currently reach.
type HandshakeState string

const (
	// HandshakeAwaitingClient: client_identity_recorded_at IS NULL. The
	// credential exists and nothing has opened a session with it. NOT-YET,
	// never a failure -- the wireframe's own words are "nothing is wrong yet".
	HandshakeAwaitingClient HandshakeState = "awaiting_client"

	// HandshakeConnected: the client initialized and sent a revision this
	// server speaks. The wireframe's "connected, and what we recorded".
	HandshakeConnected HandshakeState = "connected"

	// HandshakeConnectedProtocolAssumed: the client initialized and sent NO
	// MCP-Protocol-Version header, so the specification's floor was assumed.
	// The wireframe's "no version header at all", and its subtitle -- "treated
	// as the floor, deliberately not an error" -- is why this is a distinct
	// SUCCESS state and not a warning.
	HandshakeConnectedProtocolAssumed HandshakeState = "connected_protocol_assumed"

	// HandshakeConnectedProtocolUnrecognised: a revision is stored that this
	// server does not currently speak.
	//
	// NOT REACHABLE THROUGH TODAY'S LIVE TRANSPORT, and kept anyway. The
	// transport negotiates the header at transport.go:222 and refuses at :236
	// before dispatch, so a request carrying an unspeakable revision never
	// reaches RecordConnect and never writes this column. What CAN produce this
	// state is history: a revision that was inside supportedRevisions when it
	// was recorded and was later removed from the window. Classifying that
	// honestly costs one branch; rounding it to "connected" would print a
	// revision we no longer speak as though we did.
	HandshakeConnectedProtocolUnrecognised HandshakeState = "connected_protocol_unrecognised"
)

// HandshakeRefusal is the wireframe's Step 8 X frame -- "protocol version below
// the floor" -- and it is ALWAYS nil today.
//
// ============================================================================
// THIS IS A DATA GAP, STATED IN THE TYPE SO IT CANNOT BE MISTAKEN FOR A STATE.
// ============================================================================
//
// The wireframe wants a connection that was refused for speaking a revision
// below the floor to say so, with the exact error we returned, copyable. THE
// CONTROL PLANE DOES NOT RECORD THAT EVENT. TransportHandler.dispatch
// negotiates at transport.go:222 and calls writeProtocolRefusal at :236 (and
// again for the initialize params at :413) -- both return BEFORE
// Service.RecordConnect at :432. A refused client therefore writes NOTHING to
// mcp_grants: client_identity_recorded_at stays NULL, which is byte-identical
// to a client that has never been started at all.
//
// SO THIS ENDPOINT REPORTS A REFUSED CONNECTION AS HandshakeAwaitingClient,
// WHICH IS THE TRUTHFUL READING OF THE ROW AND IS NOT THE WIREFRAME'S FRAME.
// Reporting it as a refusal would be inventing an event out of a NULL. Closing
// the gap needs somewhere to put the refusal -- a column or a small table --
// and that is a migration, which belongs to database-engineer and not here.
//
// The Protocol block below still carries Floor, Target and Supported on every
// response, because those are properties of THIS SERVER and are true whatever
// the client did. The wizard can render "what our floor is" and the
// known-good-clients affordance from any response; what it cannot render, and
// must not, is "your client was refused" for a connection whose row cannot say
// so.
type HandshakeRefusal struct {
	// Requested is the revision the client sent.
	Requested string
	// Message is the exact operator-facing sentence the endpoint returned.
	Message string
	// At is when the refusal happened.
	At time.Time
}

// StatusProtocol is the Step 8 protocol block: what the client said, plus what
// this server requires.
//
// It wraps ClientProtocol rather than replacing it. ClientProtocol's four
// states are the vocabulary connection-model.ts already models on the
// frontend, and inventing a second vocabulary here is how two layers come to
// disagree about what "absent" means.
type StatusProtocol struct {
	// State and Version are ClassifyStoredProtocol's verdict, unchanged.
	// Version is EMPTY for never_connected and for absent, because NULL means
	// "the client sent no header" and never "unknown version".
	State   ClientProtocolState
	Version string

	// Assumed is the revision the specification told us to assume, and it is
	// set ONLY for ClientProtocolAbsent. It is a SEPARATE FIELD from Version on
	// purpose: writing the floor into Version would print a header the client
	// never sent, which is the exact manufacture ClassifyStoredProtocol's doc
	// comment refuses. The frontend renders these two together as "No protocol
	// header sent (treated as <floor>)".
	Assumed string

	// Floor, Target and Supported are properties of this server, true on every
	// response including one for a client that has never connected.
	Floor     string
	Target    string
	Supported []string
}

// ConnectionHandshake is the whole Step 8 answer.
type ConnectionHandshake struct {
	State HandshakeState
	// RecordedAt is when the client identified itself. nil for
	// HandshakeAwaitingClient and non-nil for every other state -- it is the
	// column the state is derived from, returned so the wizard can show the
	// timestamp the wireframe puts beside each completed check.
	RecordedAt *time.Time
	// ReportedClientName and ReportedClientVersion are the client's OWN
	// unverified claims. nil, never "", when nothing was reported.
	ReportedClientName    *string
	ReportedClientVersion *string
	Protocol              StatusProtocol
	// Refusal is always nil. See the HandshakeRefusal doc comment.
	Refusal *HandshakeRefusal
}

// ---------------------------------------------------------------------------
// Step 9 -- the first read
// ---------------------------------------------------------------------------

// FirstCallState is which of the wireframe's Step 9 frames a grant is in.
type FirstCallState string

const (
	// FirstCallAwaiting: the scan completed and found no mcp.tool.called row
	// for this grant. A DEFINITIVE NOT-YET -- the scan saw fewer rows than its
	// bound, so it saw all of them.
	//
	// This is BOTH of the wireframe's "L -- watching for the first call" and
	// "X -- no call ever arrived". They are the same server-side fact and they
	// differ only in how long the operator has been staring at it, which is a
	// clock the browser owns. The server does not have a threshold past which
	// a not-yet becomes a failure, must not invent one, and returns CreatedAt
	// and the handshake's RecordedAt so the wizard can apply its own.
	FirstCallAwaiting FirstCallState = "awaiting_call"

	// FirstCallSucceeded: an mcp.tool.called row exists for this grant. The
	// wireframe's "it worked".
	FirstCallSucceeded FirstCallState = "succeeded"

	// FirstCallIndeterminate: the scan hit firstCallScanLimit without finding a
	// row for this grant, so this endpoint DOES NOT KNOW.
	//
	// A THIRD STATE RATHER THAN A ROUNDED-DOWN NOT-YET. The difference matters
	// to the wizard: "no call yet" invites it to keep waiting and eventually to
	// show the X frame's troubleshooting, which would be wrong advice for a
	// connection that is working fine and merely sits behind 200 of its
	// siblings' tool calls. The honest render is "we cannot tell from here",
	// and the fix is the actor-filtered query named in FindFirstToolCall.
	FirstCallIndeterminate FirstCallState = "indeterminate"
)

// ConnectionFirstCall is the whole Step 9 answer.
type ConnectionFirstCall struct {
	State FirstCallState

	// CalledAt, ToolName and AuditEventID are set ONLY for
	// FirstCallSucceeded. They are the wireframe's "Tool it called" and its
	// "Audit row" line.
	CalledAt     *time.Time
	ToolName     string
	AuditEventID *uuid.UUID

	// LastUsedAt is mcp_grants.last_used_at, and it is REPORTED ALONGSIDE THE
	// STATE RATHER THAN USED TO DERIVE IT.
	//
	// THIS IS THE TRAP IN STEP 9 AND IT IS WORTH THE PARAGRAPH. last_used_at
	// looks like the cheap way to answer "has this connection done anything",
	// and it is wrong for this question: Service.RecordActivity stamps it from
	// the transport's tools/list arm as well as tools/call. Every MCP client
	// issues tools/list immediately after initialize, without any user asking
	// it to. Deriving Step 9 from this column would therefore flip the wizard
	// to "Connected and working -- it read your fleet" for a client that has
	// merely enumerated the tool list and read nothing at all. That is a FALSE
	// SUCCESS on the one screen whose entire job is to prove a real read
	// happened, so the state comes from the audit row and this column is
	// returned only as the weaker, honestly-labelled fact it is.
	LastUsedAt *time.Time

	// Partial is the wireframe's "P -- partial multi-site success" frame, and
	// it is ALWAYS nil.
	//
	// ========================================================================
	// NOT ANSWERABLE TODAY, DELIBERATELY LEFT UNANSWERED RATHER THAN FAKED.
	// ========================================================================
	//
	// The frame wants "17 answered, 3 stale, 2 unreachable -- and it still
	// succeeded", with the same three numbers handed to the model in a typed
	// result. internal/mcp has no typed per-site partial: a tool invocation
	// returns a string or an error, and the only incompleteness it can express
	// is whole-result truncation. The wireframe's own 2026-08-30 scoping block
	// calls this out as an uncosted prerequisite and it has its own issue.
	//
	// The failure mode being avoided is named in the wireframe: the model "does
	// not get a clean answer over an incomplete read -- that is how a fleet
	// answer becomes quietly wrong". Synthesising counts here, or reporting
	// FirstCallSucceeded as though coverage were complete when this endpoint
	// has no way to know, would put that same wrongness on the operator's
	// screen. So a successful call reports FirstCallSucceeded with Partial nil,
	// meaning "a tool call happened and we cannot characterise its coverage" --
	// never "it covered everything".
	Partial *FirstCallPartial
}

// FirstCallPartial is the per-site coverage breakdown. Nothing constructs one
// today; see the Partial field's doc comment.
type FirstCallPartial struct {
	SitesAsked  int
	Answered    int
	Stale       int
	Unreachable int
}

// ConnectionStatus is the whole polled answer: one grant, both steps, one
// instant.
type ConnectionStatus struct {
	ID uuid.UUID
	// Status is the grant's stored liveness. A revoked connection still
	// answers this endpoint -- the wizard needs to stop polling and say so,
	// and a 404 would read as "wrong id".
	Status    GrantStatus
	CreatedAt time.Time
	ExpiresAt time.Time

	Handshake ConnectionHandshake
	FirstCall ConnectionFirstCall

	// ObservedAt is when this snapshot was taken, so the wizard's relative
	// times ("4 seconds ago") are computed against the server's clock and not
	// against a browser clock that may be minutes out.
	ObservedAt time.Time
	// PollAfterMs is how long the client should wait before asking again.
	PollAfterMs int
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// ConnectionStatus answers Steps 8 and 9 for one grant.
//
// requireOrgScopedPrincipal FIRST, before any read. This is the same gate
// ListConnections and RevokeConnection take, and it is the permission-layer
// half of what mcp_grants_site_scope_select does in the database: a grant is an
// ORGANISATION-wide credential with no :siteId for RequireSiteAccess to key on,
// so a site-constrained principal is refused outright rather than filtered. A
// site-scoped collaborator reaching an org-wide grant was a live defect on this
// exact surface, and this endpoint is a READ of the same object the defect was
// about -- the client name, the client version and the protocol revision of the
// organisation's AI connections are not facts a single-site collaborator is
// entitled to.
func (s *Service) ConnectionStatus(ctx context.Context, p domain.Principal, grantID uuid.UUID) (ConnectionStatus, error) {
	if err := requireOrgScopedPrincipal(p); err != nil {
		return ConnectionStatus{}, err
	}
	if grantID == uuid.Nil {
		return ConnectionStatus{}, domain.NotFound("connection_not_found", "connection not found")
	}

	// ONE call, because the two facts must come from ONE ordered read. See
	// Store.ConnectionStatusSnapshot: the service is deliberately given no way
	// to fetch the handshake and the first call separately, because doing so is
	// what made the impossible pair representable.
	snap, err := s.store.ConnectionStatusSnapshot(ctx, p, grantID, firstCallScanLimit)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Absent, another organisation's, or refused by RLS. All three are
			// 404 and are deliberately INDISTINGUISHABLE to the caller: a 403
			// on another tenant's grant id would confirm that the id exists,
			// which is the cross-tenant leak this surface must not have.
			return ConnectionStatus{}, domain.NotFound("connection_not_found", "connection not found")
		}
		// NOT swallowed into FirstCallAwaiting. A failed audit read that
		// rendered as "no call yet" would send the operator to the wireframe's
		// troubleshooting frame for a connection that may well be working --
		// an infrastructure failure reported as a fact about the client.
		return ConnectionStatus{}, fmt.Errorf("mcp connection status: %w", err)
	}

	grant := snap.Grant
	return ConnectionStatus{
		ID:          grant.ID,
		Status:      GrantStatus(grant.Status),
		CreatedAt:   grant.CreatedAt,
		ExpiresAt:   grant.ExpiresAt,
		Handshake:   handshakeFromGrant(grant),
		FirstCall:   firstCallFrom(snap.FirstCall, timestamptzTimeOrNil(grant.LastUsedAt)),
		ObservedAt:  s.now().UTC(),
		PollAfterMs: statusPollAfterMs,
	}, nil
}

// handshakeFromGrant derives Step 8 from the two stored columns.
//
// The state is switched off ClassifyStoredProtocol's verdict rather than being
// re-derived from the columns, so there is exactly ONE place the two columns
// are read as four states and this function cannot drift from the frontend
// vocabulary it feeds.
func handshakeFromGrant(g sqlc.McpGrant) ConnectionHandshake {
	// timestamptzTimeOrNil, the same helper connectionFromGrant uses: it is the
	// one line that keeps SQL NULL out of the Go zero time, and reading the
	// column any other way here would reintroduce 0001-01-01 as a handshake
	// date on exactly the state that means "no handshake".
	recordedAt := timestamptzTimeOrNil(g.ClientIdentityRecordedAt)
	proto := ClassifyStoredProtocol(recordedAt, g.ProtocolVersion)

	h := ConnectionHandshake{
		RecordedAt:            recordedAt,
		ReportedClientName:    g.ClientName,
		ReportedClientVersion: g.ClientVersion,
		Protocol: StatusProtocol{
			State:     proto.State,
			Version:   proto.Version,
			Floor:     ProtocolFloor,
			Target:    ProtocolTarget,
			Supported: SupportedRevisions(),
		},
		// Always nil. See HandshakeRefusal.
		Refusal: nil,
	}

	switch proto.State {
	case ClientProtocolNeverConnected:
		h.State = HandshakeAwaitingClient
	case ClientProtocolAbsent:
		h.State = HandshakeConnectedProtocolAssumed
		// The one place the floor is attached, and it goes in Assumed rather
		// than Version.
		h.Protocol.Assumed = ProtocolFloor
	case ClientProtocolRecognised:
		h.State = HandshakeConnected
	case ClientProtocolUnrecognised:
		h.State = HandshakeConnectedProtocolUnrecognised
	default:
		// Unreachable while ClassifyStoredProtocol returns one of four. If a
		// fifth is ever added, this reports the NOT-YET rather than inventing a
		// success -- the fail-closed direction for a state machine whose
		// success states are claims about a client we may not have heard from.
		h.State = HandshakeAwaitingClient
	}
	return h
}

// firstCallFrom derives Step 9 from the audit scan's outcome.
//
// lastUsedAt is carried through to the struct and is NOT consulted for the
// state; see ConnectionFirstCall.LastUsedAt for why that would be a false
// success.
func firstCallFrom(c FirstToolCall, lastUsedAt *time.Time) ConnectionFirstCall {
	out := ConnectionFirstCall{LastUsedAt: lastUsedAt, Partial: nil}

	switch {
	case c.Found:
		out.State = FirstCallSucceeded
		at := c.At
		id := c.EventID
		out.CalledAt = &at
		out.AuditEventID = &id
		out.ToolName = c.ToolName
	case c.Truncated:
		out.State = FirstCallIndeterminate
	default:
		out.State = FirstCallAwaiting
	}
	return out
}

// ---------------------------------------------------------------------------
// Repo
// ---------------------------------------------------------------------------

// FirstToolCall is what the audit scan found, with its own honesty flag.
//
// Found and Truncated are SEPARATE BOOLEANS rather than one tri-state enum
// because the caller must not be able to read "not found" without also seeing
// whether the search was complete. A single "found bool" would make the
// truncated case indistinguishable from a definitive miss at the call site,
// which is the whole defect.
type FirstToolCall struct {
	// Found reports that a matching row was seen.
	Found bool
	// Truncated reports that the scan reached its bound. Meaningful only when
	// Found is false: a scan that found its row stopped looking.
	Truncated bool

	EventID  uuid.UUID
	At       time.Time
	ToolName string
}

// ConnectionSnapshot is the pair of reads Steps 8 and 9 are derived from.
//
// It exists so the two CANNOT be fetched independently. See
// Repo.ConnectionStatusSnapshot for the ordering that makes the pair
// consistent, and Store.ConnectionStatusSnapshot for why this replaced two
// separately-callable reads rather than sitting alongside them.
type ConnectionSnapshot struct {
	Grant     sqlc.McpGrant
	FirstCall FirstToolCall
}

// ConnectionStatusSnapshot reads the grant and the first tool call for Steps 8
// and 9 in ONE transaction and, more importantly, in ONE ORDER.
//
// ============================================================================
// THE ORDER IS THE FIX. THE SHARED TRANSACTION ALONE IS NOT.
// ============================================================================
//
// The defect this replaces ran GetGrant and FindFirstToolCall as two separate
// transactions. An initialize and a tool call committing between them produced
// handshake=awaiting_client together with first_call=succeeded -- the exact
// contradiction this endpoint was made a SINGLE endpoint to prevent. The
// contradiction had simply moved inside the handler.
//
// Wrapping both reads in one transaction does NOT by itself fix it, and
// believing it does is the trap here. Every helper in internal/db begins with
// p.Begin, which is READ COMMITTED, and under READ COMMITTED each STATEMENT
// takes its own snapshot -- so two statements in one transaction still straddle
// a concurrent commit exactly as two transactions did. A true transaction-wide
// snapshot needs REPEATABLE READ, which must be set at BEGIN; the tx helpers
// set the RLS GUCs with SELECT set_config before fn runs, so by the time this
// function has a tx the isolation level can no longer be changed. Raising it
// would mean a new BeginTx-based helper in internal/db, which is the tenancy
// boundary and a change of its own.
//
// So consistency is bought with ORDERING instead, which needs no isolation
// change and no new helper:
//
//	   THE INVARIANT: mcp_grants.client_identity_recorded_at is stamped by
//	   RecordClientIdentity (db/query/mcp_connections.sql, "client_identity_
//	   recorded_at = now()") and is NEVER written back to NULL. A tool call's
//	   audit row is written strictly after the initialize that recorded the
//	   client. The handshake fact is therefore MONOTONIC and always at least as
//	   old as the first-call fact.
//
//	   THE CONSEQUENCE: read the LATER fact first and the EARLIER fact last.
//	   Anything that commits between the two reads can then only make the
//	   handshake MORE advanced, never less. The reachable pairs become
//	   awaiting_client+awaiting_call, connected+awaiting_call and
//	   connected+succeeded -- all three real states. awaiting_client+succeeded
//	   is unreachable by racing, which is the whole finding.
//
// Reversing steps 2 and 3 below reintroduces the defect, which is what the
// regression test pins.
//
// WHY THE GRANT IS READ TWICE. Step 1 is an existence-and-RLS gate whose only
// product is the 404: without it a poll for an absent or another tenant's grant
// id would run the bounded audit scan -- up to firstCallScanLimit+1 rows --
// before discovering it had nothing to answer about, turning a cheap 404 into
// an audit-log scan any authenticated org principal could trigger by guessing
// ids. Its returned row is DELIBERATELY DISCARDED: it is older than the scan
// and using it would be the original bug. Step 3 re-reads the same primary key
// inside the same open transaction -- an indexed single-row read -- and that
// row is the one the handshake is derived from.
//
// THE AUDIT SCAN'S COST INSIDE THE TRANSACTION. findFirstToolCallTx reads at
// most firstCallScanLimit+1 rows from one index and holds no locks; it is a
// read-only transaction, so nothing here blocks a writer or pins a row. What
// the longer transaction does cost is a pooled connection held for the scan's
// duration rather than for two short reads, and one xmin held back for that
// window. At a two-second poll per open wizard tab that is not a vacuum
// concern, and it is strictly less connection churn than the two transactions
// it replaces.
//
// RunTenantTx and not InTenantTx, for the reason ListGrants gives: mcp_grants
// carries a RESTRICTIVE mcp_grants_site_scope_select policy as well as the
// tenant-isolation one, and InTenantTx does not set app.site_scope or
// app.allowed_site_ids, so the restrictive policy would be INERT and a
// site-constrained principal would read an organisation-wide credential with a
// 200 and no trace. Combining the reads did NOT force the plain helper: both
// reads were already on RunTenantTx and they still are, now on ONE dispatch
// instead of two. Service.ConnectionStatus refuses such a principal before
// reaching here; this is the second lock on the same door, in the layer that
// still holds if somebody later mounts this read behind a different gate.
//
// pgx.ErrNoRows is returned VERBATIM so the caller can 404 on it.
func (r *Repo) ConnectionStatusSnapshot(
	ctx context.Context,
	principal domain.Principal,
	grantID uuid.UUID,
	limit int,
) (ConnectionSnapshot, error) {
	var out ConnectionSnapshot
	err := r.pool.RunTenantTx(ctx, principal, func(tx pgx.Tx) error {
		// 1. Gate. Existence and RLS only; the row itself is thrown away.
		if _, err := getGrantTx(ctx, tx, principal.TenantID, grantID); err != nil {
			return err
		}

		// 2. The LATER fact.
		call, err := findFirstToolCallTx(ctx, tx, principal.TenantID, grantID, limit)
		if err != nil {
			return err
		}

		// 3. The EARLIER fact, read last so it can only be fresher than the
		//    scan. This is the row the handshake state comes from.
		grant, err := getGrantTx(ctx, tx, principal.TenantID, grantID)
		if err != nil {
			return err
		}

		out = ConnectionSnapshot{Grant: grant, FirstCall: call}
		return nil
	})
	if err != nil {
		return ConnectionSnapshot{}, err
	}
	return out, nil
}

// getGrantTx is the single-row grant read, on an already-open transaction so
// the caller owns the ordering. pgx.ErrNoRows passes through untouched.
func getGrantTx(ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID) (sqlc.McpGrant, error) {
	return sqlc.New(tx).GetMCPGrant(ctx, sqlc.GetMCPGrantParams{
		TenantID: tenantID,
		ID:       grantID,
	})
}

// findFirstToolCallTx looks for the OLDEST mcp.tool.called audit row belonging
// to this grant.
//
// THE OLDEST, because Step 9 asks "did the first read happen", and the wizard
// renders that one call. ListAuditEntriesFiltered orders newest-first, so the
// match is taken by walking the scanned window BACKWARDS.
//
// WHY THIS SCANS INSTEAD OF FILTERING, AND WHAT IT WOULD TAKE TO STOP.
// ListAuditEntriesFiltered is the only generated read over audit_log that this
// package can reach, and it filters on action prefix, site and time -- NOT on
// actor_id. mcp.tool.called rows carry the grant id in actor_id (audit.go's
// ActorAssistant attribution), so the match has to happen in Go. The clean fix
// is a generated query filtered by actor_type + actor_id, which is a .sql
// change and therefore database-engineer's; with it, both the bound and the
// FirstCallIndeterminate state disappear.
//
// THE BOUND IS REPORTED, NEVER SWALLOWED. limit+1 rows are requested so that
// "the window was full" is distinguishable from "the window was exactly the
// size of the data", and Truncated is set from that. A scan that fills its
// window without matching returns Found=false AND Truncated=true, and the
// service turns that pair into FirstCallIndeterminate rather than into a
// not-yet.
//
// It runs on an ALREADY-OPEN transaction rather than opening its own, so that
// ConnectionStatusSnapshot owns the order in which this read and the grant read
// happen. That ordering is load-bearing; see ConnectionStatusSnapshot.
func findFirstToolCallTx(
	ctx context.Context,
	tx pgx.Tx,
	tenantID uuid.UUID,
	grantID uuid.UUID,
	limit int,
) (FirstToolCall, error) {
	var out FirstToolCall
	want := grantID.String()

	rows, err := sqlc.New(tx).ListAuditEntriesFiltered(ctx, sqlc.ListAuditEntriesFilteredParams{
		TenantID:     tenantID,
		ActionPrefix: audit.ActionMCPToolCalled,
		// The disabled-filter sentinels this query documents: the zero uuid
		// turns the site filter off, and the zero Timestamptz values are
		// not Valid, which is the NULL that turns the time window off.
		SiteID:    uuid.Nil.String(),
		RowOffset: 0,
		RowLimit:  int32(limit) + 1,
	})
	if err != nil {
		return FirstToolCall{}, fmt.Errorf("scan mcp tool calls: %w", err)
	}

	out.Truncated = len(rows) > limit
	if out.Truncated {
		rows = rows[:limit]
	}

	// Newest-first, so walking BACKWARDS reaches the oldest row first and
	// the first hit is the one Step 9 wants.
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		// actor_type is checked as well as actor_id. actor_id is a free
		// text column shared by every actor kind, so a uuid match alone
		// would also accept a row written by a user or an api_key that
		// happened to carry this id -- and the grant id and the user id
		// live in the same column space. Both halves, or the attribution
		// is a coincidence.
		if row.ActorType != audit.ActorAssistant || row.ActorID != want {
			continue
		}
		out.Found = true
		// A found row ends the search, so Truncated stops being
		// meaningful and is cleared rather than left set for the service
		// to have to ignore.
		out.Truncated = false
		out.EventID = row.ID
		out.At = row.CreatedAt
		// target_id is the tool name -- RecordToolCall stores it there.
		out.ToolName = row.TargetID
		return out, nil
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// connectionStatus answers GET /api/v1/mcp/connections/:connectionId/status.
//
// PermAPIKeyRead, matching listConnections: this is a READ of the same object
// the list renders, so it takes the read tier and not the manage one. That
// permission is a member of authz.orgLevelPerms, which is what makes
// RequirePermission refuse a site-constrained principal outright -- the
// permission-layer half of the org-scope gate, restated at the route because a
// handler that trusts its mount point is one refactor away from ungated.
func (h *Handler) connectionStatus(c *gin.Context) {
	principal, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		// Unreachable behind RequireAuth; refusing anyway, because a handler
		// that trusts its mount point is one refactor away from anonymous.
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}

	grantID, err := uuid.Parse(c.Param(connectionIDParam))
	if err != nil {
		// 404 and not 400. A malformed id and an id belonging to another
		// organisation must answer identically, or the difference between the
		// two answers becomes an oracle for which ids exist.
		httpx.Error(c, domain.NotFound("connection_not_found", "connection not found"))
		return
	}

	st, err := h.svc.ConnectionStatus(c.Request.Context(), principal, grantID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, connectionStatusResponse(st))
}

// ---------------------------------------------------------------------------
// DTO
// ---------------------------------------------------------------------------

// connectionStatusResponse maps the domain answer onto the wire.
//
// Every nullable stays nullable. A *time.Time that became a time.Time would
// serialise the Go zero value as "0001-01-01T00:00:00Z", which is a real
// timestamp on the wire and renders as a connection that handshook in the year
// one -- an absence turned into a fact by a type choice.
func connectionStatusResponse(s ConnectionStatus) gin.H {
	h := gin.H{
		"state":                   string(s.Handshake.State),
		"recorded_at":             s.Handshake.RecordedAt,
		"reported_client_name":    s.Handshake.ReportedClientName,
		"reported_client_version": s.Handshake.ReportedClientVersion,
		"protocol": gin.H{
			"state":     string(s.Handshake.Protocol.State),
			"version":   emptyToNil(s.Handshake.Protocol.Version),
			"assumed":   emptyToNil(s.Handshake.Protocol.Assumed),
			"floor":     s.Handshake.Protocol.Floor,
			"target":    s.Handshake.Protocol.Target,
			"supported": s.Handshake.Protocol.Supported,
		},
		// Always null; the key is present so the frontend can bind it now and
		// light up when the refusal is recorded, rather than the shape changing
		// under it later.
		"refusal": nil,
	}

	f := gin.H{
		"state":          string(s.FirstCall.State),
		"called_at":      s.FirstCall.CalledAt,
		"tool_name":      emptyToNil(s.FirstCall.ToolName),
		"audit_event_id": s.FirstCall.AuditEventID,
		"last_used_at":   s.FirstCall.LastUsedAt,
		// Always null. See ConnectionFirstCall.Partial.
		"partial": nil,
	}

	return gin.H{
		"id":            s.ID,
		"status":        string(s.Status),
		"created_at":    s.CreatedAt,
		"expires_at":    s.ExpiresAt,
		"handshake":     h,
		"first_call":    f,
		"observed_at":   s.ObservedAt,
		"poll_after_ms": s.PollAfterMs,
	}
}

// emptyToNil keeps "" off the wire as a value.
//
// The domain types use "" for "not set" on the two protocol strings and on the
// tool name, because ClientProtocol already does and matching it beats adding a
// second convention. On the WIRE that must become null: a JSON "" is a value,
// and a frontend rendering `version || "none"` behaves differently from one
// rendering `version ?? "none"`. null makes both correct.
func emptyToNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
