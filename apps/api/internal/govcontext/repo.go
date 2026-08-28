package govcontext

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ErrNotFound is returned when no version row exists for the given
// organisation/site (no context has ever been written) or, for a by-id
// lookup, when the id names no row visible under the caller's tenant scope —
// which per ADR-064 Decision 12 also covers a genuinely nonexistent id and a
// pre-transfer id stamped to a different organisation identically. See
// Repo.GetOrgVersionByID / GetSiteVersionByID.
var ErrNotFound = errors.New("govcontext: not found")

// Repo is the persistence layer for both context tables. Every method runs
// under scopedTenantTx — see its doc comment for why plain InTenantTx would
// leave m123's RESTRICTIVE site-scope policies inert for a site-scoped
// collaborator, exactly the class of bug email.Repo's identical wrapper
// documents and repo_tx_dispatch_test.go / repo_site_scope_integration_test.go
// exist to catch for that package.
type Repo struct {
	pool *db.Pool
}

// NewRepo wires a Repo with the shared pgx pool.
func NewRepo(pool *db.Pool) *Repo { return &Repo{pool: pool} }

// scopedTenantTx is plain InTenantTx (app.tenant_id only) for org-scoped
// principals, workers, and any context with no principal at all. For a
// site-scoped collaborator acting on the tenant it belongs to, it is
// InScopedTenantTx, which additionally sets app.site_scope and
// app.allowed_site_ids — the GUCs m122's org_context_versions_site_scope_*
// policies and site_context_versions_site_scope policy key on. Without this,
// those RESTRICTIVE policies are never activated and silently behave as a
// tautology (m112's defect class, reproduced here verbatim from
// internal/email/repo.go's scopedTenantTx, which is this package's precedent).
func (r *Repo) scopedTenantTx(ctx context.Context, tenantID uuid.UUID, fn func(tx pgx.Tx) error) error {
	if p, ok := domain.PrincipalFromContext(ctx); ok &&
		p.IsSiteConstrained() &&
		p.TenantID == tenantID {
		return r.pool.InScopedTenantTx(ctx, tenantID, p.UserID, p.AllowedSiteIDs, fn)
	}
	return r.pool.InTenantTx(ctx, tenantID, fn)
}

// --- marshalling -----------------------------------------------------------

func marshalRestrictions(r RestrictionSet) ([]byte, error) { return json.Marshal(r) }
func marshalGuidance(g GuidanceSet) ([]byte, error)        { return json.Marshal(g) }

func unmarshalSnapshot(restrictions, guidance []byte) (Snapshot, error) {
	var s Snapshot
	if len(restrictions) > 0 {
		if err := json.Unmarshal(restrictions, &s.Restrictions); err != nil {
			return Snapshot{}, err
		}
	}
	if len(guidance) > 0 {
		if err := json.Unmarshal(guidance, &s.Guidance); err != nil {
			return Snapshot{}, err
		}
	}
	return s, nil
}

func uuidOrNil(u pgtype.UUID) uuid.UUID {
	if !u.Valid {
		return uuid.Nil
	}
	return u.Bytes
}

func nullableUUID(u uuid.UUID) pgtype.UUID {
	if u == uuid.Nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: u, Valid: true}
}

func orgRowToVersion(row sqlc.OrgContextVersion) (Version, error) {
	snap, err := unmarshalSnapshot(row.Restrictions, row.Guidance)
	if err != nil {
		return Version{}, err
	}
	return Version{
		ID:                    row.ID,
		TenantID:              row.TenantID,
		Version:               row.Version,
		Snapshot:              snap,
		AuthorType:            AuthorType(row.AuthorType),
		AuthorID:              uuidOrNil(row.AuthorID),
		Provenance:            Provenance(row.Provenance),
		RestoredFromVersionID: uuidOrNil(row.RestoredFromVersionID),
		CreatedAt:             row.CreatedAt,
	}, nil
}

func siteRowToVersion(row sqlc.SiteContextVersion) (Version, error) {
	snap, err := unmarshalSnapshot(row.Restrictions, row.Guidance)
	if err != nil {
		return Version{}, err
	}
	return Version{
		ID:                    row.ID,
		TenantID:              row.TenantID,
		SiteID:                row.SiteID,
		Version:               row.Version,
		Snapshot:              snap,
		AuthorType:            AuthorType(row.AuthorType),
		AuthorID:              uuidOrNil(row.AuthorID),
		Provenance:            Provenance(row.Provenance),
		RestoredFromVersionID: uuidOrNil(row.RestoredFromVersionID),
		CreatedAt:             row.CreatedAt,
	}, nil
}

