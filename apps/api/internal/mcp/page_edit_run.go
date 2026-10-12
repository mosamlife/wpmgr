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
	msgPageEditInput = "the operations do not follow the page edit rules, so nothing was asked"
)

// pageEditInputRefusal refuses an input the rules that need no page
// refused: a schema mismatch carries the schema, a ref named twice the
// agent's code and our fixed hint.
func pageEditInputRefusal(code string) *toolRefusal {
	if code == pageCreateBadInput {
		return argRefusal(reasonInvalidArguments, "input", "", msgAbilityArgInput, pageEditInputSchema)
	}
	return &toolRefusal{
		reason: reasonInvalidArguments,
		err: domain.Validation(ErrCodeInvalidToolArguments, msgPageEditInput).WithDetails(map[string]any{
			"argument": "input", "code": code, "hint": precheckRefusalHint(code), "retryable": false,
		}),
		meta: map[string]any{"argument": "input", "code": code},
	}
}

// pageEditOutlineProblem is the first Elementor node rule a new node of the
// call breaks, in operation and outline order, with the node in the agent's
// path syntax (operations[1].outline[0].buttons[2]). Elementor is the one
// page builder wpmgr/page-edit edits in this version.
func pageEditOutlineProblem(f pageEditFacts) *pageBuilderProblem {
	for i, op := range f.ops {
		for j, raw := range op.Outline {
			if p := elementorNodeProblem(raw, fmt.Sprintf("operations[%d].outline[%d]", i, j)); p != nil {
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
	facts, code := validatePageEditInput(input)
	if code != "" {
		return pageEditFacts{}, pageEditInputRefusal(code)
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
	facts, r := pageEditPreInputRefusal(input)
	if r != nil {
		return "", r
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
