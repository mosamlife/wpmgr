package govcontext

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Audit actions. Plain strings (audit.Event.Action has no enum), dot-separated
// to match this codebase's other context.* naming (authz.PermOrgContextRead
// etc.).
const (
	actionOrgContextPatched   = "context.org.patched"
	actionOrgContextRestored  = "context.org.restored"
	actionSiteContextPatched  = "context.site.patched"
	actionSiteContextRestored = "context.site.restored"
)

// Service is the ADR-064 write/read chokepoint. Every PATCH and restore
// route in handler.go funnels through PatchOrgContext / RestoreOrgContext /
// PatchSiteContext / RestoreSiteContext, and each of those runs, in order:
// the never-widen check (Decision 4), the secret scan (Decision 10), the
// version-conflict check (open question 2, below), and the fail-closed audit
// append in the SAME transaction as the version insert (Decision 7). No other
// path in this codebase writes to either context table.
type Service struct {
	repo     *Repo
	audit    *audit.Recorder
	resolver *Resolver
}

// NewService wires a Service. resolver may share repo as its ContextStore
// (the normal case — see main.go); it is a separate parameter so a
// SiteFactsProvider (layer 4) can be wired independently of storage.
func NewService(repo *Repo, rec *audit.Recorder, resolver *Resolver) *Service {
	return &Service{repo: repo, audit: rec, resolver: resolver}
}

// Actor bundles the identity a write is attributed to. Every PATCH/restore
// method takes one; handler.go builds it from domain.Principal.
type Actor struct {
	Type AuthorType
	ID   uuid.UUID // uuid.Nil for AuthorSystem
}

func (a Actor) actorIDString() string {
	if a.ID == uuid.Nil {
		return ""
	}
	return a.ID.String()
}

// --- organisation (layer 2) --------------------------------------------------

// GetOrgContext returns the organisation's current context. Version.Version
// == 0 means no context has ever been authored — a legitimate, populated 200
// response (empty restrictions/guidance), never a 404: ADR-064 treats "no
// version yet" as the empty state of layer 2, not its absence as a resource.
func (s *Service) GetOrgContext(ctx context.Context, tenantID uuid.UUID) (Version, error) {
	v, err := s.repo.LatestOrgVersion(ctx, tenantID)
	if errors.Is(err, ErrNotFound) {
		return Version{TenantID: tenantID}, nil
	}
	return v, err
}

func (s *Service) ListOrgContextVersions(ctx context.Context, tenantID uuid.UUID, cursor int64, limit int32) ([]Version, error) {
	return s.repo.ListOrgVersions(ctx, tenantID, cursor, limit)
}

func (s *Service) GetOrgContextVersion(ctx context.Context, tenantID, id uuid.UUID) (Version, error) {
	v, err := s.repo.GetOrgVersionByID(ctx, tenantID, id)
	if errors.Is(err, ErrNotFound) {
		return Version{}, domain.NotFound("context_version_not_found", "context version not found")
	}
	return v, err
}

// PatchOrgContextInput is a partial write: a nil field means "leave this half
// of the snapshot unchanged from the base version". BaseVersion is the
// version the caller last read (0 for "no context yet") — see the package
// doc comment on ADR-064 open question 2 (PATCH concurrency) for why this is
// mandatory, not optional.
type PatchOrgContextInput struct {
	BaseVersion  int64
	Restrictions *RestrictionSet
	Guidance     *GuidanceSet
}

