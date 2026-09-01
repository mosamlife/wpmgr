package mcp

import (
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// THE TYPED PARTIAL-FAILURE ENVELOPE.
//
// Every fleet-wide tool answers with this shape. It exists so that a partial
// read is expressible: before it, a tool returned rendered text or an error,
// and "reachable 19 of 22, 3 unanswered" had no representation at all. A fleet
// answer assembled over an incomplete read and presented as a clean answer is
// how a model tells someone something confidently false.
//
// ---------------------------------------------------------------------------
// THE COUNTING RULE, WHICH IS THE WHOLE SECURITY PROPERTY OF THIS FILE.
//
//	Asked counts THE CALLER'S OWN RESOLVED SCOPE. It never counts the tenant.
//
// A site outside the connection's scope is not refused, not greyed, not
// counted. It is ABSENT, and it must be absent from every field AND from every
// arithmetic relationship between the fields. Presence-only exists so that a
// capability gap is legible; a tenancy boundary must leak nothing, INCLUDING
// THE EXISTENCE OF A SITE. "You cannot touch clientname.com" already tells the
// asker that clientname.com exists and is one of ours.
//
// The invariant that makes the numbers safe is:
//
//	Asked == OK + len(Refusals)      -- always, exactly
//	Asked == auth.Sites.Len()        -- the caller's scope, never the tenant's
//
// Because Asked is closed over the caller's own scope, the identity balances
// using only sites the caller already knows about. There is no residual for a
// reader to subtract, so no combination of the returned numbers has a term
// that depends on how many sites the tenant holds. That is the difference
// between "hidden" and "hidden and not inferable", and only the second one is
// a tenancy boundary.
//
// THE CORRESPONDING TRAP, STATED SO NOBODY REINTRODUCES IT: any count derived
// from a TENANT-WIDE read is disqualified from this envelope, even when it
// looks like a harmless completeness flag. A page bound taken over the tenant
// and reported to a site-scoped caller is a tenant-cardinality disclosure
// wearing the shape of a truncation notice. See scopeRelativeCompleteness.
// ---------------------------------------------------------------------------

// RefusalCode is a VISIBLE refusal reason -- one the caller is entitled to be
// told, because it names something an operator can act on.
//
// IT IS A STRUCT WITH AN UNEXPORTED FIELD, AND THAT IS LOAD-BEARING RATHER
// THAN FUSSY. A bare `type RefusalCode string` lets any caller in any package
// write RefusalCode("site_out_of_scope") and hand it to the envelope, and the
// only thing standing between that line and a tenancy leak would be a test
// nobody runs on the day it is written. With an unexported field the set of
// constructible values is exactly the set of package-level vars below: a
// composite literal from outside the package is a compile error, and the zero
// value carries an empty code that newRefusal refuses.
//
// THERE IS DELIBERATELY NO site_out_of_scope VALUE ANYWHERE IN THIS TYPE.
// It is not defined and commented out, and it is not defined-but-filtered.
// It does not exist, so no code path can carry it onto the wire, and the
// compiler -- not a reviewer, and not a test -- is what enforces that.
type RefusalCode struct{ code string }

// String renders the wire form.
func (r RefusalCode) String() string { return r.code }

// MarshalText makes RefusalCode serialise as its bare code inside the
// envelope's JSON, rather than as an object wrapping an unexported field.
func (r RefusalCode) MarshalText() ([]byte, error) {
	if r.code == "" {
		// A zero-value code reaching the wire would render as "" and read as
		// "refused for no reason", which is strictly worse than an error: it
		// is a refusal the caller cannot act on and cannot even report.
		return nil, fmt.Errorf("refusal code is the zero value and has no wire form")
	}
	return []byte(r.code), nil
}

// visibleRefusalCodes is the closed set of codes that may exist, keyed by wire
// form. It is what UnmarshalText resolves against.
//
// IT IS BUILT FROM THE SAME VARS THE REST OF THE PACKAGE USES, so a code
// cannot be admitted here without also being declared above -- and
// site_out_of_scope is declared nowhere, so it cannot appear here either.
func visibleRefusalCodes() map[string]RefusalCode {
	out := map[string]RefusalCode{}
	for _, c := range []RefusalCode{
		RefusalPolicyDenied,
		RefusalCapabilityDenied,
		RefusalConnectionDisabled,
		RefusalPluginMissing,
		RefusalVersionBelowFloor,
		RefusalAgentTooOld,
		RefusalInventoryStale,
		RefusalSiteUnread,
	} {
		out[c.code] = c
	}
	return out
}

// UnmarshalText resolves a wire code against the CLOSED set above and refuses
// anything else.
//
// THE VALIDATION IS THE POINT, NOT A CONVENIENCE. Without it, encoding/json
// would happily populate the unexported field with whatever bytes arrived, and
// the compile-time guarantee that no code can construct a
// site_out_of_scope RefusalCode would be undone by a deserialiser -- the
// unconstructible value would become constructible by anyone who could hand
// this type a string. A closed lookup keeps the two halves agreeing.
func (r *RefusalCode) UnmarshalText(b []byte) error {
	c, ok := visibleRefusalCodes()[string(b)]
	if !ok {
		return fmt.Errorf("%q is not a visible refusal code", string(b))
	}
	*r = c
	return nil
}

// The visible refusal codes: the six layers that EXPLAIN.
//
// Layer 3, the connection's site scope, is absent from this list on purpose
// and is the one layer that HIDES. See the counting rule above.
var (
	// RefusalPolicyDenied is layer 1: organisation policy. Carries who
	// disabled the capability and when.
	RefusalPolicyDenied = RefusalCode{"policy_denied"}

	// RefusalCapabilityDenied is layer 2: the credential's explicit
	// capability list does not include what this tool requires. Names the
	// permission.
	RefusalCapabilityDenied = RefusalCode{"capability_denied"}

	// RefusalConnectionDisabled is layer 4: an operator's per-connection
	// narrowing of layer 2.
	RefusalConnectionDisabled = RefusalCode{"connection_disabled"}

	// RefusalPluginMissing is layer 5: the site capability document says the
	// plugin is not present.
	RefusalPluginMissing = RefusalCode{"plugin_missing"}

	// RefusalVersionBelowFloor is layer 5: present, but below the required
	// version. Carries installed vs required.
	RefusalVersionBelowFloor = RefusalCode{"version_below_floor"}

	// RefusalAgentTooOld is layer 5: the agent itself is below the floor.
	RefusalAgentTooOld = RefusalCode{"agent_too_old"}

	// RefusalInventoryStale is layer 6: the capability document is older than
	// its freshness window. MUST carry computed_at and age_hours -- see
	// Refusal.validate. A stale marker without its evidence is a worse answer
	// than no answer, because it cannot be acted on.
	RefusalInventoryStale = RefusalCode{"inventory_stale"}

	// RefusalSiteUnread is NOT ONE OF THE SEVEN LAYERS, and it is named
	// separately so nobody reads it as one. It is a control-plane fact: the
	// site is inside the caller's scope and this call did not manage to read
	// it, because the bounded page the control plane fetches did not contain
	// it.
	//
	// It exists because the alternative is worse. Without it, an in-scope
	// site that went unread is simply missing from the result, and a result
	// that quietly holds fewer sites than the caller's scope reads as a
	// complete answer about a smaller fleet. That is the same silent-partial
	// failure the envelope exists to abolish, so it gets a visible code and
	// is counted in Refused like any other.
	RefusalSiteUnread = RefusalCode{"site_unread"}
)

// Refusal is one site's refusal, with the evidence that makes it actionable.
type Refusal struct {
	// SiteID is always a site INSIDE the caller's resolved scope. Nothing
	// else can reach this struct: refusals are only ever minted for sites the
	// caller was already entitled to see.
	SiteID string `json:"site_id"`

	// Code is the visible reason. It can only ever be one of the vars above.
	Code RefusalCode `json:"code"`

	// Detail is the human-readable half, naming what an operator must change.
	Detail string `json:"detail"`

	// ComputedAt and AgeHours are the evidence for RefusalInventoryStale, and
	// are required for it. They are pointers so that "not applicable" is JSON
	// null rather than a zero time that reads as 'year 1' or an age of 0 that
	// reads as 'computed just now' -- the exact inversion of the truth.
	ComputedAt *string  `json:"computed_at,omitempty"`
	AgeHours   *float64 `json:"age_hours,omitempty"`

	// Installed and Required are the evidence for the version refusals.
	Installed string `json:"installed,omitempty"`
	Required  string `json:"required,omitempty"`
}

// validate enforces that each visible reason arrives with the evidence that
// makes it actionable.
func (r Refusal) validate() error {
	if r.Code.code == "" {
		return fmt.Errorf("refusal for site %s has the zero-value code", r.SiteID)
	}
	if r.SiteID == "" {
		return fmt.Errorf("refusal with code %s names no site", r.Code)
	}
	switch r.Code {
	case RefusalInventoryStale:
		// The brief's rule, enforced rather than documented: inventory_stale
		// without computed_at and age_hours cannot be acted on.
		if r.ComputedAt == nil || r.AgeHours == nil {
			return fmt.Errorf(
				"refusal %s for site %s must carry computed_at and age_hours", r.Code, r.SiteID)
		}
	case RefusalVersionBelowFloor, RefusalAgentTooOld:
		if r.Installed == "" || r.Required == "" {
			return fmt.Errorf(
				"refusal %s for site %s must carry installed and required versions", r.Code, r.SiteID)
		}
	}
	return nil
}

// Envelope is the typed partial-failure answer for a fleet-wide tool.
type Envelope struct {
	// Asked is the number of sites IN THE CALLER'S RESOLVED SCOPE that this
	// call covered. See the counting rule at the top of this file: it is
	// never the tenant's site count, and no field here is derived from one.
	Asked int `json:"asked"`

	// OK is the number that answered.
	OK int `json:"ok"`

	// Refused is len(Refusals), carried explicitly so a reader never has to
	// subtract to learn it. Subtraction is the habit this envelope is trying
	// to make unnecessary.
	Refused int `json:"refused"`

	// Refusals is one entry per refused site, each with its evidence.
	Refusals []Refusal `json:"refusals"`
}

// NewEnvelope builds and CHECKS an envelope. It is the only way to make one
// with a populated Refused count, so the balance invariant cannot be skipped.
//
// asked MUST be the caller's own scope cardinality. The signature cannot
// enforce that on its own -- an int is an int -- so the one call site that
// builds a fleet envelope reads it from auth.Sites.Len() and nowhere else,
// and TestEnvelopeAskedIsScopeCardinality pins it.
func NewEnvelope(asked, ok int, refusals []Refusal) (Envelope, error) {
	for _, r := range refusals {
		if err := r.validate(); err != nil {
			return Envelope{}, err
		}
	}
	if asked < 0 || ok < 0 {
		return Envelope{}, fmt.Errorf("envelope counts must be non-negative: asked=%d ok=%d", asked, ok)
	}
	if ok+len(refusals) != asked {
		// THE BALANCE CHECK IS THE LEAK DETECTOR.
		//
		// If ok+refused ever fails to equal asked, some site was counted in
		// asked and then dropped without being accounted for -- which is
		// exactly what an out-of-scope site would look like if one ever
		// reached this far. The residual it would leave is the number a
		// reader subtracts to discover that hidden sites exist, so an
		// unbalanced envelope is refused rather than rendered.
		return Envelope{}, fmt.Errorf(
			"envelope does not balance: asked=%d ok=%d refused=%d (ok+refused must equal asked; "+
				"a residual here is a site counted in asked but not accounted for)",
			asked, ok, len(refusals))
	}
	if refusals == nil {
		// A nil slice renders as JSON null; an empty one renders as []. The
		// caller should see an empty list, not a missing field.
		refusals = []Refusal{}
	}
	sort.SliceStable(refusals, func(i, j int) bool { return refusals[i].SiteID < refusals[j].SiteID })
	return Envelope{Asked: asked, OK: ok, Refused: len(refusals), Refusals: refusals}, nil
}

// StaleRefusal builds a layer-6 refusal with its required evidence attached,
// so a caller cannot construct the evidence-less form by accident.
func StaleRefusal(siteID uuid.UUID, computedAt time.Time, now time.Time) Refusal {
	at := computedAt.UTC().Format(time.RFC3339)
	age := now.Sub(computedAt).Hours()
	return Refusal{
		SiteID:     siteID.String(),
		Code:       RefusalInventoryStale,
		Detail:     "the site capability document is older than its freshness window; refresh inventory for this site",
		ComputedAt: &at,
		AgeHours:   &age,
	}
}

// ---------------------------------------------------------------------------
// LAYER 7 IS NOT IMPLEMENTED HERE, AND THIS NOTE IS THE DELIVERABLE FOR IT.
//
// Layers 5 and 6 read a cache. A plugin can be deactivated between the plan
// and the call, so every WRITE must re-check the same tests at execution time
// and raise any of the layer-5/6 codes late, per site.
//
// Phase 1 has no write tools -- registryTools() is a closed literal holding
// one read tool -- so there is no execution path for a late re-check to guard.
// The envelope shape supports it already: a late refusal is the same Refusal
// with the same codes, appended at execution rather than at planning, and the
// balance check still holds because the site was in the caller's scope both
// times.
//
// WHAT IS DEFERRED, EXPLICITLY: there is no re-check call, and no test proves
// one runs, because there is nothing to re-check. The first write tool must
// bring the re-check with it. Saying that plainly is the honest report; a
// stub that "re-checks" nothing would read as done and pass its own test.
// ---------------------------------------------------------------------------
