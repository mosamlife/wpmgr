package mcp

// wpmgr/page-edit's run path, entered from runSiteAbilityWrite after the
// capability, the entry's floor, the governed context, the agent's
// connection and the site's content-editing switch. Nothing is stored and
// nothing reaches the site before the input passes the rules that need no
// page (page_edit_input.go) and Elementor's node rules for every new node.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Refusal codes the agent answers for a page-edit operation against the
// page, and the hints the AI gets for them (our words, never the site's).
const (
	pageEditNodeNotFound    = "node_not_found"
	pageEditNodeNotEditable = "node_not_editable"
	pageEditOpNotSupported  = "op_not_supported_by_builder"
	pageEditPageTooLarge    = "page_too_large"
	pageEditAnchorInUse     = "anchor_in_use"

	hintPageEditOpsInvalid = "Name each node at most once in one call, as the node an operation changes or as the " +
		"place it puts new nodes, and never name a node that an earlier operation in the same call removed or " +
		"replaced, or a node inside one. Send further changes in a later call, after reading the page again with " +
		"wpmgr/page-structure."
	hintPageEditNodeNotFound = "A ref is not a node on this page. Read the page with wpmgr/page-structure and use " +
		"the refs it gives."
	hintPageEditNodeNotEditable = "An operation changes something this page does not allow. Change only a field " +
		"listed in the node's editable list; a locked node can be an anchor but is never changed, replaced, removed " +
		"or moved; insert into a section, column, group or columns only; never move a node into its own subtree."
	hintPageEditOpNotSupported = "The page builder of this page does not support that operation. Use set_text, " +
		"insert, replace or remove instead, as wpmgr/page-structure's answer allows."
	hintPageEditPageTooLarge = "The call adds more than 400 new nodes, or leaves the page with more than 800 nodes " +
		"or a page larger than WPMgr edits. Send fewer or smaller changes."
	hintPageEditAnchorInUse = "A section this call removes, or replaces without its anchor, is still linked to by a " +
		"node that stays on the page. Change or remove those links in the same call, or keep the section's " +
		"anchor. Read the page with wpmgr/page-structure to see each section's anchor and the nodes that link " +
		"to it."
	msgPageEditInput = "the operations do not follow the page edit rules, so nothing was asked"
)

// pageEditInputRefusal refuses an input the rules that need no page
// refused: a schema mismatch carries the schema, a ref named twice the
// agent's code and our fixed hint.
func pageEditInputRefusal(code string) *toolRefusal {
	return pageEditRefusalOf(pageCreateRefusal{Code: code})
}

// pageEditRefusalOf is pageEditInputRefusal for a refusal that may carry
// failures (the design's): the message spells out the first three and
// failures[] holds the rest. A value the design may not hold carries the
// page-edit schema.
func pageEditRefusalOf(r pageCreateRefusal) *toolRefusal {
	code := r.Code
	msg, failures := msgPageEditInput, []refusalFailure(nil)
	if len(r.Failures) > 0 {
		msg, failures = refusalEnvelope(pageCreateRefusalSummary(code), r.Failures)
	}
	if code == pageCreateBadInput {
		ref := argRefusal(reasonInvalidArguments, "input", "", msgAbilityArgInput, pageEditInputSchema)
		if len(failures) > 0 {
			ref.err = domain.Validation(ErrCodeInvalidToolArguments, msg).WithDetails(map[string]any{
				"argument": "input", "retryable": false, "schema": pageEditInputSchema, "failures": failures,
			})
		}
		return ref
	}
	details := map[string]any{"argument": "input", "code": code, "hint": precheckRefusalHint(code), "retryable": false}
	if len(failures) > 0 {
		details["failures"] = failures
	}
	return &toolRefusal{
		reason: reasonInvalidArguments,
		err:    domain.Validation(ErrCodeInvalidToolArguments, msg).WithDetails(details),
		meta:   map[string]any{"argument": "input", "code": code},
	}
}

// pageEditOutlineProblem is the first Elementor node rule a new node of the
// call breaks, in operation and outline order, with the node in the agent's
// path syntax (operations[1].outline[0].buttons[2]). Elementor is the one
// page builder wpmgr/page-edit edits in this version.
func pageEditOutlineProblem(f pageEditFacts) *pageBuilderProblem {
	for i, op := range f.ops {
		for j, raw := range op.Outline {
			if p := elementorNodeProblemWithDesign(raw, fmt.Sprintf("operations[%d].outline[%d]", i, j), f.design != nil); p != nil {
				return p
			}
		}
	}
	return nil
}

// pageEditBuilderRefusal is the refusal for a valid input whose new nodes
// break Elementor's node rules, or nil.
func pageEditBuilderRefusal(f pageEditFacts) *toolRefusal {
	p := pageEditOutlineProblem(f)
	if p == nil {
		return nil
	}
	details := map[string]any{
		"argument": "input", "code": p.code, "node": p.node, "field": p.field, "hint": p.hint, "retryable": false,
	}
	if p.allowed != nil {
		details["allowed"] = p.allowed
	}
	return &toolRefusal{
		reason: reasonInvalidArguments,
		err:    domain.Validation(ErrCodeInvalidToolArguments, msgAbilityBuilderInput).WithDetails(details),
		meta:   map[string]any{"argument": "input", "code": p.code},
	}
}

