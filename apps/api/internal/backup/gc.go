package backup

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// gcSafetyHorizon is the freshness skip applied to snapshot DELETION: a
// completed snapshot created within this window of now() is never pruned even
// when the rolling-window/keep-last rules would expire it. This protects a
// just-completed snapshot (and the chain tip it may anchor) from a race with a
// concurrent submission that has not yet stamped all of its rows. The chunk
// SWEEP has its own, stricter grace floor (min(markStart, in-flight floor)).
const gcSafetyHorizon = time.Hour

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

// RunRetentionGC applies the ADR-050 MARK-AND-SWEEP retention policy for one
// tenant. It is content-addressed-dedup aware: a chunk is deleted ONLY when it
// is unreachable from EVERY retained snapshot across ALL sites in the tenant
// (dedup is tenant-global) AND it predates the grace floor. refcount is NEVER
// consulted for any delete decision (post-ADR-050 it is observability-only):
// the agent only re-submits changed/new files, so a carry-forward chunk's
// origin file_index row lives in exactly one generation and refcount counts
// origin refs, not live refs.
//
// Phases (per tenant):
//
//  0. SELECTION — flag monthly archives, gather the expired snapshot IDs per
//     site (rolling-window + keep-last), unchanged from the pre-ADR-050 rules.
//  1. GRACE FLOOR — markStart := DB now(); inflightFloor := MIN(created_at) of
//     pending/running snapshots; effectiveFloor := min(markStart, inflightFloor).
//  2. RETAINED SET — all completed snapshots MINUS expired, UNION archives, then
//     chain-aware expansion: for each retained chained snapshot, pin generations
//     0..(chain's highest retained generation) so a carry-forward chunk's origin
//     generation stays reachable under a live tip.
//  3. MARK — liveSet := union of reachableChunks(retainedMaxGen) over the
//     retained set. FAIL-CLOSED: any error aborts the sweep (delete nothing).
//  4. SWEEP — under a per-tenant SESSION advisory lock, stream every chunk
//     (short per-page txns; object deletes outside any tx); delete (object
//     FIRST, then row) each chunk NOT in liveSet AND
//     GREATEST(created_at, last_referenced_at) < effectiveFloor. The
//     last_referenced_at term protects an OLD chunk an in-flight backup
//     re-references via tenant-global dedup (the dedup oracle bumped it).
//  5. METADATA PRUNE — delete the expired snapshot rows (cascade file_index +
//     manifest); no object delete, no refcount decref.
//
// Returns the number of snapshots deleted and chunk objects swept.
func (s *Service) RunRetentionGC(ctx context.Context, tenantID uuid.UUID) (snapshotsDeleted, chunksDeleted int, err error) {
	// --- Phase 0: SELECTION (per site) ----------------------------------------
	siteIDs, err := s.repo.ListSiteIDsWithSnapshots(ctx, tenantID)
	if err != nil {
		return 0, 0, err
	}

	// expiredIDs maps snapshot ID → site ID for every snapshot selected for
	// metadata deletion across all sites. The site ID is carried so Phase 5 can
	// construct the manifest key (tenant/<tenantID>/site/<siteID>/backup/<id>/manifest.json)
	// without an extra DB round-trip.
	expiredIDs := map[uuid.UUID]uuid.UUID{}
	// retainedMetas: every completed snapshot that survives the prune, across all
	// sites (dedup is tenant-global, so the retained set must be tenant-wide).
	var retainedMetas []SnapshotMeta
	// chainMaxRetainedGen: per chain_id, the highest retained generation.
	chainMaxRetainedGen := map[uuid.UUID]int{}

	now := s.clock.Now()
	horizonCutoff := now.Add(-gcSafetyHorizon)

	for _, siteID := range siteIDs {
		metas, merr := s.repo.ListCompletedSnapshotsForSite(ctx, tenantID, siteID)
		if merr != nil {
			return snapshotsDeleted, chunksDeleted, merr
		}

		// Resolve per-site retention settings (schedule overrides defaults).
		archiveKeep := s.monthlyArchiveKeep
		retDays := s.retentionDays
		keepLast := -1

		sched, serr := s.repo.GetSchedule(ctx, tenantID, siteID)
		if serr == nil {
			archiveKeep = int(sched.MonthlyArchiveKeep)
			retDays = int(sched.RetentionDays)
			keepLast = int(sched.KeepLast)
		} else if de, ok := domain.AsDomain(serr); !ok || de.Kind != domain.KindNotFound {
			return snapshotsDeleted, chunksDeleted, serr
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

		// Gather the prune set for this site, then apply the freshness horizon:
		// a snapshot newer than gcSafetyHorizon is force-retained.
		toDelete := gatherExpiredForSite(metas, keep, retDays, keepLast, now)
		deleteSet := map[uuid.UUID]bool{}
		for _, id := range toDelete {
			deleteSet[id] = true
		}
		for _, m := range metas {
			if deleteSet[m.ID] && m.CreatedAt.After(horizonCutoff) {
				delete(deleteSet, m.ID) // too fresh to prune
			}
		}

		for _, m := range metas {
			// Track C (m49): a locked snapshot is permanently retained — never
			// auto-pruned regardless of age / keep-last rules. Treat it like a
			// retained row even if the policy would otherwise delete it.
			if m.Locked {
				delete(deleteSet, m.ID)
			}
			if deleteSet[m.ID] {
				expiredIDs[m.ID] = siteID
				continue
			}
			// Retained. Track it and its chain's highest retained generation.
			retainedMetas = append(retainedMetas, m)
			if m.ChainID != nil {
				if g, ok := chainMaxRetainedGen[*m.ChainID]; !ok || m.Generation > g {
					chainMaxRetainedGen[*m.ChainID] = m.Generation
				}
			}
		}
	}

	// --- Phase 1: GRACE FLOOR -------------------------------------------------
	// FAIL-CLOSED: if any of the floor inputs cannot be established, abort.
	markStart, derr := s.repo.DBNow(ctx, tenantID)
	if derr != nil {
		slog.WarnContext(ctx, "backup gc: cannot read DB clock — aborting sweep (delete nothing)",
			slog.String("tenant_id", tenantID.String()), slog.Any("error", derr))
		return snapshotsDeleted, chunksDeleted, derr
	}
	inflightFloor, ferr := s.repo.ListInFlightSnapshotFloor(ctx, tenantID)
	if ferr != nil {
		slog.WarnContext(ctx, "backup gc: cannot read in-flight floor — aborting sweep (delete nothing)",
			slog.String("tenant_id", tenantID.String()), slog.Any("error", ferr))
		return snapshotsDeleted, chunksDeleted, ferr
	}
	effectiveFloor := markStart
	if !inflightFloor.IsZero() && inflightFloor.Before(effectiveFloor) {
		effectiveFloor = inflightFloor
	}

	// --- Phase 2: RETAINED SET (chain-aware expansion) ------------------------
	// For each retained chained snapshot, pin generations 0..maxRetainedGen so a
	// carry-forward chunk's origin generation is reachable under a live tip.
	// We collect the distinct snapshots to mark (by ID) to avoid double work.
	markSnaps := map[uuid.UUID]Snapshot{}
	for chainID, maxGen := range chainMaxRetainedGen {
		chainSnaps, cerr := s.repo.ListChainSnapshots(ctx, tenantID, chainID, maxGen)
		if cerr != nil {
			slog.WarnContext(ctx, "backup gc: chain expansion failed — aborting sweep (delete nothing)",
				slog.String("tenant_id", tenantID.String()),
				slog.String("chain_id", chainID.String()), slog.Any("error", cerr))
			return snapshotsDeleted, chunksDeleted, cerr
		}
		for _, cs := range chainSnaps {
			markSnaps[cs.ID] = cs
		}
	}
	// Non-chained retained snapshots (chain_id == nil) must still be marked via
	// their own manifest. They are not covered by the chain expansion above.
	for _, m := range retainedMetas {
		if m.ChainID == nil {
			if _, ok := markSnaps[m.ID]; !ok {
				snap, gerr := s.repo.GetSnapshot(ctx, tenantID, m.ID)
				if gerr != nil {
					slog.WarnContext(ctx, "backup gc: load retained snapshot failed — aborting sweep (delete nothing)",
						slog.String("tenant_id", tenantID.String()),
						slog.String("snapshot_id", m.ID.String()), slog.Any("error", gerr))
					return snapshotsDeleted, chunksDeleted, gerr
				}
				markSnaps[m.ID] = snap
			}
		}
	}

	// --- Phase 3: MARK --------------------------------------------------------
	liveSet := map[string]struct{}{}
	for _, snap := range markSnaps {
		retainedMaxGen := snap.Generation
		if snap.ChainID != nil {
			if g, ok := chainMaxRetainedGen[*snap.ChainID]; ok {
				retainedMaxGen = g
			}
		}
		reach, rerr := s.reachableChunks(ctx, tenantID, snap, retainedMaxGen)
		if rerr != nil {
			// FAIL-CLOSED: any reachability error means we cannot prove the live
			// set is complete — abort and delete nothing.
			slog.WarnContext(ctx, "backup gc: reachability failed — aborting sweep (delete nothing)",
				slog.String("tenant_id", tenantID.String()),
				slog.String("snapshot_id", snap.ID.String()), slog.Any("error", rerr))
			return snapshotsDeleted, chunksDeleted, rerr
		}
		for h := range reach {
			liveSet[h] = struct{}{}
		}
	}

	// --- Phase 4: SWEEP (under per-tenant advisory lock) ----------------------
	swept, swerr := s.sweepChunks(ctx, tenantID, liveSet, effectiveFloor)
	chunksDeleted += swept
	if swerr != nil {
		return snapshotsDeleted, chunksDeleted, swerr
	}

	// --- Phase 5: METADATA PRUNE ----------------------------------------------
	// A generation that the chain-aware expansion pinned under a live tip must
	// NOT be metadata-pruned: planRestoreChain requires every generation 0..tip
	// to exist (CHECK 1), and deleting an older generation's file_index rows would
	// make a carry-forward chunk unreachable on a LATER GC run (re-sweeping it).
	// So we skip any expired id that also appears in the marked (pinned) set.
	for id, sid := range expiredIDs {
		if _, pinned := markSnaps[id]; pinned {
			continue
		}
		if dserr := s.repo.DeleteSnapshot(ctx, tenantID, id); dserr != nil {
			return snapshotsDeleted, chunksDeleted, dserr
		}
		snapshotsDeleted++
		// Delete the per-snapshot manifest.json index object. Best-effort: the DB
		// row is already gone; an error or an absent object is logged and ignored.
		s.deleteManifestIndex(ctx, tenantID, sid, id)
	}

	return snapshotsDeleted, chunksDeleted, nil
}

// sweepChunks runs the per-tenant chunk sweep under the GC SESSION advisory lock
// (held by the repo on one pinned connection for the whole pass). The repo
// invokes this del callback for each candidate INSIDE a SHORT per-chunk
// transaction that already holds a row-level FOR UPDATE lock on the chunk and
// has re-read the FRESH created_at / last_referenced_at. So this callback runs
// with the row pinned: a concurrent dedup touch (ExistingChunkHashes) is blocked
// until the per-chunk tx commits. For a chunk NOT in the live set AND whose
// newest liveness boundary (GREATEST(created_at, last_referenced_at)) predates
// the grace floor, it deletes the object FIRST (idempotent; 404 == ok) WHILE the
// row lock is held; the repo removes the row SECOND in the same tx (re-checking
// the GREATEST predicate at the DB). Object-first/row-second means a crash
// between the two rolls the tx back, leaving a dangling row pointing at a missing
// object that the next idempotent sweep self-heals.
//
// The GREATEST(created_at, last_referenced_at) rule is the ADR-050 data-loss
// fix: an OLD chunk that an in-flight backup re-references via tenant-global
// dedup has had its last_referenced_at bumped to ~now() by the dedup oracle
// (ExistingChunkHashes), so it clears the floor and is kept even though its
// created_at is ancient and its last completed referrer expired this run. The
// FOR UPDATE serialization closes the residual TOCTOU: the touch can no longer
// land BETWEEN this floor re-check and the object delete, because the row is
// locked for the whole critical section.
//
// Returns the number of chunks swept; a false advisory-lock acquisition skips
// the tenant (returns 0, nil).
func (s *Service) sweepChunks(ctx context.Context, tenantID uuid.UUID, liveSet map[string]struct{}, floor time.Time) (int, error) {
	var (
		swept    int
		acquired bool
	)
	err := s.repo.SweepTenantChunks(ctx, tenantID, floor, &acquired, func(c SweepChunk) (bool, error) {
		if _, live := liveSet[c.Blake3]; live {
			return false, nil
		}
		// Use the newest of created_at / last_referenced_at as the liveness
		// boundary so a dedup-touched old chunk is protected.
		boundary := c.CreatedAt
		if c.LastReferencedAt.After(boundary) {
			boundary = c.LastReferencedAt
		}
		if !boundary.Before(floor) {
			return false, nil // within the grace floor — keep
		}
		// Object FIRST (idempotent; 404 == ok). The repo then deletes the row in a
		// short tx, re-checking GREATEST(created_at, last_referenced_at) < floor.
		if derr := s.store.Delete(ctx, c.S3Key); derr != nil {
			// A delete failure must NOT remove the row — leave both in place for
			// the next sweep. Surface the error so the pass is retried.
			return false, derr
		}
		swept++
		return true, nil
	})
	if !acquired {
		return 0, nil // another sweep holds the lock; skip silently.
	}
	return swept, err
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
