package govcontext

import (
	"sort"
	"time"

	"github.com/google/uuid"
)

// --- wire <-> domain ---------------------------------------------------------

// contextDTO is the wire shape of GET .../context and of one history item.
// Version 0 (AuthorType/CreatedAt zero) represents "no context has ever been
// authored" — GetOrgContext / GetSiteContext's legitimate empty state.
type contextDTO struct {
	Version               int64          `json:"version"`
	Restrictions          RestrictionSet `json:"restrictions"`
	Guidance              GuidanceSet    `json:"guidance"`
	AuthorType            string         `json:"author_type,omitempty"`
	AuthorID              *string        `json:"author_id"`
	Provenance            string         `json:"provenance,omitempty"`
	RestoredFromVersionID *string        `json:"restored_from_version_id"`
	CreatedAt             *string        `json:"created_at"`
}

func toContextDTO(v Version) contextDTO {
	dto := contextDTO{
		Version:      v.Version,
		Restrictions: v.Snapshot.Restrictions,
		Guidance:     v.Snapshot.Guidance,
	}
	if v.Version == 0 {
		return dto
	}
	dto.AuthorType = string(v.AuthorType)
	dto.Provenance = string(v.Provenance)
	if v.AuthorID != uuid.Nil {
		s := v.AuthorID.String()
		dto.AuthorID = &s
	}
	if v.RestoredFromVersionID != uuid.Nil {
		s := v.RestoredFromVersionID.String()
		dto.RestoredFromVersionID = &s
	}
	ts := v.CreatedAt.UTC().Format(time.RFC3339)
	dto.CreatedAt = &ts
	return dto
}

// versionListDTO is the wire shape of GET .../context/versions.
type versionListDTO struct {
	Items      []versionSummaryDTO `json:"items"`
	NextCursor int64               `json:"next_cursor"` // 0 = no further page
}

type versionSummaryDTO struct {
	ID                    string  `json:"id"`
	Version               int64   `json:"version"`
	AuthorType            string  `json:"author_type"`
	AuthorID              *string `json:"author_id"`
	Provenance            string  `json:"provenance"`
	RestoredFromVersionID *string `json:"restored_from_version_id"`
	CreatedAt             string  `json:"created_at"`
}

func toVersionSummaryDTO(v Version) versionSummaryDTO {
	dto := versionSummaryDTO{
		ID:         v.ID.String(),
		Version:    v.Version,
		AuthorType: string(v.AuthorType),
		Provenance: string(v.Provenance),
		CreatedAt:  v.CreatedAt.UTC().Format(time.RFC3339),
	}
	if v.AuthorID != uuid.Nil {
		s := v.AuthorID.String()
		dto.AuthorID = &s
	}
	if v.RestoredFromVersionID != uuid.Nil {
		s := v.RestoredFromVersionID.String()
		dto.RestoredFromVersionID = &s
	}
	return dto
}

func toVersionListDTO(vs []Version, limit int32) versionListDTO {
	items := make([]versionSummaryDTO, 0, len(vs))
	for _, v := range vs {
		items = append(items, toVersionSummaryDTO(v))
	}
	var next int64
	if int32(len(vs)) == limit && len(vs) > 0 {
		next = vs[len(vs)-1].Version
	}
	return versionListDTO{Items: items, NextCursor: next}
}

// versionItemDTO is the wire shape of GET .../versions/{versionId}: the full
// snapshot plus authorship, always a real row (never the "no context yet"
// synthetic zero value contextDTO can represent).
type versionItemDTO struct {
	ID                    string         `json:"id"`
	Version               int64          `json:"version"`
	Restrictions          RestrictionSet `json:"restrictions"`
	Guidance              GuidanceSet    `json:"guidance"`
	AuthorType            string         `json:"author_type"`
	AuthorID              *string        `json:"author_id"`
	Provenance            string         `json:"provenance"`
	RestoredFromVersionID *string        `json:"restored_from_version_id"`
	CreatedAt             string         `json:"created_at"`
}

