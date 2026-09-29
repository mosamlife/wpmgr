package mcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/humantext"
	"github.com/mosamlife/wpmgr/apps/api/internal/siteaddr"
)

// ---------------------------------------------------------------------------
// THE CREATION RAIL for site_cache_purge_request.
//
// It ASKS. Nothing here changes a site: the rail writes one waiting request,
// in one connection-scoped transaction, and a person decides it in WPMgr. No
// path below approves anything, and nothing an AI client sends can.
//
// The steps run in a fixed order, and every refusal has a defined answer:
//
//  1. strict decode, and for scope `url` the raw grammar, then siteaddr.Parse,
//     then the dot-segment and decoded-rune checks;
//  2. an empty scope is -32002;
//  3. a site outside auth.Sites is "absent", with no database access;
//  4. the site row is read under SingleSitePrincipal, and its OWN address is
//     reduced to the ASCII host WPMgr dials, for BOTH scopes; a site whose
//     address cannot be used is -32014, never -32603;
//  5. for `url`: the host, the port and the path must be this site's, no other
//     in-scope site may claim the page, and the stored value is rebuilt from
//     the site's own scheme, host and port with only the model's path kept;
//  6. an operator AI rule forbidding the tool is -32012;
//  7. the agent must be connected and at or above the origin-only floor;
//  8. the per-process rate limits;
//  9. the creation transaction: the grant lock, the in-line expiry of this
//     connection's own lapsed row, the caps, the insert (with the dedupe
//     read), and the audit rows, last.
// ---------------------------------------------------------------------------

// JSON-RPC codes the request tools add. -32009, -32010 and -32013 are
// deliberately left unused.
const (
	codeSiteAbsent             = -32007
	codeSiteUnreachable        = -32008
	codeSiteAgentOutdated      = -32011
	codeToolForbiddenByContext = -32012
)

// Domain codes the request tools answer with. Each maps to exactly one wire
// code in requestToolWireCodes; toolError copies only these.
const (
	ErrCodeInvalidToolArguments   = "mcp_invalid_tool_arguments"
	ErrCodeSiteAbsent             = "mcp_site_absent"
	ErrCodeSiteUnreachable        = "mcp_site_unreachable"
	ErrCodeSiteAgentOutdated      = "mcp_site_agent_outdated"
	ErrCodeToolForbiddenByContext = "mcp_tool_forbidden_by_context"
	ErrCodeRequestLimited         = "mcp_request_limited"
)

// requestToolWireCodes is the closed map from the request tools' domain codes
// to their JSON-RPC codes. toolError answers these with the error's own
// message, which on this path is always one of the constants below.
var requestToolWireCodes = map[string]int{
	ErrCodeInvalidToolArguments:   codeInvalidToolArguments,
	ErrCodeSiteAbsent:             codeSiteAbsent,
	ErrCodeSiteUnreachable:        codeSiteUnreachable,
	ErrCodeSiteAgentOutdated:      codeSiteAgentOutdated,
	ErrCodeToolForbiddenByContext: codeToolForbiddenByContext,
	ErrCodeRequestLimited:         codeRateLimited,
}

// The refusal reasons the request tools record on mcp.tool.denied. They are
// operator-facing and never sent to the model.
const (
	reasonScopeEmpty             refusalReason = "scope_empty"
	reasonSiteAbsent             refusalReason = "site_absent"
	reasonInvalidArguments       refusalReason = "invalid_arguments"
	reasonURLNotOnSite           refusalReason = "url_not_on_site"
	reasonURLOtherSite           refusalReason = "url_other_site"
	reasonURLPortAmbiguous       refusalReason = "url_port_ambiguous"
	reasonURLSameAddress         refusalReason = "url_same_address"
	reasonSiteUnreachable        refusalReason = "site_unreachable"
	reasonAgentOutdated          refusalReason = "agent_outdated"
	reasonForbiddenByContext     refusalReason = "forbidden_by_context"
	reasonPendingRequestForSite  refusalReason = "pending_request_for_site"
	reasonPendingCap             refusalReason = "pending_cap"
	reasonGrantDailyCap          refusalReason = "grant_daily_cap"
	reasonSiteHourlyCap          refusalReason = "site_hourly_cap"
	reasonRequestAbsent          refusalReason = "request_absent"
	reasonRequestRateLimited     refusalReason = "request_rate_limited"
	reasonRequestRailUnavailable refusalReason = "request_rail_unavailable"
)

