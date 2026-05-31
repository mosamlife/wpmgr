package repo

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/model"
)

const assetCols = `id, tenant_id, site_id, wp_attachment_id, title,
	original_path, original_url, original_mime, original_width, original_height,
	original_size_bytes, current_format, current_size_bytes, status, generation,
	compression_level, target_format, sizes_optimized, sizes_unoptimized,
	last_optimized_at, last_synced_at, created_at, updated_at`

func assetFromRow(row pgx.Row) (model.Asset, error) {
	var a model.Asset
	var width, height *int
	var compression, targetFormat *string
	var sizesOptRaw, sizesUnoptRaw []byte
	var lastOptimizedAt *time.Time
	if err := row.Scan(
		&a.ID, &a.TenantID, &a.SiteID, &a.WPAttachmentID, &a.Title,
		&a.OriginalPath, &a.OriginalURL, &a.OriginalMime, &width, &height,
		&a.OriginalSizeBytes, &a.CurrentFormat, &a.CurrentSizeBytes, &a.Status, &a.Generation,
		&compression, &targetFormat, &sizesOptRaw, &sizesUnoptRaw,
		&lastOptimizedAt, &a.LastSyncedAt, &a.CreatedAt, &a.UpdatedAt,
	); err != nil {
		return model.Asset{}, err
	}
	a.OriginalWidth = width
	a.OriginalHeight = height
	if compression != nil {
		a.CompressionLevel = *compression
	}
	if targetFormat != nil {
		a.TargetFormat = *targetFormat
	}
	if len(sizesOptRaw) > 0 {
		_ = json.Unmarshal(sizesOptRaw, &a.SizesOptimized)
	}
	if len(sizesUnoptRaw) > 0 {
		_ = json.Unmarshal(sizesUnoptRaw, &a.SizesUnoptimized)
	}
	a.LastOptimizedAt = lastOptimizedAt
	return a, nil
}

// UpsertAssetInput is one attachment in an agent sync-batch.
type UpsertAssetInput struct {
	WPAttachmentID    int64
	Title             string
	OriginalPath      string
	OriginalURL       string
	OriginalMime      string
	OriginalWidth     *int
	OriginalHeight    *int
	OriginalSizeBytes int64
}

// UpsertAssetsAgent upserts a batch of attachments under the agent GUC. New rows
// land in 'pending'; existing rows refresh their library metadata + last_synced_at
// but PRESERVE their optimization status/sizes (the agent's apply callback owns
// those). Returns the number of rows affected. tenantID/siteID come from the
// verified Ed25519 identity (NOT a client header).
func (r *Repo) UpsertAssetsAgent(ctx context.Context, tenantID, siteID uuid.UUID, rows []UpsertAssetInput) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	var affected int64
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		for _, a := range rows {
			tag, err := tx.Exec(ctx,
				`INSERT INTO site_media_assets
					(id, tenant_id, site_id, wp_attachment_id, title, original_path,
					 original_url, original_mime, original_width, original_height,
					 original_size_bytes, current_format, current_size_bytes, status,
					 last_synced_at, created_at, updated_at)
				 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
				         'original', $10, 'pending', now(), now(), now())
				 ON CONFLICT (site_id, wp_attachment_id) DO UPDATE
				   SET title               = EXCLUDED.title,
				       original_path        = EXCLUDED.original_path,
				       original_url         = EXCLUDED.original_url,
				       original_mime        = EXCLUDED.original_mime,
				       original_width       = EXCLUDED.original_width,
				       original_height      = EXCLUDED.original_height,
				       original_size_bytes  = EXCLUDED.original_size_bytes,
				       last_synced_at       = now(),
				       updated_at           = now()`,
				tenantID, siteID, a.WPAttachmentID, a.Title, a.OriginalPath,
				a.OriginalURL, a.OriginalMime, a.OriginalWidth, a.OriginalHeight,
				a.OriginalSizeBytes,
			)
			if err != nil {
				return domain.Internal("media_asset_upsert_failed", "failed to upsert media asset").WithCause(err)
			}
			affected += tag.RowsAffected()
		}
		return nil
	})
	return affected, err
}

// ListAssetsInput is the cursor-paginated dashboard query.
type ListAssetsInput struct {
	TenantID uuid.UUID
	SiteID   uuid.UUID
	Limit    int
	// Cursor is the last seen asset id (created_at, id) — opaque to the caller.
	Cursor string
	Status string // optional filter on status
	Format string // optional filter on current_format
	Search string // optional ILIKE on title/original_path
}

