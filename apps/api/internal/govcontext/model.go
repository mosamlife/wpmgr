// Package govcontext implements ADR-064: governed, versioned, human-authored
// context at two scopes — organisation (layer 2) and site (layer 3) of the
// seven-layer precedence order fixed by ADR-064 Decision 1 — plus the single
// shared resolver (Decision 8) that both the effective-context preview
// (GET .../context/effective) and any future model-facing assembly path call.
//
// Layer 7 (learned memory) is explicitly not built — see ADR-064's "Learned
// memory is deferred, not 'later'" section — and this package does not stub
// it: there is no field, no case, no placeholder for it anywhere below. The
// resolver walks layers 1 through 6 only. Layer 5 (approved skill
// instructions) has no backing store anywhere in this codebase yet (no Skill
// Store has shipped); it is walked structurally, alongside every other layer,
// but always contributes nothing until one exists — the same honest-absence
// pattern ADR-064 itself uses for the layer-1 restriction set (see
// layer1Restrictions below) and for site-to-organisation transfer.
//
// The package holds plain view-models decoupled from internal/db/sqlc row
// types; repo.go maps sqlc rows <-> these models, the same shape internal/perf
// uses.
//
// # Open question 2 — PATCH concurrency
//
// Decision: PATCH requires a mandatory base_version field (patchBody in
// dto.go). The service loads the latest version; if base_version does not
// match, the write is refused with 409 context_version_conflict before
// anything is checked or written, naming both the supplied and the actual
// current version. This is a conditional write expressed as a body field
// rather than an HTTP If-Match/ETag header: this codebase has no existing
// ETag precedent to extend, and a body field keeps both PATCH-time checks
// (this one and the widen-check) in one place, using the same structured
// domain.Error.Details convention every other 4xx here already uses. The
// application-level check above is advisory (it saves a wasted round trip);
// the AUTHORITATIVE guard is m122's UNIQUE (subject, version) index, which
// turns a genuinely concurrent double-write into SQLSTATE 23505
// (errVersionConflict in repo.go) rather than a silently lost update — the
// service maps that race to the identical 409 response. No ADR amendment is
// needed: open question 2 explicitly delegates the wire-format decision to
// backend-architect without asking the ADR text itself to fix one ("recording
// that the choice is unmade is this ADR's job; the wire format is not").
//
// # Open question 4 — nothing bounds a stored row's size
//
// Decision: no write-time size CHECK or new domain error is added. Decision
// 9 specifies truncation as a property of RESOLUTION (Decision 8's reader),
// not of storage, and m122's own header already argues that a storage-time
// refusal would be different behaviour needing a byte threshold ADR-064 never
// states — "designing past the specification". Decision 13 enumerates exactly
// two write refusals (409 widening, 422 credential-shaped); adding a third
// for size would need an ADR amendment this slice does not make. The only
// practical ceiling on a write is the same one every other PATCH/POST body in
// this codebase already gets for free: bindJSON's 1 MiB decode limit
// (handler.go), which is ordinary HTTP transport hygiene, not an ADR-scoped
// write refusal, so it adds no new reason code and needs no amendment. If DB
// bloat or a resource-exhaustion concern ever makes an explicit storage cap
// worth having, that is a deliberate, separate ADR-064 amendment — not
// something to back into silently here.
package govcontext

import (
	"time"

	"github.com/google/uuid"
)

// Provenance is the closed set of reasons a version exists, mirroring m122's
// database CHECK exactly. "import" is deliberately absent — see m122's header.
type Provenance string

const (
	ProvenanceManual   Provenance = "manual"
	ProvenanceRestore  Provenance = "restore"
	ProvenanceTransfer Provenance = "transfer"
)

// AuthorType is the closed set of principal kinds that can author a context
// version, mirroring m122's author_type CHECK.
type AuthorType string

const (
	AuthorUser   AuthorType = "user"
	AuthorAPIKey AuthorType = "api_key"
	AuthorSystem AuthorType = "system"
)