// PatchOrgContext applies a partial write onto the organisation's latest
// snapshot and authors a new version, or refuses per Decision 13's exactly
// two write refusals:
//   - 409 context_widen_forbidden  — the proposed restrictions would remove
//     something WPmgr's layer-1 policy set (org context has no higher writable
//     layer to check against, only the fixed layer-1 set).
//   - 409 context_version_conflict — BaseVersion does not match the current
//     version (this package's answer to open question 2 — see doc comment
//     below on the concurrency design).
//   - 422 context_secret_detected  — the proposed snapshot contains something
//     credential-shaped (Decision 10).
func (s *Service) PatchOrgContext(ctx context.Context, tenantID uuid.UUID, in PatchOrgContextInput, actor Actor) (Version, error) {
	current, err := s.repo.LatestOrgVersion(ctx, tenantID)
	currentVersion := int64(0)
	base := Snapshot{}
	if err == nil {
		currentVersion = current.Version
		base = current.Snapshot
	} else if !errors.Is(err, ErrNotFound) {
		return Version{}, err
	}

	if in.BaseVersion != currentVersion {
		return Version{}, versionConflictError(currentVersion, in.BaseVersion)
	}

	next := applyPatch(base, in.Restrictions, in.Guidance)

	// Only run the widen-check when THIS REQUEST actually proposes new
	// restrictions. A guidance-only patch (in.Restrictions == nil) carries the
	// organisation's own PREVIOUSLY-STORED restrictions forward unchanged
	// (applyPatch) — that stored value may already be stale relative to
	// layer 1 the moment layer 1 ever gains real content (it is empty today,
	// so this branch is currently unreachable in practice, but the shape is
	// identical to the site-scope bug this comment's sibling in
	// PatchSiteContext fixes, and is guarded here for the same reason: never
	// check a field the request does not touch against what the row happens
	// to already contain).
	if in.Restrictions != nil {
		if verr := checkNoWiden(next.Restrictions, []namedLayer{
			{Layer: 1, Name: "WPMgr security policy", Restrictions: layer1Restrictions},
		}); verr != nil {
			return Version{}, verr
		}
	}
	if verr := checkNoSecret(next); verr != nil {
		return Version{}, verr
	}
	// THE DELIVERY CEILING (owner ruling, reversing this package's answer to
	// ADR-064 open question 4 — see MaxDeliverableInstructionBytes). The layer
	// name passed here is the one Resolve gives layer 2, so this measures the
	// same rendered bytes the assistant surface will measure.
	//
	// EXISTING ROWS ARE NOT MIGRATED AND ARE NOT REWRITTEN. A row authored
	// before this ceiling existed stays exactly as it is: readable, listable,
	// diffable and editable. What changes for it is that the next WRITE must
	// bring it under the ceiling, and that the assistant refuses to answer
	// while it is over (ModelInstructions, render.go) instead of delivering a
	// clipped version. No migration exists or is needed: this is a Go-side
	// validation over rendered output, not a column constraint.
	if verr := checkDeliverable("organisation default", next); verr != nil {
		return Version{}, verr
	}

	expectVersion := currentVersion + 1
	v, err := s.repo.CreateOrgVersion(ctx, tenantID, expectVersion, CreateOrgVersionInput{
		Snapshot:   next,
		AuthorType: actor.Type,
		AuthorID:   actor.ID,
		Provenance: ProvenanceManual,
	}, func(tx pgx.Tx, versionID uuid.UUID) error {
		_, aerr := s.audit.RecordInTx(ctx, tx, audit.Event{
			TenantID:   tenantID,
			ActorType:  string(actor.Type),
			ActorID:    actor.actorIDString(),
			Action:     actionOrgContextPatched,
			TargetType: "org_context_version",
			TargetID:   versionID.String(),
			Metadata:   map[string]any{"version": expectVersion},
		})
		return aerr
	})
	if errors.Is(err, errVersionConflict) {
		return Version{}, versionConflictError(currentVersion, in.BaseVersion)
	}
	return v, err
}