// --- organisation (layer 2) --------------------------------------------------

// LatestOrgVersion returns the organisation's current version row. Returns
// ErrNotFound when no version has ever been written.
func (r *Repo) LatestOrgVersion(ctx context.Context, tenantID uuid.UUID) (Version, error) {
	var out Version
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).GetLatestOrgContextVersion(ctx, tenantID)
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return domain.Internal("context_org_read_failed", "failed to read organisation context").WithCause(qerr)
		}
		v, cerr := orgRowToVersion(row)
		if cerr != nil {
			return domain.Internal("context_org_decode_failed", "failed to decode stored organisation context").WithCause(cerr)
		}
		out = v
		return nil
	})
	return out, err
}

// LatestOrgSnapshot satisfies ContextStore for the resolver. ok=false with a
// nil error means no version exists yet — a legitimate empty layer 2.
func (r *Repo) LatestOrgSnapshot(ctx context.Context, tenantID uuid.UUID) (Snapshot, bool, error) {
	v, err := r.LatestOrgVersion(ctx, tenantID)
	if errors.Is(err, ErrNotFound) {
		return Snapshot{}, false, nil
	}
	if err != nil {
		return Snapshot{}, false, err
	}
	return v.Snapshot, true, nil
}

// GetOrgVersionByID returns one organisation version by id. ErrNotFound covers
// both "no such id" and "id belongs to a different tenant" identically — RLS
// and the explicit WHERE tenant_id both filter on the caller's tenant.
func (r *Repo) GetOrgVersionByID(ctx context.Context, tenantID, id uuid.UUID) (Version, error) {
	var out Version
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).GetOrgContextVersionByID(ctx, sqlc.GetOrgContextVersionByIDParams{TenantID: tenantID, ID: id})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return domain.Internal("context_org_read_failed", "failed to read organisation context version").WithCause(qerr)
		}
		v, cerr := orgRowToVersion(row)
		if cerr != nil {
			return domain.Internal("context_org_decode_failed", "failed to decode stored organisation context").WithCause(cerr)
		}
		out = v
		return nil
	})
	return out, err
}

// GetOrgVersionByVersion returns the version numbered exactly `version`, or
// ErrNotFound (no eligible predecessor — org context has no transfer analogue,
// so this only happens for version 1's non-existent version 0).
func (r *Repo) GetOrgVersionByVersion(ctx context.Context, tenantID uuid.UUID, version int64) (Version, error) {
	var out Version
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).GetOrgContextVersionByVersion(ctx, sqlc.GetOrgContextVersionByVersionParams{TenantID: tenantID, Version: version})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return domain.Internal("context_org_read_failed", "failed to read organisation context version").WithCause(qerr)
		}
		v, cerr := orgRowToVersion(row)
		if cerr != nil {
			return domain.Internal("context_org_decode_failed", "failed to decode stored organisation context").WithCause(cerr)
		}
		out = v
		return nil
	})
	return out, err
}

// ListOrgVersions returns up to limit versions strictly older than cursor
// (cursor=0 means "first page"), newest first.
func (r *Repo) ListOrgVersions(ctx context.Context, tenantID uuid.UUID, cursor int64, limit int32) ([]Version, error) {
	var out []Version
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).ListOrgContextVersions(ctx, sqlc.ListOrgContextVersionsParams{
			TenantID: tenantID,
			Column2:  cursor,
			Limit:    limit,
		})
		if qerr != nil {
			return domain.Internal("context_org_list_failed", "failed to list organisation context history").WithCause(qerr)
		}
		out = make([]Version, 0, len(rows))
		for _, row := range rows {
			v, cerr := orgRowToVersion(row)
			if cerr != nil {
				return domain.Internal("context_org_decode_failed", "failed to decode stored organisation context").WithCause(cerr)
			}
			out = append(out, v)
		}
		return nil
	})
	return out, err
}

// CreateOrgVersionInput is everything needed to author a new organisation
// context version. Version is NOT supplied by the caller — CreateOrgVersion
// computes latest+1 itself, inside the same transaction as the read, so the
// sequence stays gap-free (m122's version_key unique index turns a
// concurrent double-write into 23505 rather than silently ambiguous history —
// see service.go for how that maps to ADR-064 open question 2).
type CreateOrgVersionInput struct {
	Snapshot              Snapshot
	AuthorType            AuthorType
	AuthorID              uuid.UUID
	Provenance            Provenance
	RestoredFromVersionID uuid.UUID
}