// The wire messages. Every one is a constant: NONE of them interpolates site
// text, a host, a port number or anything the model sent.
const (
	msgArgSiteID = "site_id must be a uuid of a site in this connection's scope; get site ids " +
		"from fleet_sites_list"
	msgArgScope    = "scope must be exactly \"all\" or \"url\""
	msgArgURLNeed  = "url is required when scope is \"url\""
	msgArgURLAll   = "url must be omitted when scope is \"all\""
	msgArgUnknown  = "unknown argument; the only arguments are site_id, scope and url"
	msgArgNotJSON  = "arguments must be a JSON object"
	msgArgNotStr   = "this argument must be a string"
	msgArgURLShape = "url must be printable ASCII (write an internationalised host in Punycode and " +
		"percent-encode anything else), an http or https address with a host and no userinfo, " +
		"with no ?, #, backslash, encoded slash or backslash, and no . or .. path segment"
	msgArgURLNotOnSite = "the url must be on this site's own host and under its own path; see " +
		"site_url in fleet_sites_list"
	msgArgURLTooLong  = "url too long: the address rebuilt on this site's own scheme, host and port " +
		"would exceed 2048 bytes"
	msgTieOtherSite   = "this url belongs to a different site in this connection's scope; use that site's id"
	msgTieWritePort   = "more than one site in this connection's scope uses this host on different " +
		"ports; write this site's port in the url (see site_url in fleet_sites_list; when it shows " +
		"none, the port is 443 for https and 80 for http)"
	msgTieSameAddress = "more than one site in this connection's scope has this same address, so a " +
		"page clear cannot name one of them; tell your user"
	msgArgStatusID = "request_id must be a uuid returned by site_cache_purge_request, or omit it " +
		"to list this connection's open requests"
	msgArgStatusUnknown = "unknown argument; the only argument is request_id"

	msgAbsent = "Nothing with that id is in this connection's scope. Nothing was asked and " +
		"nothing changed."
	msgSiteUnreachable = "This site's WPMgr agent is not connected. Nothing was asked and nothing " +
		"changed."
	msgForbiddenByContext = "An operator rule forbids this tool on this site. Do not retry; tell " +
		"your user."
	msgPendingForSite = "This connection already has a different request waiting for this site. It " +
		"must be decided before another can be asked; call site_cache_purge_request_status with " +
		"its request_id."
	msgLimited = "This connection has reached a limit on cache-clear requests. Nothing was asked. " +
		"Wait retry_after_seconds before asking again."
	msgCreated = "Nothing has changed yet. An operator must approve this in WPMgr. Call " +
		"site_cache_purge_request_status with this request_id to learn the outcome."
)

// Limits and timings of the creation rail (design 4.13).
const (
	requestWindow            = 24 * time.Hour
	maxPendingPerConnection  = 10
	maxCreatedPerConnection  = 30
	createdWindowSeconds     = 24 * 60 * 60
	maxAIClearsPerSiteHour   = 12
	siteHourWindowSeconds    = 60 * 60
	requestPollAfterSeconds  = 30
	maxRawURLBytes           = 2048
	maxStoredURLBytes        = 2048
	siteLabelRunes           = 200
	grantLabelRunes          = 64
	grantRequestLockKey      = "assistant_request_grant"
	requestReviewPath        = "/ai/requests"
	suppliedEchoBytes        = 512
	pendingCapRetrySeconds   = 600
	grantDailyRetrySeconds   = 3600
	siteHourlyRetrySeconds   = 600
	stateWaitingForApproval  = "waiting_for_approval"
	scopeAll                 = "all"
	scopeURL                 = "url"
	grantViaToken            = "token"
	grantViaBrowserSignIn    = "browser_sign_in"
	unnamedConnectionLabel   = "Unnamed connection"
	limitScopePending        = "pending_requests"
	limitScopeGrantDaily     = "grant_daily"
	limitScopeSiteHourly     = "site_hourly"
	limitScopePendingForSite = "pending_request_for_site"
)

// CacheRequestCardCopyVersion names the wording of the approval card a
// request's digest covers. assistantrequest records the same constant on
// assistant.request.approved.
const CacheRequestCardCopyVersion = "2026-09-29"

// siteHostPattern is the Go mirror of the site_host_ascii CHECK.
var siteHostPattern = regexp.MustCompile(`^[!-~]{1,255}$`)

// storedURLPattern is the Go mirror of the url_backstop CHECK's pattern; the
// CHECK also refuses '?' and '#', checked beside it.
var storedURLPattern = regexp.MustCompile(`^https?://[!-~]+$`)

// toolRefusal is a refusal on the request tools' path: the domain error the
// wire answer is built from, and the operator reason recorded on
// mcp.tool.denied. The transport records the row in its own transaction
// (refuseOnWrite), so a refusal never shares a transaction with a request row.
type toolRefusal struct {
	reason refusalReason
	err    *domain.Error
	// meta is extra operator-facing metadata for the denial row.
	meta map[string]any
	// logOnly refusals write no audit row: the per-process rate limit.
	logOnly bool
}

func (r *toolRefusal) Error() string { return r.err.Error() }
func (r *toolRefusal) Unwrap() error { return r.err }

func refuse(reason refusalReason, err *domain.Error) *toolRefusal {
	return &toolRefusal{reason: reason, err: err}
}