// RestoreOrgContext creates a new organisation version whose snapshot equals
// versionID's, provenance "restore" (ADR-064 Decision 5). It is not a back
// door around the widen-check, the secret scan, or the audit transaction —
// all three run exactly as they do for PatchOrgContext.
func (s *Service) RestoreOrgContext(ctx context.Context, tenantID, versionID uuid.UUID, actor Actor) (Version, error) {
	target, err := s.repo.GetOrgVersionByID(ctx, tenantID, versionID)
	if errors.Is(err, ErrNotFound) {
		return Version{}, domain.NotFound("context_version_not_found", "context version not found")
	}
	if err != nil {
		return Version{}, err
	}

	current, err := s.repo.LatestOrgVersion(ctx, tenantID)
	currentVersion := int64(0)
	if err == nil {
		currentVersion = current.Version
	} else if !errors.Is(err, ErrNotFound) {
		return Version{}, err
	}

	if verr := checkNoWiden(target.Snapshot.Restrictions, []namedLayer{
		{Layer: 1, Name: "WPMgr security policy", Restrictions: layer1Restrictions},
	}); verr != nil {
		return Version{}, verr
	}
	if verr := checkNoSecret(target.Snapshot); verr != nil {
		return Version{}, verr
	}
	// A restore is a write, so it meets the same ceiling. Restoring a
	// pre-ceiling version that is over it is refused with the size named,
	// rather than succeeding into a row the assistant then refuses to read.
	if verr := checkDeliverable("organisation default", target.Snapshot); verr != nil {
		return Version{}, verr
	}

	expectVersion := currentVersion + 1
	v, err := s.repo.CreateOrgVersion(ctx, tenantID, expectVersion, CreateOrgVersionInput{
		Snapshot:              target.Snapshot,
		AuthorType:            actor.Type,
		AuthorID:              actor.ID,
		Provenance:            ProvenanceRestore,
		RestoredFromVersionID: versionID,
	}, func(tx pgx.Tx, newVersionID uuid.UUID) error {
		_, aerr := s.audit.RecordInTx(ctx, tx, audit.Event{
			TenantID:   tenantID,
			ActorType:  string(actor.Type),
			ActorID:    actor.actorIDString(),
			Action:     actionOrgContextRestored,
			TargetType: "org_context_version",
			TargetID:   newVersionID.String(),
			Metadata:   map[string]any{"version": expectVersion, "restored_from_version_id": versionID.String()},
		})
		return aerr
	})
	if errors.Is(err, errVersionConflict) {
		return Version{}, restoreConflictError()
	}
	return v, err
}

// DiffOrgContext compares versionID against its immediately-prior version.
// isBaseline=true means "no eligible predecessor, render a baseline" — the
// case for a genuine first version (ADR-064 Decision 5). Org context has no
// organisation-transfer analogue, so a baseline here only ever means "this is
// version 1".
func (s *Service) DiffOrgContext(ctx context.Context, tenantID, versionID uuid.UUID) (target Version, prior *Version, isBaseline bool, err error) {
	target, err = s.repo.GetOrgVersionByID(ctx, tenantID, versionID)
	if errors.Is(err, ErrNotFound) {
		return Version{}, nil, false, domain.NotFound("context_version_not_found", "context version not found")
	}
	if err != nil {
		return Version{}, nil, false, err
	}
	if target.Version <= 1 {
		return target, nil, true, nil
	}
	p, perr := s.repo.GetOrgVersionByVersion(ctx, tenantID, target.Version-1)
	if errors.Is(perr, ErrNotFound) {
		return target, nil, true, nil
	}
	if perr != nil {
		return Version{}, nil, false, perr
	}
	return target, &p, false, nil
}

// --- site (layer 3) ----------------------------------------------------------

func (s *Service) GetSiteContext(ctx context.Context, tenantID, siteID uuid.UUID) (Version, error) {
	v, err := s.repo.LatestSiteVersion(ctx, tenantID, siteID)
	if errors.Is(err, ErrNotFound) {
		return Version{TenantID: tenantID, SiteID: siteID}, nil
	}
	return v, err
}

func (s *Service) ListSiteContextVersions(ctx context.Context, tenantID, siteID uuid.UUID, cursor int64, limit int32) ([]Version, error) {
	return s.repo.ListSiteVersions(ctx, tenantID, siteID, cursor, limit)
}

func (s *Service) GetSiteContextVersion(ctx context.Context, tenantID, siteID, id uuid.UUID) (Version, error) {
	v, err := s.repo.GetSiteVersionByID(ctx, tenantID, siteID, id)
	if errors.Is(err, ErrNotFound) {
		return Version{}, domain.NotFound("context_version_not_found", "context version not found")
	}
	return v, err
}