// CreateOrgVersion inserts a new organisation context version at
// expectVersion (the version the caller believes is about to be created —
// service.go computes this from base_version+1 and passes it through so the
// insert either lands at exactly that number or fails 23505, never silently
// at a different one). Returns the inserted row.
func (r *Repo) CreateOrgVersion(ctx context.Context, tenantID uuid.UUID, expectVersion int64, in CreateOrgVersionInput, record func(tx pgx.Tx, versionID uuid.UUID) error) (Version, error) {
	var out Version
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		restrictions, rerr := marshalRestrictions(in.Snapshot.Restrictions)
		if rerr != nil {
			return domain.Internal("context_encode_failed", "failed to encode restrictions").WithCause(rerr)
		}
		guidance, gerr := marshalGuidance(in.Snapshot.Guidance)
		if gerr != nil {
			return domain.Internal("context_encode_failed", "failed to encode guidance").WithCause(gerr)
		}
		row, qerr := sqlc.New(tx).CreateOrgContextVersion(ctx, sqlc.CreateOrgContextVersionParams{
			TenantID:              tenantID,
			Version:               expectVersion,
			Restrictions:          restrictions,
			Guidance:              guidance,
			AuthorType:            string(in.AuthorType),
			AuthorID:              nullableUUID(in.AuthorID),
			Provenance:            string(in.Provenance),
			RestoredFromVersionID: nullableUUID(in.RestoredFromVersionID),
		})
		if qerr != nil {
			if isUniqueViolation(qerr) {
				return errVersionConflict
			}
			return domain.Internal("context_org_write_failed", "failed to write organisation context version").WithCause(qerr)
		}
		v, cerr := orgRowToVersion(row)
		if cerr != nil {
			return domain.Internal("context_org_decode_failed", "failed to decode stored organisation context").WithCause(cerr)
		}
		if record != nil {
			if aerr := record(tx, v.ID); aerr != nil {
				return aerr
			}
		}
		out = v
		return nil
	})
	return out, err
}

// --- site (layer 3) ----------------------------------------------------------

// LatestSiteVersion returns a site's current version row, scoped to the
// CURRENT tenant stamp (see site_context.sql — this is what confines the
// result to the destination organisation's context after a transfer with no
// extra logic). Returns ErrNotFound when no version has ever been written
// under this tenant stamp.
func (r *Repo) LatestSiteVersion(ctx context.Context, tenantID, siteID uuid.UUID) (Version, error) {
	var out Version
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).GetLatestSiteContextVersion(ctx, sqlc.GetLatestSiteContextVersionParams{TenantID: tenantID, SiteID: siteID})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return domain.Internal("context_site_read_failed", "failed to read site context").WithCause(qerr)
		}
		v, cerr := siteRowToVersion(row)
		if cerr != nil {
			return domain.Internal("context_site_decode_failed", "failed to decode stored site context").WithCause(cerr)
		}
		out = v
		return nil
	})
	return out, err
}

// LatestSiteSnapshot satisfies ContextStore for the resolver.
func (r *Repo) LatestSiteSnapshot(ctx context.Context, tenantID, siteID uuid.UUID) (Snapshot, bool, error) {
	v, err := r.LatestSiteVersion(ctx, tenantID, siteID)
	if errors.Is(err, ErrNotFound) {
		return Snapshot{}, false, nil
	}
	if err != nil {
		return Snapshot{}, false, err
	}
	return v.Snapshot, true, nil
}

// GetSiteVersionByID returns one site version by id, scoped to the current
// tenant stamp. ErrNotFound covers "no such id", "id belongs to another
// site", and "id is a pre-transfer version stamped to a different
// organisation" identically (ADR-064 Decision 12) — the restore-pointer
// validation in service.go relies on exactly this collapse.
func (r *Repo) GetSiteVersionByID(ctx context.Context, tenantID, siteID, id uuid.UUID) (Version, error) {
	var out Version
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).GetSiteContextVersionByID(ctx, sqlc.GetSiteContextVersionByIDParams{TenantID: tenantID, SiteID: siteID, ID: id})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return domain.Internal("context_site_read_failed", "failed to read site context version").WithCause(qerr)
		}
		v, cerr := siteRowToVersion(row)
		if cerr != nil {
			return domain.Internal("context_site_decode_failed", "failed to decode stored site context").WithCause(cerr)
		}
		out = v
		return nil
	})
	return out, err
}