// argRefusal is a -32003 refusal. supplied is echoed back fenced and capped:
// it is model-sent text and is never trusted as ours.
func argRefusal(reason refusalReason, argument, supplied, msg string, schema json.RawMessage) *toolRefusal {
	details := map[string]any{"argument": argument, "retryable": false}
	if supplied != "" {
		details["supplied"] = fenceSiteText(humantext.CapBytes(supplied, suppliedEchoBytes))
	}
	if len(schema) > 0 {
		details["schema"] = schema
	}
	return &toolRefusal{
		reason: reason,
		err:    domain.Validation(ErrCodeInvalidToolArguments, msg).WithDetails(details),
		meta:   map[string]any{"argument": argument},
	}
}

func absentRefusal(reason refusalReason, meta map[string]any) *toolRefusal {
	return &toolRefusal{
		reason: reason,
		err:    domain.NotFound(ErrCodeSiteAbsent, msgAbsent).WithDetails(map[string]any{"retryable": false}),
		meta:   meta,
	}
}

func scopeEmptyRefusal() *toolRefusal {
	return refuse(reasonScopeEmpty, domain.Forbidden(ErrCodeScopeEmpty,
		"this connection's site scope resolves to no sites, so there is nothing it may ask about. "+
			"This is a refusal, not an empty fleet: check the grant's site scope."))
}

func limitRefusal(reason refusalReason, scope string, retryAfter int) *toolRefusal {
	return refuse(reason, domain.RateLimited(ErrCodeRequestLimited, msgLimited).WithDetails(map[string]any{
		"limit_scope":         scope,
		"retry_after_seconds": retryAfter,
		"retryable":           true,
	}))
}

func siteAddressUnusableRefusal() *toolRefusal {
	return refuse(reasonSiteAddressUnusable,
		domain.Unavailable(ErrCodeSiteAddressUnusable, siteAddressUnusableMessage))
}

// ---------------------------------------------------------------------------
// Step 1: arguments
// ---------------------------------------------------------------------------

// cachePurgeArgs are the decoded, validated arguments.
type cachePurgeArgs struct {
	SiteID uuid.UUID
	Scope  string
	// page is set for scope `url` only.
	page *pageURL
}

// pageURL is a model-supplied page address that passed the grammar.
type pageURL struct {
	raw string
	u   *url.URL
	// escapedPath is kept verbatim in the stored value; it is never
	// re-decoded.
	escapedPath string
}

// decodeStrictObject decodes raw as a JSON object whose keys are all in
// allowed, spelled exactly. encoding/json matches struct fields without
// regard to case, so a struct would accept "SITE_ID"; a map does not.
func decodeStrictObject(raw json.RawMessage, allowed map[string]bool, unknownMsg string, schema json.RawMessage) (map[string]json.RawMessage, *toolRefusal) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return map[string]json.RawMessage{}, nil
	}
	var obj map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return nil, argRefusal(reasonInvalidArguments, "arguments", "", msgArgNotJSON, schema)
	}
	if dec.More() {
		return nil, argRefusal(reasonInvalidArguments, "arguments", "", msgArgNotJSON, schema)
	}
	for k := range obj {
		if !allowed[k] {
			return nil, argRefusal(reasonInvalidArguments, k, "", unknownMsg, schema)
		}
	}
	return obj, nil
}

// stringArg reads one string argument. present is false for an absent key or
// a JSON null.
func stringArg(obj map[string]json.RawMessage, key string, schema json.RawMessage) (string, bool, *toolRefusal) {
	v, ok := obj[key]
	if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
		return "", false, nil
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return "", false, argRefusal(reasonInvalidArguments, key, string(v), msgArgNotStr, schema)
	}
	return s, true, nil
}

// parseStrictUUID accepts the canonical 36-character form only.
func parseStrictUUID(s string) (uuid.UUID, bool) {
	if len(s) != 36 {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(s)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, false
	}
	return id, true
}

// decodeCachePurgeArgs is rail step 1.
func decodeCachePurgeArgs(raw json.RawMessage) (cachePurgeArgs, *toolRefusal) {
	schema := cachePurgeRequestSchema
	obj, ref := decodeStrictObject(raw, map[string]bool{"site_id": true, "scope": true, "url": true}, msgArgUnknown, schema)
	if ref != nil {
		return cachePurgeArgs{}, ref
	}
	sid, ok, ref := stringArg(obj, "site_id", schema)
	if ref != nil {
		return cachePurgeArgs{}, ref
	}
	id, valid := parseStrictUUID(sid)
	if !ok || !valid {
		return cachePurgeArgs{}, argRefusal(reasonInvalidArguments, "site_id", sid, msgArgSiteID, schema)
	}
	scope, ok, ref := stringArg(obj, "scope", schema)
	if ref != nil {
		return cachePurgeArgs{}, ref
	}
	if !ok || (scope != scopeAll && scope != scopeURL) {
		return cachePurgeArgs{}, argRefusal(reasonInvalidArguments, "scope", scope, msgArgScope, schema)
	}
	rawURL, hasURL, ref := stringArg(obj, "url", schema)
	if ref != nil {
		return cachePurgeArgs{}, ref
	}
	out := cachePurgeArgs{SiteID: id, Scope: scope}
	switch {
	case scope == scopeAll && hasURL:
		return cachePurgeArgs{}, argRefusal(reasonInvalidArguments, "url", rawURL, msgArgURLAll, schema)
	case scope == scopeURL && !hasURL:
		return cachePurgeArgs{}, argRefusal(reasonInvalidArguments, "url", "", msgArgURLNeed, schema)
	case scope == scopeURL:
		page, ok := parsePageURL(rawURL)
		if !ok {
			return cachePurgeArgs{}, argRefusal(reasonInvalidArguments, "url", rawURL, msgArgURLShape, schema)
		}
		out.page = page
	}
	return out, nil
}

