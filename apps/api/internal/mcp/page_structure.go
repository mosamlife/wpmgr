package mcp

// wpmgr/page-structure and the parts of the builder edit contract both
// builder-edit abilities share: the structure read's input and output, the
// drafts this control plane names to the agent (p.allowed_draft_ids), the
// agent floor, and the one answer the AI gets for a page WPMgr may not read
// or edit.
//
// page-structure is a free read: it runs on the read path, its output is
// projected onto a known shape, and every string the site chose is fenced.
// The values the AI sends back to wpmgr/page-edit (node refs and the base
// fingerprint) pass only when they match the contract's own patterns, so
// they reach the AI unchanged and nothing else does.

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// AbilityPageStructure is wpmgr/page-structure: the structure of a page
// built with a page builder, read without changing anything.
const AbilityPageStructure = "wpmgr/page-structure"

// pageStructureInputSchema is the agent's
// BuilderContract::pageStructureInputSchema() byte for byte (a test compares
// it with the agent's fixture).
//
//go:embed page_structure_schema.json
var pageStructureInputSchemaFile []byte

var pageStructureInputSchema = json.RawMessage(strings.TrimSuffix(string(pageStructureInputSchemaFile), "\n"))

// pageStructureMaxNodes is BuilderContract::MAX_STRUCTURE_NODES.
const pageStructureMaxNodes = 500

// pageStructureInputBad reports whether input breaks the page-structure
// schema: post_id (a whole number of at least 1), an optional node ref and
// an optional max_nodes of 1..500, and nothing else.
func pageStructureInputBad(input []byte) bool {
	top, ok := jsonObjectOf(input)
	if !ok || !keysExactly(top, []string{"post_id", "node", "max_nodes"}, "post_id") {
		return true
	}
	if id, ok := jsonIntOf(top["post_id"]); !ok || id < 1 {
		return true
	}
	if raw, has := top["node"]; has {
		if _, ok := pageEditRef(raw); !ok {
			return true
		}
	}
	if raw, has := top["max_nodes"]; has {
		if n, ok := jsonIntOf(raw); !ok || n < 1 || n > pageStructureMaxNodes {
			return true
		}
	}
	return false
}

// builderEditInputPostID is the post an input of either builder-edit
// ability names, or ok=false when it names none.
func builderEditInputPostID(input []byte) (int64, bool) {
	top, ok := jsonObjectOf(input)
	if !ok {
		return 0, false
	}
	id, ok := jsonIntOf(top["post_id"])
	return id, ok && id >= 1
}

// isBuilderEditAbility: name is one of the two builder-edit abilities.
func isBuilderEditAbility(name string) bool {
	return name == AbilityPageStructure || name == AbilityPageEdit
}

// ---------------------------------------------------------------------------
// The output
// ---------------------------------------------------------------------------

var (
	pageStructureBuilderToken = regexp.MustCompile(`^[a-z0-9]{1,32}$`)
	pageStructureWordToken    = regexp.MustCompile(`^[a-z_]{1,32}$`)
	pageStructureFieldToken   = regexp.MustCompile(`^(?:text|url|alt|caption)$`)
)

// tokenLeaf is a string leaf the AI uses as given: a value matching re passes
// unchanged, any other value is dropped.
func tokenLeaf(re *regexp.Regexp) *outShape { return &outShape{token: re} }

var (
	intLeaf  = &outShape{scalar: "int"}
	boolLeaf = &outShape{scalar: "bool"}
)

// pageStructureOutputShape mirrors the agent's page-structure answer:
// {post_id, builder, builder_version, format, status, editable,
// base_fingerprint, node_count, truncated, nodes}, each node {ref, parent,
// kind, level?, editable, from_the_site} or, locked, {ref, parent, kind,
// label}. Site text is only ever under from_the_site, and fenced.
var pageStructureOutputShape = obj(map[string]*outShape{
	"post_id": intLeaf, "builder": tokenLeaf(pageStructureBuilderToken), "builder_version": leaf,
	"format": tokenLeaf(pageStructureWordToken), "status": leaf, "editable": boolLeaf,
	"base_fingerprint": tokenLeaf(pageEditFingerprintPattern), "node_count": intLeaf, "truncated": boolLeaf,
	"nodes": list(obj(map[string]*outShape{
		"ref": tokenLeaf(pageEditRefPattern), "parent": tokenLeaf(pageEditRefPattern), "kind": tokenLeaf(pageStructureWordToken),
		"level": intLeaf, "editable": list(tokenLeaf(pageStructureFieldToken)), "label": leaf,
		"from_the_site": obj(map[string]*outShape{"text": leaf, "url": leaf, "alt": leaf, "caption": leaf}),
	})),
})

