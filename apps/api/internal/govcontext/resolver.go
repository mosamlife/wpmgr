package govcontext

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// DefaultContextByteBudget is the total resolved-context byte budget (ADR-064
// Decision 9) when a Resolver does not set one explicitly. ADR-064 mandates
// counting in bytes and the truncation order; it states no concrete number.
// 64 KiB is a judgment call sized generously for a full org+site context (each
// restriction/guidance field is short, human-typed text) while still bounding
// what a single request hands to a model. Revisit this number, not the
// mechanism, if it ever proves wrong in practice.
const DefaultContextByteBudget = 64 * 1024

// ContextStore is the subset of persistence the resolver needs: the latest
// snapshot for layers 2 and 3. Declared as an interface, not the concrete
// *Repo, so this file stays a pure, DB-free unit under test — Repo (repo.go)
// is the only implementation and is exercised separately, through the real
// tx-dispatch path, by the integration tests.
type ContextStore interface {
	// LatestOrgSnapshot returns the organisation's current (layer 2) content.
	// ok=false with a nil error means "no version has ever been written" — a
	// legitimate empty state, not a failure. A non-nil error means resolution
	// could not complete and MUST cause Resolve to refuse (Decision 14).
	LatestOrgSnapshot(ctx context.Context, tenantID uuid.UUID) (snap Snapshot, ok bool, err error)
	// LatestSiteSnapshot is LatestOrgSnapshot's site-scope (layer 3) sibling.
	LatestSiteSnapshot(ctx context.Context, tenantID, siteID uuid.UUID) (snap Snapshot, ok bool, err error)
}

// SiteFactsProvider is ADR-064 layer 4's seam. Unlike ContextStore, an error
// or a nil provider here degrades to an EMPTY layer 4, never a refusal — see
// SiteFacts's doc comment for why facts are a soft dependency and layers 2/3
// are not.
type SiteFactsProvider interface {
	GetFacts(ctx context.Context, tenantID, siteID uuid.UUID) (SiteFacts, error)
}

// Resolver assembles ADR-064's seven-layer precedence order (layers 1-6 only —
// see model.go's package doc for why 7 is absent, not merely empty) into one
// ResolvedContext. It is the single function ADR-064 Decision 8 requires both
// the effective-context preview and any future model-facing assembly path to
// call — Resolve is that function; the preview handler (handler.go) calls it
// with session=nil and nothing else.
type Resolver struct {
	Store       ContextStore
	Facts       SiteFactsProvider // nil => layer 4 always contributes nothing
	BudgetBytes int               // 0 => DefaultContextByteBudget
}

