package mcp

// The design a wpmgr/page-edit call is compiled with.
//
// A designed page is made with a design (page-create's `design` object). A
// page-edit that adds nodes to it is compiled with the same design: the
// input's own `design` when it sends one, else the design of the page-create
// request that made the draft, read here by the draft's post. The design the
// site compiles with is then a member of the input the site is asked about,
// the input the request stores and the input its digest covers, so nothing
// the site compiles with is outside what a person approves.
//
// A draft made without a design (outline version 2) has none to give. An edit
// of it that adds only version 2 nodes is sent as written; one that adds a
// section or a pattern has no design to compile with and is refused as
// design_not_ready.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// eligibleDraftInputQueries are the two statements eligibleDraftCreateInput
// runs: the one that names the eligible draft, and the one that returns the
// input or retained design of the request it names.
type eligibleDraftInputQueries interface {
	GetEligibleCreatedDraft(ctx context.Context, arg sqlc.GetEligibleCreatedDraftParams) (sqlc.GetEligibleCreatedDraftRow, error)
	GetEligibleCreatedDraftInput(ctx context.Context, arg sqlc.GetEligibleCreatedDraftInputParams) (sqlc.GetEligibleCreatedDraftInputRow, error)
}

var _ eligibleDraftInputQueries = (*sqlc.Queries)(nil)

// errDraftInputNamesAnotherRequest is the read whose two statements named two
// requests: nothing of it is used.
var errDraftInputNamesAnotherRequest = errors.New("mcp: the draft and its creation input name different requests")

// eligibleDraftCreateInput is the input of the request that made the eligible
// draft for postID, or its retained design after content clearing. It is nil
// when the control plane names no draft for it. Both
// statements carry the tenant, the site and the post, and the id each returns
// must be the same request's.
func eligibleDraftCreateInput(ctx context.Context, q eligibleDraftInputQueries, tenantID, siteID uuid.UUID, postID int64) ([]byte, error) {
	if postID < 1 {
		return nil, nil
	}
	draft, err := q.GetEligibleCreatedDraft(ctx, sqlc.GetEligibleCreatedDraftParams{
		TenantID: tenantID, SiteID: siteID, PostID: postID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if draft.CreatedPostID == nil || *draft.CreatedPostID != postID {
		return nil, nil
	}
	created, err := q.GetEligibleCreatedDraftInput(ctx, sqlc.GetEligibleCreatedDraftInputParams{
		TenantID: tenantID, SiteID: siteID, PostID: postID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if created.ID != draft.ID {
		return nil, errDraftInputNamesAnotherRequest
	}
	if created.ContentClearedAt.Valid {
		return created.RetainedDraft, nil
	}
	return []byte(created.InputJson), nil
}

// draftDesignOf is the design a page-create input made its draft with: the
// design of an input of outline version 3, judged again by the validator as
// it stands. Nil for any other input (a version 2 draft has no design), and
// for a design that validator refuses.
func draftDesignOf(createInput []byte) *pageDesign {
	top, ok := jsonObjectOf(createInput)
	if !ok {
		return nil
	}
	if version, ok := pageOutlineVersionOf(top); !ok || version != pageOutlineVersionDesigned {
		return nil
	}
	design, problem := validatePageDesign(top["design"])
	if problem != nil {
		return nil
	}
	return &design
}

// pageEditInputWithDesign is input with design appended as its last member;
// every member the AI sent stays as it was written. ok is false when the
// result would be larger than a page-edit input may be.
func pageEditInputWithDesign(input []byte, design pageDesign) ([]byte, bool) {
	body := bytes.TrimRight(input, " \t\r\n")
	if len(body) < 2 || body[len(body)-1] != '}' {
		return nil, false
	}
	enc, err := json.Marshal(design)
	if err != nil {
		return nil, false
	}
	out := make([]byte, 0, len(body)+len(`,"design":`)+len(enc)+1)
	out = append(out, body[:len(body)-1]...)
	out = append(out, `,"design":`...)
	out = append(out, enc...)
	out = append(out, '}')
	if len(out) > pageEditMaxInputBytes {
		return nil, false
	}
	return out, true
}

// pageEditHasDesignedNodes reports whether a node the call adds is one that
// only a design can compile: a section or a pattern, at any depth.
func pageEditHasDesignedNodes(f pageEditFacts) bool {
	for _, op := range f.ops {
		for _, raw := range op.Outline {
			var v any
			if json.Unmarshal(raw, &v) == nil && jsonHasDesignedNode(v) {
				return true
			}
		}
	}
	return false
}

func jsonHasDesignedNode(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		if kind, _ := x["type"].(string); kind == "section" || kind == "pattern" {
			return true
		}
		for _, child := range x {
			if jsonHasDesignedNode(child) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(x, jsonHasDesignedNode)
	}
	return false
}

// msgPageEditDesignMissing is the reason a call that adds sections or
// patterns has no design to compile them with.
const msgPageEditDesignMissing = "missing; this page was made without a design, so send the whole design with " +
	"new sections or patterns: colors, fonts, scale, radius and density"

// pageEditDesignMissingRefusal refuses a call that adds sections or patterns
// to a page that has no design and sends none.
func pageEditDesignMissingRefusal() *toolRefusal {
	return pageEditRefusalOf(pageCreateRefusal{
		Code: pageDesignNotReady,
		Failures: []refusalFailure{{
			Key: "design", Reason: msgPageEditDesignMissing, Allowed: slices.Clone(designTopKeys),
		}},
	})
}

// pageEditInheritDesign gives a call that adds nodes the design of the draft
// it edits. A design in the input is the design and the draft is not read; a
// call that adds no node needs none. It returns the input to send the site
// and its facts, a refusal of the call, or an error that fails it.
func (s *Service) pageEditInheritDesign(ctx context.Context, eng *abilityEngine, site abilitySite, input []byte, f pageEditFacts) ([]byte, pageEditFacts, *toolRefusal, error) {
	if f.design != nil || !f.addsNodes() {
		return input, f, nil, nil
	}
	var inherited *pageDesign
	if eng.drafts != nil {
		created, err := eng.drafts.EligibleDraftCreateInput(ctx, site.p, site.row.ID, f.postID)
		if err != nil {
			return nil, f, nil, fmt.Errorf("read the draft's design: %w", err)
		}
		inherited = draftDesignOf(created)
	}
	if inherited == nil {
		if pageEditHasDesignedNodes(f) {
			return nil, f, pageEditDesignMissingRefusal(), nil
		}
		return input, f, nil, nil
	}
	withDesign, ok := pageEditInputWithDesign(input, *inherited)
	if !ok {
		return nil, f, pageEditInputRefusal(pageCreateBadInput), nil
	}
	f.design = inherited
	return withDesign, f, nil, nil
}