// ListAssets returns a page of assets ordered by created_at DESC, id DESC, plus
// a next cursor (empty when exhausted).
func (r *Repo) ListAssets(ctx context.Context, in ListAssetsInput) ([]model.Asset, string, error) {
	limit := in.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var out []model.Asset
	var nextCursor string
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		args := []any{in.TenantID, in.SiteID}
		q := `SELECT ` + assetCols + `
			  FROM site_media_assets
			  WHERE tenant_id = $1 AND site_id = $2`
		if in.Status != "" {
			args = append(args, in.Status)
			q += ` AND status = $` + strconv.Itoa(len(args))
		}
		if in.Format != "" {
			args = append(args, in.Format)
			q += ` AND current_format = $` + strconv.Itoa(len(args))
		}
		if in.Search != "" {
			// Strip LIKE metacharacters so a search term can't smuggle wildcards
			// that DoS the index scan; the value is also bound as a parameter.
			args = append(args, "%"+trimLikeWildcards(in.Search)+"%")
			n := strconv.Itoa(len(args))
			q += ` AND (title ILIKE $` + n + ` OR original_path ILIKE $` + n + `)`
		}
		if in.Cursor != "" {
			if cid, err := uuid.Parse(in.Cursor); err == nil {
				args = append(args, cid)
				// keyset: rows strictly older than the cursor row.
				q += ` AND created_at < (SELECT created_at FROM site_media_assets WHERE id = $` +
					strconv.Itoa(len(args)) + `)`
			}
		}
		args = append(args, limit+1)
		q += ` ORDER BY created_at DESC, id DESC LIMIT $` + strconv.Itoa(len(args))

		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return domain.Internal("media_assets_list_failed", "failed to list media assets").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			a, err := assetFromRow(rows)
			if err != nil {
				return domain.Internal("media_assets_list_failed", "failed to read media asset").WithCause(err)
			}
			out = append(out, a)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(out) > limit {
			nextCursor = out[limit-1].ID.String()
			out = out[:limit]
		}
		return nil
	})
	return out, nextCursor, err
}

// GetAsset returns a single asset by id (tenant-scoped).
func (r *Repo) GetAsset(ctx context.Context, tenantID, assetID uuid.UUID) (model.Asset, error) {
	var out model.Asset
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT `+assetCols+` FROM site_media_assets WHERE tenant_id = $1 AND id = $2`,
			tenantID, assetID)
		a, err := assetFromRow(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("media_asset_not_found", "media asset not found")
		}
		if err != nil {
			return domain.Internal("media_asset_get_failed", "failed to get media asset").WithCause(err)
		}
		out = a
		return nil
	})
	return out, err
}

// ListPendingAssetIDs returns up to limit asset ids in a site that are eligible
// for optimization (pending or failed). Used by the "all_pending" optimize path.
func (r *Repo) ListPendingAssetIDs(ctx context.Context, tenantID, siteID uuid.UUID, limit int) ([]model.Asset, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	var out []model.Asset
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+assetCols+`
			 FROM site_media_assets
			 WHERE tenant_id = $1 AND site_id = $2 AND status IN ('pending', 'failed')
			 ORDER BY created_at ASC
			 LIMIT $3`,
			tenantID, siteID, limit)
		if err != nil {
			return domain.Internal("media_pending_list_failed", "failed to list pending assets").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			a, err := assetFromRow(rows)
			if err != nil {
				return domain.Internal("media_pending_list_failed", "failed to read pending asset").WithCause(err)
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

// SetAssetStatus transitions an asset's status (tenant-scoped, operator path).
func (r *Repo) SetAssetStatus(ctx context.Context, tenantID, assetID uuid.UUID, status model.AssetStatus) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE site_media_assets SET status = $3, updated_at = now()
			 WHERE tenant_id = $1 AND id = $2`,
			tenantID, assetID, status)
		if err != nil {
			return domain.Internal("media_asset_status_failed", "failed to set asset status").WithCause(err)
		}
		return nil
	})
}

// ApplyOptimizedInput is the agent's post-apply asset snapshot (the salient
// fields of the wpmgr_image_optimization blob — ADR-043 / media-postmeta-blob).
type ApplyOptimizedInput struct {
	CurrentFormat    string
	CurrentSizeBytes int64
	Status           model.AssetStatus
	CompressionLevel string
	TargetFormat     string
	SizesOptimized   []string
	SizesUnoptimized map[string]string
}