// RestrictionSet is ADR-064 Decision 3's closed, structured "restrictions"
// kind: named boundaries a policy can set ("never do X"), where "does this
// edit widen what a higher layer set" is a well-defined comparison. Every
// field here is a deny-list. The never-widen check (Decision 4) runs over
// exactly these fields and no others — see widen.go.
//
// This is the "inner shape ... fixed in Go" m122's header assigns to S4
// (ADR-064 fixes no restriction vocabulary of its own). It is a closed,
// extensible-without-migration set: adding a field here is a Go change, not a
// schema change, because the column beneath it is jsonb by design.
type RestrictionSet struct {
	// ForbiddenTools names tool/action identifiers the model must never invoke
	// for this subject (e.g. a destructive site operation).
	ForbiddenTools []string `json:"forbidden_tools,omitempty"`
	// ForbiddenDomains names external domains the model must never fetch,
	// link to, or treat as a data source for this subject.
	ForbiddenDomains []string `json:"forbidden_domains,omitempty"`
	// ForbiddenTopics names subject matter the model must never discuss or
	// act on for this subject (e.g. a regulated-content boundary).
	ForbiddenTopics []string `json:"forbidden_topics,omitempty"`
}

// IsEmpty reports whether every deny-list in the set is empty.
func (r RestrictionSet) IsEmpty() bool {
	return len(r.ForbiddenTools) == 0 && len(r.ForbiddenDomains) == 0 && len(r.ForbiddenTopics) == 0
}

// GuidanceSet is ADR-064 Decision 3's "guidance" kind: free text, illustrated
// by the ADR itself as "brand voice, audience, terminology notes, style
// preferences". "Wider" and "narrower" are not defined relations over prose —
// Decision 1 and the Consequences section are both explicit that no mechanical
// check runs over these fields, and none is built here. See widen.go's doc
// comment for why building one would be worse than the honest absence.
type GuidanceSet struct {
	BrandVoice  string `json:"brand_voice,omitempty"`
	Audience    string `json:"audience,omitempty"`
	Terminology string `json:"terminology,omitempty"`
	Style       string `json:"style,omitempty"`
}

// IsEmpty reports whether every field in the set is empty.
func (g GuidanceSet) IsEmpty() bool {
	return g.BrandVoice == "" && g.Audience == "" && g.Terminology == "" && g.Style == ""
}

// Snapshot is the full content of one version row — "the full resulting
// snapshot" ADR-064 Decision 3 requires every version to carry.
type Snapshot struct {
	Restrictions RestrictionSet `json:"restrictions"`
	Guidance     GuidanceSet    `json:"guidance"`
}

// layer1Restrictions is WPMgr's immutable, code-defined security policy —
// ADR-064 Decision 1's layer 1. Decision 4 is explicit that this is "not a
// row in either context table" — it is code, consulted by both the write-time
// widen-check and the read-time resolution walk.
//
// It is empty today. No product requirement has yet named a concrete
// WPMgr-wide rule for this layer, and ADR-064 does not invent one on this
// slice's behalf — S4's job is the mechanism (a layer-1 set both walks
// genuinely consult), not its eventual content. An empty RestrictionSet
// contributes no forbidden items to the read-time union and can never itself
// be "widened" by a lower layer (there is nothing in it to remove), so every
// org/site write still runs the real comparison against real code — it is
// just compared against zero items today. When a concrete WPMgr-wide rule is
// decided, it is added here, in Go, never as a migration (Decision 4).
var layer1Restrictions = RestrictionSet{}

// Version is one row of an organisation's or a site's context history —
// ADR-064 Decision 3's append-only version log, domain-model form.
type Version struct {
	ID       uuid.UUID
	TenantID uuid.UUID // the stamp: for site rows, the org that owned the site AT THIS WRITE (Decision 3)
	SiteID   uuid.UUID // uuid.Nil for an organisation-scope version
	Version  int64

	Snapshot Snapshot

	AuthorType AuthorType
	AuthorID   uuid.UUID // uuid.Nil when AuthorType == AuthorSystem

	Provenance            Provenance
	RestoredFromVersionID uuid.UUID // uuid.Nil unless Provenance == ProvenanceRestore

	CreatedAt time.Time
}

// SessionInput is ADR-064 layer 6: information scoped to one conversation or
// one run, supplied by the caller, never persisted and never read from
// storage. A nil SessionInput (or its zero value) means "no session in
// progress" — exactly what Decision 8 requires the effective-context preview
// to pass, since a preview request has no live run behind it.
type SessionInput struct {
	// Text is the free-form session-scoped context for this run. Session
	// content is layer 6 and carries no restrictions of its own (only layers
	// 1-3 do, per Decision 3) — it can only ever be guidance-shaped.
	Text string
}

