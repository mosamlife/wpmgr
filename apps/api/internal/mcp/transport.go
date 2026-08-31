package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// Streamable HTTP transport (design §6, "Transport")
//
// ONE endpoint, POST, one origin, one TLS boundary. Server-sent events are not
// required of a client in Phase 1, so this handler answers every request with
// a single JSON body and never upgrades. The server terminates entirely on the
// control plane: no MCP endpoint, adapter or inbound auth surface ships inside
// the WordPress plugin.
// ---------------------------------------------------------------------------

// TransportPath is the single MCP endpoint. It is published to users on the
// connect screen as https://app.wpmgr.app/mcp, so it is a compatibility
// surface and not an implementation detail.
const TransportPath = "/mcp"

// Page bound for the one read tool. The byte cap usually binds first; this
// bounds the QUERY so a very large fleet cannot make one request read the
// whole sites table.
const sitesPageBound = 500

// maxRequestBytes caps the JSON-RPC request body. Phase 1's largest legitimate
// request is an initialize with client info -- a few hundred bytes -- so 256
// KiB is orders of magnitude of headroom and still refuses a body sized to
// exhaust a shared instance.
const maxRequestBytes = 256 * 1024

// JSON-RPC 2.0 reserved codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// Server-defined codes, closed set, in the JSON-RPC implementation-defined
// range. They are stable wire contract: a client branches on the number, and
// the message is for the human and the model.
const (
	// codeProtocolUnsupported covers BOTH refusal cases of the revision
	// negotiation. The two are distinguished by the message, which names the
	// floor and the target, and by the HTTP status, which is 400 for both.
	codeProtocolUnsupported = -32001

	// codeScopeEmpty is the named refusal for a grant whose site scope
	// resolves to nothing. It is NEVER an empty success: an empty result that
	// reads as "nothing to do" is how a scoping bug becomes invisible.
	codeScopeEmpty = -32002

	// codeInvalidToolArguments carries the offending argument and the schema
	// inline, so the model corrects in one round trip.
	codeInvalidToolArguments = -32003

	// codeToolNotAvailable is the single code for the TWO reasons a named tool
	// is not reachable that a caller must not be able to tell apart: no such
	// tool exists, or this connection's capabilities do not cover it. One code
	// for two reasons is deliberate and is the whole disclosure decision -- see
	// the note on AuthorizeTool. A client branching on the number learns "ask
	// tools/list", which is the only correct action in both cases.
	//
	// AN EMPTY SITE SCOPE IS NOT ONE OF THEM and does not answer -32004. It
	// answers codeScopeEmpty (-32002) above, by name, because a connection
	// whose capability set already covers the tool is entitled to know the tool
	// exists -- see the asymmetry note on visible(). An earlier version of this
	// comment claimed -32004 covered the scope-empty case too. It never did:
	// TestToolsCall_EmptyScopeIsRefusedNotAnEmptyList asserts -32002.
	codeToolNotAvailable = -32004
)

// TransportHandler serves the single MCP endpoint. It is separate from Handler
// (the OAuth endpoints) because the two have different authentication: the
// OAuth handler authenticates an operator session or a PKCE code, this one
// authenticates a bearer connection token on every single request.
type TransportHandler struct {
	svc     *Service
	log     *slog.Logger
	version string
}

// NewTransportHandler builds the MCP transport handler. version is reported in
// initialize's serverInfo.
func NewTransportHandler(svc *Service, log *slog.Logger, version string) *TransportHandler {
	return &TransportHandler{svc: svc, log: log, version: version}
}