// GetSiteVersionByVersion returns the version numbered exactly `version` under
// the current tenant stamp, or ErrNotFound — which the caller (service.go's
// diff) must read as "no eligible predecessor, render a baseline" per
// ADR-064 Decision 5, whether the cause is a true first version or a
// transfer's stamp boundary.
func (r *Repo) GetSiteVersionByVersion(ctx context.Context, tenantID, siteID uuid.UUID, version int64) (Version, error) {
	var out Version
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, qerr := sqlc.New(tx).GetSiteContextVersionByVersion(ctx, sqlc.GetSiteContextVersionByVersionParams{TenantID: tenantID, SiteID: siteID, Version: version})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return domain.Internal("context_site_read_failed", "failed to read site context version").WithCause(qerr)
		}
		v, cerr := siteRowToVersion(row)
		if cerr != nil {
			return domain.Internal("context_site_decode_failed", "failed to decode stored site context").WithCause(cerr)
		}
		out = v
		return nil
	})
	return out, err
}

// ListSiteVersions returns up to limit versions strictly older than cursor
// (cursor=0 means "first page"), newest first, scoped to the current tenant
// stamp only — which is what makes list history "additionally scoped to
// versions stamped with the site's current organisation" (Decision 13) true.
func (r *Repo) ListSiteVersions(ctx context.Context, tenantID, siteID uuid.UUID, cursor int64, limit int32) ([]Version, error) {
	var out []Version
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).ListSiteContextVersions(ctx, sqlc.ListSiteContextVersionsParams{
			TenantID: tenantID,
			SiteID:   siteID,
			Column3:  cursor,
			Limit:    limit,
		})
		if qerr != nil {
			return domain.Internal("context_site_list_failed", "failed to list site context history").WithCause(qerr)
		}
		out = make([]Version, 0, len(rows))
		for _, row := range rows {
			v, cerr := siteRowToVersion(row)
			if cerr != nil {
				return domain.Internal("context_site_decode_failed", "failed to decode stored site context").WithCause(cerr)
			}
			out = append(out, v)
		}
		return nil
	})
	return out, err
}

// CreateSiteVersionInput is everything needed to author a new site context
// version. Version is computed by the caller (service.go), same reasoning as
// CreateOrgVersionInput.
type CreateSiteVersionInput struct {
	Snapshot              Snapshot
	AuthorType            AuthorType
	AuthorID              uuid.UUID
	Provenance            Provenance
	RestoredFromVersionID uuid.UUID
}

// CreateSiteVersion inserts a new site context version at expectVersion,
// stamped to tenantID — which MUST be the site's CURRENT owning organisation
// (ADR-064 Decision 3: the stamp is set once, at write time, from whoever
// owns the site right now). record, if non-nil, runs inside the SAME
// transaction as the insert and its error aborts the whole write — this is
// the hook service.go uses to append the fail-closed audit entry atomically
// (ADR-064 Decision 7).
func (r *Repo) CreateSiteVersion(ctx context.Context, tenantID, siteID uuid.UUID, expectVersion int64, in CreateSiteVersionInput, record func(tx pgx.Tx, versionID uuid.UUID) error) (Version, error) {
	var out Version
	err := r.scopedTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		restrictions, rerr := marshalRestrictions(in.Snapshot.Restrictions)
		if rerr != nil {
			return domain.Internal("context_encode_failed", "failed to encode restrictions").WithCause(rerr)
		}
		guidance, gerr := marshalGuidance(in.Snapshot.Guidance)
		if gerr != nil {
			return domain.Internal("context_encode_failed", "failed to encode guidance").WithCause(gerr)
		}
		row, qerr := sqlc.New(tx).CreateSiteContextVersion(ctx, sqlc.CreateSiteContextVersionParams{
			TenantID:              tenantID,
			SiteID:                siteID,
			Version:               expectVersion,
			Restrictions:          restrictions,
			Guidance:              guidance,
			AuthorType:            string(in.AuthorType),
			AuthorID:              nullableUUID(in.AuthorID),
			Provenance:            string(in.Provenance),
			RestoredFromVersionID: nullableUUID(in.RestoredFromVersionID),
		})
		if qerr != nil {
			if isUniqueViolation(qerr) {
				return errVersionConflict
			}
			return domain.Internal("context_site_write_failed", "failed to write site context version").WithCause(qerr)
		}
		v, cerr := siteRowToVersion(row)
		if cerr != nil {
			return domain.Internal("context_site_decode_failed", "failed to decode stored site context").WithCause(cerr)
		}
		if record != nil {
			if aerr := record(tx, v.ID); aerr != nil {
				return aerr
			}
		}
		out = v
		return nil
	})
	return out, err
}