// parsePageURL is rail steps 1.1 to 1.4 for a model-supplied page address.
func parsePageURL(raw string) (*pageURL, bool) {
	// 1.1 Raw grammar, before any parser. Every byte printable ASCII; no
	// "all" in any case; no backslash, '?' or '#' anywhere (the bytes the
	// url_backstop CHECK refuses in the stored value); no encoded slash or
	// backslash in any case, so a decoded separator can only come from a
	// literal one.
	if raw == "" || len(raw) > maxRawURLBytes {
		return nil, false
	}
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; c < 0x21 || c > 0x7e {
			return nil, false
		}
	}
	if strings.EqualFold(raw, "all") || strings.ContainsAny(raw, `\?#`) {
		return nil, false
	}
	lower := strings.ToLower(raw)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
		return nil, false
	}
	// 1.2 siteaddr.Parse: http(s) only, a host, no userinfo, query or
	// fragment.
	_, u, ok := siteaddr.Parse(raw)
	if !ok || u == nil {
		return nil, false
	}
	// 1.3 Dot segments and runes, on the DECODED path.
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return nil, false
		}
	}
	for _, r := range u.Path {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) || unicode.IsSpace(r) {
			return nil, false
		}
	}
	// 1.4 The kept value is the escaped path.
	return &pageURL{raw: raw, u: u, escapedPath: u.EscapedPath()}, true
}

// ---------------------------------------------------------------------------
// Steps 4 and 5: the site's own address, and the page on it
// ---------------------------------------------------------------------------

// siteAddress is a site row's stored address reduced to what the rail uses.
type siteAddress struct {
	addr siteaddr.Address
	// host is the HostKey of the stored host: the ASCII form WPMgr dials,
	// checked against the site_host pattern.
	host string
	// port is the effective port: the stored one, else 443 or 80.
	port string
}

// siteAddressOf is rail step 4's derivation, for BOTH scopes. ok is false
// when the stored address fails Parse, its host has no HostKey, or the key
// fails the site_host pattern: the -32014 cases.
func siteAddressOf(stored string) (siteAddress, bool) {
	a, su, ok := siteaddr.Parse(stored)
	if !ok || su == nil {
		return siteAddress{}, false
	}
	hk, ok := siteaddr.HostKey(su.Hostname())
	if !ok || !siteHostPattern.MatchString(hk) {
		return siteAddress{}, false
	}
	return siteAddress{addr: a, host: hk, port: effectivePort(a)}, true
}

func effectivePort(a siteaddr.Address) string {
	if a.Port != "" {
		return a.Port
	}
	if a.Scheme == "https" {
		return "443"
	}
	return "80"
}

// pathCovers reports whether a site at normalised path sp covers decoded
// path p: equal, or under sp followed by a slash.
func pathCovers(sp, p string) bool {
	return p == sp || strings.HasPrefix(p, sp+"/")
}

// tieKind is which of the three tie messages applies.
type tieKind int

const (
	tieNone tieKind = iota
	tieOtherSite
	tieWritePort
	tieSameAddress
)

// classifyTie is rail step 5's tie rule. others is every in-scope site's
// address; self is skipped by id. The model's scheme is never compared.
func classifyTie(selfID uuid.UUID, self siteAddress, page *pageURL, others []siteAddressRow) tieKind {
	writtenPort := page.u.Port()
	decoded := page.u.Path
	var tied []siteAddress
	for _, o := range others {
		if o.ID == selfID {
			continue
		}
		oa, ok := siteAddressOf(o.URL)
		if !ok || oa.host != self.host {
			continue
		}
		if writtenPort != "" && oa.port != writtenPort {
			continue
		}
		if !pathCovers(oa.addr.Path, decoded) || len(oa.addr.Path) < len(self.addr.Path) {
			continue
		}
		tied = append(tied, oa)
	}
	if len(tied) == 0 {
		return tieNone
	}
	for _, t := range tied {
		if t.port == self.port && len(t.addr.Path) > len(self.addr.Path) {
			return tieOtherSite
		}
	}
	if writtenPort == "" {
		allOther := true
		for _, t := range tied {
			if t.port == self.port {
				allOther = false
				break
			}
		}
		if allOther {
			return tieWritePort
		}
	}
	return tieSameAddress
}

// pageOnSite is rail step 5 up to the tie check: host, port and path.
func pageOnSite(self siteAddress, page *pageURL) bool {
	mk, ok := siteaddr.HostKey(page.u.Hostname())
	if !ok || mk != self.host {
		return false
	}
	if p := page.u.Port(); p != "" && p != self.port {
		return false
	}
	return pathCovers(self.addr.Path, page.u.Path)
}