// Register mounts the ONE endpoint on the given group.
//
// It is mounted on the ROOT engine, deliberately not on the session-auth group:
// an MCP client carries a bearer token and no cookie, so loading a session per
// request would cost a Redis round trip for nothing and would create a standing
// trap where this path might come to depend on a session principal.
func (h *TransportHandler) Register(r gin.IRouter) {
	r.POST(TransportPath, h.serve)

	// OPTIONS IS THE CORS PREFLIGHT AND IS ANSWERED, NOT REFUSED.
	//
	// It was in the 405 list below, and that combination made this endpoint
	// UNREACHABLE from any browser-hosted MCP client. The sequence is: the
	// client reads our discovery documents, which set
	// Access-Control-Allow-Origin: * specifically so a browser may read them
	// (discovery.go), learns this endpoint's URL, and then cannot call it,
	// because its preflight is answered 405 and the browser never issues the
	// POST. We published an invitation to a door we had locked.
	//
	// A preflight is not avoidable by a conforming client. Authorization,
	// MCP-Protocol-Version, MCP-Session-Id and Last-Event-ID are all
	// non-safelisted request headers, and so is Content-Type: application/json
	// (the safelist covers only form and text/plain bodies) -- so essentially
	// every real MCP request forces one.
	r.OPTIONS(TransportPath, h.preflight)

	// EVERY OTHER VERB ON THIS PATH ANSWERS 405, NOT 404.
	//
	// This is the same rule as the 401-not-404 one below, on the other axis,
	// and it was got wrong here first: a 404 on a verb hides a routing failure
	// behind something that looks like a deliberate refusal, exactly as a 404
	// on an unauthenticated request would. The path is PUBLISHED to users as
	// https://app.wpmgr.app/mcp, so `curl` against it -- which sends GET -- is
	// the first thing anyone does, and gin's bare "404 page not found" tells
	// that operator the service is not deployed.
	//
	// GET and DELETE are additionally REAL parts of the Streamable HTTP
	// transport (the SSE stream and session teardown). Phase 1 offers neither,
	// and the transport's own answer for a server that does not offer them is
	// 405 -- so a conforming client that opens the GET stream first learns
	// "this server does not do that" instead of "there is no server here".
	// The specification names 405 for both by name, for the GET stream
	// ("or else return HTTP 405 Method Not Allowed, indicating that the server
	// does not offer an SSE stream at this endpoint") and for DELETE ("the
	// server MAY respond to this request with HTTP 405 Method Not Allowed,
	// indicating that the server does not allow clients to terminate
	// sessions"). Neither is a placeholder for unfinished work.
	//
	// Registered explicitly per verb rather than via HandleMethodNotAllowed,
	// because that flag is engine-global and would change the response of
	// every other route in the API as a side effect of this slice.
	for _, verb := range []string{
		http.MethodGet,
		http.MethodHead,
		http.MethodPut,
		http.MethodPatch,
		http.MethodDelete,
	} {
		r.Handle(verb, TransportPath, h.methodNotAllowed)
	}
}

// ---------------------------------------------------------------------------
// CORS
// ---------------------------------------------------------------------------

// corsAllowMethods is what a browser may ATTEMPT, which is deliberately not the
// same set as the Allow header's "what will succeed".
//
// GET and DELETE are listed even though both answer 405. Omitting them would
// make the browser block the request before our 405 could be read, and the
// client would surface an opaque CORS network error -- indistinguishable from
// "the server is down". That is precisely the confusion the 405-not-404 rule
// above exists to prevent, so the same reasoning applies one layer out: let the
// request through so it can receive an honest refusal.
var corsAllowMethods = strings.Join([]string{
	http.MethodPost, http.MethodGet, http.MethodDelete,
}, ", ")

// corsAllowHeaders is every header the Streamable HTTP transport specifies a
// client may send, plus the Authorization header the authorization spec adds.
//
// Header names are case-insensitive both in HTTP and in the CORS matching
// algorithm, which matters here: the session header is spelled Mcp-Session-Id
// in revision 2025-06-18 and MCP-Session-Id in 2025-11-25. One spelling covers
// both revisions, and neither has to be tracked as the window moves.
//
// Last-Event-ID and the session header are listed even though Phase 1 issues no
// session id and offers no resumable stream: a client that sends them anyway is
// conforming, and a preflight that omitted them would fail the whole request
// rather than let it through to the answer it should get.
var corsAllowHeaders = strings.Join([]string{
	"Authorization",
	"Content-Type",
	"Accept",
	ProtocolHeader,
	"Mcp-Session-Id",
	"Last-Event-ID",
}, ", ")

// corsExposeHeaders is what browser JavaScript may READ off our responses.
// Without this the client can see the status code and nothing else.
//
// WWW-Authenticate is the load-bearing one: writeUnauthorized sets it to name
// the scheme, and a browser client that cannot read it learns only "401" with
// no indication of how to authenticate.
var corsExposeHeaders = strings.Join([]string{
	"WWW-Authenticate", "Mcp-Session-Id", "Allow",
}, ", ")

