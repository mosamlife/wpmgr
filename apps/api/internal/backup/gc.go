package backup

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// RunRetentionGCAllTenants runs the retention GC for every tenant that has
// completed snapshots. Used by the periodic GC job. Per-tenant errors are
// returned aggregated as the first failure; the caller logs and continues on
// the next interval.
func (s *Service) RunRetentionGCAllTenants(ctx context.Context) (totalSnapshots, totalChunks int, err error) {
	tenants, lerr := s.repo.ListTenantsForGC(ctx)
	if lerr != nil {
		return 0, 0, lerr
	}
	for _, tenantID := range tenants {
		sd, cd, gerr := s.RunRetentionGC(ctx, tenantID)
		totalSnapshots += sd
		totalChunks += cd
		if gerr != nil && err == nil {
			err = gerr
		}
	}
	return totalSnapshots, totalChunks, err
}

// RunRetentionGC applies the retention policy for one tenant: it flags the
// monthly-archive snapshots (newest per month, up to monthly_archive_keep), then
// deletes completed, non-archived snapshots older than the rolling window. For
// each deleted snapshot it decrements the refcount of every chunk the manifest
// referenced; chunks that reach refcount zero are deleted from object storage
// and their rows removed. Shared chunks (still referenced by a surviving
// snapshot) are retained.
//
// This is the authoritative GC entry point used by the periodic GC job (per
// tenant) and by tests. It returns the number of snapshots deleted and chunks
// removed from storage.
func (s *Service) RunRetentionGC(ctx context.Context, tenantID uuid.UUID) (snapshotsDeleted, chunksDeleted int, err error) {
	// 1. Flag monthly archives per site so they survive the rolling-window prune.
	siteIDs, err := s.repo.ListSiteIDsWithSnapshots(ctx, tenantID)
	if err != nil {
		return 0, 0, err
	}
	for _, siteID := range siteIDs {
		metas, merr := s.repo.ListCompletedSnapshotsForSite(ctx, tenantID, siteID)
		if merr != nil {
			return snapshotsDeleted, chunksDeleted, merr
		}
		keep := archiveIDs(metas, s.monthlyArchiveKeep)
		for _, m := range metas {
			want := keep[m.ID]
			if m.Archived != want {
				if aerr := s.repo.SetSnapshotArchived(ctx, tenantID, m.ID, want); aerr != nil {
					return snapshotsDeleted, chunksDeleted, aerr
				}
			}
		}
	}

	// 2. Prune the rolling window: expired = completed, non-archived, older than
	// retentionDays.
	cutoff := s.clock.Now().Add(-time.Duration(s.retentionDays) * 24 * time.Hour)
	expired, err := s.repo.ListExpiredSnapshots(ctx, tenantID, cutoff)
	if err != nil {
		return snapshotsDeleted, chunksDeleted, err
	}
	for _, snap := range expired {
		orphans, derr := s.repo.DeleteSnapshotAndDecref(ctx, tenantID, snap.ID)
		if derr != nil {
			return snapshotsDeleted, chunksDeleted, derr
		}
		snapshotsDeleted++

		// Delete orphaned chunk objects from storage, then remove their rows. We
		// delete from S3 first so a crash leaves a zero-refcount row (reconcilable)
		// rather than a dangling object with no row.
		var deletedHashes []string
		for _, o := range orphans {
			if serr := s.store.Delete(ctx, o.S3Key); serr != nil {
				// Best-effort: leave the row (refcount 0) so a later GC retries the
				// object delete; do not fail the whole GC for one object.
				continue
			}
			deletedHashes = append(deletedHashes, o.Blake3)
			chunksDeleted++
		}
		if len(deletedHashes) > 0 {
			if oerr := s.repo.DeleteOrphanChunks(ctx, tenantID, deletedHashes); oerr != nil {
				return snapshotsDeleted, chunksDeleted, oerr
			}
		}
	}
	return snapshotsDeleted, chunksDeleted, nil
}