// pageEditPreInputRefusal is everything the control plane checks in a
// page-edit input before the site sees it, in order: the rules that need no
// page, then Elementor's node rules for every new node.
func pageEditPreInputRefusal(input []byte) (pageEditFacts, *toolRefusal) {
	facts, refusal := checkPageEditInput(input)
	if refusal.Code != "" {
		return pageEditFacts{}, pageEditRefusalOf(refusal)
	}
	if r := pageEditBuilderRefusal(facts); r != nil {
		return pageEditFacts{}, r
	}
	return facts, nil
}

// runPageEdit checks a page-edit input and prechecks it on the site, with
// the draft this control plane names for its post read in this call. A
// refusal's audit row names the ability and the post (withBuilderEditTarget).
func (s *Service) runPageEdit(ctx context.Context, auth AuthorizedRequest, eng *abilityEngine, site abilitySite, e *sqlc.AbilityCatalogue, host string, input []byte) (string, error) {
	out, err := s.runPageEditChecked(ctx, auth, eng, site, e, host, input)
	return out, withBuilderEditTarget(err, AbilityPageEdit, input)
}

func (s *Service) runPageEditChecked(ctx context.Context, auth AuthorizedRequest, eng *abilityEngine, site abilitySite, e *sqlc.AbilityCatalogue, host string, input []byte) (string, error) {
	// Native operations enforce their floor before the operation grammar.
	if PageEditAgentFloor(input) == agentcmd.MinAgentVersionForNativeEdit {
		if fr := pageEditFloorRefusal(site.row.AgentVersion, input); fr != nil {
			return "", fr
		}
	}
	facts, r := pageEditPreInputRefusal(input)
	if r != nil {
		return "", r
	}
	// The per-input floor: a design, a section, a pattern or a pattern path
	// needs a newer agent than the entry's own floor, which every other edit
	// keeps. It is decided before the site is asked.
	if fr := pageEditFloorRefusal(site.row.AgentVersion, input); fr != nil {
		return "", fr
	}
	// An entry that enables no page builder WPMgr edits leaves no page
	// eligible: the one answer for an ineligible page.
	if !pageBuilderEnabled(e.Limits, pageBuilderElementor) {
		return "", builderEditIneligibleRefusal(AbilityPageEdit, nil)
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
	allowed, err := s.allowedDraftIDsFor(ctx, eng, site, input)
	if err != nil {
		return "", fmt.Errorf("site_ability_run: %w", err)
	}
	// The nodes a call adds to a draft made with a design are compiled with
	// that design: the site is asked about, and the request stores, the input
	// with the design in it. A page that is no eligible draft is left to the
	// site, whose one answer for it is the same for every such page.
	if len(allowed) > 0 {
		var dr *toolRefusal
		input, facts, dr, err = s.pageEditInheritDesign(ctx, eng, site, input, facts)
		if dr != nil {
			return "", dr
		}
		if err != nil {
			return "", fmt.Errorf("site_ability_run: %w", err)
		}
		// A design the draft gave is a member of the input now, and the floor
		// follows the input the site would be asked about.
		if fr := pageEditFloorRefusal(site.row.AgentVersion, input); fr != nil {
			return "", fr
		}
	}
	precheckID := uuid.New()
	callCtx, cancel := context.WithTimeout(ctx, abilityRunTimeout)
	resp, err := eng.agent.AbilityRun(callCtx, site.row.ID, site.row.Url, agentcmd.AbilityRunCall{
		Mode: agentcmd.AbilityRunModePrecheck, RequestID: precheckID,
		Entry: entryBytes, EntrySHA256: entrySum, Input: input, AllowedDraftIDs: allowed,
	})
	cancel()
	if err != nil {
		var refusal *agentcmd.AbilityRunRefusal
		if errors.As(err, &refusal) {
			if builderEditIneligible(AbilityPageEdit, refusal) {
				return "", builderEditIneligibleRefusal(AbilityPageEdit, refusal)
			}
			if refusal.Code == notRunnableEditingNotEnabled {
				return "", &toolRefusal{
					reason: reasonAbilityPrecheckRefused,
					err: domain.Conflict(ErrCodeInvalidToolArguments, msgAbilityEditingOff).
						WithDetails(map[string]any{"code": refusal.Code, "retryable": false}),
					meta: map[string]any{"code": refusal.Code},
				}
			}
			return "", sitePrecheckRefusal(AbilityPageEdit, refusal)
		}
		return "", refuse(reasonSiteUnreachable, domain.Unavailable(ErrCodeSiteUnreachable,
			msgSiteUnreachable).WithDetails(map[string]any{"retryable": true}))
	}
	// The answer is accepted only once its edit preview is bound to this
	// input (R2); any difference refuses the request and stores nothing.
	checked, ok := verifyPageEditPrecheck(resp, entrySum, input, facts)
	if !ok {
		return "", precheckMismatchRefusal()
	}
	card, err := buildPageEditCardFacts(checked, s.now())
	if errors.Is(err, errPageEditCardTooLarge) {
		return "", precheckMismatchRefusal()
	}
	if err != nil {
		return "", err
	}
	// Stored under the id its precheck was sent with: the write's new node
	// ids derive from it, so they are the ones the card showed.
	res, err := s.createPageEditRequest(ctx, eng.writes, auth, site.row, host, e, entrySum, input, card, checked, precheckID)
	if err != nil {
		return "", err
	}
	// The approval package decides it, after the commit.
	res = s.decideAbility(ctx, eng.writes, auth, res)
	b, err := json.Marshal(res)
	if err != nil {
		return "", fmt.Errorf("encode ability request result: %w", err)
	}
	return string(b), nil
}

// precheckMismatchRefusal is the refusal for a page-edit precheck answer
// that does not bind to the input: nothing was asked.
func precheckMismatchRefusal() *toolRefusal {
	return &toolRefusal{
		reason: reasonAbilityPrecheckRefused,
		err: domain.Unavailable(ErrCodeSiteUnreachable, msgAbilitySiteAnswer).
			WithDetails(map[string]any{"code": pageEditCodePrecheckMismatch, "retryable": false}),
		meta: map[string]any{"code": pageEditCodePrecheckMismatch},
	}
}

// msgPageEditDesignedOutdated is the update-required message of an edit that
// needs the agent that builds designed pages.
const msgPageEditDesignedOutdated = "Editing a page with a design, a section or a pattern needs the WPMgr plugin " +
	agentcmd.MinAgentVersionForDesignedPages + " or later on this site. Nothing was asked."

// PageEditAgentFloor is the first agent release that runs this wpmgr/page-edit
// input: MinAgentVersionForNativeEdit when it holds add_native, set_native or
// duplicate; MinAgentVersionForDesignedPages when it carries a design, when an
// operation puts a section or a pattern on the page (at any depth of its
// outline), or when set_text names a pattern path;
// MinAgentVersionForBuilderEdit otherwise. It reads only those members, so the
// floor does not wait on the other rules of the input. An input it cannot read
// counts as designed, so a floor derived from it fails closed. The run path
// and the dispatch worker both decide with it.
func PageEditAgentFloor(input []byte) string {
	top, ok := jsonObjectOf(input)
	if !ok {
		return agentcmd.MinAgentVersionForDesignedPages
	}
	if ops, ok := jsonArrayOf(top["operations"]); ok {
		for _, rawOp := range ops {
			op, ok := jsonObjectOf(rawOp)
			if !ok {
				continue
			}
			kind, _ := jsonStringOf(op["op"])
			if kind == "add_native" || kind == "set_native" || kind == "duplicate" {
				return agentcmd.MinAgentVersionForNativeEdit
			}
		}
	}
	if _, has := top["design"]; has {
		return agentcmd.MinAgentVersionForDesignedPages
	}
	raw, has := top["operations"]
	if !has {
		return agentcmd.MinAgentVersionForBuilderEdit
	}
	ops, ok := jsonArrayOf(raw)
	if !ok {
		return agentcmd.MinAgentVersionForDesignedPages
	}
	for _, rawOp := range ops {
		op, ok := jsonObjectOf(rawOp)
		if !ok || pageEditOpIsDesigned(op) {
			return agentcmd.MinAgentVersionForDesignedPages
		}
	}
	return agentcmd.MinAgentVersionForBuilderEdit
}

// pageEditOpIsDesigned reports whether one operation uses what only a designed
// page has: new nodes that hold a section or a pattern, or a set_text field
// that is a pattern path. An outline that is not JSON counts as designed.
func pageEditOpIsDesigned(op map[string]json.RawMessage) bool {
	if field, ok := jsonStringOf(op["field"]); ok && pageEditPathPattern.MatchString(field) {
		return true
	}
	outline, has := op["outline"]
	if !has {
		return false
	}
	var v any
	return json.Unmarshal(outline, &v) != nil || jsonHasDesignedNode(v)
}

// pageEditFloorRefusal is the run path's per-input floor: nil when an agent of
// this version runs the page-edit input, else the update-required refusal
// (site_agent_outdated) naming the floor PageEditAgentFloor gives it. The
// entry's own floor is classify's, so an input whose floor is
// MinAgentVersionForBuilderEdit is never refused here.
func pageEditFloorRefusal(agentVersion string, input []byte) *toolRefusal {
	floor := PageEditAgentFloor(input)
	if floor == agentcmd.MinAgentVersionForBuilderEdit || abilityAgentMeetsFloor(agentVersion, &floor) {
		return nil
	}
	msg := msgPageEditDesignedOutdated
	if floor == agentcmd.MinAgentVersionForNativeEdit {
		msg = msgNativeEditOutdated
	}
	return refuse(reasonAgentOutdated, domain.Conflict(ErrCodeSiteAgentOutdated,
		msg).WithDetails(map[string]any{
		"min_agent_version": floor, "retryable": false,
	}))
}
