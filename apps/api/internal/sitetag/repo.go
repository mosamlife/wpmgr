package sitetag

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Repo is the persistence interface for the tag registry.
type Repo interface {
	List(ctx context.Context, tenantID uuid.UUID) ([]Tag, error)
	Create(ctx context.Context, in CreateInput) (Tag, error)
	Update(ctx context.Context, in UpdateInput) (UpdateResult, error)
	Delete(ctx context.Context, tenantID, id uuid.UUID) error
	BulkApply(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID, add, remove []string) (map[uuid.UUID]bool, error)
}

type pgRepo struct {
	pool *db.Pool
}

// NewRepo builds a Repo backed by the pgx pool with RLS enforcement.
func NewRepo(pool *db.Pool) Repo {
	return &pgRepo{pool: pool}
}

func (r *pgRepo) List(ctx context.Context, tenantID uuid.UUID) ([]Tag, error) {
	var out []Tag
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListTagsWithUsage(ctx, tenantID)
		if err != nil {
			return domain.Internal("tag_list_failed", "failed to list tags").WithCause(err)
		}
		out = make([]Tag, 0, len(rows))
		for _, row := range rows {
			out = append(out, Tag{
				ID:         row.ID,
				TenantID:   tenantID,
				Name:       row.Name,
				Color:      row.Color,
				UsageCount: row.UsageCount,
				CreatedAt:  row.CreatedAt,
			})
		}
		return nil
	})
	return out, err
}

func (r *pgRepo) Create(ctx context.Context, in CreateInput) (Tag, error) {
	var out Tag
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).CreateTag(ctx, sqlc.CreateTagParams{
			TenantID: in.TenantID,
			Name:     in.Name,
			Color:    in.Color,
		})
		if err != nil {
			if isUniqueViolation(err) {
				return domain.Conflict("tag_name_exists", "a tag with this name already exists")
			}
			return domain.Internal("tag_create_failed", "failed to create tag").WithCause(err)
		}
		// A freshly created tag is never yet assigned to any site.
		out = fromRow(row, 0)
		return nil
	})
	return out, err
}

