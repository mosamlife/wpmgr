package mcp

// The write branch of site_ability_run (engine v4 §1.4 steps 5, 7, 8, 9 and
// amendments W1, W2) and the real site_ability_request_status (§1.5).
//
// A write entry never runs here. It is prechecked on the site, then stored as
// a request a person approves; the dispatch worker (internal/abilityrequest)
// sends it. The creation transaction is Track A's: the grant lock, the
// connection's own lapsed rows expired, the caps, the insert with the dedupe
// read on conflict, and mcp.tool.called last, in the same transaction (R1).

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/humantext"
)

// AbilityPageCreate is the one write entry E2 runs.
const AbilityPageCreate = "wpmgr/page-create"

// AbilityCardCopyVersion names the wording of the ability approval card a
// request's digest covers. internal/abilityrequest records it on approval.
const AbilityCardCopyVersion int32 = 1

// AbilityCardCopyVersionLayout is the card wording of a wpmgr/page-create
// request whose outline holds a block beyond heading, paragraph and list:
// the layout outline, with the images' facts from card_facts.
const AbilityCardCopyVersionLayout int32 = 2

// Limits of the ability creation rail (engine v4 §1.4 step 8).
const (
	maxPendingAbilityPerConnection   = 10
	maxCreatedAbilityPerConnection   = 30
	maxAbilityRequestsPerSiteHour    = 12
	maxPendingCreationsPerConnSite   = 5
	abilityRequestWindow             = 24 * time.Hour
	abilityStatusListLimit           = 20
	abilityTitleExcerptRunes         = 200
	abilityRequestReviewPathTemplate = "/sites/%s/content"

	limitScopeAbilityPending      = "pending_ability_requests"
	limitScopeAbilityGrantDaily   = "ability_grant_daily"
	limitScopeAbilitySiteHourly   = "ability_site_hourly"
	limitScopeAbilityCreationSite = "pending_creations_for_site"
	notRunnableCapabilityNotHeld  = "capability_not_held"
	notRunnableEditingNotEnabled  = "content_editing_not_enabled"
)

const (
	msgAbilityCreated = "Nothing has changed yet. A person must approve this in WPMgr. Review it on " +
		"the site's Content tab in WPMgr. Call site_ability_request_status with this request_id to " +
		"learn the outcome."
	msgAbilityLimited = "This connection has reached a limit on ability requests. Nothing was asked. " +
		"Wait retry_after_seconds before asking again."
	msgAbilitySiteAnswer    = "the site's answer could not be checked, so nothing was asked"
	msgAbilityEditingOff    = "Content editing is not enabled on this site. A person must enable it in WPMgr (the site's Content tab) before the AI can create pages. Nothing was asked."
	msgAbilityWriteOutdated = "This site's WPMgr agent is too old to create pages. The agent must be updated to " +
		agentcmd.MinAgentVersionForPageCreate + " or later. Nothing was asked."
	msgAbilityLayoutOutdated = "Layout blocks need the WPMgr plugin " + agentcmd.MinAgentVersionForPageLayout +
		" or later on this site. An outline of headings, paragraphs and lists works now."
	msgAbilityLayoutInput = "the outline does not follow the page layout rules, so nothing was asked"
)

const reasonAbilityPrecheckRefused refusalReason = "ability_precheck_refused"

var (
	hex64Pattern       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	fingerprintPattern = regexp.MustCompile(`^[!-~]{1,256}$`)
)

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

// abilityRequestQueries is every statement the ability creation transaction
// runs, as sqlc generated them.
type abilityRequestQueries interface {
	TakeAssistantRequestXactLock(ctx context.Context, arg sqlc.TakeAssistantRequestXactLockParams) error
	ExpireLapsedPendingAbilityRequestsForGrantSite(ctx context.Context, arg sqlc.ExpireLapsedPendingAbilityRequestsForGrantSiteParams) ([]uuid.UUID, error)
	CountLivePendingAbilityRequestsForGrant(ctx context.Context, arg sqlc.CountLivePendingAbilityRequestsForGrantParams) (int64, error)
	CountLivePendingAbilityRequestsForGrantSite(ctx context.Context, arg sqlc.CountLivePendingAbilityRequestsForGrantSiteParams) (int64, error)
	CountAbilityRequestsForGrantSince(ctx context.Context, arg sqlc.CountAbilityRequestsForGrantSinceParams) (int64, error)
	CountAbilityRequestsOnSiteSince(ctx context.Context, arg sqlc.CountAbilityRequestsOnSiteSinceParams) (int64, error)
	InsertAbilityRequest(ctx context.Context, arg sqlc.InsertAbilityRequestParams) (sqlc.AssistantAbilityRequest, error)
	GetPendingAbilityRequestForTarget(ctx context.Context, arg sqlc.GetPendingAbilityRequestForTargetParams) (sqlc.AssistantAbilityRequest, error)
}

var _ abilityRequestQueries = (*sqlc.Queries)(nil)

// AbilityStatusRow is the status tool's narrow projection. It holds no
// digest, nonce, input, grant label, decider or site-reported text.
type AbilityStatusRow = sqlc.GetAbilityRequestStatusForGrantRow

// AbilityRequestStore is the ability write rail's database surface. *Repo
// implements it; every method runs connection-scoped (runConnectionTx).
type AbilityRequestStore interface {
	RunAbilityRequestTx(ctx context.Context, principal domain.Principal, fn func(tx pgx.Tx, q abilityRequestQueries) error) error
	ReadAbilityRequestStatus(ctx context.Context, principal domain.Principal, grantID, requestID uuid.UUID) (AbilityStatusRow, bool, error)
	ListOpenAbilityRequestStatus(ctx context.Context, principal domain.Principal, grantID uuid.UUID, limit int32) ([]AbilityStatusRow, error)
}

