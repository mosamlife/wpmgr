package mcp

// wpmgr/rest-read and wpmgr/rest-write (engine E3-G2): one reviewed WordPress
// REST route from rest_route_catalogue (m161), called on the site by the
// agent as the service principal (E3-A3, RC1).
//
// The control plane sends the route row's exact bytes (abilities.RouteBytes)
// and their hash. A route is offered only while its stored route_sha256 is
// set and still reproduces from the row: a NULL hash (never stamped) or a
// stale one fails closed here, at the worker, and on the site.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/humantext"
)

// The two catalogue entries over reviewed REST routes (owner ruling 1).
const (
	AbilityRestRead  = "wpmgr/rest-read"
	AbilityRestWrite = "wpmgr/rest-write"
)

const (
	notRunnableNoRoutes = "no_reviewed_routes"
	msgAbilityRestOutdated = "This site's WPMgr agent is too old to use the site's own API. The agent must be updated to " +
		agentcmd.MinAgentVersionForRestCall + " or later. Nothing was asked."
	msgAbilityRestRoute = "input.route_id must name a reviewed route this tool offers; call site_ability_describe for the list"
	msgAbilityRestInput = "input must be {route_id, path, query, body} with only the keys and types the route lists in site_ability_describe"
	hintRestInput       = "Send only route_id, path, query and body, with the keys and types site_ability_describe lists for that route. " +
		"Path values are whole numbers; status, context, author and similar keys are set by WPMgr and cannot be sent."
	hintPostNotEditable = "There is no page or post of that kind with that id that WPMgr may edit. Read the list of published pages or posts first and use an id from it."

	// msgPostContentWouldChange is the AI's and the card's text for
	// post_content_would_change.
	msgPostContentWouldChange = "This page contains content the WPMgr user can't save, so WPMgr won't change its title. Edit it in WordPress."

	// Card copy (ruling 5).
	restEffectLive     = "Published immediately"
	restEffectNotLive  = "Saved to the post; it is not published"
	restUndoNotExact   = "Undo may not restore the exact characters of the previous value: this site would alter them when saving."
	restCardValueRunes = 1000
)

// isRestAbility names the two REST entries.
func isRestAbility(name string) bool { return name == AbilityRestRead || name == AbilityRestWrite }

// RouteEncoder returns the exact route bytes to send and their sha256,
// refusing a route whose stored hash is NULL or no longer reproduces.
// Production passes abilities.SendableRoute.
type RouteEncoder func(sqlc.RestRouteCatalogue) ([]byte, string, error)

// RestRouteStore reads the global route catalogue. *Repo implements it; an
// AbilityStore without it offers no routes.
type RestRouteStore interface {
	RestRoutes(ctx context.Context, p domain.Principal) ([]sqlc.RestRouteCatalogue, error)
}

var _ RestRouteStore = (*Repo)(nil)

// RestRoutes implements RestRouteStore: every route, enabled or not.
func (r *Repo) RestRoutes(ctx context.Context, p domain.Principal) ([]sqlc.RestRouteCatalogue, error) {
	var out []sqlc.RestRouteCatalogue
	err := r.pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
		var err error
		out, err = sqlc.New(tx).ListRestRoutes(ctx)
		return err
	})
	return out, err
}

// SetRouteEncoder wires the route encoder. Without it both REST entries
// answer as having no reviewed routes.
func (s *Service) SetRouteEncoder(enc RouteEncoder) {
	if s.abilities != nil {
		s.abilities.route = enc
	}
}

// sendableRoute is one route that may be offered and sent now.
type sendableRoute struct {
	row   sqlc.RestRouteCatalogue
	bytes []byte
	sum   string
}

// offeredRoutes are the routes an entry of class `class` may run: enabled,
// that class, stamped, and reproducing their stored hash. Ordered by id.
func (e *abilityEngine) offeredRoutes(ctx context.Context, p domain.Principal, class string) ([]sendableRoute, error) {
	rs, ok := e.store.(RestRouteStore)
	if !ok || e.route == nil {
		return nil, nil
	}
	rows, err := rs.RestRoutes(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("read rest route catalogue: %w", err)
	}
	out := make([]sendableRoute, 0, len(rows))
	for _, r := range rows {
		if !r.Enabled || r.Class != class || r.RouteSha256 == nil {
			continue
		}
		b, sum, err := e.route(r)
		if err != nil || sum != *r.RouteSha256 {
			continue
		}
		out = append(out, sendableRoute{row: r, bytes: b, sum: sum})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].row.RouteID < out[j].row.RouteID })
	return out, nil
}