// storedPageURL is rail step 5's rebuild: the site's own scheme, HostKey
// host and port, and only the model's escaped path.
func storedPageURL(self siteAddress, page *pageURL) string {
	return siteaddr.Join(self.addr.Scheme, self.host, self.addr.Port, page.escapedPath)
}

// satisfiesURLBackstop is the Go mirror of the url_backstop CHECK.
func satisfiesURLBackstop(s string) bool {
	return len(s) <= maxStoredURLBytes && storedURLPattern.MatchString(s) && !strings.ContainsAny(s, "?#")
}

// ---------------------------------------------------------------------------
// The rail
// ---------------------------------------------------------------------------

// requestRail returns the rail's store, or an error naming what is missing.
// It is the one definition of "the rail is installed": SetWriteToolsEnabled
// and liveRegistry use it, and so does every invocation.
func (s *Service) requestRail() (requestStore, error) {
	rs, ok := s.store.(requestStore)
	if !ok || rs == nil {
		return nil, errors.New("the store does not implement the request rail")
	}
	if s.audit == nil {
		return nil, errors.New("no audit recorder: the rail records in its own transaction")
	}
	if s.context == nil {
		return nil, errors.New("no governed-context resolver: operator rules could not be enforced")
	}
	if s.requestLimit == nil {
		return nil, errors.New("no request rate limiter")
	}
	return rs, nil
}

func railUnavailable(err error) *toolRefusal {
	return &toolRefusal{
		reason: reasonRequestRailUnavailable,
		err: domain.Internal("mcp_request_rail_unavailable", genericToolFailure).
			WithCause(err),
	}
}

// createdResult is the success answer. site_name, site_url and url are fenced.
type createdResult struct {
	RequestID        string  `json:"request_id"`
	State            string  `json:"state"`
	Existing         bool    `json:"existing"`
	SiteID           string  `json:"site_id"`
	SiteName         string  `json:"site_name"`
	SiteURL          string  `json:"site_url"`
	Scope            string  `json:"scope"`
	URL              *string `json:"url"`
	ExpiresAt        string  `json:"expires_at"`
	PollAfterSeconds int     `json:"poll_after_seconds"`
	ReviewPath       string  `json:"review_path"`
	Message          string  `json:"message"`
}

// requestSiteCachePurge is the creation rail's entry point.
func (s *Service) requestSiteCachePurge(ctx context.Context, auth AuthorizedRequest, raw json.RawMessage) (string, error) {
	rs, err := s.requestRail()
	if err != nil {
		return "", railUnavailable(err)
	}

	// 1. Strict decode and the URL grammar.
	args, ref := decodeCachePurgeArgs(raw)
	if ref != nil {
		return "", ref
	}
	// 2. Empty scope.
	if auth.Sites.IsEmpty() {
		return "", scopeEmptyRefusal()
	}
	// 3. Membership, with no database access.
	absentMeta := map[string]any{"site_id": args.SiteID.String()}
	if !auth.Sites.Allows(args.SiteID) {
		return "", absentRefusal(reasonSiteAbsent, absentMeta)
	}
	// 4. The scoped row read, then the site's own address for both scopes.
	p, err := SingleSitePrincipal(auth, args.SiteID)
	if errors.Is(err, ErrSiteNotInScope) {
		return "", absentRefusal(reasonSiteAbsent, absentMeta)
	}
	if err != nil {
		return "", fmt.Errorf("build single-site principal: %w", err)
	}
	rows, _, err := s.store.ListSitesForRead(ctx, p, 1)
	if err != nil {
		return "", fmt.Errorf("read site for cache request: %w", err)
	}
	if len(rows) != 1 || rows[0].ID != args.SiteID {
		return "", absentRefusal(reasonSiteAbsent, absentMeta)
	}
	row := rows[0]
	self, ok := siteAddressOf(row.Url)
	if !ok {
		return "", siteAddressUnusableRefusal()
	}

	// 5. The page, for `url` only.
	var stored *string
	if args.Scope == scopeURL {
		st, ref, err := s.pageForRequest(ctx, rs, auth, row.ID, self, args.page)
		if err != nil {
			return "", err
		}
		if ref != nil {
			return "", ref
		}
		stored = &st
	}

	// 6. Governed context, at site scope, fail-closed.
	entry, forbidden, err := s.ForbiddenByContext(ctx, auth.TenantID, row.ID, ToolSiteCachePurgeRequest)
	if err != nil {
		return "", refuse(reasonContextUnavailable, domain.Internal(ErrCodeContextUnavailable,
			"this site's governed context cannot be resolved").WithCause(err))
	}
	if forbidden {
		r := refuse(reasonForbiddenByContext, domain.Forbidden(ErrCodeToolForbiddenByContext,
			msgForbiddenByContext).WithDetails(map[string]any{"retryable": false}))
		r.meta = map[string]any{"matched_entry": humantext.CapRunes(humantext.Clean(entry), 200)}
		return "", r
	}

	// 7. The agent.
	if !agentConnectedEnough(row.ConnectionState) {
		return "", refuse(reasonSiteUnreachable, domain.Unavailable(ErrCodeSiteUnreachable,
			msgSiteUnreachable).WithDetails(map[string]any{"retryable": true}))
	}
	if !AgentMeetsOriginOnlyFloor(row.AgentVersion) {
		return "", refuse(reasonAgentOutdated, domain.Conflict(ErrCodeSiteAgentOutdated,
			agentOutdatedMessage).WithDetails(map[string]any{
			"min_agent_version": MinAgentVersionForOriginOnlyPurge,
			"retryable":         false,
		}))
	}

	// 8. The per-process limits. Log only.
	if d := s.requestLimit.allow(auth.GrantID, row.ID); !d.allowed {
		r := refuse(reasonRequestRateLimited, domain.RateLimited(ErrCodeRequestLimited, msgLimited).
			WithDetails(map[string]any{
				"limit_scope":         d.scope,
				"retry_after_seconds": int(d.retryAfter / time.Second),
				"retryable":           true,
			}))
		r.logOnly = true
		return "", r
	}

	// 9. The creation transaction.
	res, err := s.createRequest(ctx, rs, auth, row, self, args.Scope, stored)
	if err != nil {
		return "", err
	}
	res.SiteName = fenceSiteText(row.Name)
	res.SiteURL = fenceSiteText(row.Url)
	b, err := json.Marshal(res)
	if err != nil {
		return "", fmt.Errorf("encode cache request result: %w", err)
	}
	return string(b), nil
}