// Resolve walks layers 1 through 6 for one site and returns what a caller
// should be handed. It NEVER returns a partial or empty ResolvedContext on
// failure — ADR-064 Decision 14: "If context cannot be loaded, the call is
// refused outright. It is never given an empty, partial, or stale-but-unmarked
// result as a stand-in." Every returned error is domain.ServiceUnavailable
// ("context_unavailable"), the single reason code Decision 13 names for this
// path, so every caller — including a future model-facing assembly path — can
// treat it as one hard-failure case without inspecting the cause.
//
// tenantID must be the site's CURRENT owning organisation (the caller's active
// tenant on every real route this package registers) — Resolve does not
// verify site ownership itself; callers reach it only after
// authz.RequireSiteAccess has already confirmed the caller may see this site.
func (r *Resolver) Resolve(ctx context.Context, tenantID, siteID uuid.UUID, session *SessionInput) (ResolvedContext, error) {
	if r.Store == nil {
		// A Resolver built without a store is a wiring bug, not a transient
		// failure — but Decision 14's rule is unconditional: never proceed on
		// missing context, for any reason. Refuse the same way a real load
		// failure would.
		return ResolvedContext{}, domain.ServiceUnavailable("context_unavailable",
			"effective context could not be resolved: no context store configured")
	}

	orgSnap, _, err := r.Store.LatestOrgSnapshot(ctx, tenantID)
	if err != nil {
		return ResolvedContext{}, refusal("organisation context", err)
	}
	siteSnap, _, err := r.Store.LatestSiteSnapshot(ctx, tenantID, siteID)
	if err != nil {
		return ResolvedContext{}, refusal("site context", err)
	}

	var facts SiteFacts
	// factsUnavailable defaults true: "no provider wired" and "the provider
	// call failed" are both "we do not know", never "verified empty" — see
	// LayerContribution.FactsUnavailable's doc comment. Only a SUCCESSFUL
	// call, whatever it returns, clears it.
	factsUnavailable := true
	if r.Facts != nil {
		if f, ferr := r.Facts.GetFacts(ctx, tenantID, siteID); ferr == nil {
			facts = f
			factsUnavailable = false
		}
		// A facts ERROR still does not refuse the whole resolution — layer 4
		// is observational, not authoritative, and only layers 2/3 (Decision
		// 14) can fail the call outright. It is recorded as unavailable
		// (above), never silently presented as a verified empty result.
	}

	var sessionText string
	if session != nil {
		sessionText = session.Text
	}

	layers := []LayerContribution{
		{Layer: 1, Name: "WPMgr security policy", Restrictions: layer1Restrictions},
		{Layer: 2, Name: "organisation default", Restrictions: orgSnap.Restrictions, Guidance: orgSnap.Guidance},
		{Layer: 3, Name: "site override", Restrictions: siteSnap.Restrictions, Guidance: siteSnap.Guidance},
		{Layer: 4, Name: "detected site facts", Facts: &facts, FactsUnavailable: factsUnavailable},
		{Layer: 5, Name: "approved skill instructions"}, // structurally present, always empty: no skill store exists yet
		{Layer: 6, Name: "session context", Session: sessionText},
	}

	budget := r.BudgetBytes
	if budget <= 0 {
		budget = DefaultContextByteBudget
	}
	for i := range layers {
		layers[i].Bytes = layerBytes(layers[i])
	}
	truncated := applyBudget(layers, budget)

	total := 0
	for _, l := range layers {
		total += l.Bytes
	}

	// applyBudget only ever drops layers 2-6, in that order, never layer 1
	// (Decision 9). If the total STILL exceeds budget after every reducible
	// layer has been fully emptied, the budget cannot be met by truncation at
	// all — layer 1 alone is over budget. Emitting an over-budget result
	// would violate the budget silently; emitting a plausible-looking
	// truncated one that still lies about its own size would be worse. This
	// refuses instead, the same reasoning that makes Decision 14 refuse
	// rather than proceed on a load failure: an honest failure beats a
	// silently wrong success. layer1Restrictions is empty today, so this is
	// unreachable in practice until layer 1 gains real content — see
	// resolver_test.go for how it is proven anyway.
	if total > budget {
		return ResolvedContext{}, domain.ServiceUnavailable("context_unavailable",
			"effective context could not be resolved: the resolved context exceeds its byte budget even after truncating every reducible layer").
			WithDetails(map[string]any{"total_bytes": total, "budget_bytes": budget})
	}

	return ResolvedContext{
		SiteID:       siteID,
		Layers:       layers,
		Restrictions: unionRestrictions(layer1Restrictions, orgSnap.Restrictions, siteSnap.Restrictions),
		TotalBytes:   total,
		BudgetBytes:  budget,
		Truncated:    truncated,
	}, nil
}

func refusal(what string, cause error) *domain.Error {
	return domain.ServiceUnavailable("context_unavailable",
		"effective context could not be resolved: failed to load "+what).WithCause(cause)
}

// unionRestrictions is the read-time half of Decision 4: "a restriction may
// only be added to or left alone by every layer below the one that set it".
// This union, not the write-time widen-check, is what actually holds that
// invariant — see ResolvedContext.Restrictions doc comment for why a leaf's
// STORED restrictions are routinely stale relative to a higher layer (a
// guidance-only patch never re-checks or refreshes them, and PatchOrgContext
// never touches a site row at all) and why that staleness is safe precisely
// because this union re-reads every layer's CURRENT row on every call.
func unionRestrictions(layers ...RestrictionSet) RestrictionSet {
	tools := map[string]struct{}{}
	domains := map[string]struct{}{}
	topics := map[string]struct{}{}
	for _, l := range layers {
		for _, v := range l.ForbiddenTools {
			tools[v] = struct{}{}
		}
		for _, v := range l.ForbiddenDomains {
			domains[v] = struct{}{}
		}
		for _, v := range l.ForbiddenTopics {
			topics[v] = struct{}{}
		}
	}
	return RestrictionSet{
		ForbiddenTools:   sortedKeys(tools),
		ForbiddenDomains: sortedKeys(domains),
		ForbiddenTopics:  sortedKeys(topics),
	}
}

func sortedKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// layerBytes is this codebase's one honest unit for Decision 9: there is no
// tokenizer on this side of the boundary, so a layer's "size" is the byte
// length of its own JSON encoding — deterministic, and it shrinks exactly
// when a field is dropped by applyBudget, which is what makes truncation
// accounting correct rather than approximate.
func layerBytes(l LayerContribution) int {
	b, _ := json.Marshal(struct {
		Restrictions RestrictionSet `json:"restrictions,omitempty"`
		Guidance     GuidanceSet    `json:"guidance,omitempty"`
		Facts        *SiteFacts     `json:"facts,omitempty"`
		Session      string         `json:"session,omitempty"`
	}{l.Restrictions, l.Guidance, l.Facts, l.Session})
	return len(b)
}