// ApplyOptimizedAgent finalizes an asset row after the agent applies optimized
// variants on disk. Runs under the agent GUC; bumps generation; sets
// last_optimized_at. Returns the updated asset.
func (r *Repo) ApplyOptimizedAgent(ctx context.Context, tenantID, siteID uuid.UUID, wpAttachmentID int64, in ApplyOptimizedInput) (model.Asset, error) {
	sizesOpt, _ := json.Marshal(orEmptySlice(in.SizesOptimized))
	sizesUnopt, _ := json.Marshal(orEmptyMap(in.SizesUnoptimized))
	var out model.Asset
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`UPDATE site_media_assets
			 SET current_format     = $4,
			     current_size_bytes  = $5,
			     status              = $6,
			     compression_level   = $7,
			     target_format       = $8,
			     sizes_optimized     = $9,
			     sizes_unoptimized   = $10,
			     generation          = generation + 1,
			     last_optimized_at   = now(),
			     updated_at          = now()
			 WHERE tenant_id = $1 AND site_id = $2 AND wp_attachment_id = $3
			 RETURNING `+assetCols,
			tenantID, siteID, wpAttachmentID, in.CurrentFormat, in.CurrentSizeBytes,
			in.Status, nilIfEmpty(in.CompressionLevel), nilIfEmpty(in.TargetFormat),
			sizesOpt, sizesUnopt)
		a, err := assetFromRow(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("media_asset_not_found", "media asset not found for apply")
		}
		if err != nil {
			return domain.Internal("media_asset_apply_failed", "failed to apply optimized asset").WithCause(err)
		}
		out = a
		return nil
	})
	return out, err
}

// RestoreAssetAgent marks an asset restored (or originals_deleted-aware) after
// the agent reverts on disk. Runs under the agent GUC. Returns the updated asset.
func (r *Repo) RestoreAssetAgent(ctx context.Context, tenantID, siteID uuid.UUID, wpAttachmentID int64) (model.Asset, error) {
	var out model.Asset
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`UPDATE site_media_assets
			 SET status             = 'restored',
			     current_format     = 'original',
			     current_size_bytes  = original_size_bytes,
			     compression_level   = NULL,
			     target_format       = NULL,
			     sizes_optimized     = '[]'::jsonb,
			     sizes_unoptimized   = '{}'::jsonb,
			     updated_at          = now()
			 WHERE tenant_id = $1 AND site_id = $2 AND wp_attachment_id = $3
			 RETURNING `+assetCols,
			tenantID, siteID, wpAttachmentID)
		a, err := assetFromRow(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("media_asset_not_found", "media asset not found for restore")
		}
		if err != nil {
			return domain.Internal("media_asset_restore_failed", "failed to restore asset").WithCause(err)
		}
		out = a
		return nil
	})
	return out, err
}

// Summary returns the dashboard rollup for a site.
func (r *Repo) Summary(ctx context.Context, tenantID, siteID uuid.UUID) (model.AssetSummary, error) {
	var s model.AssetSummary
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT
			    count(*) AS total,
			    count(*) FILTER (WHERE status = 'optimized') AS optimized,
			    count(*) FILTER (WHERE status IN ('pending', 'restored')) AS pending,
			    count(*) FILTER (WHERE status = 'failed') AS failed,
			    coalesce(sum(GREATEST(original_size_bytes - current_size_bytes, 0))
			        FILTER (WHERE status = 'optimized'), 0) AS bytes_saved
			 FROM site_media_assets
			 WHERE tenant_id = $1 AND site_id = $2`,
			tenantID, siteID)
		if err := row.Scan(&s.Total, &s.Optimized, &s.Pending, &s.Failed, &s.BytesSaved); err != nil {
			return domain.Internal("media_summary_failed", "failed to compute media summary").WithCause(err)
		}
		return nil
	})
	return s, err
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func orEmptySlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func orEmptyMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// trimLikeWildcards is a tiny guard so a search term can't smuggle SQL LIKE
// wildcards that DoS the index scan. (Defensive; the param is already bound.)
func trimLikeWildcards(s string) string {
	return strings.NewReplacer("%", "", "_", "").Replace(s)
}
