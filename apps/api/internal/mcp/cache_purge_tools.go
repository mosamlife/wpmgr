package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
)

// ---------------------------------------------------------------------------
// THE TWO CACHE-REQUEST TOOLS.
//
// site_cache_purge_request ASKS an operator to clear the page cache on one
// site. It changes nothing by itself: it writes a pending request, a person
// who may clear caches on that site approves or declines it in WPMgr, and only
// an approved request is ever sent to the site. No automation can approve a
// request (ADR-061).
//
// site_cache_purge_request_status reads what became of a request this
// connection made. It is a read.
//
// Both require CapCachePurge, which only ScopeCache confers. Both are absent
// from tools/list, and answer a call exactly as an unknown name does, while
// write tools are switched off on this server (Service.SetWriteToolsEnabled).
// ---------------------------------------------------------------------------

const (
	ToolSiteCachePurgeRequest       = "site_cache_purge_request"
	ToolSiteCachePurgeRequestStatus = "site_cache_purge_request_status"
)

// ErrCodeSiteAddressUnusable refuses a cache-clear request, for either scope,
// when WPMgr cannot use the site's own stored address to build the request.
// Nothing was asked and nothing changed; only an operator can fix it.
const ErrCodeSiteAddressUnusable = "mcp_site_address_unusable"

// codeSiteAddressUnusable is its JSON-RPC code. -32009, -32010 and -32013 are
// deliberately left unused.
const codeSiteAddressUnusable = -32014

// siteAddressUnusableMessage is the ONE wire message for that refusal. It is a
// constant and interpolates nothing: it never carries site text.
const siteAddressUnusableMessage = "WPMgr cannot use this site's stored address, so nothing " +
	"was asked. An operator must correct the site's address. Do not retry; tell your user."

// reasonSiteAddressUnusable is the operator-facing refusal reason recorded on
// mcp.tool.denied for that refusal.
const reasonSiteAddressUnusable refusalReason = "site_address_unusable"

// ToolAnnotations are the MCP tool annotations a descriptor may carry.
// Pointers so an unset hint is omitted rather than sent as false.
type ToolAnnotations struct {
	ReadOnlyHint    *bool `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool `json:"openWorldHint,omitempty"`
}

func boolPtr(b bool) *bool { return &b }

// cachePurgeRequestSchema: the `url` pattern admits printable ASCII only; the
// full grammar is enforced by the creation rail, and the description states
// it so a model can comply in one round trip.
var cachePurgeRequestSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "site_id": {"type": "string", "format": "uuid"},
    "scope": {"type": "string", "enum": ["all", "url"]},
    "url": {"type": "string", "maxLength": 2048, "pattern": "^https?://[!-~]+$"}
  },
  "required": ["site_id", "scope"],
  "additionalProperties": false
}`)

var cachePurgeRequestStatusSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "request_id": {"type": "string", "format": "uuid"}
  },
  "additionalProperties": false
}`)

// cachePurgeRequestDescription renders the request tool's description. The
// hosting-cache sentence comes from the reach table and nowhere else.
func cachePurgeRequestDescription() string {
	return "Asks an operator to clear the page cache on ONE site in this connection's scope. " +
		"It changes nothing by itself: the request waits for a person to approve it in WPMgr " +
		"and closes unanswered after 24 hours. If approved, scope `all` deletes WPMgr's own " +
		"cache directory for the site, and scope `url` deletes WPMgr's cached files for one " +
		"page address. The address must be printable ASCII (write an internationalised host " +
		"in its Punycode form and percent-encode anything else in the path), on the site's " +
		"own host and under the site's own path, with no `?` or `#` anywhere, and no " +
		"userinfo, backslash, encoded slash or `.`/`..` segments. A port, if you write one, " +
		"must be the site's; if two sites in this connection's scope share a host on " +
		"different ports, write the port. WPMgr keeps only the path and uses the site's own " +
		"scheme, host and port. " +
		hostingCacheSentence(hostingCacheReachTable()) +
		" A `url` clear also purges that exact address from a CDN configured in WPMgr. Pages " +
		"load slower until the cache refills. Call `" + ToolSiteCachePurgeRequestStatus +
		"` with the returned `request_id` to learn the outcome. While this connection has a " +
		"request waiting for a site, asking again with the same scope and url returns that " +
		"request; a different scope or url is refused until it is decided. Get site ids from " +
		"`" + ToolFleetSitesList + "`. It does not preload or change settings."
}