var _ AbilityRequestStore = (*Repo)(nil)

// EnableAbilityWrites wires the write branch. Without it a write entry is
// refused as writes_not_available. The server-wide write switch
// (SetWriteToolsEnabled) must also be on.
func (s *Service) EnableAbilityWrites(store AbilityRequestStore) error {
	if s.abilities == nil {
		return fmt.Errorf("%w: ability tools are not enabled", ErrAbilityToolsUnavailable)
	}
	if store == nil {
		return fmt.Errorf("%w: no ability request store", ErrAbilityToolsUnavailable)
	}
	s.abilities.writes = store
	return nil
}

// ---------------------------------------------------------------------------
// Entry checks
// ---------------------------------------------------------------------------

// ownWriteAbilities are the write entries this control plane can run.
var ownWriteAbilities = map[string]struct{}{AbilityPageCreate: {}, AbilityRestWrite: {}, AbilityPageEdit: {}}

// writeAgentFloor is the first agent release that runs a write entry.
func writeAgentFloor(name string) string {
	switch name {
	case AbilityRestWrite:
		return agentcmd.MinAgentVersionForRestCall
	case AbilityPageEdit:
		return agentcmd.MinAgentVersionForBuilderEdit
	}
	return agentcmd.MinAgentVersionForPageCreate
}

// gateWriteCapability marks every write entry not runnable, with reason
// capability_not_held, for a connection without CapAbilityRequest, so
// discover and describe say what run would answer (v4 §1.4).
func gateWriteCapability(all []classified, auth AuthorizedRequest) {
	if auth.Capabilities.Allows(CapAbilityRequest) {
		return
	}
	for i := range all {
		if all[i].entry != nil && all[i].entry.Class == "write" {
			all[i].runnable = false
			all[i].reason = abilityStrPtr(notRunnableCapabilityNotHeld)
		}
	}
}

// writeEntryRunnable is classify's write arm: the reason a reviewed,
// admitted, enabled wpmgr write entry cannot run on this site, or "".
func writeEntryRunnable(e *sqlc.AbilityCatalogue, inv *sqlc.SiteAbilityInventory, agentVersion string) string {
	if _, ok := ownWriteAbilities[e.Name]; !ok {
		return notRunnableWritesOff
	}
	if e.ApprovalMode != "per_call" || e.Snapshot == "none" || e.OperatorPermission == nil ||
		e.EntrySha256 == nil {
		// Not admissible as a write (v4 §4.4), or not yet stamped, so no
		// request made against it could ever be dispatched (W1).
		return notRunnableNotAdmitted
	}
	if inv == nil {
		return notRunnableNotOnSite
	}
	if inv.OwnerOk != nil && !*inv.OwnerOk {
		return notRunnableOwnerMismatch
	}
	if !abilityAgentMeetsFloor(agentVersion, e.MinAgentVersion) ||
		!abilityAgentMeetsFloor(agentVersion, abilityStrPtr(writeAgentFloor(e.Name))) {
		return notRunnableAgentOutdated
	}
	return ""
}

// ---------------------------------------------------------------------------
// Input: wpmgr/page-create. The grammar lives in page_create_input.go; these
// are the refusals the write branch gives for it before anything reaches the
// site.
// ---------------------------------------------------------------------------

// pageCreateInputRefusal refuses an input the grammar refused. A schema
// mismatch carries the schema; a layout, link or editor problem carries the
// agent's code and our fixed hint for it (never the site's words).
func pageCreateInputRefusal(code string) *toolRefusal {
	if code == pageCreateBadInput {
		return argRefusal(reasonInvalidArguments, "input", "", msgAbilityArgInput, pageCreateInputSchema)
	}
	return &toolRefusal{
		reason: reasonInvalidArguments,
		err: domain.Validation(ErrCodeInvalidToolArguments, msgAbilityLayoutInput).WithDetails(map[string]any{
			"argument": "input", "code": code, "hint": precheckRefusalHint(code), "retryable": false,
		}),
		meta: map[string]any{"argument": "input", "code": code},
	}
}

// ---------------------------------------------------------------------------
// Digests the control plane recomputes from the site's answer
// ---------------------------------------------------------------------------