func toVersionItemDTO(v Version) versionItemDTO {
	dto := versionItemDTO{
		ID:           v.ID.String(),
		Version:      v.Version,
		Restrictions: v.Snapshot.Restrictions,
		Guidance:     v.Snapshot.Guidance,
		AuthorType:   string(v.AuthorType),
		Provenance:   string(v.Provenance),
		CreatedAt:    v.CreatedAt.UTC().Format(time.RFC3339),
	}
	if v.AuthorID != uuid.Nil {
		s := v.AuthorID.String()
		dto.AuthorID = &s
	}
	if v.RestoredFromVersionID != uuid.Nil {
		s := v.RestoredFromVersionID.String()
		dto.RestoredFromVersionID = &s
	}
	return dto
}

// listDiffDTO is a field-level restriction diff (ADR-064 Decision 5:
// "field-level (added/removed list entries) for restriction fields").
type listDiffDTO struct {
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
}

// fieldDiffDTO is a line-level guidance diff. ADR-064 asks for "line-level"
// diffing over free text; this package's diff is deliberately old/new-value
// level rather than a true line-by-line algorithm — guidance fields are short
// operator-authored text, not documents, and old/new is a faithful, simple
// rendering of what changed. A finer line-diff can be layered on later
// without changing the wire contract's shape (old/new stay the base case).
type fieldDiffDTO struct {
	Old string `json:"old,omitempty"`
	New string `json:"new,omitempty"`
}

// diffDTO is the wire shape of GET .../versions/{versionId}/diff.
type diffDTO struct {
	Version  versionItemDTO   `json:"version"`
	Baseline bool             `json:"baseline"`
	Prior    *versionItemDTO  `json:"prior"`
	Diff     *snapshotDiffDTO `json:"diff"`
}

type snapshotDiffDTO struct {
	ForbiddenTools   *listDiffDTO  `json:"forbidden_tools,omitempty"`
	ForbiddenDomains *listDiffDTO  `json:"forbidden_domains,omitempty"`
	ForbiddenTopics  *listDiffDTO  `json:"forbidden_topics,omitempty"`
	BrandVoice       *fieldDiffDTO `json:"brand_voice,omitempty"`
	Audience         *fieldDiffDTO `json:"audience,omitempty"`
	Terminology      *fieldDiffDTO `json:"terminology,omitempty"`
	Style            *fieldDiffDTO `json:"style,omitempty"`
}

func toDiffDTO(target Version, prior *Version, isBaseline bool) diffDTO {
	out := diffDTO{Version: toVersionItemDTO(target), Baseline: isBaseline}
	if isBaseline || prior == nil {
		return out
	}
	p := toVersionItemDTO(*prior)
	out.Prior = &p
	d := diffSnapshots(prior.Snapshot, target.Snapshot)
	out.Diff = &d
	return out
}

// diffSnapshots computes ADR-064 Decision 5's per-version diff: "field-level
// (added/removed list entries) for restriction fields, line-level for
// guidance fields". Only fields that actually changed are populated (nil
// omitted field = unchanged), matching the wire DTO's omitempty pointers.
func diffSnapshots(prior, current Snapshot) snapshotDiffDTO {
	var out snapshotDiffDTO
	if d := listDiff(prior.Restrictions.ForbiddenTools, current.Restrictions.ForbiddenTools); d != nil {
		out.ForbiddenTools = d
	}
	if d := listDiff(prior.Restrictions.ForbiddenDomains, current.Restrictions.ForbiddenDomains); d != nil {
		out.ForbiddenDomains = d
	}
	if d := listDiff(prior.Restrictions.ForbiddenTopics, current.Restrictions.ForbiddenTopics); d != nil {
		out.ForbiddenTopics = d
	}
	out.BrandVoice = fieldDiff(prior.Guidance.BrandVoice, current.Guidance.BrandVoice)
	out.Audience = fieldDiff(prior.Guidance.Audience, current.Guidance.Audience)
	out.Terminology = fieldDiff(prior.Guidance.Terminology, current.Guidance.Terminology)
	out.Style = fieldDiff(prior.Guidance.Style, current.Guidance.Style)
	return out
}