// Update handles color-only updates, plain renames, and rename-with-merge, in
// ONE transaction: the registry row + every site/pairing-code carrying the
// old name are rewritten together, so a reader can never observe a
// half-renamed tenant.
func (r *pgRepo) Update(ctx context.Context, in UpdateInput) (UpdateResult, error) {
	var out UpdateResult
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)

		cur, err := q.GetTag(ctx, sqlc.GetTagParams{ID: in.ID, TenantID: in.TenantID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("tag_not_found", "tag not found")
			}
			return domain.Internal("tag_get_failed", "failed to load tag").WithCause(err)
		}

		trimmedName := ""
		if in.Name != nil {
			trimmedName = strings.TrimSpace(*in.Name)
		}

		// Color-only update (Name absent, or Name equal to the current name —
		// a "rename" to the same value is just a no-op rename, avoid tripping
		// the unique-constraint/merge path for it).
		if in.Name == nil || trimmedName == cur.Name {
			if in.Color == nil {
				// True no-op: nothing to change. Return the row as-is.
				usage, uerr := q.CountSitesWithTag(ctx, sqlc.CountSitesWithTagParams{TenantID: in.TenantID, Name: cur.Name})
				if uerr != nil {
					return domain.Internal("tag_usage_count_failed", "failed to count tag usage").WithCause(uerr)
				}
				out = UpdateResult{Tag: fromRow(cur, usage)}
				return nil
			}
			row, rerr := q.RecolorTag(ctx, sqlc.RecolorTagParams{ID: in.ID, TenantID: in.TenantID, Color: *in.Color})
			if rerr != nil {
				return domain.Internal("tag_update_failed", "failed to update tag color").WithCause(rerr)
			}
			usage, uerr := q.CountSitesWithTag(ctx, sqlc.CountSitesWithTagParams{TenantID: in.TenantID, Name: row.Name})
			if uerr != nil {
				return domain.Internal("tag_usage_count_failed", "failed to count tag usage").WithCause(uerr)
			}
			out = UpdateResult{Tag: fromRow(row, usage)}
			return nil
		}

		// Rename attempt: the UNIQUE(tenant_id, name) constraint on site_tags
		// is what detects a collision with an existing tag. Run it inside a
		// SAVEPOINT (pgx.Tx.Begin on an existing Tx) so a caught
		// unique_violation rolls back ONLY this statement rather than
		// poisoning the whole transaction — Postgres aborts every subsequent
		// statement in a tx once one errors (SQLSTATE 25P02), and the merge
		// path below still needs to run more statements in the same tx.
		row, rerr := renameTagRowSavepoint(ctx, tx, in.ID, in.TenantID, trimmedName)
		if rerr != nil {
			if !isUniqueViolation(rerr) {
				return domain.Internal("tag_rename_failed", "failed to rename tag").WithCause(rerr)
			}
			if !in.Merge {
				return domain.Conflict("tag_name_exists", "a tag with this name already exists")
			}
			// Merge into the existing survivor.
			survivor, gerr := q.GetTagByName(ctx, sqlc.GetTagByNameParams{TenantID: in.TenantID, Name: trimmedName})
			if gerr != nil {
				return domain.Internal("tag_merge_lookup_failed", "failed to resolve the merge target tag").WithCause(gerr)
			}
			if err := q.RewriteSiteTagName(ctx, sqlc.RewriteSiteTagNameParams{
				TenantID: in.TenantID, OldName: cur.Name, NewName: survivor.Name,
			}); err != nil {
				return domain.Internal("tag_merge_rewrite_failed", "failed to rewrite site tags for merge").WithCause(err)
			}
			if err := q.RewritePairingCodeTagName(ctx, sqlc.RewritePairingCodeTagNameParams{
				TenantID: in.TenantID, OldName: cur.Name, NewName: survivor.Name,
			}); err != nil {
				return domain.Internal("tag_merge_rewrite_failed", "failed to rewrite pairing code tags for merge").WithCause(err)
			}
			// Delete the source (losing) registry row.
			if _, derr := q.DeleteTagRow(ctx, sqlc.DeleteTagRowParams{ID: cur.ID, TenantID: in.TenantID}); derr != nil {
				return domain.Internal("tag_merge_delete_failed", "failed to delete the merged-away tag").WithCause(derr)
			}
			// An explicit color on the merge request overrides the survivor's
			// color; otherwise the survivor keeps its own color unchanged.
			if in.Color != nil {
				row2, cerr := q.RecolorTag(ctx, sqlc.RecolorTagParams{ID: survivor.ID, TenantID: in.TenantID, Color: *in.Color})
				if cerr != nil {
					return domain.Internal("tag_update_failed", "failed to update merged tag color").WithCause(cerr)
				}
				survivor = row2
			}
			usage, uerr := q.CountSitesWithTag(ctx, sqlc.CountSitesWithTagParams{TenantID: in.TenantID, Name: survivor.Name})
			if uerr != nil {
				return domain.Internal("tag_usage_count_failed", "failed to count tag usage").WithCause(uerr)
			}
			out = UpdateResult{Tag: fromRow(survivor, usage), Merged: true, OldName: cur.Name, MergedInto: survivor.Name}
			return nil
		}

		// Plain rename succeeded (no collision): propagate the new name onto
		// every site/pairing-code that currently carries the OLD name.
		if err := q.RewriteSiteTagName(ctx, sqlc.RewriteSiteTagNameParams{
			TenantID: in.TenantID, OldName: cur.Name, NewName: trimmedName,
		}); err != nil {
			return domain.Internal("tag_rename_rewrite_failed", "failed to rewrite site tags").WithCause(err)
		}
		if err := q.RewritePairingCodeTagName(ctx, sqlc.RewritePairingCodeTagNameParams{
			TenantID: in.TenantID, OldName: cur.Name, NewName: trimmedName,
		}); err != nil {
			return domain.Internal("tag_rename_rewrite_failed", "failed to rewrite pairing code tags").WithCause(err)
		}
		if in.Color != nil {
			row2, cerr := q.RecolorTag(ctx, sqlc.RecolorTagParams{ID: in.ID, TenantID: in.TenantID, Color: *in.Color})
			if cerr != nil {
				return domain.Internal("tag_update_failed", "failed to update tag color").WithCause(cerr)
			}
			row = row2
		}
		usage, uerr := q.CountSitesWithTag(ctx, sqlc.CountSitesWithTagParams{TenantID: in.TenantID, Name: row.Name})
		if uerr != nil {
			return domain.Internal("tag_usage_count_failed", "failed to count tag usage").WithCause(uerr)
		}
		out = UpdateResult{Tag: fromRow(row, usage), OldName: cur.Name}
		return nil
	})
	return out, err
}