func restClassOf(name string) string {
	if name == AbilityRestWrite {
		return "write"
	}
	return "read"
}

// ---------------------------------------------------------------------------
// Input: {route_id, path, query, body}, checked against the route row. The
// agent re-checks every rule (RC1); this keeps a bad call from reaching it
// and names the route the request is bound to.
// ---------------------------------------------------------------------------

var (
	restRouteIDPattern = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	restPathIntPattern = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)
	// restBadChars mirrors the agent's RE_BAD_CHARS: controls (newline
	// allowed in a body value only), bidi controls, BOM, noncharacters.
	restBadChars = regexp.MustCompile(`[\x{0000}-\x{0009}\x{000B}-\x{001F}\x{007F}-\x{009F}\x{200B}-\x{200F}\x{202A}-\x{202E}\x{2066}-\x{2069}\x{FEFF}\x{FFFE}\x{FFFF}]`)
)

// restForbiddenKeys mirror the agent's RestCall::FORBIDDEN_KEYS.
var restForbiddenKeys = map[string]struct{}{
	"_method": {}, "_embed": {}, "_envelope": {}, "_jsonp": {}, "_fields": {}, "password": {},
	"context": {}, "status": {}, "author": {}, "meta": {}, "slug": {}, "template": {},
}

// restKeySpec is one typed key of a route row (path_params, query_keys,
// body_keys).
type restKeySpec struct {
	Type     string   `json:"type"`
	Min      *int64   `json:"min"`
	Max      *int64   `json:"max"`
	MaxLen   *int     `json:"max_len"`
	MaxItems *int     `json:"max_items"`
	Values   []string `json:"values"`
	Required bool     `json:"required"`
}

// restInput is a checked input.
type restInput struct {
	routeID  string
	path     map[string]int64
	body     map[string]string
	targetID int64
}

func decodeObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if len(raw) == 0 {
		return map[string]json.RawMessage{}, true
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return nil, false
	}
	return m, true
}

func restSpecs(raw []byte) (map[string]restKeySpec, bool) {
	out := map[string]restKeySpec{}
	if len(raw) == 0 {
		return out, true
	}
	return out, json.Unmarshal(raw, &out) == nil
}

// restInputRouteID reads input.route_id only.
func restInputRouteID(input []byte) (string, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(input, &m) != nil {
		return "", false
	}
	var id string
	if json.Unmarshal(m["route_id"], &id) != nil || !restRouteIDPattern.MatchString(id) {
		return "", false
	}
	return id, true
}

// checkRestInput validates input against the route. ok false refuses.
func checkRestInput(input []byte, r sqlc.RestRouteCatalogue) (restInput, bool) {
	var top map[string]json.RawMessage
	if json.Unmarshal(input, &top) != nil || len(top) == 0 {
		return restInput{}, false
	}
	for k := range top {
		switch k {
		case "route_id", "path", "query", "body":
		default:
			return restInput{}, false
		}
	}
	var id string
	if json.Unmarshal(top["route_id"], &id) != nil || id != r.RouteID {
		return restInput{}, false
	}
	out := restInput{routeID: id, path: map[string]int64{}, body: map[string]string{}}
	for _, part := range []struct {
		name string
		spec []byte
	}{{"path", r.PathParams}, {"query", r.QueryKeys}, {"body", r.BodyKeys}} {
		given, ok := decodeObject(top[part.name])
		if !ok {
			return restInput{}, false
		}
		specs, ok := restSpecs(part.spec)
		if !ok {
			return restInput{}, false
		}
		for k, v := range given {
			if _, bad := restForbiddenKeys[k]; bad {
				return restInput{}, false
			}
			sp, known := specs[k]
			if !known || !restValueOK(part.name, v, sp) {
				return restInput{}, false
			}
			switch part.name {
			case "path":
				var n int64
				_ = json.Unmarshal(v, &n)
				out.path[k] = n
			case "body":
				var s string
				_ = json.Unmarshal(v, &s)
				out.body[k] = s
			}
		}
		for k, sp := range specs {
			if _, present := given[k]; sp.Required && !present {
				return restInput{}, false
			}
		}
	}
	if r.Class == "write" {
		var tgt struct {
			Param string `json:"param"`
		}
		if json.Unmarshal(r.Target, &tgt) != nil || out.path[tgt.Param] < 1 || len(out.body) == 0 {
			return restInput{}, false
		}
		if t, ok := out.body["title"]; ok && strings.TrimSpace(t) == "" {
			return restInput{}, false
		}
		out.targetID = out.path[tgt.Param]
	}
	return out, true
}