// writeCORS sets the headers that must appear on an ACTUAL response, as opposed
// to a preflight. It runs on every verb and BEFORE authentication, because a
// response the browser refuses to hand to the client is not a refusal the
// client can act on -- an unreadable 401 is indistinguishable from a network
// failure.
//
// THE ORIGIN IS "*" AND THERE IS DELIBERATELY NO Access-Control-Allow-Credentials.
//
// "*" is safe HERE for a reason specific to this endpoint, and the reason is
// not that discovery.go does the same thing one file over -- that surface
// publishes public metadata, this one carries a credential, and they do not
// automatically share a policy. It is safe because this endpoint has NO ambient
// authentication to steal. It is mounted on the root engine and never on the
// session-auth group, so no cookie authenticates it; the only credential it
// accepts is a bearer token that the calling page's own JavaScript must already
// hold and set explicitly. A hostile page therefore gains nothing from being
// allowed to send a request it could only authenticate with a token it would
// have to have stolen first.
//
// Allow-Credentials: true is the setting that WOULD be a hole -- it would make
// the browser attach ambient credentials to cross-origin requests -- and it is
// also incompatible with "*". It is not set, and must not be.
//
// Narrowing "*" to an allowlist would buy nothing today and cost real
// interoperability: browser-hosted MCP clients are served from origins we
// cannot enumerate, which is the entire reason this surface offers dynamic
// client registration. It would also not constrain a non-browser client at all,
// since CORS is enforced by the browser and not by us.
//
// COUPLING WITH ORIGIN VALIDATION, WHICH IS NOT YET BUILT. Revision 2025-11-25
// hardened the transport's Origin rule to a MUST ("Servers MUST validate the
// Origin header on all incoming connections ... If the Origin header is present
// and invalid, servers MUST respond with HTTP 403 Forbidden"), and this server
// does not validate Origin at all. That is tracked separately. The two
// mechanisms are NOT the same thing and do not conflict, but they do constrain
// each other: if Origin validation lands as an ALLOWLIST, then this "*" must
// become a reflection of the single allowed origin plus a "Vary: Origin"
// response header, and the 403 must be applied to the preflight as well as to
// the real request, or a browser client gets a preflight that succeeds followed
// by a POST that is forbidden. Whoever builds that half changes this function
// in the same commit.
func writeCORS(c *gin.Context) {
	c.Header("Access-Control-Allow-Origin", "*")
	c.Header("Access-Control-Expose-Headers", corsExposeHeaders)
}

// preflight answers the CORS preflight for the MCP endpoint.
//
// IT DOES NOT AUTHENTICATE, AND THAT IS NOT A HOLE.
//
// A browser strips Authorization from a preflight by construction -- the whole
// purpose of the OPTIONS round trip is to ask permission BEFORE attaching the
// credential -- so requiring a bearer here would refuse every preflight ever
// sent and restore exactly the lockout this handler exists to fix. Nothing is
// served in response: the body is empty and the only thing disclosed is which
// methods and headers this endpoint accepts, which is already public in the
// discovery documents and in this file's published path.
//
// The bearer requirement is untouched. A successful preflight buys the caller
// the right to SEND the real request, and that real request meets exactly the
// same 401 it does today.
func (h *TransportHandler) preflight(c *gin.Context) {
	writeCORS(c)
	c.Header("Access-Control-Allow-Methods", corsAllowMethods)
	c.Header("Access-Control-Allow-Headers", corsAllowHeaders)
	// Ten minutes. Chrome caps preflight caching at 7200s and the value only
	// trades a round trip against how long a policy change takes to reach a
	// client that has already cached it; short is the safer side of that.
	c.Header("Access-Control-Max-Age", "600")
	c.Status(http.StatusNoContent)
}

// methodNotAllowed answers a non-POST verb on the published path with a JSON
// 405 that names the one verb this transport accepts.
//
// It deliberately does NOT authenticate first. The verb is wrong regardless of
// who is asking, and answering 401 to an unauthenticated GET would send an
// operator to rotate a credential when the actual fix is to send a POST.
func (h *TransportHandler) methodNotAllowed(c *gin.Context) {
	// The 405 is only useful if the browser lets the client read it. Without
	// this, a browser-hosted client probing the GET stream sees an opaque CORS
	// failure instead of the honest "we do not offer that here".
	writeCORS(c)
	c.Header("Allow", http.MethodPost)
	c.JSON(http.StatusMethodNotAllowed, newErrorResponse(nil, codeInvalidRequest,
		fmt.Sprintf("%s is not supported on %s. This is a Streamable HTTP MCP endpoint and "+
			"accepts POST only; server-sent events and session teardown are not offered in "+
			"Phase 1. The endpoint IS deployed -- this is a refusal, not a missing route.",
			c.Request.Method, TransportPath), nil))
}

// ---------------------------------------------------------------------------
// JSON-RPC envelope
// ---------------------------------------------------------------------------

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type jsonrpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