// SiteFacts is ADR-064 layer 4: what the control plane or the agent has
// observed about a site, delivered as data the model reads, never as
// directions it follows (Decision 1, Vocabulary section). This package does
// not own fact collection; SiteFactsProvider (resolver.go) is the seam a
// caller wires to whatever already-stored facts exist. A provider that is not
// wired, or that has nothing recorded yet for this site, contributes an empty
// SiteFacts — that is a legitimate state (a never-scanned site), not a
// resolution failure; only layers 2 and 3 (Decision 3's stored tables) can
// fail resolution outright (Decision 14).
type SiteFacts struct {
	WPVersion   string `json:"wp_version,omitempty"`
	PHPVersion  string `json:"php_version,omitempty"`
	Multisite   bool   `json:"multisite,omitempty"`
	ActiveTheme string `json:"active_theme,omitempty"`
}

// IsEmpty reports whether no fact has been recorded.
func (f SiteFacts) IsEmpty() bool {
	return f.WPVersion == "" && f.PHPVersion == "" && !f.Multisite && f.ActiveTheme == ""
}

// LayerContribution is one layer's surviving contribution to a resolved
// context, in Decision 1 order — what Decision 8's preview renders per layer,
// and the unit a model-facing caller receives inside Decision 11's (not yet
// built) fence. Restrictions and Guidance are never merged across layers: a
// lower layer's guidance is "always taken as additional context, never as a
// retraction of a higher layer's guidance" (Decision 4), so each layer's own
// contribution is kept separate here rather than flattened into one blob.
type LayerContribution struct {
	Layer        int    // 1-6, per ADR-064 Decision 1. Never 7.
	Name         string // human-readable layer name, for the preview
	Restrictions RestrictionSet
	Guidance     GuidanceSet
	Facts        *SiteFacts // populated only for Layer == 4
	Session      string     // populated only for Layer == 6

	Bytes     int  // this layer's serialised size, counted before truncation
	Truncated bool // true if this layer's contribution was cut short by budget (Decision 9)
}

// ResolvedContext is the output of Resolve (resolver.go) — Decision 8's
// "exact resolved text a given site's model-facing surface will be handed".
type ResolvedContext struct {
	SiteID uuid.UUID

	// Layers holds each layer's surviving contribution, in Decision 1 order
	// (1..6; never 7). A layer with nothing to contribute is still present
	// (empty Restrictions/Guidance/Facts/Session) so the preview can show "this
	// layer added nothing" rather than omitting the layer silently.
	Layers []LayerContribution

	// Restrictions is the READ-TIME UNION of every layer 1-3 restriction
	// (Decision 4: "a restriction may only be added to or left alone by every
	// layer below the one that set it").
	//
	// This union is NOT belt-and-braces over a write-time guarantee — it is
	// the ONLY thing that holds the invariant. A leaf's STORED restrictions
	// are routinely stale relative to a higher layer: PatchOrgContext never
	// touches any site row, so the instant an organisation narrows its
	// policy, every existing site row stops restating it — permanently, not
	// for a transaction-width window — and PatchSiteContext deliberately
	// skips its own widen-check on a guidance-only write (service.go) rather
	// than compare a field the request never touched against the org's
	// current value. The write-time check in widen.go only ever runs when a
	// caller actually proposes a new restrictions value for that layer; it
	// is real protection against a caller widening what THEY hold, but it
	// is not what keeps a stale, unrelated layer's row from under-reporting
	// what is actually enforced. This field, recomputed from every layer's
	// CURRENT row on every call, is what does that.
	//
	// This field is computed from the UNTRUNCATED layer 2/3 snapshots and is
	// NEVER subject to Decision 9's byte-budget truncation, unlike each
	// entry in Layers below. Decision 4's second enforcement path — the
	// tool-dispatch chokepoint that consults "this Decision's resolved
	// restriction set before invocation" — must not be weakened by how much
	// prose happened to fit in a budget sized for a model's context window;
	// that chokepoint reads THIS field, never a layer's (possibly truncated)
	// display copy. Only the free-text prose shown per layer in Layers is
	// ever truncated.
	Restrictions RestrictionSet

	TotalBytes  int
	BudgetBytes int
	Truncated   bool // true if ANY layer was truncated
}