func restValueOK(part string, v json.RawMessage, sp restKeySpec) bool {
	switch sp.Type {
	case "int":
		var n int64
		if json.Unmarshal(v, &n) != nil || sp.Min == nil || sp.Max == nil || n < *sp.Min || n > *sp.Max {
			return false
		}
		return part != "path" || restPathIntPattern.MatchString(fmt.Sprint(n))
	case "string":
		var s string
		if json.Unmarshal(v, &s) != nil || sp.MaxLen == nil || !utf8.ValidString(s) ||
			restBadChars.MatchString(s) || utf8.RuneCountInString(s) > *sp.MaxLen {
			return false
		}
		return part == "body" || !strings.Contains(s, "\n")
	case "enum":
		var s string
		if json.Unmarshal(v, &s) != nil {
			return false
		}
		for _, x := range sp.Values {
			if x == s {
				return true
			}
		}
		return false
	case "int_list":
		var ns []int64
		if json.Unmarshal(v, &ns) != nil || len(ns) == 0 || sp.MaxItems == nil || len(ns) > *sp.MaxItems {
			return false
		}
		for _, n := range ns {
			if !restPathIntPattern.MatchString(fmt.Sprint(n)) {
				return false
			}
		}
		return true
	}
	return false
}

// pickRestRoute finds the offered route an input names.
func pickRestRoute(routes []sendableRoute, input []byte) (*sendableRoute, bool) {
	id, ok := restInputRouteID(input)
	if !ok {
		return nil, false
	}
	for i := range routes {
		if routes[i].row.RouteID == id {
			return &routes[i], true
		}
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// describe: the routes block
// ---------------------------------------------------------------------------

// describeRoute is one route as the model sees it. Every string is ours.
type describeRoute struct {
	RouteID     string          `json:"route_id"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Method      string          `json:"method"`
	Path        json.RawMessage `json:"path"`
	Query       json.RawMessage `json:"query"`
	Body        json.RawMessage `json:"body"`
	Effect      string          `json:"effect"`
}

var restInputSchema = json.RawMessage(`{"type":"object","properties":{` +
	`"route_id":{"type":"string","pattern":"^[a-z0-9-]{1,64}$","description":"one route_id from routes"},` +
	`"path":{"type":"object"},"query":{"type":"object"},"body":{"type":"object"}},` +
	`"required":["route_id"],"additionalProperties":false}`)

func describeRoutesOf(routes []sendableRoute) []describeRoute {
	out := make([]describeRoute, 0, len(routes))
	for _, r := range routes {
		out = append(out, describeRoute{
			RouteID: r.row.RouteID, Title: r.row.Title, Description: r.row.Description, Method: r.row.Method,
			Path: rawJSONOr(r.row.PathParams), Query: rawJSONOr(r.row.QueryKeys), Body: rawJSONOr(r.row.BodyKeys),
			Effect: r.row.EffectCopy,
		})
	}
	return out
}

func rawJSONOr(b []byte) json.RawMessage {
	if len(b) == 0 || !json.Valid(b) {
		return json.RawMessage(`{}`)
	}
	return append(json.RawMessage(nil), b...)
}

// ---------------------------------------------------------------------------
// rest-read
// ---------------------------------------------------------------------------

// runRestRead runs one read route. Every generic gate (runnable, context,
// reachability) has already passed in runSiteAbility.
func (s *Service) runRestRead(ctx context.Context, auth AuthorizedRequest, eng *abilityEngine, site abilitySite, c *classified, input []byte) (string, error) {
	routes, err := eng.offeredRoutes(ctx, site.p, "read")
	if err != nil {
		return "", err
	}
	rt, ok := pickRestRoute(routes, input)
	if !ok {
		return "", argRefusal(reasonInvalidArguments, "input", "", msgAbilityRestRoute, restInputSchema)
	}
	if _, ok := checkRestInput(input, rt.row); !ok {
		return "", argRefusal(reasonInvalidArguments, "input", "", msgAbilityRestInput, restInputSchema)
	}
	if d := eng.readLimit.allow(auth.GrantID, site.row.ID); !d.allowed {
		r := refuse(reasonRequestRateLimited, domain.RateLimited(ErrCodeRequestLimited, msgAbilityReadLimited).
			WithDetails(map[string]any{
				"limit_scope":         d.scope,
				"retry_after_seconds": int(d.retryAfter / time.Second),
				"retryable":           true,
			}))
		r.logOnly = true
		return "", r
	}
	entryBytes, entrySum, err := eng.entry(*c.entry)
	if err != nil {
		return "", notRunnableRefusal(notRunnableDisabled)
	}
	callCtx, cancel := context.WithTimeout(ctx, abilityRunTimeout)
	resp, err := eng.agent.AbilityRun(callCtx, site.row.ID, site.row.Url, agentcmd.AbilityRunCall{
		Mode: agentcmd.AbilityRunModeRead, RequestID: uuid.New(),
		Entry: entryBytes, EntrySHA256: entrySum, Input: input, Route: rt.bytes, RouteSHA256: rt.sum,
	})
	cancel()
	if err != nil {
		var refusal *agentcmd.AbilityRunRefusal
		if errors.As(err, &refusal) {
			return "", vendorRefusal(refusal)
		}
		return "", refuse(reasonSiteUnreachable, domain.Unavailable(ErrCodeSiteUnreachable,
			msgSiteUnreachable).WithDetails(map[string]any{"retryable": true}))
	}
	if resp.RouteID != rt.row.RouteID || resp.RouteSHA256 != rt.sum {
		return "", refuse(reasonSiteUnreachable, domain.Unavailable(ErrCodeSiteUnreachable,
			msgSiteUnreachable).WithDetails(map[string]any{"retryable": false}))
	}
	out, truncated := fenceRouteOutput(rt.row.OutputFields, resp.Output, abilityRunDefaultOutputBytes)
	b, err := json.Marshal(runResult{
		Name: c.name, AsOf: s.now().UTC().Format(time.RFC3339), Output: out, Truncated: truncated,
	})
	if err != nil {
		return "", fmt.Errorf("encode run result: %w", err)
	}
	return string(b), nil
}

// fenceRouteOutput projects a route's output onto the row's output_fields:
// a key the row does not list never reaches the answer, and every string
// leaf is fenced. A row without a valid shape returns no output.
func fenceRouteOutput(fields []byte, raw json.RawMessage, maxBytes int) (json.RawMessage, bool) {
	shape, err := parseOutShape(fields)
	if err != nil || len(raw) == 0 {
		return json.RawMessage(`null`), false
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return json.RawMessage(`null`), true
	}
	b, err := json.Marshal(projectOutput(v, shape))
	if err != nil || len(b) > maxBytes {
		return json.RawMessage(`null`), true
	}
	return b, false
}

// ---------------------------------------------------------------------------
// rest-write: precheck, verification and the card
// ---------------------------------------------------------------------------

// restTargetFacts is precheck's target_facts. Strings are SITE TEXT.
type restTargetFacts struct {
	ID            int64  `json:"id"`
	PostType      string `json:"post_type"`
	Status        string `json:"status"`
	Live          bool   `json:"live"`
	TitleBefore   string `json:"title_before"`
	ExcerptBefore string `json:"excerpt_before"`
}

// restChange is one precheck change. before is SITE TEXT; after is the
// value the AI asked for; stored is what the site will store for it.
type restChange struct {
	Key    string `json:"key"`
	Before string `json:"before"`
	After  string `json:"after"`
	Stored string `json:"stored"`
}

// checkedRestPrecheck is a precheck answer the control plane verified.
type checkedRestPrecheck struct {
	precheckDigest  string
	baseFingerprint string
	target          restTargetFacts
	changes         []restChange
	undoExact       bool
}

// restPostFieldOrder is the agent's POST_FIELDS order.
var restPostFieldOrder = []string{"title", "excerpt"}

// restPrecheckDigest is the agent's RestCall::precheckDigest: sha256 of
// json_encode([entry_sha256, route_sha256, sha256(input), base_fingerprint]).
// All four are lowercase hex, which PHP and Go encode identically.
func restPrecheckDigest(entrySum, routeSum string, input []byte, baseFP string) string {
	b, _ := phpJSONStringArray(entrySum, routeSum, sha256Hex(input), baseFP)
	return sha256Hex(b)
}

// verifyRestWritePrecheck checks the site's precheck answer against what
// this control plane sent: the digest is recomputed from bytes we hold, the
// target is the post the input names and of the route's type, and every
// change is exactly a body value we sent.
func verifyRestWritePrecheck(resp agentcmd.AbilityRunResponse, entrySum string, rt sendableRoute, input []byte, in restInput) (checkedRestPrecheck, bool) {
	if !resp.Valid || resp.RouteID != rt.row.RouteID || !hex64Pattern.MatchString(resp.PrecheckDigest) ||
		!hex64Pattern.MatchString(resp.BaseFingerprint) || resp.UndoExact == nil {
		return checkedRestPrecheck{}, false
	}
	if restPrecheckDigest(entrySum, rt.sum, input, resp.BaseFingerprint) != resp.PrecheckDigest {
		return checkedRestPrecheck{}, false
	}
	var tf restTargetFacts
	dec := json.NewDecoder(bytes.NewReader(resp.TargetFacts))
	dec.DisallowUnknownFields()
	if len(resp.TargetFacts) == 0 || dec.Decode(&tf) != nil {
		return checkedRestPrecheck{}, false
	}
	var tgt struct {
		PostType string `json:"post_type"`
	}
	if json.Unmarshal(rt.row.Target, &tgt) != nil || tf.ID != in.targetID || tf.PostType != tgt.PostType ||
		tf.Live != (tf.Status == "publish") {
		return checkedRestPrecheck{}, false
	}
	var changes []restChange
	cd := json.NewDecoder(bytes.NewReader(resp.Changes))
	cd.DisallowUnknownFields()
	if len(resp.Changes) == 0 || cd.Decode(&changes) != nil || len(changes) != len(in.body) {
		return checkedRestPrecheck{}, false
	}
	i := 0
	for _, k := range restPostFieldOrder {
		v, sent := in.body[k]
		if !sent {
			continue
		}
		if changes[i].Key != k || changes[i].After != v {
			return checkedRestPrecheck{}, false
		}
		i++
	}
	return checkedRestPrecheck{
		precheckDigest: resp.PrecheckDigest, baseFingerprint: resp.BaseFingerprint,
		target: tf, changes: changes, undoExact: *resp.UndoExact,
	}, true
}

// restCardFacts is the structured card (card_facts). Every site string sits
// in a from_the_site slot, cleaned and capped; everything else is ours or the
// AI's requested value.
type restCardFacts struct {
	RouteID     string           `json:"route_id"`
	RouteTitle  string           `json:"route_title"`
	Method      string           `json:"method"`
	Target      restCardTarget   `json:"target"`
	Changes     []restCardChange `json:"changes"`
	EffectCopy  string           `json:"effect_copy"`
	Live        bool             `json:"live"`
	EffectLabel string           `json:"effect_label"`
	Undo        string           `json:"undo"`
	UndoExact   bool             `json:"undo_exact"`
	UndoNote    *string          `json:"undo_note"`
}

type restCardTarget struct {
	ID          int64              `json:"id"`
	PostType    string             `json:"post_type"`
	FromTheSite restCardTargetSite `json:"from_the_site"`
}

type restCardTargetSite struct {
	Status      string `json:"status"`
	TitleBefore string `json:"title_before"`
}

type restCardChange struct {
	Key         string             `json:"key"`
	Label       string             `json:"label"`
	After       string             `json:"after"`
	FromTheSite restCardChangeSite `json:"from_the_site"`
}

type restCardChangeSite struct {
	Before string `json:"before"`
}

// cardSiteText cleans and caps one site string for the card.
func cardSiteText(s string) string {
	return humantext.CapRunes(humantext.Clean(s), restCardValueRunes)
}

// buildRestCardFacts renders the card from the verified precheck and the
// route's arg_render labels.
func buildRestCardFacts(rt sqlc.RestRouteCatalogue, pc checkedRestPrecheck) restCardFacts {
	var labels map[string]struct {
		Label string `json:"label"`
	}
	_ = json.Unmarshal(rt.ArgRender, &labels)
	f := restCardFacts{
		RouteID: rt.RouteID, RouteTitle: rt.Title, Method: rt.Method,
		Target: restCardTarget{ID: pc.target.ID, PostType: pc.target.PostType, FromTheSite: restCardTargetSite{
			Status: cardSiteText(pc.target.Status), TitleBefore: cardSiteText(pc.target.TitleBefore),
		}},
		Changes:    make([]restCardChange, 0, len(pc.changes)),
		EffectCopy: rt.EffectCopy, Live: pc.target.Live, EffectLabel: restEffectNotLive,
		Undo: rt.Snapshot, UndoExact: pc.undoExact,
	}
	if pc.target.Live {
		f.EffectLabel = restEffectLive
	}
	if !pc.undoExact {
		n := restUndoNotExact
		f.UndoNote = &n
	}
	for _, ch := range pc.changes {
		label := ch.Key
		if l, ok := labels[ch.Key]; ok && l.Label != "" {
			label = l.Label
		}
		f.Changes = append(f.Changes, restCardChange{
			Key: ch.Key, Label: label, After: ch.After,
			FromTheSite: restCardChangeSite{Before: cardSiteText(ch.Before)},
		})
	}
	return f
}

// buildRestWriteRequestFacts computes the stored facts and presented_digest
// for a rest-write request. The digest covers route_id, route_sha256, the
// exact input and the exact card_facts bytes (v4 §4.1), so a card shown with
// other facts cannot be approved.
func buildRestWriteRequestFacts(auth AuthorizedRequest, row sqlc.Site, host string, e *sqlc.AbilityCatalogue, entrySum string, rt sendableRoute, input, card []byte, pc checkedRestPrecheck, now time.Time) (abilityRequestFacts, error) {
	f := abilityRequestFacts{
		siteLabel:    humantext.CapRunes(humantext.Clean(row.Name), siteLabelRunes),
		siteHost:     host,
		grantLabel:   humantext.CapRunes(humantext.Clean(auth.GrantName), grantLabelRunes),
		grantVia:     grantViaToken,
		setupClient:  auth.SetupClient,
		titleExcerpt: humantext.CapRunes(humantext.Clean(pc.target.TitleBefore), abilityTitleExcerptRunes),
		expiresAt:    now.UTC().Add(abilityRequestWindow).Truncate(time.Microsecond),
	}
	if strings.TrimSpace(f.grantLabel) == "" {
		f.grantLabel = unnamedConnectionLabel
	}
	if auth.ViaOAuth {
		f.grantVia = grantViaBrowserSignIn
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return abilityRequestFacts{}, fmt.Errorf("read digest nonce: %w", err)
	}
	f.nonce = hex.EncodeToString(nonce[:])
	f.digest = restWriteDigest(auth, row, e, entrySum, rt, input, card, pc, f)
	return f, nil
}

// restWriteDigest is the presented_digest over a rest-write request's facts.
func restWriteDigest(auth AuthorizedRequest, row sqlc.Site, e *sqlc.AbilityCatalogue, entrySum string, rt sendableRoute, input, card []byte, pc checkedRestPrecheck, f abilityRequestFacts) string {
	canonical := map[string]any{
		"copy_version":     AbilityCardCopyVersion,
		"mode":             "write",
		"ability_name":     e.Name,
		"entry_id":         e.EntryID.String(),
		"entry_sha256":     entrySum,
		"operator_perm":    *e.OperatorPermission,
		"route_id":         rt.row.RouteID,
		"route_sha256":     rt.sum,
		"input_json":       string(input),
		"input_sha256":     sha256Hex(input),
		"target":           fmt.Sprintf("post:%d", pc.target.ID),
		"title_excerpt":    f.titleExcerpt,
		"card_facts":       string(card),
		"precheck_digest":  pc.precheckDigest,
		"base_fingerprint": pc.baseFingerprint,
		"snapshot":         e.Snapshot,
		"effect_copy":      e.EffectCopy,
		"site_id":          row.ID.String(),
		"site_label":       f.siteLabel,
		"site_host":        f.siteHost,
		"grant_id":         auth.GrantID.String(),
		"grant_label":      f.grantLabel,
		"grant_via":        f.grantVia,
		"setup_client":     f.setupClient,
		"expires_at":       f.expiresAt.Format(time.RFC3339Nano),
		"digest_nonce":     f.nonce,
	}
	b, _ := json.Marshal(canonical)
	return sha256Hex(b)
}

// runRestWrite is the rest-write arm of runSiteAbilityWrite, entered after
// the capability, context, reachability and content-editing gates.
func (s *Service) runRestWrite(ctx context.Context, auth AuthorizedRequest, eng *abilityEngine, site abilitySite, e *sqlc.AbilityCatalogue, host string, input []byte) (string, error) {
	routes, err := eng.offeredRoutes(ctx, site.p, "write")
	if err != nil {
		return "", err
	}
	rt, ok := pickRestRoute(routes, input)
	if !ok {
		return "", argRefusal(reasonInvalidArguments, "input", "", msgAbilityRestRoute, restInputSchema)
	}
	in, ok := checkRestInput(input, rt.row)
	if !ok {
		return "", argRefusal(reasonInvalidArguments, "input", "", msgAbilityRestInput, restInputSchema)
	}
	if d := eng.readLimit.allow(auth.GrantID, site.row.ID); !d.allowed {
		r := refuse(reasonRequestRateLimited, domain.RateLimited(ErrCodeRequestLimited, msgAbilityReadLimited).
			WithDetails(map[string]any{
				"limit_scope":         d.scope,
				"retry_after_seconds": int(d.retryAfter / time.Second),
				"retryable":           true,
			}))
		r.logOnly = true
		return "", r
	}
	entryBytes, entrySum, err := eng.entry(*e)
	if err != nil || e.EntrySha256 == nil || *e.EntrySha256 != entrySum {
		return "", notRunnableRefusal(notRunnableNotAdmitted)
	}
	callCtx, cancel := context.WithTimeout(ctx, abilityRunTimeout)
	resp, err := eng.agent.AbilityRun(callCtx, site.row.ID, site.row.Url, agentcmd.AbilityRunCall{
		Mode: agentcmd.AbilityRunModePrecheck, RequestID: uuid.New(),
		Entry: entryBytes, EntrySHA256: entrySum, Input: input, Route: rt.bytes, RouteSHA256: rt.sum,
	})
	cancel()
	if err != nil {
		var refusal *agentcmd.AbilityRunRefusal
		if errors.As(err, &refusal) {
			if refusal.Code == notRunnableEditingNotEnabled {
				return "", &toolRefusal{
					reason: reasonAbilityPrecheckRefused,
					err: domain.Conflict(ErrCodeInvalidToolArguments, msgAbilityEditingOff).
						WithDetails(map[string]any{"code": refusal.Code, "retryable": false}),
					meta: map[string]any{"code": refusal.Code},
				}
			}
			r := vendorRefusal(refusal)
			r.reason = reasonAbilityPrecheckRefused
			if hint := restPrecheckHint(refusal.Code); hint != "" && r.err.Details != nil {
				r.err.Details["hint"] = hint
			}
			return "", r
		}
		return "", refuse(reasonSiteUnreachable, domain.Unavailable(ErrCodeSiteUnreachable,
			msgSiteUnreachable).WithDetails(map[string]any{"retryable": true}))
	}
	pc, ok := verifyRestWritePrecheck(resp, entrySum, *rt, input, in)
	if !ok {
		return "", &toolRefusal{
			reason: reasonAbilityPrecheckRefused,
			err: domain.Unavailable(ErrCodeSiteUnreachable, msgAbilitySiteAnswer).
				WithDetails(map[string]any{"retryable": false}),
			meta: map[string]any{"code": "precheck_unverifiable"},
		}
	}
	card, err := json.Marshal(buildRestCardFacts(rt.row, pc))
	if err != nil {
		return "", fmt.Errorf("encode card facts: %w", err)
	}
	res, err := s.createRestWriteRequest(ctx, eng.writes, auth, site.row, host, e, entrySum, *rt, input, card, pc)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(res)
	if err != nil {
		return "", fmt.Errorf("encode ability request result: %w", err)
	}
	return string(b), nil
}

// restPrecheckHint is our fixed hint for an input-related refusal.
func restPrecheckHint(code string) string {
	switch code {
	case "bad_input", "route_param_invalid", "route_key_not_allowed", "route_key_forbidden":
		return hintRestInput
	case "sanitiser_changed_value":
		return hintSanitiserChanged
	case "post_not_editable":
		return hintPostNotEditable
	}
	return ""
}

// createRestWriteRequest is step 8 for rest-write, in one connection-scoped
// transaction: the same lock, expiry, caps and one-pending dedupe as
// createAbilityRequest, keyed on the target post.
func (s *Service) createRestWriteRequest(ctx context.Context, store AbilityRequestStore, auth AuthorizedRequest, row sqlc.Site, host string, e *sqlc.AbilityCatalogue, entrySum string, rt sendableRoute, input, card []byte, pc checkedRestPrecheck) (abilityCreatedResult, error) {
	var out abilityCreatedResult
	targetKey := fmt.Sprintf("post:%d", pc.target.ID)
	targetID := pc.target.ID
	err := store.RunAbilityRequestTx(ctx, connectionScopedPrincipal(auth), func(tx pgx.Tx, q abilityRequestQueries) error {
		if err := q.TakeAssistantRequestXactLock(ctx, sqlc.TakeAssistantRequestXactLockParams{
			LockKey: grantRequestLockKey, LockID: auth.GrantID.String(),
		}); err != nil {
			return fmt.Errorf("take the connection's request lock: %w", err)
		}
		expired, err := q.ExpireLapsedPendingAbilityRequestsForGrantSite(ctx, sqlc.ExpireLapsedPendingAbilityRequestsForGrantSiteParams{
			TenantID: auth.TenantID, SiteID: row.ID, ProposedByGrantID: auth.GrantID,
		})
		if err != nil {
			return fmt.Errorf("expire lapsed ability requests: %w", err)
		}
		readExisting := func() (sqlc.AssistantAbilityRequest, bool, error) {
			ex, err := q.GetPendingAbilityRequestForTarget(ctx, sqlc.GetPendingAbilityRequestForTargetParams{
				TenantID: auth.TenantID, SiteID: row.ID, ProposedByGrantID: auth.GrantID,
				AbilityName: e.Name, TargetKey: &targetKey,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return ex, false, nil
			}
			return ex, err == nil, err
		}
		if capRef, err := checkAbilityRequestCaps(ctx, q, auth, row.ID, e.Name); err != nil {
			return err
		} else if capRef != nil {
			ex, found, err := readExisting()
			if err != nil {
				return fmt.Errorf("read the waiting ability request: %w", err)
			}
			if !found {
				return capRef
			}
			out = abilityResultFromRow(ex, true)
			return s.recordAbilityCreation(ctx, tx, auth, expired, out, *e.OperatorPermission)
		}
		f, err := buildRestWriteRequestFacts(auth, row, host, e, entrySum, rt, input, card, pc, s.now())
		if err != nil {
			return err
		}
		postType := pc.target.PostType
		routeID, routeSum := rt.row.RouteID, rt.sum
		done := false
		for attempt := 0; attempt < 2 && !done; attempt++ {
			ins, err := q.InsertAbilityRequest(ctx, sqlc.InsertAbilityRequestParams{
				TenantID: auth.TenantID, SiteID: row.ID, ProposedByGrantID: auth.GrantID,
				EntryID: e.EntryID, EntrySha256: entrySum, AbilityName: e.Name,
				OperatorPermission: *e.OperatorPermission,
				InputJson:          string(input), InputSha256: sha256Hex(input), TargetPostID: &targetID,
				PrecheckDigest: pc.precheckDigest, BaseFingerprint: pc.baseFingerprint,
				SiteLabel: f.siteLabel, SiteHost: f.siteHost, GrantLabel: f.grantLabel, GrantVia: f.grantVia,
				SetupClient: f.setupClient, TitleExcerpt: &f.titleExcerpt, PostType: &postType,
				EffectCopy: e.EffectCopy, Snapshot: e.Snapshot, CardCopyVersion: AbilityCardCopyVersion,
				DigestNonce: f.nonce, PresentedDigest: f.digest, ExpiresAt: f.expiresAt,
				RouteID: &routeID, RouteSha256: &routeSum, CardFacts: card,
			})
			if err == nil {
				out = abilityResultFromRow(ins, false)
				done = true
				break
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("insert the ability request: %w", err)
			}
			ex, found, err := readExisting()
			if err != nil {
				return fmt.Errorf("read the waiting ability request: %w", err)
			}
			if !found {
				continue
			}
			out = abilityResultFromRow(ex, true)
			done = true
		}
		if !done {
			return errors.New("the ability insert conflicted twice and the waiting row was gone both times")
		}
		return s.recordAbilityCreation(ctx, tx, auth, expired, out, *e.OperatorPermission)
	})
	if err != nil {
		return abilityCreatedResult{}, err
	}
	return out, nil
}