func newResponse(id json.RawMessage, result any) jsonrpcResponse {
	return jsonrpcResponse{JSONRPC: "2.0", ID: nullID(id), Result: result}
}

func newErrorResponse(id json.RawMessage, code int, msg string, data json.RawMessage) jsonrpcResponse {
	return jsonrpcResponse{
		JSONRPC: "2.0",
		ID:      nullID(id),
		Error:   &jsonrpcError{Code: code, Message: msg, Data: data},
	}
}

// nullID renders an absent id as JSON null, which is what JSON-RPC requires on
// an error that could not be attributed to a request.
func nullID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

// ---------------------------------------------------------------------------
// The request path
// ---------------------------------------------------------------------------

func (h *TransportHandler) serve(c *gin.Context) {
	// 0. CORS HEADERS BEFORE AUTHENTICATION, so that the 401 below is readable
	// by a browser-hosted client. A response the browser refuses to expose is
	// not a refusal anyone can act on: it surfaces as a network error, which
	// sends the caller looking for an outage rather than at their token.
	writeCORS(c)

	// 1. AUTHENTICATE FIRST, AND ANSWER 401 -- NEVER 404.
	//
	// A 404 hides a routing failure behind something that looks like a
	// deliberate refusal. Those are different facts and the caller must be
	// able to tell them apart: 404 here would make "the endpoint is not
	// deployed" and "your token is not valid" indistinguishable, and the first
	// is an outage while the second is expected.
	auth, err := h.svc.Authenticate(c.Request.Context(), bearerToken(c))
	if err != nil {
		h.writeUnauthorized(c, err)
		return
	}

	// 2. NEGOTIATE THE REVISION HEADER. Three cases, three answers.
	neg := NegotiateProtocol(c.GetHeader(ProtocolHeader))

	// Log the header on EVERY request from the first request. No surveyed
	// client documents this header, so the real distribution is unknowable
	// from documentation and this log is the only way the question is ever
	// answered. Absence is logged as an explicit false, not as a blank.
	h.log.InfoContext(c.Request.Context(), "mcp request",
		slog.String("tenant_id", auth.TenantID.String()),
		slog.String("grant_id", auth.GrantID.String()),
		slog.Bool("protocol_header_present", neg.Present()),
		slog.String("protocol_header", neg.Raw),
		slog.Int("scoped_sites", auth.Sites.Len()),
	)

	if neg.Refused() {
		h.writeProtocolRefusal(c, nil, neg)
		return
	}

	// 3. Parse the envelope, under a HARD BYTE CAP.
	//
	// Authentication has already run, so this is an authenticated-only path --
	// but that is not sufficient reason to read an unbounded body. A stolen or
	// leaked connection token would otherwise buffer arbitrary bytes into a
	// shared Cloud Run instance, and an OOM there is a CROSS-TENANT
	// AVAILABILITY EVENT: one tenant's compromised credential takes down every
	// tenant on the instance. Every other body-accepting public POST in this
	// tree caps its read (internal/rum, internal/site, internal/backup); this
	// one was the exception.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBytes)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		// MaxBytesReader's error is reported as 413 and not as a parse
		// failure: "your request is too large" and "your JSON is malformed"
		// are different facts and a client can act on only one of them.
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			c.JSON(http.StatusRequestEntityTooLarge, newErrorResponse(nil, codeInvalidRequest,
				fmt.Sprintf("the request body exceeds the %d-byte limit for this endpoint", maxRequestBytes), nil))
			return
		}
		c.JSON(http.StatusBadRequest, newErrorResponse(nil, codeParseError, "could not read the request body", nil))
		return
	}

	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		// BATCHING IS REFUSED ON EVERY REVISION IN THE WINDOW, AND ON THE FLOOR
		// THAT IS A DELIBERATE DIVERGENCE FROM THE SPECIFICATION.
		//
		// JSON-RPC batching was removed in revision 2025-06-18, whose changelog
		// opens with "Remove support for JSON-RPC batching" -- NOT in 2025-11-25,
		// as this comment claimed until it was checked against the changelog.
		//
		// So the window is not uniform. 2025-06-18 and 2025-11-25 forbid
		// batching and refusing it is conformant. 2025-03-26 -- our floor, and
		// the revision NegotiateProtocol assumes for the header-less client we
		// expect to be the common case -- PERMITS it, and we refuse it anyway.
		//
		// What a floor client actually experiences: it POSTs a JSON array, and
		// gets HTTP 400 carrying a JSON-RPC error with code -32600 and a null
		// id, whose message tells it to send one request per POST. It is not
		// downgraded, not partially served, and not silently given the first
		// element of its batch. The refusal is total and it says why.
		//
		// This is a product decision and not an unfinished edge: implementing
		// batching would mean implementing partial failure across a batch --
		// per-element authorization, per-element audit rows, and an answer for
		// "three succeeded and one was refused" -- to serve a shape that the
		// two newer revisions have already deleted and that no surveyed client
		// emits. Half-answering a batch would be worse than refusing it.
		//
		// DO NOT "FIX" THIS BY IMPLEMENTING BATCHING because the floor allows
		// it. If it is ever revisited, the question to answer first is whether
		// any real 2025-03-26 client sends batches, and the request log added
		// in serve() is what answers it.
		c.JSON(http.StatusBadRequest, newErrorResponse(nil, codeInvalidRequest,
			"JSON-RPC batching is not supported; send one request per POST", nil))
		return
	}

	var req jsonrpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		c.JSON(http.StatusBadRequest, newErrorResponse(nil, codeParseError, "the request body is not valid JSON", nil))
		return
	}
	if req.JSONRPC != "2.0" {
		c.JSON(http.StatusBadRequest, newErrorResponse(req.ID, codeInvalidRequest,
			`"jsonrpc" must be exactly "2.0"`, nil))
		return
	}
	if strings.TrimSpace(req.Method) == "" {
		c.JSON(http.StatusBadRequest, newErrorResponse(req.ID, codeInvalidRequest,
			`"method" is required`, nil))
		return
	}

	// A request with no id is a NOTIFICATION: it takes no response body. 202
	// with an empty body is what the Streamable HTTP transport specifies.
	//
	// THE CHECK IS BEFORE DISPATCH, AND THAT ORDERING IS THE POINT.
	//
	// It used to be after: dispatch ran, the tool EXECUTED, and then the 202
	// swallowed the result. An id-less tools/call was therefore a
	// fire-and-forget invocation channel -- the caller triggered the work and
	// received an empty body, so nothing it did was observable in its own
	// response.
	//
	// That is harmless only while every tool is a read. The moment a write tool
	// lands it is exactly the shape the proposal machinery exists to prevent: an
	// effect with no answer, no result to show a user, and no record the caller
	// ever sees. Fixing it after that tool ships would mean fixing it under
	// pressure, so it is fixed now, while the only thing it can suppress is a
	// list of sites.
	//
	// Returning early also costs nothing correct. Every method this server
	// implements that is legitimately sent as a notification
	// (notifications/initialized, ping) is a no-op whose only product IS the
	// response, and initialize is required by the spec to carry an id.
	if len(req.ID) == 0 {
		c.Status(http.StatusAccepted)
		return
	}

	resp, status, handled := h.dispatch(c, auth, neg, req)
	if !handled {
		return // dispatch already wrote the response
	}
	c.JSON(status, resp)
}