// agentOutdatedMessage names the floor, which is our constant.
var agentOutdatedMessage = "This site's WPMgr agent is too old to clear only this site's cache, so " +
	"nothing was asked. The agent must be updated to " + MinAgentVersionForOriginOnlyPurge +
	" or later."

// pageForRequest is rail step 5: the page must be on this site, no other
// in-scope site may claim it, and the stored value is rebuilt.
func (s *Service) pageForRequest(ctx context.Context, rs requestStore, auth AuthorizedRequest, siteID uuid.UUID, self siteAddress, page *pageURL) (string, *toolRefusal, error) {
	schema := cachePurgeRequestSchema
	if !pageOnSite(self, page) {
		return "", argRefusal(reasonURLNotOnSite, "url", page.raw, msgArgURLNotOnSite, schema), nil
	}
	others, err := rs.ListSiteAddressesInScope(ctx, connectionScopedPrincipal(auth))
	if err != nil {
		return "", nil, fmt.Errorf("read in-scope site addresses: %w", err)
	}
	switch classifyTie(siteID, self, page, others) {
	case tieOtherSite:
		return "", argRefusal(reasonURLOtherSite, "url", page.raw, msgTieOtherSite, schema), nil
	case tieWritePort:
		return "", argRefusal(reasonURLPortAmbiguous, "url", page.raw, msgTieWritePort, schema), nil
	case tieSameAddress:
		return "", argRefusal(reasonURLSameAddress, "url", page.raw, msgTieSameAddress, schema), nil
	}
	stored := storedPageURL(self, page)
	if len(stored) > maxStoredURLBytes {
		return "", argRefusal(reasonInvalidArguments, "url", page.raw, msgArgURLTooLong, schema), nil
	}
	if !satisfiesURLBackstop(stored) {
		// Unreachable by construction; a mismatch is a defect, not a refusal.
		return "", nil, errors.New("rebuilt page address fails the stored-url backstop")
	}
	return stored, nil, nil
}

// requestFacts are the stored facts a request's digest covers.
type requestFacts struct {
	siteLabel   string
	siteHost    string
	grantLabel  string
	grantVia    string
	setupClient *string
	nonce       string
	digest      string
	expiresAt   time.Time
}

// buildRequestFacts is creation-transaction step 4.
func buildRequestFacts(auth AuthorizedRequest, siteID uuid.UUID, siteName, siteHost, scope string, stored *string, now time.Time) (requestFacts, error) {
	f := requestFacts{
		siteLabel:   humantext.CapRunes(humantext.Clean(siteName), siteLabelRunes),
		siteHost:    siteHost,
		grantLabel:  humantext.CapRunes(humantext.Clean(auth.GrantName), grantLabelRunes),
		grantVia:    grantViaToken,
		setupClient: auth.SetupClient,
		expiresAt:   now.UTC().Add(requestWindow).Truncate(time.Microsecond),
	}
	if strings.TrimSpace(f.grantLabel) == "" {
		f.grantLabel = unnamedConnectionLabel
	}
	if auth.ViaOAuth {
		f.grantVia = grantViaBrowserSignIn
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return requestFacts{}, fmt.Errorf("read digest nonce: %w", err)
	}
	f.nonce = hex.EncodeToString(nonce[:])
	canonical := map[string]any{
		"site_id":      siteID.String(),
		"site_label":   f.siteLabel,
		"site_host":    f.siteHost,
		"scope":        scope,
		"url":          stored,
		"grant_id":     auth.GrantID.String(),
		"grant_label":  f.grantLabel,
		"grant_via":    f.grantVia,
		"setup_client": f.setupClient,
		"expires_at":   f.expiresAt.Format(time.RFC3339Nano),
		"copy_version": CacheRequestCardCopyVersion,
		"digest_nonce": f.nonce,
	}
	b, err := json.Marshal(canonical)
	if err != nil {
		return requestFacts{}, fmt.Errorf("encode digest facts: %w", err)
	}
	sum := sha256.Sum256(b)
	f.digest = hex.EncodeToString(sum[:])
	return f, nil
}