// PatchSiteContextInput mirrors PatchOrgContextInput at site scope.
type PatchSiteContextInput struct {
	BaseVersion  int64
	Restrictions *RestrictionSet
	Guidance     *GuidanceSet
}

// PatchSiteContext is PatchOrgContext's site-scope sibling. The widen-check
// runs against BOTH layers above site (Decision 4: "checked against both its
// organisation's layer-2 policy and WPMgr's layer-1 policy"), outermost
// first, so a rejection always names the FIRST (highest, outermost) layer it
// violates.
func (s *Service) PatchSiteContext(ctx context.Context, tenantID, siteID uuid.UUID, in PatchSiteContextInput, actor Actor) (Version, error) {
	current, err := s.repo.LatestSiteVersion(ctx, tenantID, siteID)
	currentVersion := int64(0)
	base := Snapshot{}
	if err == nil {
		currentVersion = current.Version
		base = current.Snapshot
	} else if !errors.Is(err, ErrNotFound) {
		return Version{}, err
	}

	if in.BaseVersion != currentVersion {
		return Version{}, versionConflictError(currentVersion, in.BaseVersion)
	}

	next := applyPatch(base, in.Restrictions, in.Guidance)

	// Only run the widen-check when THIS REQUEST actually proposes new
	// restrictions (in.Restrictions != nil). A guidance-only patch carries the
	// site's own PREVIOUSLY-STORED restrictions forward unchanged (applyPatch)
	// — and that stored value is routinely stale the moment the organisation
	// narrows its policy AFTER this site's last restriction write, because
	// PatchOrgContext never touches any site row. Comparing that carried-
	// forward, stale value against the organisation's CURRENT restrictions
	// would refuse a write that never touches restrictions at all: every site
	// under a newly-narrowed org would be locked out of even a guidance-only
	// edit, reporting "would remove X" for an X the caller never mentioned.
	// Checking only when the caller actually supplies Restrictions compares
	// against what the request changes, not what the row happens to carry.
	//
	// This does NOT weaken enforcement: the resolved restriction set a caller
	// is actually held to is unionRestrictions' READ-TIME union of layer 1 +
	// the organisation's CURRENT row + the site's CURRENT row (resolver.go),
	// recomputed fresh on every read — never the site's possibly-stale stored
	// value alone. See model.go's ResolvedContext.Restrictions doc comment,
	// which this fix's discovery corrected from a false "the write-time check
	// already guarantees a superset" claim to this honest one: the read-time
	// union is what actually holds the invariant, not the write-time check.
	//
	// RestoreSiteContext below deliberately does NOT take this shortcut: a
	// restore always proposes the target version's FULL stored restrictions
	// as this write's value, which is a genuine, explicit proposal for that
	// field — never "leave unchanged" — so it must always be checked.
	if in.Restrictions != nil {
		orgSnap, _, oerr := s.repo.LatestOrgSnapshot(ctx, tenantID)
		if oerr != nil {
			return Version{}, oerr
		}
		if verr := checkNoWiden(next.Restrictions, []namedLayer{
			{Layer: 1, Name: "WPMgr security policy", Restrictions: layer1Restrictions},
			{Layer: 2, Name: "organisation default", Restrictions: orgSnap.Restrictions},
		}); verr != nil {
			return Version{}, verr
		}
	}
	if verr := checkNoSecret(next); verr != nil {
		return Version{}, verr
	}
	// The same ceiling at site scope. No site-scoped model surface exists yet
	// (the two shipped tools are fleet-wide and resolve at organisation
	// scope), so this is applied AHEAD of that surface rather than because of
	// it: without it, every site row authored between now and then could be
	// written at up to the 64 KiB resolution budget and be undeliverable the
	// day the surface ships. It measures the site layer's own contribution,
	// so it is conservative by the organisation guidance a real site-scope
	// resolution would also carry — a site write that passes here can still
	// be refused at read time by ModelInstructions once both layers are
	// rendered together, and that refusal names the actual combined size.
	if verr := checkDeliverable("site override", next); verr != nil {
		return Version{}, verr
	}

	expectVersion := currentVersion + 1
	v, err := s.repo.CreateSiteVersion(ctx, tenantID, siteID, expectVersion, CreateSiteVersionInput{
		Snapshot:   next,
		AuthorType: actor.Type,
		AuthorID:   actor.ID,
		Provenance: ProvenanceManual,
	}, func(tx pgx.Tx, versionID uuid.UUID) error {
		_, aerr := s.audit.RecordInTx(ctx, tx, audit.Event{
			TenantID:   tenantID,
			ActorType:  string(actor.Type),
			ActorID:    actor.actorIDString(),
			Action:     actionSiteContextPatched,
			TargetType: "site_context_version",
			TargetID:   versionID.String(),
			Metadata:   map[string]any{"version": expectVersion, "site_id": siteID.String()},
		})
		return aerr
	})
	if errors.Is(err, errVersionConflict) {
		return Version{}, versionConflictError(currentVersion, in.BaseVersion)
	}
	return v, err
}