// dispatch routes one JSON-RPC method. The returned bool is false when the
// handler has already written the response itself.
func (h *TransportHandler) dispatch(
	c *gin.Context,
	auth AuthorizedRequest,
	neg Negotiation,
	req jsonrpcRequest,
) (jsonrpcResponse, int, bool) {
	ctx := c.Request.Context()

	switch req.Method {
	case "initialize":
		resp, ok := h.initialize(c, auth, neg, req)
		return resp, http.StatusOK, ok

	case "notifications/initialized", "ping":
		return newResponse(req.ID, map[string]any{}), http.StatusOK, true

	case "tools/list":
		// THE ACTIVITY STAMP IS BEFORE THE ANSWER, NOT AFTER IT. See
		// stampActivity.
		if resp, refused := h.stampActivity(ctx, auth, req); refused {
			return resp, http.StatusOK, true
		}
		// VisibleTools, NOT Tools. Tools() is the whole registry and is not a
		// request-path value; VisibleTools filters by this connection's
		// capability set and site scope through the SAME predicate tools/call
		// applies. An empty list here is a truthful answer for a connection
		// that reaches nothing, not an error.
		return newResponse(req.ID, map[string]any{"tools": VisibleTools(auth)}), http.StatusOK, true

	case "tools/call":
		if resp, refused := h.stampActivity(ctx, auth, req); refused {
			return resp, http.StatusOK, true
		}
		return h.callTool(ctx, auth, req), http.StatusOK, true

	// Resources and prompts are OUT OF SCOPE for Phase 1 and are refused by
	// name rather than answered with an empty list. An empty list would claim
	// "this server has no resources", which is a different and false statement
	// from "this server does not implement resources".
	case "resources/list", "resources/read", "resources/templates/list",
		"prompts/list", "prompts/get":
		return newErrorResponse(req.ID, codeMethodNotFound,
				fmt.Sprintf("%q is not implemented: this server exposes tools only", req.Method), nil),
			http.StatusOK, true

	default:
		return newErrorResponse(req.ID, codeMethodNotFound,
			fmt.Sprintf("unknown method %q", req.Method), nil), http.StatusOK, true
	}
}