// createRequest is rail step 9, in one connection-scoped transaction.
func (s *Service) createRequest(ctx context.Context, rs requestStore, auth AuthorizedRequest, row sqlc.Site, self siteAddress, scope string, stored *string) (createdResult, error) {
	var out createdResult
	err := rs.RunRequestTx(ctx, connectionScopedPrincipal(auth), func(tx pgx.Tx, q requestQueries) error {
		// 9.1 The grant lock, bound as text. It serialises this
		// connection's caps and its dedupe.
		if err := q.TakeAssistantRequestXactLock(ctx, sqlc.TakeAssistantRequestXactLockParams{
			LockKey: grantRequestLockKey, LockID: auth.GrantID.String(),
		}); err != nil {
			return fmt.Errorf("take the connection's request lock: %w", err)
		}
		// 9.2 This connection's own lapsed waiting row for this site.
		expired, err := q.ExpireLapsedPendingAssistantCachePurgeRequest(ctx,
			sqlc.ExpireLapsedPendingAssistantCachePurgeRequestParams{
				TenantID: auth.TenantID, SiteID: row.ID, ProposedByGrantID: auth.GrantID,
			})
		if err != nil {
			return fmt.Errorf("expire a lapsed waiting request: %w", err)
		}
		// 9.3 The caps, read-only, across every site in scope.
		if capRef, err := checkRequestCaps(ctx, q, auth, row.ID); err != nil {
			return err
		} else if capRef != nil {
			// A repeat of the request already waiting is not a new request,
			// so a cap never refuses it.
			existing, err := q.GetPendingAssistantCachePurgeRequestForGrantSite(ctx,
				sqlc.GetPendingAssistantCachePurgeRequestForGrantSiteParams{
					TenantID: auth.TenantID, SiteID: row.ID, ProposedByGrantID: auth.GrantID,
				})
			if errors.Is(err, pgx.ErrNoRows) {
				return capRef
			}
			if err != nil {
				return fmt.Errorf("read the waiting request: %w", err)
			}
			res, ref := dedupeAnswer(existing, scope, stored)
			if ref != nil {
				return ref
			}
			out = res
			return s.recordCreation(ctx, tx, auth, expired, out)
		}
		// 9.4 Facts and digest.
		facts, err := buildRequestFacts(auth, row.ID, row.Name, self.host, scope, stored, s.now())
		if err != nil {
			return err
		}
		// 9.5 and 9.6 The insert, and the dedupe read on conflict, retried
		// once when a person decided the waiting row in between.
		done := false
		for attempt := 0; attempt < 2 && !done; attempt++ {
			ins, err := q.InsertAssistantCachePurgeRequest(ctx, sqlc.InsertAssistantCachePurgeRequestParams{
				TenantID:          auth.TenantID,
				SiteID:            row.ID,
				ProposedByGrantID: auth.GrantID,
				Scope:             scope,
				Url:               stored,
				SiteLabel:         facts.siteLabel,
				SiteHost:          facts.siteHost,
				GrantLabel:        facts.grantLabel,
				GrantVia:          facts.grantVia,
				SetupClient:       facts.setupClient,
				DigestNonce:       facts.nonce,
				PresentedDigest:   facts.digest,
				ExpiresAt:         facts.expiresAt,
			})
			if err == nil {
				out = resultFromRow(ins, false)
				done = true
				break
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("insert the request: %w", err)
			}
			existing, err := q.GetPendingAssistantCachePurgeRequestForGrantSite(ctx,
				sqlc.GetPendingAssistantCachePurgeRequestForGrantSiteParams{
					TenantID: auth.TenantID, SiteID: row.ID, ProposedByGrantID: auth.GrantID,
				})
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return fmt.Errorf("read the waiting request: %w", err)
			}
			res, ref := dedupeAnswer(existing, scope, stored)
			if ref != nil {
				return ref
			}
			out = res
			done = true
		}
		if !done {
			return errors.New("the insert conflicted twice and the waiting row was gone both times")
		}
		// 9.7 Audit, last.
		return s.recordCreation(ctx, tx, auth, expired, out)
	})
	if err != nil {
		return createdResult{}, err
	}
	return out, nil
}