// RestoreSiteContext is RestoreOrgContext's site-scope sibling. It refuses
// unconditionally, for every caller, when versionID names a pre-transfer
// version — ADR-064 Decision 12: "restore on a pre-transfer version id is
// refused outright and unconditionally, for every caller". That refusal needs
// no special-case code here: GetSiteVersionByID already returns ErrNotFound
// for a pre-transfer id (its tenant stamp differs from the caller's current
// tenant, and RLS + the explicit WHERE both filter on it — see repo.go), so a
// pre-transfer restore attempt surfaces as the same 404 as a genuinely
// nonexistent id, which is itself the correct behaviour: Decision 6 already
// means the caller's own access to that id ended when the transfer happened.
func (s *Service) RestoreSiteContext(ctx context.Context, tenantID, siteID, versionID uuid.UUID, actor Actor) (Version, error) {
	target, err := s.repo.GetSiteVersionByID(ctx, tenantID, siteID, versionID)
	if errors.Is(err, ErrNotFound) {
		return Version{}, domain.NotFound("context_version_not_found", "context version not found")
	}
	if err != nil {
		return Version{}, err
	}

	current, err := s.repo.LatestSiteVersion(ctx, tenantID, siteID)
	currentVersion := int64(0)
	if err == nil {
		currentVersion = current.Version
	} else if !errors.Is(err, ErrNotFound) {
		return Version{}, err
	}

	orgSnap, _, oerr := s.repo.LatestOrgSnapshot(ctx, tenantID)
	if oerr != nil {
		return Version{}, oerr
	}
	if verr := checkNoWiden(target.Snapshot.Restrictions, []namedLayer{
		{Layer: 1, Name: "WPMgr security policy", Restrictions: layer1Restrictions},
		{Layer: 2, Name: "organisation default", Restrictions: orgSnap.Restrictions},
	}); verr != nil {
		return Version{}, verr
	}
	if verr := checkNoSecret(target.Snapshot); verr != nil {
		return Version{}, verr
	}
	if verr := checkDeliverable("site override", target.Snapshot); verr != nil {
		return Version{}, verr
	}

	expectVersion := currentVersion + 1
	v, err := s.repo.CreateSiteVersion(ctx, tenantID, siteID, expectVersion, CreateSiteVersionInput{
		Snapshot:              target.Snapshot,
		AuthorType:            actor.Type,
		AuthorID:              actor.ID,
		Provenance:            ProvenanceRestore,
		RestoredFromVersionID: versionID,
	}, func(tx pgx.Tx, newVersionID uuid.UUID) error {
		_, aerr := s.audit.RecordInTx(ctx, tx, audit.Event{
			TenantID:   tenantID,
			ActorType:  string(actor.Type),
			ActorID:    actor.actorIDString(),
			Action:     actionSiteContextRestored,
			TargetType: "site_context_version",
			TargetID:   newVersionID.String(),
			Metadata:   map[string]any{"version": expectVersion, "site_id": siteID.String(), "restored_from_version_id": versionID.String()},
		})
		return aerr
	})
	if errors.Is(err, errVersionConflict) {
		return Version{}, restoreConflictError()
	}
	return v, err
}

