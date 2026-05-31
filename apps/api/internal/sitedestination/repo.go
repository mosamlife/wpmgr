package sitedestination

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Repo is the tenant-scoped persistence layer for site destinations. Every
// mutating call runs inside an InTenantTx so the row-level security policy
// added by the M7 migration filters every row by the active tenant — a query
// that forgets its tenant filter still cannot leak across tenants.
type Repo struct {
	pool *db.Pool
}

// NewRepo builds a Repo backed by the shared pgx pool.
func NewRepo(pool *db.Pool) *Repo {
	return &Repo{pool: pool}
}

// CreateInput is the validated input for inserting a destination.
type CreateInput struct {
	TenantID       uuid.UUID
	SiteID         uuid.UUID
	Kind           Kind
	Label          string
	Endpoint       string
	Region         string
	Bucket         string
	PathPrefix     string
	AccessKeyID    string
	SecretKeyEnc   []byte
	ForcePathStyle bool
	IsDefault      bool
}

// UpdateInput captures the patchable fields for an existing destination. A nil
// pointer field means "leave unchanged"; a non-nil empty value means "clear".
type UpdateInput struct {
	Label          *string
	Endpoint       *string
	Region         *string
	Bucket         *string
	PathPrefix     *string
	AccessKeyID    *string
	SecretKeyEnc   []byte // nil = leave; non-nil (incl. empty) = replace.
	ForcePathStyle *bool
	IsDefault      *bool
}

// Create inserts a new destination. If IsDefault=true is requested the caller
// is expected to have already cleared the previous default within the same
// tenant + site (or rely on SetDefault, which atomicises that swap).
func (r *Repo) Create(ctx context.Context, in CreateInput) (SiteDestination, error) {
	if in.TenantID == uuid.Nil {
		return SiteDestination{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	if !ValidKind(in.Kind) {
		return SiteDestination{}, domain.Validation("invalid_kind", "destination kind must be cp, local, or s3_compat")
	}

	var out SiteDestination
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		// If IsDefault is requested, clear any existing default within the
		// same (tenant, site) so the partial unique index on is_default
		// doesn't reject the insert.
		if in.IsDefault {
			if _, err := tx.Exec(ctx,
				`UPDATE site_destinations SET is_default = false, updated_at = now()
				 WHERE tenant_id = $1 AND site_id = $2 AND is_default = true`,
				in.TenantID, in.SiteID,
			); err != nil {
				return domain.Internal("site_destination_clear_default_failed", "failed to clear previous default").WithCause(err)
			}
		}

		row := tx.QueryRow(ctx,
			`INSERT INTO site_destinations
				(id, tenant_id, site_id, kind, label, endpoint, region, bucket,
				 path_prefix, access_key_id, secret_key_enc, force_path_style,
				 is_default, created_at, updated_at)
			 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, now(), now())
			 RETURNING id, tenant_id, site_id, kind, label, endpoint, region, bucket,
				 path_prefix, access_key_id, secret_key_enc, force_path_style,
				 is_default, created_at, updated_at`,
			in.TenantID, in.SiteID, string(in.Kind), in.Label,
			in.Endpoint, in.Region, in.Bucket, in.PathPrefix,
			in.AccessKeyID, in.SecretKeyEnc, in.ForcePathStyle, in.IsDefault,
		)
		var (
			d       SiteDestination
			kindStr string
			siteCol pgtype.UUID
			secret  sql.RawBytes
		)
		if err := row.Scan(
			&d.ID, &d.TenantID, &siteCol, &kindStr, &d.Label,
			&d.Endpoint, &d.Region, &d.Bucket, &d.PathPrefix,
			&d.AccessKeyID, &secret, &d.ForcePathStyle,
			&d.IsDefault, &d.CreatedAt, &d.UpdatedAt,
		); err != nil {
			return domain.Internal("site_destination_create_failed", "failed to create destination").WithCause(err)
		}
		d.Kind = Kind(kindStr)
		if siteCol.Valid {
			d.SiteID = siteCol.Bytes
		}
		if len(secret) > 0 {
			d.SecretKeyEnc = append([]byte(nil), secret...)
		}
		out = d
		return nil
	})
	return out, err
}