func cachePurgeRequestStatusDescription() string {
	return "Reports what became of a cache-clear request this connection made with `" +
		ToolSiteCachePurgeRequest + "`. Pass `request_id` for one request, or no arguments " +
		"to list this connection's open requests (at most 20, newest first). It changes " +
		"nothing. A request is decided by a person in WPMgr: its state is one of " +
		"waiting_for_approval, approved_not_started, running, done, declined, withdrawn or " +
		"expired. Call again no sooner than `poll_after_seconds`."
}

// cachePurgeToolPolicies are the two registry entries. Called from the
// registryTools literal; a fresh value on every call.
func cachePurgeToolPolicies() []ToolPolicy {
	return []ToolPolicy{{
		Name:                 ToolSiteCachePurgeRequest,
		Description:          cachePurgeRequestDescription(),
		InputSchema:          cachePurgeRequestSchema,
		Capability:           CapCachePurge,
		OperatorPermission:   authz.PermSiteCachePurge,
		RequiresSiteScope:    true,
		Effect:               EffectRequest,
		FencesSiteOriginText: true,
		Annotations: &ToolAnnotations{
			ReadOnlyHint:    boolPtr(false),
			DestructiveHint: boolPtr(false),
			IdempotentHint:  boolPtr(true),
			OpenWorldHint:   boolPtr(false),
		},
		invoke: func(ctx context.Context, svc *Service, auth AuthorizedRequest, args json.RawMessage) (string, error) {
			return svc.requestSiteCachePurge(ctx, auth, args)
		},
	}, {
		Name:                 ToolSiteCachePurgeRequestStatus,
		Description:          cachePurgeRequestStatusDescription(),
		InputSchema:          cachePurgeRequestStatusSchema,
		Capability:           CapCachePurge,
		OperatorPermission:   authz.PermSiteCachePurge,
		RequiresSiteScope:    true,
		Effect:               EffectRead,
		FencesSiteOriginText: true,
		Annotations: &ToolAnnotations{
			ReadOnlyHint: boolPtr(true),
		},
		invoke: func(ctx context.Context, svc *Service, auth AuthorizedRequest, args json.RawMessage) (string, error) {
			return svc.siteCachePurgeRequestStatus(ctx, auth, args)
		},
	}}
}

// ErrWriteToolsUnavailable is SetWriteToolsEnabled's refusal to switch the
// tools on for a Service that cannot run them.
var ErrWriteToolsUnavailable = errors.New("mcp: write tools cannot be switched on")

// SetWriteToolsEnabled is the server-wide switch for tools that are not
// reads. It is read once at startup (WPMGR_MCP_WRITE_TOOLS, and whether the
// services those tools need were built) and handed here. The zero value is
// OFF, so a Service nobody switched on never lists or runs them.
//
// THE SWITCH CANNOT TURN ON A TOOL WHOSE RAIL IS ABSENT. Asked for on, it
// refuses, stays off and returns ErrWriteToolsUnavailable unless the rail is
// installed: the store implements the request rail, an audit recorder is
// wired (the rail records in its own transaction), a governed-context
// resolver is wired (operator rules must be enforceable) and the request
// limiter exists. main.go fails boot on that error rather than serving tools
// that would answer every call with an internal failure. liveRegistry checks
// the same thing again on every call, so a copy made afterwards without one
// of them cannot serve the tools either.
func (s *Service) SetWriteToolsEnabled(on bool) error {
	if !on {
		s.writeToolsEnabled = false
		return nil
	}
	if _, err := s.requestRail(); err != nil {
		s.writeToolsEnabled = false
		return fmt.Errorf("%w: %v", ErrWriteToolsUnavailable, err)
	}
	s.writeToolsEnabled = true
	return nil
}

// WriteToolsEnabled reports whether this Service serves the request tools:
// the switch is on AND the rail is installed. main.go hands this value, not
// the environment's, to the approval side, so the two halves cannot disagree.
func (s *Service) WriteToolsEnabled() bool {
	if !s.writeToolsEnabled {
		return false
	}
	_, err := s.requestRail()
	return err == nil
}

// liveRegistry is the surface this server actually serves: the registry,
// minus every tool whose capability is a request while write tools are
// switched off or their rail is absent. The switch is applied HERE, in the
// transport's view, and not inside AuthorizeTool, so a check that asks "is
// this tool permitted for this grant" gets the same answer whichever way the
// switch is set.
func (s *Service) liveRegistry() []ToolPolicy {
	entries := registryTools()
	if s.WriteToolsEnabled() {
		return entries
	}
	out := make([]ToolPolicy, 0, len(entries))
	for _, e := range entries {
		if eff, ok := CapabilityEffect(e.Capability); ok && eff == EffectRead {
			out = append(out, e)
		}
	}
	return out
}
