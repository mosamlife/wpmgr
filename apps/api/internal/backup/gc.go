package backup

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
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
// Per-site schedule values drive retention when a schedule exists for the site:
//   - retention_days (age-based cutoff)
//   - keep_last (count-based lower bound — the GC respects whichever limit is
//     stricter, i.e. it may prune more aggressively than age alone when
//     keep_last is smaller than what the age window would retain)
//   - monthly_archive_keep (long-term archive count)
//
// When no schedule exists for a site, the server-wide defaults apply.
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

		// Resolve per-site retention settings from the schedule, falling back to
		// the server-wide defaults when no schedule exists.
		archiveKeep := s.monthlyArchiveKeep
		retDays := s.retentionDays
		keepLast := -1 // -1 = no count-based limit (pre-M17 behaviour)

		sched, serr := s.repo.GetSchedule(ctx, tenantID, siteID)
		if serr == nil {
			// Schedule found: use per-schedule values.
			archiveKeep = int(sched.MonthlyArchiveKeep)
			retDays = int(sched.RetentionDays)
			keepLast = int(sched.KeepLast)
		} else {
			// Only propagate non-NotFound errors.
			if de, ok := domain.AsDomain(serr); !ok || de.Kind != domain.KindNotFound {
				return snapshotsDeleted, chunksDeleted, serr
			}
		}

		keep := archiveIDs(metas, archiveKeep)
		for _, m := range metas {
			want := keep[m.ID]
			if m.Archived != want {
				if aerr := s.repo.SetSnapshotArchived(ctx, tenantID, m.ID, want); aerr != nil {
					return snapshotsDeleted, chunksDeleted, aerr
				}
			}
		}

		// 2a. Per-site prune: build the set of IDs to delete for this site.
		toDelete := gatherExpiredForSite(metas, keep, retDays, keepLast, s.clock.Now())
		for _, id := range toDelete {
			orphans, derr := s.repo.DeleteSnapshotAndDecref(ctx, tenantID, id)
			if derr != nil {
				return snapshotsDeleted, chunksDeleted, derr
			}
			snapshotsDeleted++

			var deletedHashes []string
			for _, o := range orphans {
				if serr2 := s.store.Delete(ctx, o.S3Key); serr2 != nil {
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
	}
	return snapshotsDeleted, chunksDeleted, nil
}

// gatherExpiredForSite returns the snapshot IDs that should be deleted for a
// single site, applying the strictest of the age-based (retentionDays) and
// count-based (keepLast) limits. Monthly archives are never deleted.
//
// metas must be sorted newest-first (as ListCompletedSnapshotsForSite returns).
// keepLast < 0 disables the count-based limit (pre-M17 behaviour for sites
// without a schedule).
func gatherExpiredForSite(metas []SnapshotMeta, archives map[uuid.UUID]bool, retentionDays, keepLast int, now time.Time) []uuid.UUID {
	cutoff := now.Add(-time.Duration(retentionDays) * 24 * time.Hour)

	// Identify non-archived snapshots in newest-first order.
	var candidates []SnapshotMeta
	for _, m := range metas {
		if !archives[m.ID] {
			candidates = append(candidates, m)
		}
	}

	// Sort newest-first (should already be, but defensive).
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].CreatedAt.After(candidates[j].CreatedAt)
	})

	// Build delete set: union of (age-expired) and (count-over-keepLast),
	// but never delete what the other limit would retain.
	//
	// Strategy: keep the newest min(keepLast, <age-retained count>) snapshots;
	// delete the rest. When keepLast < 0 (no count limit), only the age cutoff
	// applies.
	var toDelete []uuid.UUID
	for i, m := range candidates {
		aged := !m.CreatedAt.After(cutoff)
		overCount := keepLast >= 0 && i >= keepLast

		if aged || overCount {
			toDelete = append(toDelete, m.ID)
		}
	}
	return toDelete
}
