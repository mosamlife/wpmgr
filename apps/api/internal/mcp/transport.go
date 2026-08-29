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

	// 3. Parse the envelope.
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, newErrorResponse(nil, codeParseError, "could not read the request body", nil))
		return
	}

	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		// Batching was removed in revision 2025-11-25 and is not implemented
		// for the older revisions in the window either. Refusing by name is
		// better than half-answering a batch.
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
	isNotification := len(req.ID) == 0

	resp, status, handled := h.dispatch(c, auth, neg, req)
	if !handled {
		return // dispatch already wrote the response
	}
	if isNotification {
		c.Status(http.StatusAccepted)
		return
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
		return newResponse(req.ID, map[string]any{"tools": Tools()}), http.StatusOK, true

	case "tools/call":
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
		// A failed audit write is NOT swallowed. The connect record is how an
		// operator sees what is attached to their organisation, and a session
		// that proceeded after failing to record itself would be an
		// unattributable connection.
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

func (h *TransportHandler) callTool(ctx context.Context, auth AuthorizedRequest, req jsonrpcRequest) jsonrpcResponse {
	var p toolCallParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return newErrorResponse(req.ID, codeInvalidParams, "tools/call params could not be parsed", nil)
	}

	switch p.Name {
	case ToolListSites:
		text, err := h.svc.ListSitesForModel(ctx, auth)
		if err != nil {
			return h.toolError(req.ID, err)
		}
		return newResponse(req.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
		})
	default:
		// A tool the grant does not cover is ABSENT from tools/list, so there
		// is nothing to refuse; an unknown name here is a client error.
		data, _ := json.Marshal(map[string]any{
			"argument":    "name",
			"supplied":    p.Name,
			"known_tools": toolNames(),
		})
		return newErrorResponse(req.ID, codeInvalidToolArguments,
			fmt.Sprintf("unknown tool %q", p.Name), data)
	}
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

func toolNames() []string {
	ts := Tools()
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
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