// GetByID fetches a single destination by id within the tenant scope. RLS
// guarantees a cross-tenant id can never resolve.
func (r *Repo) GetByID(ctx context.Context, tenantID, id uuid.UUID) (SiteDestination, error) {
	var out SiteDestination
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		d, err := scanOne(ctx, tx,
			`SELECT id, tenant_id, site_id, kind, label, endpoint, region, bucket,
				path_prefix, access_key_id, secret_key_enc, force_path_style,
				is_default, created_at, updated_at
			 FROM site_destinations WHERE id = $1 AND tenant_id = $2`,
			id, tenantID,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("site_destination_not_found", "destination not found")
			}
			return domain.Internal("site_destination_get_failed", "failed to load destination").WithCause(err)
		}
		out = d
		return nil
	})
	return out, err
}

// ListBySite returns every destination configured for a site, ordered by
// is_default DESC, created_at ASC (so the default reads first).
func (r *Repo) ListBySite(ctx context.Context, tenantID, siteID uuid.UUID) ([]SiteDestination, error) {
	var out []SiteDestination
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, site_id, kind, label, endpoint, region, bucket,
				path_prefix, access_key_id, secret_key_enc, force_path_style,
				is_default, created_at, updated_at
			 FROM site_destinations
			 WHERE tenant_id = $1 AND site_id = $2
			 ORDER BY is_default DESC, created_at ASC`,
			tenantID, siteID,
		)
		if err != nil {
			return domain.Internal("site_destination_list_failed", "failed to list destinations").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanRow(rows)
			if err != nil {
				return domain.Internal("site_destination_list_failed", "failed to read destinations").WithCause(err)
			}
			out = append(out, d)
		}
		if err := rows.Err(); err != nil {
			return domain.Internal("site_destination_list_failed", "failed to iterate destinations").WithCause(err)
		}
		return nil
	})
	return out, err
}

// GetDefaultForSite returns the destination flagged is_default=true for a site
// (or pgx.ErrNoRows if none exists). Used by the blobstore Registry to route
// presigns when a snapshot has no destination_id set yet.
func (r *Repo) GetDefaultForSite(ctx context.Context, tenantID, siteID uuid.UUID) (SiteDestination, error) {
	var out SiteDestination
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		d, err := scanOne(ctx, tx,
			`SELECT id, tenant_id, site_id, kind, label, endpoint, region, bucket,
				path_prefix, access_key_id, secret_key_enc, force_path_style,
				is_default, created_at, updated_at
			 FROM site_destinations
			 WHERE tenant_id = $1 AND site_id = $2 AND is_default = true
			 LIMIT 1`,
			tenantID, siteID,
		)
		if err != nil {
			return err
		}
		out = d
		return nil
	})
	return out, err
}

// Update applies the non-nil fields of in onto the row. Returns the row after
// the update for echoing back to the caller.
func (r *Repo) Update(ctx context.Context, tenantID, id uuid.UUID, in UpdateInput) (SiteDestination, error) {
	var out SiteDestination
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		// Default swap: if IsDefault=true is requested, clear sibling defaults
		// within the same (tenant, site). We need to look up the site first
		// because UpdateInput doesn't carry it explicitly.
		if in.IsDefault != nil && *in.IsDefault {
			var siteID pgtype.UUID
			if err := tx.QueryRow(ctx,
				`SELECT site_id FROM site_destinations WHERE id = $1 AND tenant_id = $2`,
				id, tenantID,
			).Scan(&siteID); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return domain.NotFound("site_destination_not_found", "destination not found")
				}
				return domain.Internal("site_destination_get_failed", "failed to load destination").WithCause(err)
			}
			if siteID.Valid {
				if _, err := tx.Exec(ctx,
					`UPDATE site_destinations SET is_default = false, updated_at = now()
					 WHERE tenant_id = $1 AND site_id = $2 AND is_default = true AND id <> $3`,
					tenantID, uuid.UUID(siteID.Bytes), id,
				); err != nil {
					return domain.Internal("site_destination_clear_default_failed", "failed to clear previous default").WithCause(err)
				}
			}
		}

		// COALESCE pattern: each column either stays put ($N is NULL) or takes
		// the new value. secret_key_enc uses a sentinel: when in.SecretKeyEnc
		// is nil we skip; when it's non-nil (even empty) we replace.
		setSecret := in.SecretKeyEnc != nil
		var (
			label, endpoint, region, bucket, pathPrefix, accessKeyID *string
			forcePathStyle, isDefault                                *bool
		)
		label = in.Label
		endpoint = in.Endpoint
		region = in.Region
		bucket = in.Bucket
		pathPrefix = in.PathPrefix
		accessKeyID = in.AccessKeyID
		forcePathStyle = in.ForcePathStyle
		isDefault = in.IsDefault

		// Build a single UPDATE with COALESCEs so the SQL is one statement.
		row := tx.QueryRow(ctx,
			`UPDATE site_destinations SET
				label             = COALESCE($3, label),
				endpoint          = COALESCE($4, endpoint),
				region            = COALESCE($5, region),
				bucket            = COALESCE($6, bucket),
				path_prefix       = COALESCE($7, path_prefix),
				access_key_id     = COALESCE($8, access_key_id),
				secret_key_enc    = CASE WHEN $9::boolean THEN $10::bytea ELSE secret_key_enc END,
				force_path_style  = COALESCE($11, force_path_style),
				is_default        = COALESCE($12, is_default),
				updated_at        = now()
			 WHERE id = $1 AND tenant_id = $2
			 RETURNING id, tenant_id, site_id, kind, label, endpoint, region, bucket,
				path_prefix, access_key_id, secret_key_enc, force_path_style,
				is_default, created_at, updated_at`,
			id, tenantID,
			label, endpoint, region, bucket, pathPrefix, accessKeyID,
			setSecret, in.SecretKeyEnc,
			forcePathStyle, isDefault,
		)
		d, err := scanRowFromQueryRow(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("site_destination_not_found", "destination not found")
			}
			return domain.Internal("site_destination_update_failed", "failed to update destination").WithCause(err)
		}
		out = d
		return nil
	})
	return out, err
}

// SetDefault flips is_default=true on the row, clearing any sibling default
// within the same (tenant, site). Convenience wrapper over Update for the
// dedicated "Make default" UI control.
func (r *Repo) SetDefault(ctx context.Context, tenantID, id uuid.UUID) (SiteDestination, error) {
	isDefault := true
	return r.Update(ctx, tenantID, id, UpdateInput{IsDefault: &isDefault})
}

// Delete removes a destination. Returns NotFound when the row doesn't exist or
// is owned by another tenant (RLS hides it either way).
func (r *Repo) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, `DELETE FROM site_destinations WHERE id = $1 AND tenant_id = $2`, id, tenantID)
		if err != nil {
			return domain.Internal("site_destination_delete_failed", "failed to delete destination").WithCause(err)
		}
		if ct.RowsAffected() == 0 {
			return domain.NotFound("site_destination_not_found", "destination not found")
		}
		return nil
	})
}

// scanOne loads exactly one row from a parametrised SELECT, returning
// pgx.ErrNoRows when the row is missing.
func scanOne(ctx context.Context, tx pgx.Tx, sqlText string, args ...any) (SiteDestination, error) {
	return scanRowFromQueryRow(tx.QueryRow(ctx, sqlText, args...))
}

// scanRow reads a SiteDestination from a *pgx.Rows cursor.
func scanRow(rows pgx.Rows) (SiteDestination, error) {
	var (
		d       SiteDestination
		kindStr string
		siteCol pgtype.UUID
		secret  []byte
	)
	if err := rows.Scan(
		&d.ID, &d.TenantID, &siteCol, &kindStr, &d.Label,
		&d.Endpoint, &d.Region, &d.Bucket, &d.PathPrefix,
		&d.AccessKeyID, &secret, &d.ForcePathStyle,
		&d.IsDefault, &d.CreatedAt, &d.UpdatedAt,
	); err != nil {
		return SiteDestination{}, err
	}
	d.Kind = Kind(kindStr)
	if siteCol.Valid {
		d.SiteID = siteCol.Bytes
	}
	d.SecretKeyEnc = secret
	return d, nil
}

// scanRowFromQueryRow is the QueryRow counterpart to scanRow.
func scanRowFromQueryRow(row pgx.Row) (SiteDestination, error) {
	var (
		d       SiteDestination
		kindStr string
		siteCol pgtype.UUID
		secret  []byte
	)
	if err := row.Scan(
		&d.ID, &d.TenantID, &siteCol, &kindStr, &d.Label,
		&d.Endpoint, &d.Region, &d.Bucket, &d.PathPrefix,
		&d.AccessKeyID, &secret, &d.ForcePathStyle,
		&d.IsDefault, &d.CreatedAt, &d.UpdatedAt,
	); err != nil {
		return SiteDestination{}, err
	}
	d.Kind = Kind(kindStr)
	if siteCol.Valid {
		d.SiteID = siteCol.Bytes
	}
	d.SecretKeyEnc = secret
	return d, nil
}

// asPgUUID converts a uuid.UUID into the pgtype representation used for
// nullable inputs. Kept here in case future flows insert with NULL site_id.
func asPgUUID(u uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: u, Valid: true}
}

// _ keeps imports honest while still letting helpers stay package-local.
var _ = time.Time{}
var _ = asPgUUID