// ---------------------------------------------------------------------------
// initialize
// ---------------------------------------------------------------------------

type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
	ClientInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"clientInfo"`
}

func (h *TransportHandler) initialize(
	c *gin.Context,
	auth AuthorizedRequest,
	headerNeg Negotiation,
	req jsonrpcRequest,
) (jsonrpcResponse, bool) {
	var p initializeParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return newErrorResponse(req.ID, codeInvalidParams,
				"initialize params could not be parsed", nil), true
		}
	}

	// The params carry the revision the client actually wants to speak; the
	// header governs subsequent requests. They are negotiated by the SAME
	// classifier so that neither path can drift into accepting something the
	// other refuses. An absent params revision assumes the floor for the same
	// reason an absent header does -- refusing here would reject a client that
	// the header rule was written to accept.
	paramNeg := NegotiateProtocol(p.ProtocolVersion)
	if paramNeg.Refused() {
		h.writeProtocolRefusal(c, req.ID, paramNeg)
		return jsonrpcResponse{}, false
	}

	// Record the connect: client name, client version, the protocol header
	// value OR ITS ABSENCE, and the time.
	//
	// The value persisted is the HEADER, not the negotiated revision. Absence
	// is stored as NULL and is a fact worth storing, not a blank: a stamped
	// client_identity_recorded_at with a NULL protocol_version is exactly the
	// "connected and sent no header" signal, and writing the negotiated floor
	// into that column instead would manufacture a header the client never
	// sent.
	var headerValue *string
	if headerNeg.Present() {
		v := headerNeg.Raw
		headerValue = &v
	}
	if err := h.svc.RecordConnect(c.Request.Context(), auth,
		p.ClientInfo.Name, p.ClientInfo.Version, headerValue); err != nil {
		// NOT an audit write -- RecordConnect updates mcp_grants (client name,
		// version, protocol header) via Store.RecordClientIdentity, and that
		// update is NOT swallowed on failure. The connect record is how an
		// operator sees what is attached to their organisation, and a session
		// that proceeded after failing to record itself would be an
		// unattributable connection. (The actual audit_events row for this
		// surface is ActionMCPToolCalled, written per tool call by
		// Service.RecordToolCall -- initialize itself is not a tool call and
		// writes no audit row.)
		h.log.ErrorContext(c.Request.Context(), "mcp connect record failed",
			slog.String("tenant_id", auth.TenantID.String()),
			slog.String("grant_id", auth.GrantID.String()),
			slog.String("error", err.Error()),
		)
		return newErrorResponse(req.ID, codeInternalError,
			"the connection could not be recorded; the session was not established", nil), true
	}

	return newResponse(req.ID, map[string]any{
		"protocolVersion": paramNeg.Version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo": map[string]any{
			"name":    "wpmgr",
			"version": h.version,
		},
	}), true
}

// ---------------------------------------------------------------------------
// tools/call
// ---------------------------------------------------------------------------

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// stampActivity records that this connection was used, and reports whether the
// request must be refused because the record could not be written. GH #605.
//
// IT RUNS BEFORE THE TOOL, AND THE ORDERING IS THE POINT. Stamping after a
// successful call would leave every refused or failed call unrecorded, so a
// connection whose calls are all being refused would look idle and would
// eventually idle-expire -- silently converting a permissions problem into an
// expired credential. What this column answers is "did anyone drive this
// connection", and driving it is what the request already is by the time this
// runs: Authenticate has passed, so the caller holds a live token on a live,
// unexpired grant.
//
// A FAILED STAMP REFUSES THE REQUEST rather than being logged and dropped. See
// Service.RecordActivity for why this one is not best-effort like the audit
// append in callTool. The refusal is the INTERNAL code, never an auth or a
// tool-availability code: nothing is wrong with the caller's token or its
// capability set, and telling it otherwise would send an operator rotating a
// credential that was never the problem.
func (h *TransportHandler) stampActivity(ctx context.Context, auth AuthorizedRequest, req jsonrpcRequest) (jsonrpcResponse, bool) {
	if err := h.svc.RecordActivity(ctx, auth); err != nil {
		h.log.ErrorContext(ctx, "mcp activity stamp failed",
			slog.String("tenant_id", auth.TenantID.String()),
			slog.String("grant_id", auth.GrantID.String()),
			slog.String("token_id", auth.TokenID.String()),
			slog.String("method", req.Method),
			slog.String("error", err.Error()),
		)
		return newErrorResponse(req.ID, codeInternalError,
			"this request could not be recorded against the connection, so it was not served", nil), true
	}
	return jsonrpcResponse{}, false
}

// callTool is S7's EXIT GATE, and the gate is the absence of a `default` arm.
//
// This function used to switch on the tool name with a `default` that answered
// "unknown tool" and listed every tool the server has. Two things were wrong
// with it, and only the second is a vulnerability:
//
//  1. It enumerated the whole surface to any caller holding any valid token.
//
//  2. MORE IMPORTANTLY, the filtering lived on tools/list alone. The switch
//     matched a registered name and dispatched -- with no reference to what
//     this connection may reach. So a model that never called tools/list, and
//     simply guessed a name, was dispatched on it. The permission check was on
//     the discovery path, and discovery is not something an attacker has to
//     use.
//
// There is now NO switch and therefore no arm to fall through. The name is
// resolved by AuthorizeTool, which applies the same visible() predicate
// tools/list applies, and the implementation hangs off the registry entry that
// resolution returned. A name that AuthorizeTool did not authorize has no
// invoke function to reach.
func (h *TransportHandler) callTool(ctx context.Context, auth AuthorizedRequest, req jsonrpcRequest) jsonrpcResponse {
	var p toolCallParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return newErrorResponse(req.ID, codeInvalidParams, "tools/call params could not be parsed", nil)
	}

	entry, reason, err := AuthorizeTool(p.Name, auth)
	if err != nil {
		// THE OPERATOR LOG IS WRITTEN FOR EVERY REFUSAL WITH A REASON, not
		// only for the ones the wire blurs.
		//
		// It used to be written only inside the not-available branch, which
		// left the scope-empty refusal with NO operator line anywhere: it
		// returned early to toolError, and toolError logs nothing for a domain
		// error. That made a comment two lines up ("the operator log
		// distinguishes ... site scope empty") false, and the false half was
		// load-bearing -- "the operator can still tell them apart" is part of
		// what justifies blurring the other two on the wire. An operator
		// debugging a scope problem got nothing on either side.
		//
		// So: log first, on the reason, and let the wire answer be decided
		// separately below. The three reasons are distinguished HERE; only two
		// of them are blurred on the wire.
		if reason != "" {
			h.log.WarnContext(ctx, "mcp tool refused",
				slog.String("tenant_id", auth.TenantID.String()),
				slog.String("grant_id", auth.GrantID.String()),
				slog.String("requested_tool", p.Name),
				slog.String("refusal_reason", string(reason)),
				slog.Int("held_capabilities", auth.Capabilities.Len()),
				slog.Int("scoped_sites", auth.Sites.Len()),
			)
		}

		if de, ok := domain.AsDomain(err); ok && de.Code == ErrCodeToolNotAvailable {
			// available_tools is this connection's OWN tools/list answer, so it
			// discloses nothing it was not already shown, and it lets a model
			// that mistyped a name it legitimately holds correct in one round
			// trip.
			data, _ := json.Marshal(map[string]any{
				"argument":        "name",
				"supplied":        p.Name,
				"available_tools": visibleToolNames(auth),
			})
			return newErrorResponse(req.ID, codeToolNotAvailable, de.Message, data)
		}
		// Everything else keeps its own named answer: mcp_scope_empty becomes
		// -32002 through toolError, and a non-domain error (a registry entry
		// with no implementation) becomes the internal code. Neither is
		// reported as a permission refusal.
		return h.toolError(req.ID, err)
	}

	h.log.InfoContext(ctx, "mcp tool call",
		slog.String("tenant_id", auth.TenantID.String()),
		slog.String("grant_id", auth.GrantID.String()),
		slog.String("tool", entry.Name),
		slog.String("capability", string(entry.Capability)),
		slog.String("operator_permission", string(entry.OperatorPermission)),
	)

	text, err := entry.invoke(ctx, h.svc, auth, p.Arguments)
	if err != nil {
		return h.toolError(req.ID, err)
	}

	// The operator-facing audit row (ActionMCPToolCalled), for this call and
	// no other outcome: a refusal above never reached a tenant's data and is
	// already covered by the WarnContext log at the refusal site. BEST-EFFORT
	// like RecordConnect's client-identity write is not -- a failure here is
	// logged, never returned to the caller, because withholding a successful
	// read's answer over a failure to record having answered it would make an
	// observational feature take down the read path it observes.
	if err := h.svc.RecordToolCall(ctx, auth, entry.Name, string(entry.OperatorPermission)); err != nil {
		h.log.ErrorContext(ctx, "mcp tool call audit write failed",
			slog.String("tenant_id", auth.TenantID.String()),
			slog.String("grant_id", auth.GrantID.String()),
			slog.String("tool", entry.Name),
			slog.String("error", err.Error()),
		)
	}

	return newResponse(req.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
	})
}

// toolError maps a domain refusal onto a named JSON-RPC code, and an infra
// failure onto the internal code WITHOUT leaking its message.
func (h *TransportHandler) toolError(id json.RawMessage, err error) jsonrpcResponse {
	if de, ok := domain.AsDomain(err); ok {
		if de.Code == ErrCodeScopeEmpty {
			data, _ := json.Marshal(map[string]any{"code": de.Code})
			return newErrorResponse(id, codeScopeEmpty, de.Message, data)
		}
		return newErrorResponse(id, codeInvalidParams, de.Message, nil)
	}
	h.log.Error("mcp tool call failed", slog.String("error", err.Error()))
	return newErrorResponse(id, codeInternalError, "the tool call failed", nil)
}

// ---------------------------------------------------------------------------
// Refusals
// ---------------------------------------------------------------------------

// writeUnauthorized answers 401 -- NEVER 404 -- and names the scheme so a
// client knows how to authenticate.
func (h *TransportHandler) writeUnauthorized(c *gin.Context, err error) {
	c.Header("WWW-Authenticate", `Bearer realm="wpmgr-mcp"`)

	msg := "a valid bearer token is required"
	code := ErrCodeInvalidGrant
	if de, ok := domain.AsDomain(err); ok && de.Kind == domain.KindUnauthorized {
		msg = de.Message
		code = de.Code
	} else if !errors.As(err, new(*domain.Error)) {
		// An infrastructure failure is NOT reported as an auth refusal: that
		// would tell the caller their token is bad when the database is down,
		// and they would rotate a credential that was never the problem.
		h.log.Error("mcp authenticate failed", slog.String("error", err.Error()))
		c.Header("WWW-Authenticate", "")
		c.JSON(http.StatusInternalServerError,
			newErrorResponse(nil, codeInternalError, "the request could not be authenticated", nil))
		return
	}

	data, _ := json.Marshal(map[string]any{"code": code})
	c.JSON(http.StatusUnauthorized, newErrorResponse(nil, codeInvalidRequest, msg, data))
}

// writeProtocolRefusal answers 400 for BOTH refusal cases, with a message that
// names the floor and the target so the user can act on it.
//
// Note what this does NOT do: it never downgrades, and it never serves a
// quietly reduced surface. Both are the silent-coercion failure this codebase
// is governed by -- an unsupported revision answered with a working response is
// a claim that the client's revision is spoken here.
func (h *TransportHandler) writeProtocolRefusal(c *gin.Context, id json.RawMessage, neg Negotiation) {
	var msg string
	switch neg.Outcome {
	case NegotiationBelowFloor:
		msg = fmt.Sprintf(
			"MCP protocol revision %s is below this server's compatibility floor. "+
				"The floor is %s and the target is %s. Revisions below the floor drop fields "+
				"the approval flow requires, so they are refused rather than downgraded to.",
			neg.Raw, ProtocolFloor, ProtocolTarget)
	default:
		msg = fmt.Sprintf(
			"MCP protocol revision %q is not supported. The floor is %s and the target is %s; "+
				"this server speaks %s.",
			neg.Raw, ProtocolFloor, ProtocolTarget, strings.Join(SupportedRevisions(), ", "))
	}

	data, _ := json.Marshal(map[string]any{
		"floor":     ProtocolFloor,
		"target":    ProtocolTarget,
		"supported": SupportedRevisions(),
		"requested": neg.Raw,
	})
	c.JSON(http.StatusBadRequest, newErrorResponse(id, codeProtocolUnsupported, msg, data))
}

// bearerToken extracts the credential from the Authorization header. It
// returns "" for every malformed shape, and "" is refused by Authenticate.
func bearerToken(c *gin.Context) string {
	h := c.GetHeader("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}