// Delete removes a tag fleet-wide: the registry row, every site currently
// carrying it, and every unexpired/unredeemed pairing code carrying it — all
// in one transaction.
func (r *pgRepo) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		cur, err := q.GetTag(ctx, sqlc.GetTagParams{ID: id, TenantID: tenantID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("tag_not_found", "tag not found")
			}
			return domain.Internal("tag_get_failed", "failed to load tag").WithCause(err)
		}
		if err := q.RemoveSiteTagName(ctx, sqlc.RemoveSiteTagNameParams{TenantID: tenantID, Name: cur.Name}); err != nil {
			return domain.Internal("tag_delete_failed", "failed to remove tag from sites").WithCause(err)
		}
		if err := q.RemovePairingCodeTagName(ctx, sqlc.RemovePairingCodeTagNameParams{TenantID: tenantID, Name: cur.Name}); err != nil {
			return domain.Internal("tag_delete_failed", "failed to remove tag from pairing codes").WithCause(err)
		}
		n, err := q.DeleteTagRow(ctx, sqlc.DeleteTagRowParams{ID: id, TenantID: tenantID})
		if err != nil {
			return domain.Internal("tag_delete_failed", "failed to delete tag").WithCause(err)
		}
		if n == 0 {
			return domain.NotFound("tag_not_found", "tag not found")
		}
		return nil
	})
}

// BulkApply upserts `add` into the registry once, then applies the
// dedup(tags ∪ add) − remove delta to each of siteIDs, ALL in one
// transaction. siteIDs must already be filtered to sites the caller is
// authorized to touch (see handler.go's CanAccessSite gate) — a siteID that
// does not exist in this tenant simply yields updated=false (0 rows
// affected), never an error, so the caller can distinguish "not found" from
// "not authorized" without either aborting the whole batch.
func (r *pgRepo) BulkApply(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID, add, remove []string) (map[uuid.UUID]bool, error) {
	updated := make(map[uuid.UUID]bool, len(siteIDs))
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if len(add) > 0 {
			if err := q.UpsertTagNames(ctx, sqlc.UpsertTagNamesParams{TenantID: tenantID, Names: add}); err != nil {
				return domain.Internal("tag_bulk_apply_failed", "failed to register tag names").WithCause(err)
			}
		}
		addArr := add
		if addArr == nil {
			addArr = []string{}
		}
		removeArr := remove
		if removeArr == nil {
			removeArr = []string{}
		}
		for _, siteID := range siteIDs {
			n, err := q.ApplyTagDeltaToSite(ctx, sqlc.ApplyTagDeltaToSiteParams{
				Add:      addArr,
				Remove:   removeArr,
				TenantID: tenantID,
				SiteID:   siteID,
			})
			if err != nil {
				return domain.Internal("tag_bulk_apply_failed", "failed to apply tag delta").WithCause(err)
			}
			updated[siteID] = n > 0
		}
		return nil
	})
	return updated, err
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// renameTagRowSavepoint runs RenameTagRow inside a SAVEPOINT so a caught
// unique_violation can be rolled back without aborting the caller's whole
// transaction (see the call site's comment).
func renameTagRowSavepoint(ctx context.Context, tx pgx.Tx, id, tenantID uuid.UUID, name string) (sqlc.SiteTag, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return sqlc.SiteTag{}, err
	}
	row, err := sqlc.New(sp).RenameTagRow(ctx, sqlc.RenameTagRowParams{ID: id, TenantID: tenantID, Name: name})
	if err != nil {
		_ = sp.Rollback(ctx)
		return sqlc.SiteTag{}, err
	}
	if err := sp.Commit(ctx); err != nil {
		return sqlc.SiteTag{}, err
	}
	return row, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func fromRow(row sqlc.SiteTag, usage int64) Tag {
	return Tag{
		ID:         row.ID,
		TenantID:   row.TenantID,
		Name:       row.Name,
		Color:      row.Color,
		UsageCount: usage,
		CreatedAt:  row.CreatedAt,
	}
}