func listDiff(before, after []string) *listDiffDTO {
	beforeSet := make(map[string]struct{}, len(before))
	for _, v := range before {
		beforeSet[v] = struct{}{}
	}
	afterSet := make(map[string]struct{}, len(after))
	for _, v := range after {
		afterSet[v] = struct{}{}
	}
	var added, removed []string
	for v := range afterSet {
		if _, ok := beforeSet[v]; !ok {
			added = append(added, v)
		}
	}
	for v := range beforeSet {
		if _, ok := afterSet[v]; !ok {
			removed = append(removed, v)
		}
	}
	if len(added) == 0 && len(removed) == 0 {
		return nil
	}
	sort.Strings(added)
	sort.Strings(removed)
	return &listDiffDTO{Added: added, Removed: removed}
}

func fieldDiff(before, after string) *fieldDiffDTO {
	if before == after {
		return nil
	}
	return &fieldDiffDTO{Old: before, New: after}
}

// --- effective-context preview (Decision 8) ----------------------------------

type layerContributionDTO struct {
	Layer        int            `json:"layer"`
	Name         string         `json:"name"`
	Restrictions RestrictionSet `json:"restrictions"`
	Guidance     GuidanceSet    `json:"guidance"`
	Facts        *SiteFacts     `json:"facts,omitempty"`
	// FactsUnavailable is populated only for layer 4. true means layer 4
	// could not be loaded (no facts source wired, or the load failed) — an
	// UNKNOWN state, never to be read as "this site has no facts". false
	// means the load succeeded, whatever it found — including nothing, which
	// is then a verified, known-empty result. See LayerContribution's doc
	// comment (model.go).
	// No omitempty: this field exists specifically to distinguish "known
	// false" (facts loaded successfully, this site genuinely has none) from
	// absence. With omitempty, false is never emitted, so the wire could not
	// tell "known empty" apart from "an older server" or "the field got
	// dropped somewhere in the DTO mapping" — the exact conflation this field
	// was added to prevent, recreated one layer up.
	FactsUnavailable bool   `json:"facts_unavailable"`
	Session          string `json:"session,omitempty"`
	Bytes            int    `json:"bytes"`
	Truncated        bool   `json:"truncated"`
}

type effectiveContextDTO struct {
	SiteID       string                 `json:"site_id"`
	Layers       []layerContributionDTO `json:"layers"`
	Restrictions RestrictionSet         `json:"restrictions"`
	TotalBytes   int                    `json:"total_bytes"`
	BudgetBytes  int                    `json:"budget_bytes"`
	Truncated    bool                   `json:"truncated"`
}

func toEffectiveContextDTO(rc ResolvedContext) effectiveContextDTO {
	layers := make([]layerContributionDTO, 0, len(rc.Layers))
	for _, l := range rc.Layers {
		layers = append(layers, layerContributionDTO{
			Layer:            l.Layer,
			Name:             l.Name,
			Restrictions:     l.Restrictions,
			Guidance:         l.Guidance,
			Facts:            l.Facts,
			FactsUnavailable: l.FactsUnavailable,
			Session:          l.Session,
			Bytes:            l.Bytes,
			Truncated:        l.Truncated,
		})
	}
	return effectiveContextDTO{
		SiteID:       rc.SiteID.String(),
		Layers:       layers,
		Restrictions: rc.Restrictions,
		TotalBytes:   rc.TotalBytes,
		BudgetBytes:  rc.BudgetBytes,
		Truncated:    rc.Truncated,
	}
}

// --- request bodies ----------------------------------------------------------

// patchBody is the request body for PATCH .../context. A nil Restrictions or
// Guidance means "leave unchanged" — see applyPatch (service.go). BaseVersion
// is required (this package's answer to ADR-064 open question 2): 0 means
// "I believe no context has been authored yet".
type patchBody struct {
	BaseVersion  *int64          `json:"base_version"`
	Restrictions *RestrictionSet `json:"restrictions"`
	Guidance     *GuidanceSet    `json:"guidance"`
}