// phpJSONString encodes s exactly as PHP's json_encode does with no flags:
// "/" escaped, every non-ASCII code point as lowercase \uXXXX (UTF-16), the
// short escapes for \b \f \n \r \t, other controls as \u00XX. ok is false
// for invalid UTF-8, where PHP's json_encode fails.
func phpJSONString(s string) (string, bool) {
	if !utf8.ValidString(s) {
		return "", false
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '/':
			b.WriteString(`\/`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			switch {
			case r < 0x20:
				fmt.Fprintf(&b, `\u%04x`, r)
			case r < 0x80:
				b.WriteRune(r)
			case r < 0x10000:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				r -= 0x10000
				fmt.Fprintf(&b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
			}
		}
	}
	b.WriteByte('"')
	return b.String(), true
}

// phpJSONStringArray is json_encode of a list of strings.
func phpJSONStringArray(items ...string) ([]byte, bool) {
	parts := make([]string, len(items))
	for i, it := range items {
		enc, ok := phpJSONString(it)
		if !ok {
			return nil, false
		}
		parts[i] = enc
	}
	return []byte("[" + strings.Join(parts, ",") + "]"), true
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// pagePreview is precheck's preview member. Media is present exactly when
// the outline has an image (an answer for a text-only outline is the same
// bytes as before images existed).
type pagePreview struct {
	PostType string          `json:"post_type"`
	Editor   string          `json:"editor"`
	Status   string          `json:"status"`
	Title    string          `json:"title"`
	Content  string          `json:"content"`
	Media    json.RawMessage `json:"media,omitempty"`
}

// checkedPrecheck is a precheck answer the control plane verified.
type checkedPrecheck struct {
	precheckDigest  string
	previewDigest   string
	baseFingerprint string
	preview         pagePreview
	// media are the verified image facts, in the outline's media order.
	media []pageMediaFact
	// builder is what the card shows of the page builder that builds the
	// page (verifyBuilderPageCreatePrecheck); nil for a WordPress editor.
	builder *PageCardBuilder
}

// precheckDigestsWellFormed: the answer says valid and its digests have
// their shapes.
func precheckDigestsWellFormed(resp agentcmd.AbilityRunResponse) bool {
	return resp.Valid && hex64Pattern.MatchString(resp.PrecheckDigest) &&
		hex64Pattern.MatchString(resp.PreviewDigest) && fingerprintPattern.MatchString(resp.BaseFingerprint)
}

// precheckDigestMatches: the precheck digest is the one the entry, the
// input we hold, the base fingerprint and the preview digest give.
func precheckDigestMatches(resp agentcmd.AbilityRunResponse, entrySum string, input []byte) bool {
	pre, _ := phpJSONStringArray(entrySum, sha256Hex(input), resp.BaseFingerprint, resp.PreviewDigest)
	return sha256Hex(pre) == resp.PrecheckDigest
}

// verifyPreviewMedia reads preview.media: absent when the outline has no
// image, else the facts of the outline's attachment ids in order, each
// usable.
func verifyPreviewMedia(raw json.RawMessage, f pageCreateFacts) ([]pageMediaFact, bool) {
	if len(f.mediaIDs) == 0 {
		return nil, raw == nil
	}
	media, ok := parsePageMedia(raw)
	if !ok || len(media) != len(f.mediaIDs) {
		return nil, false
	}
	for i := range media {
		if media[i].ID != f.mediaIDs[i] {
			return nil, false
		}
	}
	return media, true
}

// verifyPageCreatePrecheck checks the site's precheck answer against what
// this control plane sent: the digests are recomputed from bytes we hold
// (R2), so a precheck for other input, another entry or another preview is
// refused before anything is stored. With images, the facts must name the
// outline's attachment ids in order, each fact must be usable, the base
// fingerprint must be the one those facts give (so the facts the card shows
// are the facts the write re-checks), and the content's image tags must carry
// exactly the facts' addresses. A page built with a page builder is checked
// by verifyBuilderPageCreatePrecheck, never here.
func verifyPageCreatePrecheck(resp agentcmd.AbilityRunResponse, entrySum string, input []byte, f pageCreateFacts) (checkedPrecheck, bool) {
	if f.builder != "" || !precheckDigestsWellFormed(resp) {
		return checkedPrecheck{}, false
	}
	var pv pagePreview
	dec := json.NewDecoder(bytes.NewReader(resp.Preview))
	dec.DisallowUnknownFields()
	if len(resp.Preview) == 0 || dec.Decode(&pv) != nil {
		return checkedPrecheck{}, false
	}
	if pv.PostType != f.postType || pv.Editor != f.editor || pv.Status != "draft" || pv.Title != f.title {
		return checkedPrecheck{}, false
	}
	media, ok := verifyPreviewMedia(pv.Media, f)
	if !ok {
		return checkedPrecheck{}, false
	}
	if base, ok := pageCreateBaseFingerprint(f.postType, media); !ok || base != resp.BaseFingerprint {
		return checkedPrecheck{}, false
	}
	if !pageContentImagesMatch(pv.Content, media) {
		return checkedPrecheck{}, false
	}
	prev, ok := phpJSONStringArray(pv.Editor, pv.PostType, "draft", pv.Title, pv.Content)
	if !ok || sha256Hex(prev) != resp.PreviewDigest {
		return checkedPrecheck{}, false
	}
	if !precheckDigestMatches(resp, entrySum, input) {
		return checkedPrecheck{}, false
	}
	return checkedPrecheck{
		precheckDigest: resp.PrecheckDigest, previewDigest: resp.PreviewDigest,
		baseFingerprint: resp.BaseFingerprint, preview: pv, media: media,
	}, true
}

// ---------------------------------------------------------------------------
// The write branch
// ---------------------------------------------------------------------------

// abilityCreatedResult is the success answer.
type abilityCreatedResult struct {
	RequestID        string `json:"request_id"`
	State            string `json:"state"`
	Existing         bool   `json:"existing"`
	SiteID           string `json:"site_id"`
	Name             string `json:"name"`
	Approval         string `json:"approval"`
	Undo             string `json:"undo"`
	ExpiresAt        string `json:"expires_at"`
	PollAfterSeconds int    `json:"poll_after_seconds"`
	ReviewPath       string `json:"review_path"`
	Message          string `json:"message"`
}

func abilityLimitRefusal(reason refusalReason, scope string, retryAfter int) *toolRefusal {
	return &toolRefusal{
		reason: reason,
		err: domain.RateLimited(ErrCodeRequestLimited, msgAbilityLimited).WithDetails(map[string]any{
			"limit_scope": scope, "retry_after_seconds": retryAfter, "retryable": true,
		}),
	}
}

// runSiteAbilityWrite is the write branch, entered from runSiteAbility after
// the strict decode, the site fence and the catalogue lookup.
func (s *Service) runSiteAbilityWrite(ctx context.Context, auth AuthorizedRequest, eng *abilityEngine, site abilitySite, c *classified, input []byte) (string, error) {
	// The capability, before anything reaches the site (W2).
	if !auth.Capabilities.Allows(CapAbilityRequest) {
		return "", notRunnableRefusal(notRunnableCapabilityNotHeld)
	}
	if c.reason != nil {
		if *c.reason == notRunnableAgentOutdated && c.name == AbilityPageEdit {
			return "", builderEditOutdatedRefusal()
		}
		if *c.reason == notRunnableAgentOutdated {
			msg := msgAbilityWriteOutdated
			if c.name == AbilityRestWrite {
				msg = msgAbilityRestOutdated
			}
			return "", refuse(reasonAgentOutdated, domain.Conflict(ErrCodeSiteAgentOutdated,
				msg).WithDetails(map[string]any{
				"min_agent_version": writeAgentFloor(c.name), "retryable": false,
			}))
		}
		return "", notRunnableRefusal(*c.reason)
	}
	e := c.entry
	if eng.writes == nil || !s.WriteToolsEnabled() {
		return "", notRunnableRefusal(notRunnableWritesOff)
	}
	matched, forbidden, err := s.ForbiddenByContext(ctx, auth.TenantID, site.row.ID, ToolSiteAbilityRun)
	if err != nil {
		return "", refuse(reasonContextUnavailable, domain.Internal(ErrCodeContextUnavailable,
			"this site's governed context cannot be resolved").WithCause(err))
	}
	if forbidden {
		r := refuse(reasonForbiddenByContext, domain.Forbidden(ErrCodeToolForbiddenByContext,
			msgForbiddenByContext).WithDetails(map[string]any{"retryable": false}))
		r.meta = map[string]any{"matched_entry": matched}
		return "", r
	}
	if !agentConnectedEnough(site.row.ConnectionState) || eng.agent == nil {
		return "", refuse(reasonSiteUnreachable, domain.Unavailable(ErrCodeSiteUnreachable,
			msgSiteUnreachable).WithDetails(map[string]any{"retryable": true}))
	}
	self, ok := siteAddressOf(site.row.Url)
	if !ok {
		return "", siteAddressUnusableRefusal()
	}
	// Writes are refused until a person enabled content editing on the site.
	if !site.row.ContentEditingEnabledAt.Valid {
		return "", &toolRefusal{
			reason: reasonAbilityPrecheckRefused,
			err: domain.Conflict(ErrCodeInvalidToolArguments, msgAbilityEditingOff).
				WithDetails(map[string]any{"code": notRunnableEditingNotEnabled, "retryable": false}),
			meta: map[string]any{"code": notRunnableEditingNotEnabled},
		}
	}
	if e.Name == AbilityRestWrite {
		return s.runRestWrite(ctx, auth, eng, site, e, self.host, input)
	}
	if e.Name == AbilityPageEdit {
		return s.runPageEdit(ctx, auth, eng, site, e, self.host, input)
	}
	// Step 5: our grammar, including a layout the classic editor cannot
	// hold. Then the per-input floor: a layout outline needs a newer agent
	// than the entry's own floor, which text-only outlines keep.
	facts, code := validatePageCreateInput(input)
	if code != "" {
		return "", pageCreateInputRefusal(code)
	}
	// A page builder the entry enables, and the builder's node rules.
	if r := pageCreateBuilderRefusal(facts, e.Limits, input); r != nil {
		return "", r
	}
	// (The entry's own floor was checked by classify: c.reason above.)
	if floor := PageCreateAgentFloor(input); floor != agentcmd.MinAgentVersionForPageCreate &&
		!abilityAgentMeetsFloor(site.row.AgentVersion, &floor) {
		msg := msgAbilityLayoutOutdated
		if floor == agentcmd.MinAgentVersionForBuilderAdapters {
			msg = msgAbilityBuilderOutdated
		}
		return "", refuse(reasonAgentOutdated, domain.Conflict(ErrCodeSiteAgentOutdated,
			msg).WithDetails(map[string]any{
			"min_agent_version": floor, "retryable": false,
		}))
	}
	// The precheck counts against the read limits (W2).
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
	// Step 7: precheck, synchronously. Nothing is persisted on a refusal. A
	// page builder's node ids derive from the request id, so the tree is
	// checked against this call's id.
	precheckID := uuid.New()
	callCtx, cancel := context.WithTimeout(ctx, abilityRunTimeout)
	resp, err := eng.agent.AbilityRun(callCtx, site.row.ID, site.row.Url, agentcmd.AbilityRunCall{
		Mode: agentcmd.AbilityRunModePrecheck, RequestID: precheckID,
		Entry: entryBytes, EntrySHA256: entrySum, Input: input,
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
			details := map[string]any{"code": refusal.Code, "retryable": refusal.Retryable}
			if hint := precheckRefusalHint(refusal.Code); hint != "" {
				details["hint"] = hint
			}
			return "", &toolRefusal{
				reason: reasonAbilityPrecheckRefused,
				err:    domain.Validation(ErrCodeInvalidToolArguments, msgAbilityAgentRefused).WithDetails(details),
				meta:   map[string]any{"code": refusal.Code},
			}
		}
		return "", refuse(reasonSiteUnreachable, domain.Unavailable(ErrCodeSiteUnreachable,
			msgSiteUnreachable).WithDetails(map[string]any{"retryable": true}))
	}
	var checked checkedPrecheck
	if facts.builder != "" {
		checked, ok = verifyBuilderPageCreatePrecheck(resp, entrySum, input, facts, precheckID.String())
	} else {
		checked, ok = verifyPageCreatePrecheck(resp, entrySum, input, facts)
	}
	if !ok {
		return "", &toolRefusal{
			reason: reasonAbilityPrecheckRefused,
			err: domain.Unavailable(ErrCodeSiteUnreachable, msgAbilitySiteAnswer).
				WithDetails(map[string]any{"retryable": false}),
			meta: map[string]any{"code": "precheck_unverifiable"},
		}
	}
	// Step 8: the creation transaction, which stores the request under the
	// id its precheck was sent with.
	res, err := s.createAbilityRequest(ctx, eng.writes, auth, site.row, self.host, e, entrySum, input, facts, checked, precheckID)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(res)
	if err != nil {
		return "", fmt.Errorf("encode ability request result: %w", err)
	}
	return string(b), nil
}

// Fixed hints for a precheck refused over its input (GH #823). The site's
// own detail text is untrusted and never reaches the AI; these are ours, and
// they describe the rules the page-create builder enforces on every text
// value.
const (
	hintCreateContentInvalid = "Write the title and every outline text as plain text: headings, paragraphs, list items, " +
		"alt text, captions, quotes and citations, button text and table cells. " +
		"Use parentheses instead of square brackets (only a bracketed number such as [1] is allowed, and never in alt text). " +
		"No HTML, shortcodes, comments or template syntax: none of < > {{ }} {% %} or backticks. " +
		"No line breaks, control characters or invisible formatting characters; put each paragraph in its own outline node. " +
		"No empty text, except that alt text and a table cell may be empty. A title is at most 200 characters, " +
		"each text at most 5000, a caption 500, a citation 200, button text 80, a table cell 500 and alt text 300, " +
		"the whole page at most 60000. " +
		"Alt text may not contain a character reference such as &amp; or &#47; (an & then a name or a number and a ;): " +
		"write the character itself, or put a space after the &."
	hintBadInput = "The input does not match the ability's schema. Call site_ability_describe and send only the fields it lists, " +
		"with the types it gives."
	hintSanitiserChanged = "This site would alter the content when saving it. Remove anything that looks like markup, " +
		"shortcodes, entities or special characters, and write plain sentences."
	hintLayoutInvalid = "Fix the page layout. A top-level item can be any block, a group (a section) or columns; " +
		"a group holds blocks or columns; a column holds blocks only. Columns hold 2 to 4 columns, and a group or a column " +
		"holds 1 to 50 blocks. Widths are optional: one whole number from 10 to 90 per column, adding up to 100. " +
		"A page holds at most 400 blocks, 20 images, 12 buttons (1 to 3 in each buttons block) and 10 tables. " +
		"A table has 1 to 50 rows of 1 to 6 cells, with every row and the header the same width. A quote holds 1 to 10 paragraphs."
	hintLinkInvalid = "Write each button link as an https:// address with a lowercase https, a host name such as " +
		"example.com and no user name or password, or as a path on this site that starts with a single /. " +
		"Use only ASCII letters, digits and - . _ ~ / ? # ! $ ( ) * + , ; = % @, write every % as % and two hex digits, " +
		"and keep the link to 2048 characters. " +
		"A colon may appear only in an https:// address: a path on this site may not contain : or &# anywhere, " +
		"so write a colon in a path as %3A. " +
		"An & may appear only where it does not start a character reference: a link or an image address may not " +
		"contain a character reference such as &amp; or &#47; (an & then a name or a number and a ;), " +
		"so write the character itself."
	hintImageNotAvailable = "An image the outline names is not one WPMgr may use: it must be a JPEG, PNG, GIF, WebP or AVIF " +
		"in this site's media library that is not attached to a draft, private or password-protected page. " +
		"Find another attachment id with wpmgr/rest-read route wp-v2-media-list."
	hintImageURLUnusable = "WordPress gave an address for one of the images that WPMgr cannot use. " +
		"Pick another image with wpmgr/rest-read route wp-v2-media-list."
	hintLayoutNeedsBlockEditor = "The classic editor cannot hold groups, columns, buttons, spacers or image captions. " +
		"With editor wordpress_classic, use only headings, paragraphs, lists, quotes, tables, separators and images " +
		"without captions. Send editor wordpress_blocks only for a site that uses the block editor."
)

// precheckRefusalHints are the input-related refusal codes and their fixed
// hints. Every key is an agent refusal code, so it is in
// agentcmd.AbilityRunRefusalCodes: a code outside that set arrives as
// unknown and its hint could never be given.
var precheckRefusalHints = map[string]string{
	"create_content_invalid":        hintCreateContentInvalid,
	pageCreateBadInput:              hintBadInput,
	"sanitiser_changed_new_content": hintSanitiserChanged,
	pageCreateLayoutInvalid:         hintLayoutInvalid,
	pageCreateLinkInvalid:           hintLinkInvalid,
	"image_not_available":           hintImageNotAvailable,
	"image_url_unusable":            hintImageURLUnusable,
	pageCreateNeedsBlockEditor:      hintLayoutNeedsBlockEditor,
	pageBuilderNotEnabled:           hintBuilderNotEnabled,
	pageBuilderNotAvailable:         hintBuilderNotAvailable,
	pageNodeNotSupported:            hintNodeNotSupportedByBuilder,
	pageImageAltFromLibrary:         hintImageAltFromLibrary,
	pageEditOpsInvalid:              hintPageEditOpsInvalid,
	pageEditNodeNotFound:            hintPageEditNodeNotFound,
	pageEditNodeNotEditable:         hintPageEditNodeNotEditable,
	pageEditOpNotSupported:          hintPageEditOpNotSupported,
	pageEditPageTooLarge:            hintPageEditPageTooLarge,
}

// precheckRefusalHint maps an input-related refusal code to a fixed hint,
// or "" for a code that is not about the input.
func precheckRefusalHint(code string) string {
	return precheckRefusalHints[code]
}

// abilityRequestFacts are the stored facts a request's digest covers.
type abilityRequestFacts struct {
	siteLabel    string
	siteHost     string
	grantLabel   string
	grantVia     string
	setupClient  *string
	titleExcerpt string
	nonce        string
	digest       string
	expiresAt    time.Time
	// card is the stored card_facts (nil when the request has none) and
	// copyVersion the card wording; the digest covers both.
	card        []byte
	copyVersion int32
}

// buildAbilityRequestFacts computes the card facts and presented_digest over
// them, the nonce and the card copy version (engine v4 §4.1). A page with
// images stores its images' facts as card_facts; an outline beyond heading,
// paragraph and list is card copy version 2.
func buildAbilityRequestFacts(auth AuthorizedRequest, row sqlc.Site, host string, e *sqlc.AbilityCatalogue, entrySum string, input []byte, facts pageCreateFacts, pc checkedPrecheck, now time.Time) (abilityRequestFacts, error) {
	f := abilityRequestFacts{
		siteLabel:    humantext.CapRunes(humantext.Clean(row.Name), siteLabelRunes),
		siteHost:     host,
		grantLabel:   humantext.CapRunes(humantext.Clean(auth.GrantName), grantLabelRunes),
		grantVia:     grantViaToken,
		setupClient:  auth.SetupClient,
		titleExcerpt: humantext.CapRunes(humantext.Clean(facts.title), abilityTitleExcerptRunes),
		expiresAt:    now.UTC().Add(abilityRequestWindow).Truncate(time.Microsecond),
		copyVersion:  AbilityCardCopyVersion,
	}
	if facts.usesLayout {
		f.copyVersion = AbilityCardCopyVersionLayout
	}
	card, err := pageCardFactsJSON(pc.media, pc.builder)
	if err != nil {
		return abilityRequestFacts{}, err
	}
	f.card = card
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
	f.digest = pageCreateDigest(auth, row, e, entrySum, input, facts, pc, f)
	return f, nil
}

// pageCreateDigest is the presented_digest over a page-create request's
// facts: the card copy version and the exact card_facts bytes (JSON null
// without a card) with everything else the card shows or the write binds.
func pageCreateDigest(auth AuthorizedRequest, row sqlc.Site, e *sqlc.AbilityCatalogue, entrySum string, input []byte, facts pageCreateFacts, pc checkedPrecheck, f abilityRequestFacts) string {
	var cardText any
	if f.card != nil {
		cardText = string(f.card)
	}
	canonical := map[string]any{
		"copy_version":     f.copyVersion,
		"mode":             "write",
		"ability_name":     e.Name,
		"entry_id":         e.EntryID.String(),
		"entry_sha256":     entrySum,
		"operator_perm":    *e.OperatorPermission,
		"input_json":       string(input),
		"input_sha256":     sha256Hex(input),
		"target":           "new",
		"title_excerpt":    f.titleExcerpt,
		"card_facts":       cardText,
		"editor":           facts.editor,
		"post_type":        facts.postType,
		"precheck_digest":  pc.precheckDigest,
		"preview_digest":   pc.previewDigest,
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
	b, _ := json.Marshal(canonical) // strings, an int, a string pointer and nil only
	return sha256Hex(b)
}

// createAbilityRequest is step 8, in one connection-scoped transaction. A
// new request is stored under requestID, the id its precheck was sent with:
// the dispatch worker sends the write under the stored id, so the write the
// site receives names the request the site prechecked, and a page builder's
// node ids, which derive from that id, are the ones the card showed. A
// repeat of a request already waiting answers the waiting row, under its own
// id.
func (s *Service) createAbilityRequest(ctx context.Context, store AbilityRequestStore, auth AuthorizedRequest, row sqlc.Site, host string, e *sqlc.AbilityCatalogue, entrySum string, input []byte, facts pageCreateFacts, pc checkedPrecheck, requestID uuid.UUID) (abilityCreatedResult, error) {
	var out abilityCreatedResult
	inputSum := sha256Hex(input)
	targetKey := "new:" + inputSum
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
			// A repeat of the request already waiting is not a new request.
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
		f, err := buildAbilityRequestFacts(auth, row, host, e, entrySum, input, facts, pc, s.now())
		if err != nil {
			return err
		}
		editor, postType, preview := facts.editor, facts.postType, pc.previewDigest
		done := false
		for attempt := 0; attempt < 2 && !done; attempt++ {
			ins, err := q.InsertAbilityRequest(ctx, sqlc.InsertAbilityRequestParams{
				ID:       uuidToPG(requestID),
				TenantID: auth.TenantID, SiteID: row.ID, ProposedByGrantID: auth.GrantID,
				EntryID: e.EntryID, EntrySha256: entrySum, AbilityName: e.Name,
				OperatorPermission: *e.OperatorPermission,
				InputJson:          string(input), InputSha256: inputSum,
				PrecheckDigest: pc.precheckDigest, PreviewDigest: &preview, BaseFingerprint: pc.baseFingerprint,
				SiteLabel: f.siteLabel, SiteHost: f.siteHost, GrantLabel: f.grantLabel, GrantVia: f.grantVia,
				SetupClient: f.setupClient, TitleExcerpt: &f.titleExcerpt, Editor: &editor, PostType: &postType,
				EffectCopy: e.EffectCopy, Snapshot: e.Snapshot, CardCopyVersion: f.copyVersion,
				DigestNonce: f.nonce, PresentedDigest: f.digest, ExpiresAt: f.expiresAt,
				CardFacts: f.card,
			})
			if err == nil {
				out = abilityResultFromRow(ins, false)
				done = true
				break
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("insert the ability request: %w", err)
			}
			// The one-pending index: the same input is already waiting.
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

func checkAbilityRequestCaps(ctx context.Context, q abilityRequestQueries, auth AuthorizedRequest, siteID uuid.UUID, name string) (*toolRefusal, error) {
	pending, err := q.CountLivePendingAbilityRequestsForGrant(ctx, sqlc.CountLivePendingAbilityRequestsForGrantParams{
		TenantID: auth.TenantID, ProposedByGrantID: auth.GrantID,
	})
	if err != nil {
		return nil, fmt.Errorf("count waiting ability requests: %w", err)
	}
	if pending >= maxPendingAbilityPerConnection {
		return abilityLimitRefusal(reasonPendingCap, limitScopeAbilityPending, pendingCapRetrySeconds), nil
	}
	created, err := q.CountAbilityRequestsForGrantSince(ctx, sqlc.CountAbilityRequestsForGrantSinceParams{
		TenantID: auth.TenantID, ProposedByGrantID: auth.GrantID, WindowSeconds: createdWindowSeconds,
	})
	if err != nil {
		return nil, fmt.Errorf("count ability requests in the last day: %w", err)
	}
	if created >= maxCreatedAbilityPerConnection {
		return abilityLimitRefusal(reasonGrantDailyCap, limitScopeAbilityGrantDaily, grantDailyRetrySeconds), nil
	}
	onSite, err := q.CountAbilityRequestsOnSiteSince(ctx, sqlc.CountAbilityRequestsOnSiteSinceParams{
		TenantID: auth.TenantID, SiteID: siteID, WindowSeconds: siteHourWindowSeconds,
	})
	if err != nil {
		return nil, fmt.Errorf("count ability requests on the site: %w", err)
	}
	if onSite >= maxAbilityRequestsPerSiteHour {
		return abilityLimitRefusal(reasonSiteHourlyCap, limitScopeAbilitySiteHourly, siteHourlyRetrySeconds), nil
	}
	creations, err := q.CountLivePendingAbilityRequestsForGrantSite(ctx, sqlc.CountLivePendingAbilityRequestsForGrantSiteParams{
		TenantID: auth.TenantID, SiteID: siteID, ProposedByGrantID: auth.GrantID, AbilityName: &name,
	})
	if err != nil {
		return nil, fmt.Errorf("count waiting creations on the site: %w", err)
	}
	if creations >= maxPendingCreationsPerConnSite {
		return abilityLimitRefusal(reasonPendingCap, limitScopeAbilityCreationSite, pendingCapRetrySeconds), nil
	}
	return nil, nil
}

func abilityResultFromRow(r sqlc.AssistantAbilityRequest, existing bool) abilityCreatedResult {
	return abilityCreatedResult{
		RequestID:        r.ID.String(),
		State:            stateWaitingForApproval,
		Existing:         existing,
		SiteID:           r.SiteID.String(),
		Name:             r.AbilityName,
		Approval:         "per_call",
		Undo:             r.Snapshot,
		ExpiresAt:        r.ExpiresAt.UTC().Format(time.RFC3339),
		PollAfterSeconds: requestPollAfterSeconds,
		ReviewPath:       fmt.Sprintf(abilityRequestReviewPathTemplate, r.SiteID),
		Message:          msgAbilityCreated,
	}
}

// recordAbilityCreation writes the in-line expiries and mcp.tool.called in
// the creation transaction, after the request row (R1), and marks the call so
// the transport does not record it twice.
func (s *Service) recordAbilityCreation(ctx context.Context, tx pgx.Tx, auth AuthorizedRequest, expired []uuid.UUID, res abilityCreatedResult, operatorPermission string) error {
	if err := s.requireRecorder(); err != nil {
		return err
	}
	for _, id := range expired {
		if _, err := s.audit.RecordInTx(ctx, tx, audit.Event{
			TenantID:   auth.TenantID,
			ActorType:  audit.ActorSystem,
			Action:     audit.ActionAbilityRequestExpired,
			TargetType: audit.TargetTypeAssistantAbilityRequest,
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
		TargetID:   ToolSiteAbilityRun,
		Metadata: map[string]any{
			"grant_name":          auth.GrantName,
			"operator_permission": operatorPermission,
			"tool":                ToolSiteAbilityRun,
			"ability":             res.Name,
			"request_id":          res.RequestID,
			"site_id":             res.SiteID,
			"existing":            res.Existing,
		},
	})
	if err == nil {
		markRequestRowRecorded(ctx)
	}
	return auditFailure(err)
}

// ---------------------------------------------------------------------------
// site_ability_request_status (engine v4 §1.5)
// ---------------------------------------------------------------------------

// abilityStatus is one request as the model sees it. Site-origin text is
// fenced; the title excerpt is the AI's own input, cleaned and capped.
type abilityStatus struct {
	RequestID          string  `json:"request_id"`
	SiteID             string  `json:"site_id"`
	SiteName           string  `json:"site_name"`
	Name               string  `json:"name"`
	Title              *string `json:"title"`
	Editor             *string `json:"editor"`
	PostType           *string `json:"post_type"`
	Effect             string  `json:"effect"`
	State              string  `json:"state"`
	CreatedAt          string  `json:"created_at"`
	ExpiresAt          string  `json:"expires_at"`
	DecidedAt          *string `json:"decided_at"`
	WaitingReason      *string `json:"waiting_reason"`
	Outcome            *string `json:"outcome"`
	Code               *string `json:"code"`
	CreatedPostID      *int64  `json:"created_post_id"`
	Restored           *bool   `json:"restored"`
	Trashed            *bool   `json:"trashed"`
	Undo               *string `json:"undo"`
	UndoAvailableUntil *string `json:"undo_available_until"`
}

func tsStringPtr(t pgtype.Timestamptz) *string {
	if !t.Valid {
		return nil
	}
	s := t.Time.UTC().Format(time.RFC3339)
	return &s
}

// abilityStateFor maps the row state onto the §1.5 vocabulary.
func abilityStateFor(r AbilityStatusRow, now time.Time) string {
	switch r.State {
	case "pending":
		if !r.ExpiresAt.After(now) {
			return "expired"
		}
		return stateWaitingForApproval
	case "approved":
		return "approved_not_started"
	case "dispatched":
		return "running"
	case "outcome_unknown":
		if r.Outcome == nil {
			return "running"
		}
		return "done"
	case "done", "failed", "not_sent":
		return "done"
	default: // declined, withdrawn, expired
		return r.State
	}
}

func abilityStatusFromRow(r AbilityStatusRow, now time.Time) abilityStatus {
	out := abilityStatus{
		RequestID: r.ID.String(), SiteID: r.SiteID.String(), SiteName: fenceSiteText(r.SiteLabel),
		Name: r.AbilityName, Editor: r.Editor, PostType: r.PostType, Effect: r.EffectCopy,
		State:     abilityStateFor(r, now),
		CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339), ExpiresAt: r.ExpiresAt.UTC().Format(time.RFC3339),
		DecidedAt: tsStringPtr(r.DecidedAt), WaitingReason: r.LastAttemptCode,
		Outcome: r.Outcome, CreatedPostID: r.CreatedPostID, Restored: r.Restored, Trashed: r.Trashed,
		Undo: r.UndoState, UndoAvailableUntil: tsStringPtr(r.UndoAvailableUntil),
	}
	if r.TitleExcerpt != nil {
		t := fenceSiteText(*r.TitleExcerpt)
		out.Title = &t
	}
	switch {
	case r.OutcomeCode != nil:
		out.Code = r.OutcomeCode
	case r.NotSentReason != nil:
		out.Code = r.NotSentReason
	}
	if r.State == "outcome_unknown" && r.Outcome == nil {
		u := "outcome_unknown"
		out.Outcome = &u
	}
	return out
}

func (s *Service) abilityRequestStatus(ctx context.Context, eng *abilityEngine, auth AuthorizedRequest, idText string, present bool) (string, error) {
	if auth.Sites.IsEmpty() {
		return "", scopeEmptyRefusal()
	}
	if eng.writes == nil {
		if present {
			return "", absentRefusal(reasonRequestAbsent, map[string]any{"request_id": idText})
		}
		return `{"requests":[],"supported":false,"poll_after_seconds":30}`, nil
	}
	p := connectionScopedPrincipal(auth)
	now := s.now()
	if present {
		id, err := uuid.Parse(idText)
		if err != nil {
			return "", argRefusal(reasonInvalidArguments, "request_id", idText, msgAbilityRequestAbsent, nil)
		}
		row, found, err := eng.writes.ReadAbilityRequestStatus(ctx, p, auth.GrantID, id)
		if err != nil {
			return "", fmt.Errorf("read ability request status: %w", err)
		}
		// R5: the grant's CURRENT site scope applies to every row, here as
		// well as in the scoped transaction: a site dropped from the grant
		// answers the same as a request that never existed.
		if !found || !auth.Sites.Allows(row.SiteID) {
			return "", absentRefusal(reasonRequestAbsent, map[string]any{"request_id": idText})
		}
		b, err := json.Marshal(map[string]any{
			"request": abilityStatusFromRow(row, now), "poll_after_seconds": requestPollAfterSeconds,
		})
		return string(b), err
	}
	rows, err := eng.writes.ListOpenAbilityRequestStatus(ctx, p, auth.GrantID, abilityStatusListLimit)
	if err != nil {
		return "", fmt.Errorf("list ability request status: %w", err)
	}
	list := make([]abilityStatus, 0, len(rows))
	for _, r := range rows {
		if !auth.Sites.Allows(r.SiteID) {
			continue
		}
		list = append(list, abilityStatusFromRow(r, now))
	}
	b, err := json.Marshal(map[string]any{
		"requests": list, "supported": true, "poll_after_seconds": requestPollAfterSeconds,
		"limit": abilityStatusListLimit,
	})
	return string(b), err
}