// checkRequestCaps is creation-transaction step 3. Under the connection's
// principal the per-connection counts see its rows on every site in scope.
func checkRequestCaps(ctx context.Context, q requestQueries, auth AuthorizedRequest, siteID uuid.UUID) (*toolRefusal, error) {
	pending, err := q.CountLivePendingAssistantCachePurgeRequestsForGrant(ctx,
		sqlc.CountLivePendingAssistantCachePurgeRequestsForGrantParams{TenantID: auth.TenantID, ProposedByGrantID: auth.GrantID})
	if err != nil {
		return nil, fmt.Errorf("count waiting requests: %w", err)
	}
	if pending >= maxPendingPerConnection {
		return limitRefusal(reasonPendingCap, limitScopePending, pendingCapRetrySeconds), nil
	}
	created, err := q.CountAssistantCachePurgeRequestsForGrantSince(ctx,
		sqlc.CountAssistantCachePurgeRequestsForGrantSinceParams{
			TenantID: auth.TenantID, ProposedByGrantID: auth.GrantID, WindowSeconds: createdWindowSeconds,
		})
	if err != nil {
		return nil, fmt.Errorf("count requests in the last day: %w", err)
	}
	if created >= maxCreatedPerConnection {
		return limitRefusal(reasonGrantDailyCap, limitScopeGrantDaily, grantDailyRetrySeconds), nil
	}
	clears, err := q.CountAssistantCachePurgesOnSiteSince(ctx, sqlc.CountAssistantCachePurgesOnSiteSinceParams{
		TenantID: auth.TenantID, SiteID: siteID, WindowSeconds: siteHourWindowSeconds,
	})
	if err != nil {
		return nil, fmt.Errorf("count AI clears on the site: %w", err)
	}
	if clears >= maxAIClearsPerSiteHour {
		return limitRefusal(reasonSiteHourlyCap, limitScopeSiteHourly, siteHourlyRetrySeconds), nil
	}
	return nil, nil
}

// dedupeAnswer is creation-transaction step 6 for a waiting row this
// connection already has on the site: the same request is existing:true, a
// different one is refused naming the caller's own row.
func dedupeAnswer(existing sqlc.AssistantCachePurgeRequest, scope string, stored *string) (createdResult, *toolRefusal) {
	same := existing.Scope == scope &&
		((existing.Url == nil && stored == nil) ||
			(existing.Url != nil && stored != nil && *existing.Url == *stored))
	if same {
		return resultFromRow(existing, true), nil
	}
	details := map[string]any{
		"limit_scope": limitScopePendingForSite,
		"request_id":  existing.ID.String(),
		"scope":       existing.Scope,
		"url":         fencedURL(existing.Url),
		"retryable":   false,
	}
	return createdResult{}, &toolRefusal{
		reason: reasonPendingRequestForSite,
		err:    domain.Conflict(ErrCodeRequestLimited, msgPendingForSite).WithDetails(details),
		meta:   map[string]any{"request_id": existing.ID.String()},
	}
}

func fencedURL(u *string) *string {
	if u == nil {
		return nil
	}
	f := fenceSiteText(*u)
	return &f
}

func resultFromRow(r sqlc.AssistantCachePurgeRequest, existing bool) createdResult {
	return createdResult{
		RequestID:        r.ID.String(),
		State:            stateWaitingForApproval,
		Existing:         existing,
		SiteID:           r.SiteID.String(),
		Scope:            r.Scope,
		URL:              fencedURL(r.Url),
		ExpiresAt:        r.ExpiresAt.UTC().Format(time.RFC3339),
		PollAfterSeconds: requestPollAfterSeconds,
		ReviewPath:       requestReviewPath,
		Message:          msgCreated,
	}
}

// recordCreation is creation-transaction step 7: the in-line expiry's row,
// then mcp.tool.called, both in the creation transaction and after its
// request-row writes.
func (s *Service) recordCreation(ctx context.Context, tx pgx.Tx, auth AuthorizedRequest, expired []uuid.UUID, res createdResult) error {
	if err := s.requireRecorder(); err != nil {
		return err
	}
	for _, id := range expired {
		if _, err := s.audit.RecordInTx(ctx, tx, audit.Event{
			TenantID:   auth.TenantID,
			ActorType:  audit.ActorSystem,
			Action:     audit.ActionAssistantRequestExpired,
			TargetType: audit.TargetTypeAssistantCachePurgeRequest,
			TargetID:   id.String(),
			Metadata:   map[string]any{"request_id": id.String(), "expired_by": "creation"},
		}); err != nil {
			return auditFailure(err)
		}
	}
	_, err := s.audit.RecordInTx(ctx, tx, audit.Event{
		TenantID:   auth.TenantID,
		ActorType:  audit.ActorAssistant,
		ActorID:    auth.GrantID.String(),
		Action:     audit.ActionMCPToolCalled,
		TargetType: "mcp_tool",
		TargetID:   ToolSiteCachePurgeRequest,
		Metadata: map[string]any{
			"grant_name":          auth.GrantName,
			"operator_permission": string(authz.PermSiteCachePurge),
			"tool":                ToolSiteCachePurgeRequest,
			"request_id":          res.RequestID,
			"site_id":             res.SiteID,
			"scope":               res.Scope,
			"existing":            res.Existing,
		},
	})
	return auditFailure(err)
}