// DiffSiteContext is DiffOrgContext's site-scope sibling. isBaseline=true
// covers BOTH cases Decision 5 defines identically: a genuine first version,
// and the first version after a transfer (whose immediately-prior row is
// stamped to a different organisation and therefore invisible under the
// current tenant — see GetSiteVersionByVersion's doc comment).
func (s *Service) DiffSiteContext(ctx context.Context, tenantID, siteID, versionID uuid.UUID) (target Version, prior *Version, isBaseline bool, err error) {
	target, err = s.repo.GetSiteVersionByID(ctx, tenantID, siteID, versionID)
	if errors.Is(err, ErrNotFound) {
		return Version{}, nil, false, domain.NotFound("context_version_not_found", "context version not found")
	}
	if err != nil {
		return Version{}, nil, false, err
	}
	if target.Version <= 1 {
		return target, nil, true, nil
	}
	p, perr := s.repo.GetSiteVersionByVersion(ctx, tenantID, siteID, target.Version-1)
	if errors.Is(perr, ErrNotFound) {
		return target, nil, true, nil
	}
	if perr != nil {
		return Version{}, nil, false, perr
	}
	return target, &p, false, nil
}

// --- effective-context preview (Decision 8) ----------------------------------

// GetEffectiveContext is Decision 8's preview: it calls the exact same
// Resolve function a future model-facing assembly path would call, with
// session=nil (Decision 8: "the preview never carries live session content,
// because none exists at preview time"). If resolution cannot complete, the
// call is refused (Decision 14) — never a partial or empty preview.
func (s *Service) GetEffectiveContext(ctx context.Context, tenantID, siteID uuid.UUID) (ResolvedContext, error) {
	if s.resolver == nil {
		return ResolvedContext{}, domain.ServiceUnavailable("context_unavailable",
			"effective context could not be resolved: no resolver configured")
	}
	return s.resolver.Resolve(ctx, tenantID, siteID, nil)
}

// --- shared helpers ----------------------------------------------------------

// applyPatch builds the new full snapshot ADR-064 Decision 13 requires PATCH
// to produce: "the server applies them onto the latest version's full
// snapshot". A nil field in the patch means "unchanged"; a non-nil field
// REPLACES the corresponding half of the snapshot wholesale (restrictions and
// guidance are each patched as one unit, not deep-merged key by key) — this
// is the field-boundary PATCH does apply at; deep-merging inside restrictions
// or guidance would blur which write is responsible for the resulting value.
func applyPatch(base Snapshot, restrictions *RestrictionSet, guidance *GuidanceSet) Snapshot {
	next := base
	if restrictions != nil {
		next.Restrictions = *restrictions
	}
	if guidance != nil {
		next.Guidance = *guidance
	}
	return next
}

// versionConflictError is ADR-064 open question 2's answer — see this
// package's doc comment on PatchOrgContextInput.BaseVersion for the full
// design. This is the SAME reason code whether the mismatch was caught by
// the application-level check above (the common case) or by the database's
// unique index rejecting a genuinely concurrent double-write (errVersionConflict,
// mapped here) — a caller never needs to distinguish the two; both mean
// "reread the current version and retry".
func versionConflictError(current, supplied int64) *domain.Error {
	return domain.Conflict("context_version_conflict", fmt.Sprintf(
		"base_version %d does not match the current version %d — reread the current context and retry",
		supplied, current,
	)).WithDetails(map[string]any{"current_version": current, "supplied_base_version": supplied})
}

// restoreConflictError is versionConflictError's restore-path variant: restore
// has no client-supplied base_version to report a mismatch against (Decision
// 13 documents no request body for it), so a lost race against a concurrent
// writer is reported without one.
func restoreConflictError() *domain.Error {
	return domain.Conflict("context_version_conflict",
		"another write landed concurrently — reread the current context and retry the restore")
}