// ---------------------------------------------------------------------------
// The drafts this control plane names (p.allowed_draft_ids)
// ---------------------------------------------------------------------------

// eligibleDraftQueries is the one statement eligibleDraftIDs runs.
type eligibleDraftQueries interface {
	GetEligibleCreatedDraft(ctx context.Context, arg sqlc.GetEligibleCreatedDraftParams) (sqlc.GetEligibleCreatedDraftRow, error)
}

var _ eligibleDraftQueries = (*sqlc.Queries)(nil)

// eligibleDraftIDs is p.allowed_draft_ids for one call about postID: [postID]
// when a done wpmgr/page-create on this site created that post and its undo
// has not trashed it and is not trashing it, [] otherwise. Only the input's
// post is ever named, so p stays bounded however many drafts WPMgr made on
// the site. It never returns nil, so the list is always sent.
func eligibleDraftIDs(ctx context.Context, q eligibleDraftQueries, tenantID, siteID uuid.UUID, postID int64) ([]int64, error) {
	if postID < 1 {
		return []int64{}, nil
	}
	row, err := q.GetEligibleCreatedDraft(ctx, sqlc.GetEligibleCreatedDraftParams{
		TenantID: tenantID, SiteID: siteID, PostID: postID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return []int64{}, nil
	}
	if err != nil {
		return nil, err
	}
	if row.CreatedPostID == nil || *row.CreatedPostID != postID {
		return []int64{}, nil
	}
	return []int64{postID}, nil
}

// EligibleDraftIDsTx is eligibleDraftIDs in the caller's transaction: the
// dispatch worker's p.allowed_draft_ids for a page-edit write, read at
// dispatch.
func EligibleDraftIDsTx(ctx context.Context, q *sqlc.Queries, tenantID, siteID uuid.UUID, postID int64) ([]int64, error) {
	return eligibleDraftIDs(ctx, q, tenantID, siteID, postID)
}

// EligibleDraftIDs implements DraftEligibilityStore: eligibleDraftIDs, in
// the connection's site-scoped transaction.
func (r *Repo) EligibleDraftIDs(ctx context.Context, principal domain.Principal, siteID uuid.UUID, postID int64) ([]int64, error) {
	var out []int64
	err := r.runConnectionTx(ctx, principal, "read eligible draft", func(tx pgx.Tx) error {
		ids, err := eligibleDraftIDs(ctx, sqlc.New(tx), principal.TenantID, siteID, postID)
		out = ids
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DraftEligibilityStore reads which drafts this control plane names to the
// agent. *Repo implements it.
type DraftEligibilityStore interface {
	EligibleDraftIDs(ctx context.Context, principal domain.Principal, siteID uuid.UUID, postID int64) ([]int64, error)
}

var _ DraftEligibilityStore = (*Repo)(nil)

// allowedDraftIDsFor is p.allowed_draft_ids for one builder-edit call, read
// in this call, never copied from an earlier one.
func (s *Service) allowedDraftIDsFor(ctx context.Context, eng *abilityEngine, site abilitySite, input []byte) ([]int64, error) {
	if eng.drafts == nil {
		return nil, errors.New("mcp: no draft eligibility store")
	}
	postID, ok := builderEditInputPostID(input)
	if !ok {
		return []int64{}, nil
	}
	return eng.drafts.EligibleDraftIDs(ctx, site.p, site.row.ID, postID)
}

// ---------------------------------------------------------------------------
// Refusals both builder-edit abilities give
// ---------------------------------------------------------------------------

const (
	msgAbilityBuilderEditOutdated = "Reading and editing pages built with a page builder needs the WPMgr plugin " +
		agentcmd.MinAgentVersionForBuilderEdit + " or later on this site. Nothing was read or asked."
	msgPageStructureNotReadable = "WPMgr cannot read the structure of this page, so nothing was read"
	msgPageEditNotEligible      = "WPMgr cannot edit this page, so nothing was asked"
	// hintPageStructureNotReadable and hintPageEditNotEligible are the one
	// answer the AI gets for every page its tool may not read or edit,
	// whatever the site said about it.
	hintPageStructureNotReadable = "WPMgr can read the structure of a published page, or of a draft WPMgr created " +
		"for you with wpmgr/page-create."
	hintPageEditNotEligible = "WPMgr edits only drafts it created with wpmgr/page-create in a page builder WPMgr " +
		"supports. For block-editor and classic pages use wpmgr/content-edit."
	// refusalPostNotReadable and refusalTargetNotEligible are the agent's
	// codes for a page outside what each ability may touch.
	refusalPostNotReadable   = "post_not_readable"
	refusalTargetNotEligible = "target_not_eligible"
)

// builderEditDetailToken is the agent's detail word for an ineligible page,
// kept only on the person-visible record.
var builderEditDetailToken = regexp.MustCompile(`^[a-z_]{1,40}$`)

// builderEditIneligibleRefusal is the one answer for a page the ability may
// not read (page-structure) or edit (page-edit): the same bytes for every
// such page and every agent code that means it. The agent's detail word goes
// to the audit record only, never to the AI.
func builderEditIneligibleRefusal(name string, refusal *agentcmd.AbilityRunRefusal) *toolRefusal {
	code, msg, hint := refusalPostNotReadable, msgPageStructureNotReadable, hintPageStructureNotReadable
	if name == AbilityPageEdit {
		code, msg, hint = refusalTargetNotEligible, msgPageEditNotEligible, hintPageEditNotEligible
	}
	meta := map[string]any{"code": code}
	if refusal != nil {
		meta["agent_code"] = refusal.Code
		if builderEditDetailToken.MatchString(refusal.Detail) {
			meta["detail"] = refusal.Detail
		}
	}
	return &toolRefusal{
		reason: reasonAbilityAgentRefused,
		err: domain.Validation(ErrCodeInvalidToolArguments, msg).
			WithDetails(map[string]any{"code": code, "hint": hint, "retryable": false}),
		meta: meta,
	}
}

// withBuilderEditTarget names, on the audit row of a builder-edit refusal,
// the ability and the post the AI asked about: the person-visible record of
// a request that never became a card (the connection's activity). The AI's
// answer is unchanged, so it stays the same bytes for every page. post_id
// is the input's own positive integer; an input without one names no post.
// Any other error passes through.
func withBuilderEditTarget(err error, name string, input []byte) error {
	tr, ok := err.(*toolRefusal)
	if !ok || tr == nil {
		return err
	}
	meta := make(map[string]any, len(tr.meta)+2)
	for k, v := range tr.meta {
		meta[k] = v
	}
	meta["ability"] = name
	if id, ok := builderEditInputPostID(input); ok {
		meta["post_id"] = id
	}
	out := *tr
	out.meta = meta
	return &out
}

// builderEditIneligible reports whether an agent refusal of a builder-edit
// call means the page is outside what the ability may touch.
func builderEditIneligible(name string, refusal *agentcmd.AbilityRunRefusal) bool {
	return refusal != nil && isBuilderEditAbility(name) &&
		(refusal.Code == refusalPostNotReadable || refusal.Code == refusalTargetNotEligible)
}

// builderEditOutdatedRefusal is the refusal for a site whose agent is below
// MinAgentVersionForBuilderEdit.
func builderEditOutdatedRefusal() *toolRefusal {
	return refuse(reasonAgentOutdated, domain.Conflict(ErrCodeSiteAgentOutdated,
		msgAbilityBuilderEditOutdated).WithDetails(map[string]any{
		"min_agent_version": agentcmd.MinAgentVersionForBuilderEdit, "retryable": false,
	}))
}
