package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// SiteInfo is the minimal site projection the backup service needs: identity,
// the agent target URL, enrollment status, and the age PUBLIC recipient backups
// for the site are encrypted to. WpTimezone/WpGmtOffset are the M17 timezone
// fields for schedule computation.
type SiteInfo struct {
	ID           uuid.UUID
	URL          string
	Enrolled     bool
	AgeRecipient string
	WpTimezone   string
	WpGmtOffset  float64
}

// SiteLookup resolves the target site (implemented by the site service, wired
// in main, so this package needs no site import).
type SiteLookup interface {
	GetBackupSiteInfo(ctx context.Context, tenantID, siteID uuid.UUID) (SiteInfo, error)
}

// Enqueuer schedules the background backup/restore/GC jobs (River, wired in
// main).
type Enqueuer interface {
	EnqueueBackup(ctx context.Context, tenantID, snapshotID uuid.UUID) error
	// EnqueueRestore enqueues a restore job. restoreRunID is the restore_run row
	// that was already persisted by CreateRestore; the worker reads it from the
	// job args to update the run status as it progresses. uuid.Nil is accepted
	// when the restore run store is not wired (graceful degradation).
	EnqueueRestore(ctx context.Context, tenantID, snapshotID uuid.UUID, sel RestoreSelection, restoreRunID uuid.UUID) error
}

// Presigner mints presigned PUT/GET URLs over object storage and reports keys.
type Presigner interface {
	PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error)
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	Delete(ctx context.Context, key string) error
}

// PresignerForSnapshot is the ADR-036 P1 storage adapter routing interface:
// given a snapshot it returns the Presigner that should service its
// chunk-upload/download for the snapshot's destination. The blobstore.Registry
// in main implements this; the legacy `s.store` field is kept as the fallback
// when no Registry is wired (dev / tests).
type PresignerForSnapshot interface {
	PresignerForSnapshot(ctx context.Context, snap Snapshot) (Presigner, error)
}

// Service holds the backup orchestration logic.
type Service struct {
	repo     Repo
	sites    SiteLookup
	enqueuer Enqueuer
	store    Presigner
	// registry is the optional ADR-036 P1 router: when non-nil, presignPut
	// consults it to find the right Presigner for the snapshot's destination.
	// Falls back to `store` (the CP-global Store) when nil OR when the
	// registry returns the same default Store anyway.
	registry   PresignerForSnapshot
	clock      domain.Clock
	hub        *Hub
	presignTTL time.Duration
	// retention defaults (overridable per schedule).
	retentionDays      int
	monthlyArchiveKeep int
	// restoreRuns persists first-class restore run entities + their phase logs.
	// Optional: when nil, the restore_run persistence is silently skipped (so
	// existing integrations that have not set the store yet keep working).
	restoreRuns RestoreRunStore
	// scheduleRuns persists backup_schedule_runs (M17 queue materialization).
	// Optional: when nil, schedule run rows are skipped (graceful degradation).
	scheduleRuns ScheduleRunStore
}

// SetRegistry wires the ADR-036 P1 storage-adapter router. Calling this AFTER
// NewService routes every subsequent presign through the registry; callers
// that haven't migrated keep the legacy single-store path. Safe to call once
// at startup before serving traffic.
func (s *Service) SetRegistry(r PresignerForSnapshot) { s.registry = r }

// Config tunes the service.
type Config struct {
	PresignTTL         time.Duration
	RetentionDays      int
	MonthlyArchiveKeep int
}

// SetEnqueuer wires the River enqueuer after the River client is started
// (resolving the client<-enqueuer<-service<-worker construction cycle, mirroring
// the update package). MUST be called before any backup/restore is created.
func (s *Service) SetEnqueuer(e Enqueuer) { s.enqueuer = e }

// SetHub wires the in-process SSE pub/sub hub. Optional: when nil, all Publish
// calls are no-ops (so unit/integration tests need not construct one). Mirrors
// the M3 update hub wiring; see internal/backup/hub.go.
func (s *Service) SetHub(h *Hub) { s.hub = h }

// SetRestoreRunStore wires the restore-run persistence store. Call this once
// at startup (after NewService) before serving traffic. Optional: if never
// called, restore_run rows are not created (graceful degradation).
func (s *Service) SetRestoreRunStore(rs RestoreRunStore) { s.restoreRuns = rs }

// SetScheduleRunStore wires the schedule-run persistence store (M17). Call
// once at startup after NewService. Optional: if never called, schedule_run
// rows are silently skipped (graceful degradation).
func (s *Service) SetScheduleRunStore(ss ScheduleRunStore) { s.scheduleRuns = ss }

// publish is a nil-safe helper around hub.Publish.
func (s *Service) publish(ev BackupEvent) {
	if s.hub == nil {
		return
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = s.clock.Now().UTC()
	}
	s.hub.Publish(ev)
}

// NewService builds a backup Service.
func NewService(repo Repo, sites SiteLookup, enqueuer Enqueuer, store Presigner, clock domain.Clock, cfg Config) *Service {
	if cfg.PresignTTL <= 0 {
		cfg.PresignTTL = time.Hour
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = 30
	}
	if cfg.MonthlyArchiveKeep < 0 {
		cfg.MonthlyArchiveKeep = 0
	}
	return &Service{
		repo:               repo,
		sites:              sites,
		enqueuer:           enqueuer,
		store:              store,
		clock:              clock,
		presignTTL:         cfg.PresignTTL,
		retentionDays:      cfg.RetentionDays,
		monthlyArchiveKeep: cfg.MonthlyArchiveKeep,
	}
}

// CreateBackup validates the request, records a pending snapshot, and enqueues a
// background backup job. The site MUST be enrolled and have an age recipient set
// (a backup the operator could never decrypt is useless).
func (s *Service) CreateBackup(ctx context.Context, tenantID, siteID, createdBy uuid.UUID, kind string) (Snapshot, error) {
	if tenantID == uuid.Nil {
		return Snapshot{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	kind = strings.TrimSpace(strings.ToLower(kind))
	if kind == "" {
		kind = KindFull
	}
	if !validKind(kind) {
		return Snapshot{}, domain.Validation("invalid_kind", "kind must be files, db, or full")
	}

	si, err := s.sites.GetBackupSiteInfo(ctx, tenantID, siteID)
	if err != nil {
		return Snapshot{}, err
	}
	if !si.Enrolled {
		return Snapshot{}, domain.Validation("site_not_enrolled", "the site is not enrolled; only enrolled sites can be backed up")
	}
	if si.AgeRecipient == "" {
		return Snapshot{}, domain.Validation("age_recipient_missing", "the site has no age recipient set; configure backup encryption (PUT the backup schedule with a recipient or set it on the site) before backing up")
	}

	snap, err := s.repo.CreateSnapshot(ctx, CreateSnapshotInput{
		TenantID:     tenantID,
		SiteID:       siteID,
		CreatedBy:    createdBy,
		Kind:         kind,
		AgeRecipient: si.AgeRecipient,
	})
	if err != nil {
		return Snapshot{}, err
	}
	if err := s.enqueuer.EnqueueBackup(ctx, tenantID, snap.ID); err != nil {
		return snap, err
	}
	return snap, nil
}

// GetSnapshot returns a snapshot with its manifest entries.
func (s *Service) GetSnapshot(ctx context.Context, tenantID, snapshotID uuid.UUID) (Snapshot, []ManifestEntry, error) {
	if tenantID == uuid.Nil {
		return Snapshot{}, nil, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	snap, err := s.repo.GetSnapshot(ctx, tenantID, snapshotID)
	if err != nil {
		return Snapshot{}, nil, err
	}
	entries, err := s.repo.ListManifest(ctx, tenantID, snapshotID)
	if err != nil {
		return Snapshot{}, nil, err
	}
	return snap, entries, nil
}

// ListSnapshots returns a page of a site's snapshots.
func (s *Service) ListSnapshots(ctx context.Context, tenantID, siteID uuid.UUID, limit, offset int32) ([]Snapshot, error) {
	if tenantID == uuid.Nil {
		return nil, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	limit, offset = normalizePage(limit, offset)
	return s.repo.ListSnapshotsForSite(ctx, tenantID, siteID, limit, offset)
}

// RestoreSelection is the (possibly partial) restore request: full, by path, or
// by db table, plus the M6 component switch and the keep-old-files toggle.
//
// Components scopes the restore to a subset of the snapshot's content kinds —
// either {"files"}, {"db"}, or both. Empty means "all components", which is the
// historical default and identical to a snapshot-kind-driven full restore. The
// component switch is composable with Paths/DBTables: ["files"] + Paths=[...]
// restores only the listed files; ["db"] + DBTables=[...] restores only the
// listed tables.
//
// KeepOldFiles is a UI-driven safety affordance plumbed through to the agent so
// the agent can decide whether to preserve the pre-restore wp-content tree as a
// rollback fallback. The CP does not act on this flag itself.
type RestoreSelection struct {
	Full         bool
	Paths        []string
	DBTables     []string
	Components   []string
	KeepOldFiles bool
}

// CreateRestoreResult is returned by CreateRestore so callers get both the
// snapshot and the newly-created restore_run ID.
type CreateRestoreResult struct {
	Snapshot     Snapshot
	RestoreRunID uuid.UUID // uuid.Nil when the store is not wired
}

// CreateRestore validates a restore request against the snapshot's manifest,
// inserts a restore_runs row (if the store is wired), and enqueues a
// background restore job. The selection is validated here so an invalid
// path/table fails fast with a 422 (the worker only assembles the presigned
// plan).
func (s *Service) CreateRestore(ctx context.Context, tenantID, snapshotID uuid.UUID, sel RestoreSelection, triggeredBy string) (CreateRestoreResult, error) {
	if tenantID == uuid.Nil {
		return CreateRestoreResult{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	snap, err := s.repo.GetSnapshot(ctx, tenantID, snapshotID)
	if err != nil {
		return CreateRestoreResult{}, err
	}
	if snap.Status != StatusCompleted {
		return CreateRestoreResult{}, domain.Validation("snapshot_not_restorable", "only a completed snapshot can be restored")
	}
	entries, err := s.repo.ListManifest(ctx, tenantID, snapshotID)
	if err != nil {
		return CreateRestoreResult{}, err
	}
	// Validate the selection resolves to >=1 entry.
	if _, err := selectEntries(entries, sel); err != nil {
		return CreateRestoreResult{}, err
	}

	// Persist the restore run (best-effort: a store failure is returned as an
	// error since the run ID must be threaded into the job args).
	var restoreRunID uuid.UUID
	if s.restoreRuns != nil {
		// Derive the "mode" from the selection: full/partial files/db.
		mode := deriveWireKind(snap.Kind, sel.Components)
		if sel.Full || (len(sel.Paths) == 0 && len(sel.DBTables) == 0 && len(sel.Components) == 0) {
			mode = snap.Kind // full restore of whatever the snapshot was
		}
		run, rerr := s.restoreRuns.CreateRestoreRun(ctx, CreateRestoreRunInput{
			TenantID:    tenantID,
			SiteID:      snap.SiteID,
			SnapshotID:  snapshotID,
			Mode:        mode,
			Components:  sel.Components,
			Selection:   marshalSelection(sel),
			TriggeredBy: triggeredBy,
		})
		if rerr != nil {
			return CreateRestoreResult{}, rerr
		}
		restoreRunID = run.ID
	}

	if err := s.enqueuer.EnqueueRestore(ctx, tenantID, snapshotID, sel, restoreRunID); err != nil {
		return CreateRestoreResult{}, err
	}
	return CreateRestoreResult{Snapshot: snap, RestoreRunID: restoreRunID}, nil
}

// PlanRestore assembles the ADR-034 v0.8.1 restore plan for a (possibly partial)
// selection. Used by the restore worker. It resolves each selected manifest
// entry's ordered chunk hashes to their object-store keys and mints a presigned
// GET per chunk.
//
// Wire shape: per-artifact-part `logical_path` (taken from the manifest entry's
// stored Path) with an ordered list of presigned chunks. The other M4 per-entry
// fields (entry_kind / table_name / mode / size) are intentionally NOT on the
// wire — the agent's restore engine drives reassembly off the logical_path
// filename and chunk count, not off CP-side typing. CP still uses those fields
// internally (selection routing, DB validation) but they stay off the wire.
//
// `restoreID` is the CP-generated dedup key the worker minted for this attempt;
// it is echoed back in every agent /progress POST so the SSE event carries it.
func (s *Service) PlanRestore(ctx context.Context, tenantID, snapshotID uuid.UUID, sel RestoreSelection, restoreID, progressEndpoint string) (agentcmd.RestoreRequest, Snapshot, SiteInfo, error) {
	snap, err := s.repo.GetSnapshot(ctx, tenantID, snapshotID)
	if err != nil {
		return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, err
	}
	si, err := s.sites.GetBackupSiteInfo(ctx, tenantID, snap.SiteID)
	if err != nil {
		return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, err
	}
	entries, err := s.repo.ListManifest(ctx, tenantID, snapshotID)
	if err != nil {
		return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, err
	}
	selected, err := selectEntries(entries, sel)
	if err != nil {
		return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, err
	}

	// Resolve all distinct chunk hashes across selected entries to s3 keys.
	distinct := map[string]struct{}{}
	for _, e := range selected {
		for _, h := range e.ChunkHashes {
			distinct[h] = struct{}{}
		}
	}
	hashes := make([]string, 0, len(distinct))
	for h := range distinct {
		hashes = append(hashes, h)
	}
	chunks, err := s.repo.ExistingChunkHashes(ctx, tenantID, hashes)
	if err != nil {
		return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, err
	}

	// Mint a presigned GET per distinct chunk (deduped across entries).
	getURLs := make(map[string]agentcmd.RestoreChunk, len(chunks))
	for h, c := range chunks {
		// Defense-in-depth: the s3 key MUST be namespaced to this tenant. The
		// content-addressed key the repo stored is chunks/<tenant>/<blake3>;
		// never presign a key outside this tenant's prefix.
		expected := chunkS3Key(tenantID, h)
		if c.S3Key != expected {
			return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, domain.Internal("backup_chunk_key_mismatch", "stored chunk key is outside the tenant prefix")
		}
		url, perr := s.store.PresignGet(ctx, c.S3Key, s.presignTTL)
		if perr != nil {
			return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, domain.Internal("backup_presign_get_failed", "failed to presign chunk").WithCause(perr)
		}
		getURLs[h] = agentcmd.RestoreChunk{Hash: h, URL: url, Size: c.Size}
	}

	// Derive the agent-wire `kind` from the requested components (M6 / Track 2).
	// Empty Components = no component filter = use whatever the snapshot itself
	// was taken as (the historical behaviour). When Components IS set, the wire
	// kind narrows accordingly so the agent skips the unrequested half of the
	// restore engine even on a `full` snapshot.
	wireKind := deriveWireKind(snap.Kind, sel.Components)

	// P0 URL rewriter (ADR-036): derive target URLs from the live site for
	// the agent's URL_REWRITE phase. Source URLs come from the snapshot
	// itself — populated at backup time when the manifest carries them
	// (post-ADR-036), empty otherwise (the agent falls back to reading the
	// dump's banner comments). V1 simplification: we send target_site_url
	// and target_home_url derived from Site.URL; the agent derives content
	// and upload URLs from there. When source URLs match target URLs the
	// agent's URL_REWRITE phase short-circuits to a no-op.
	targetSiteURL := strings.TrimRight(si.URL, "/")
	targetHomeURL := targetSiteURL // same default — Site.URL IS the home_url
	out := agentcmd.RestoreRequest{
		SnapshotID:       snapshotID.String(),
		RestoreID:        restoreID,
		Kind:             wireKind,
		ProgressEndpoint: progressEndpoint,
		ChunkBytes:       agentcmd.ChunkBytes,
		KeepOldFiles:     sel.KeepOldFiles,
		// P0 URL rewriter: target side.
		TargetSiteURL: targetSiteURL,
		TargetHomeURL: targetHomeURL,
		// P0 URL rewriter: source side — empty for pre-ADR-036 snapshots,
		// agent then reads the dump banner. Don't emit empty strings as
		// non-omitempty since the JSON tag is omitempty already.
		SourceSiteURL:    snap.SourceSiteURL,
		SourceHomeURL:    snap.SourceHomeURL,
		SourceContentURL: snap.SourceContentURL,
		SourceUploadURL:  snap.SourceUploadURL,
	}
	for _, e := range selected {
		re := agentcmd.RestoreEntry{LogicalPath: e.Path}
		for _, h := range e.ChunkHashes {
			rc, ok := getURLs[h]
			if !ok {
				return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, domain.Internal("backup_chunk_missing", "manifest references a chunk that is no longer stored")
			}
			re.Chunks = append(re.Chunks, rc)
		}
		out.Manifest.Entries = append(out.Manifest.Entries, re)
	}
	return out, snap, si, nil
}

// PresignChunks is the agent-facing dedup step: given candidate ciphertext chunk
// hashes for an in-flight snapshot, it returns presigned PUT URLs for ONLY the
// hashes NOT already stored for the tenant. The s3 key is content-addressed and
// tenant-namespaced so a presign can never target another tenant's prefix.
func (s *Service) PresignChunks(ctx context.Context, tenantID, snapshotID uuid.UUID, hashes []string) (map[string]string, error) {
	// The snapshot must exist in this tenant and be in progress.
	snap, err := s.repo.GetSnapshot(ctx, tenantID, snapshotID)
	if err != nil {
		return nil, err
	}
	if snap.Status == StatusCompleted || snap.Status == StatusFailed {
		return nil, domain.Validation("snapshot_not_in_progress", "the snapshot is no longer accepting uploads")
	}
	existing, err := s.repo.ExistingChunkHashes(ctx, tenantID, hashes)
	if err != nil {
		return nil, err
	}
	// P1 storage adapter: route presign through registry. The registry returns
	// the per-destination Store when the snapshot carries a non-nil
	// DestinationID; otherwise it falls back to the CP-global Store. When no
	// registry is wired we keep the legacy single-store path verbatim.
	presigner := s.store
	if s.registry != nil {
		p, perr := s.registry.PresignerForSnapshot(ctx, snap)
		if perr != nil {
			return nil, domain.Internal("backup_presign_route_failed", "failed to resolve presigner for snapshot").WithCause(perr)
		}
		if p != nil {
			presigner = p
		}
	}
	uploads := map[string]string{}
	for _, h := range hashes {
		if _, ok := existing[h]; ok {
			continue // dedup: already stored; skip.
		}
		key := chunkS3Key(tenantID, h)
		url, perr := presigner.PresignPut(ctx, key, s.presignTTL)
		if perr != nil {
			return nil, domain.Internal("backup_presign_put_failed", "failed to presign chunk upload").WithCause(perr)
		}
		uploads[h] = url
	}
	return uploads, nil
}

// SubmitManifest is the agent-facing manifest submission: it records the
// manifest, upserts not-yet-stored chunks, increments refcounts, and completes
// the snapshot. Returns the total chunk references and newly-stored chunk count.
func (s *Service) SubmitManifest(ctx context.Context, tenantID, snapshotID uuid.UUID, req agentcmd.SubmitManifestRequest) (int64, int64, error) {
	snap, err := s.repo.GetSnapshot(ctx, tenantID, snapshotID)
	if err != nil {
		return 0, 0, err
	}
	if snap.Status == StatusCompleted {
		return 0, 0, domain.Conflict("snapshot_already_completed", "the snapshot manifest was already recorded")
	}

	in := RecordManifestInput{
		TenantID:   tenantID,
		SnapshotID: snapshotID,
		Chunks:     map[string]ChunkUpload{},
	}
	for _, e := range req.Entries {
		if e.Path == "" {
			return 0, 0, domain.Validation("invalid_manifest_entry", "manifest entry has empty path")
		}
		entryKind := e.EntryKind
		if entryKind == "" {
			entryKind = EntryKindFile
		}
		hashes := make([]string, 0, len(e.Chunks))
		for _, c := range e.Chunks {
			if !isHexHash(c.Blake3) {
				return 0, 0, domain.Validation("invalid_chunk_hash", "manifest chunk hash is not a valid blake3 hex digest")
			}
			hashes = append(hashes, c.Blake3)
			in.Chunks[c.Blake3] = ChunkUpload{Blake3: c.Blake3, Size: c.Size, S3Key: chunkS3Key(tenantID, c.Blake3)}
		}
		in.Entries = append(in.Entries, ManifestEntryInput{
			Path:        e.Path,
			EntryKind:   entryKind,
			TableName:   e.TableName,
			ChunkHashes: hashes,
			Size:        e.Size,
			Mode:        int32(e.Mode),
		})
	}
	chunkRefs, stored, err := s.repo.RecordManifest(ctx, in)
	if err != nil {
		return chunkRefs, stored, err
	}
	// Publish a terminal completed event so live SSE subscribers see the
	// final state without waiting for a poll. Best-effort: a fetch error
	// here only loses live smoothness (the handler re-reads from DB).
	if completed, gerr := s.repo.GetSnapshot(ctx, tenantID, snapshotID); gerr == nil {
		s.publish(BackupEvent{
			SnapshotID:  snapshotID,
			Phase:       "completed",
			PhaseDetail: map[string]any{"chunk_refs": chunkRefs, "stored": stored},
			Status:      completed.Status,
		})
	}
	// Reconcile the linked schedule run to 'completed' (best-effort; 0-row
	// no-op when no schedule run is linked to this snapshot).
	if s.scheduleRuns != nil {
		_, _ = s.scheduleRuns.SetScheduleRunStatusBySnapshot(ctx, tenantID, snapshotID, SetScheduleRunStatusInput{
			TenantID:    tenantID,
			Status:      ScheduleRunStatusCompleted,
			SetFinished: true,
		})
	}
	return chunkRefs, stored, nil
}

// MarkRunning transitions a snapshot to running (called by the backup worker).
// Reconciliation: also transitions any linked schedule run to 'running'.
func (s *Service) MarkRunning(ctx context.Context, tenantID, snapshotID uuid.UUID) (Snapshot, error) {
	snap, err := s.repo.MarkSnapshotRunning(ctx, tenantID, snapshotID)
	if err != nil {
		return snap, err
	}
	s.publish(BackupEvent{
		SnapshotID:  snapshotID,
		Phase:       "started",
		PhaseDetail: map[string]any{},
		Status:      snap.Status,
	})
	// Reconcile the linked schedule run to 'running' (best-effort: no linked
	// run is a harmless 0-row no-op from SetScheduleRunStatusBySnapshot).
	if s.scheduleRuns != nil {
		_, _ = s.scheduleRuns.SetScheduleRunStatusBySnapshot(ctx, tenantID, snapshotID, SetScheduleRunStatusInput{
			TenantID:   tenantID,
			Status:     ScheduleRunStatusRunning,
			SetStarted: true,
		})
	}
	return snap, nil
}

// FailSnapshot marks a snapshot failed (called by the backup worker on error or
// by the progress watchdog). Reconciliation: also transitions any linked
// schedule run to 'failed' so history never sticks on 'running'.
func (s *Service) FailSnapshot(ctx context.Context, tenantID, snapshotID uuid.UUID, msg string) (Snapshot, error) {
	snap, err := s.repo.FailSnapshot(ctx, tenantID, snapshotID, msg)
	if err != nil {
		return snap, err
	}
	s.publish(BackupEvent{
		SnapshotID:  snapshotID,
		Phase:       "failed",
		PhaseDetail: map[string]any{"error": msg},
		Status:      snap.Status,
	})
	// Reconcile the linked schedule run to 'failed' (best-effort).
	if s.scheduleRuns != nil {
		errMsg := msg
		_, _ = s.scheduleRuns.SetScheduleRunStatusBySnapshot(ctx, tenantID, snapshotID, SetScheduleRunStatusInput{
			TenantID:    tenantID,
			Status:      ScheduleRunStatusFailed,
			Error:       &errMsg,
			SetFinished: true,
		})
	}
	return snap, nil
}

// MaxProgressPayloadBytes bounds the size of a single agent progress POST. The
// shape is `{phase: "...", phase_detail: {...}}` — phase is one of a fixed set
// of short strings; phase_detail is the per-chunk telemetry. 4 KiB is generous
// (e.g. 100 chunks-of progress fits comfortably) without giving a compromised
// agent a path to bloat backup_snapshots.progress rows.
const MaxProgressPayloadBytes = 4 * 1024

// allowedProgressPhases is the closed set of phase values the runner may post.
// Keeping it closed defends against typos in the runner (which would silently
// render an unknown phase in the UI) and against a compromised agent posting
// arbitrary phase strings to mask its activity.
// Two phase vocabularies are accepted: the original ADR-032 phpbu phases
// (compressing_files / encrypting / uploading) and the ADR-033 WPvivid-pattern
// runner phases (archiving_files / encrypting_uploading). The set is the union
// of both — keeping the older names accepted lets older agents (if any survive
// a partial rollout) still post without 422s.
var allowedProgressPhases = map[string]struct{}{
	// Common to both engines.
	"started":             {},
	"dumping_db":          {},
	"submitting_manifest": {},
	"completed":           {},
	"failed":              {},
	// ADR-032 phpbu-pipeline phases (kept for backward compat).
	"compressing_files": {},
	"encrypting":        {},
	"uploading":         {},
	// ADR-033 WPvivid-pattern TaskRunner phases (backup side).
	"queued":               {},
	"archiving_files":      {},
	"encrypting_uploading": {},
	// ADR-033 / ADR-034 RESTORE phases (closed set; match the agent's exact
	// strings). `completed` and `failed` are reused as the terminal phases for
	// both backup AND restore.
	"preflight":          {},
	"download_artifacts": {},
	"verify_artifacts":   {},
	"maintenance_on":     {},
	"stage_files":        {},
	"swap_files":         {},
	"restore_db":         {},
	"migrate_db":         {}, // V0 skipped but allow the value
	"url_rewrite":        {}, // agent's actual search-replace phase (RestoreRunner::PHASE_URL_REWRITE)
	"swap_db":            {},
	"post_hooks":         {},
	"maintenance_off":    {},
	"cleanup":            {},
	"rolled_back":        {},
}

// RecordProgress validates and persists a single agent progress POST. Returns
// the bytes that were stored (canonical JSON) for logging/debugging — NEVER
// log the raw agent input verbatim, only the validated shape.
//
// When the phase is a restore phase AND an active restore run exists for the
// snapshot, this method additionally:
//   - appends a restore_run_events row (phase, status, message, detail);
//   - updates restore_runs.current_phase;
//   - on a terminal restore phase (completed/failed/rolled_back) transitions
//     the run status to the terminal state (idempotent guard in the repo).
//
// The restore-run writes are best-effort: a failure there is logged but does
// NOT abort the primary progress recording or the SSE publish.
func (s *Service) RecordProgress(ctx context.Context, tenantID, snapshotID uuid.UUID, phase string, phaseDetail map[string]any) ([]byte, error) {
	if tenantID == uuid.Nil {
		return nil, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	if _, ok := allowedProgressPhases[phase]; !ok {
		return nil, domain.Validation("invalid_phase", "unknown progress phase")
	}
	// Re-marshal to canonicalize the payload (drops unknown top-level keys, caps
	// nesting via the bounded size below).
	payload := map[string]any{"phase": phase}
	if phaseDetail != nil {
		payload["phase_detail"] = phaseDetail
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, domain.Validation("invalid_phase_detail", "phase_detail is not JSON-serializable")
	}
	if len(raw) > MaxProgressPayloadBytes {
		return nil, domain.Validation("progress_too_large", "progress payload exceeds size cap")
	}
	snap, err := s.repo.UpdateSnapshotProgress(ctx, tenantID, snapshotID, raw)
	if err != nil {
		return nil, err
	}
	// Fan out the validated progress to live SSE subscribers. The Status mirrors
	// the snapshot's current status (the runner's "completed"/"failed" phase is
	// only authoritative once the manifest is submitted / the worker marks it).
	s.publish(BackupEvent{
		SnapshotID:  snapshotID,
		Phase:       phase,
		PhaseDetail: phaseDetail,
		Status:      snap.Status,
	})

	// Restore-run event persistence (best-effort). Only act when:
	//   1. The restore run store is wired.
	//   2. The phase is a restore phase.
	if s.restoreRuns != nil && isRestorePhase(phase) {
		s.persistRestoreRunEvent(ctx, tenantID, snapshotID, phase, phaseDetail)
	}

	return raw, nil
}

// persistRestoreRunEvent is the best-effort restore-run event persistence
// helper. It is called from RecordProgress with the validated phase; any
// failure is silently swallowed so the existing progress recording is never
// broken by restore-run bookkeeping.
func (s *Service) persistRestoreRunEvent(ctx context.Context, tenantID, snapshotID uuid.UUID, phase string, phaseDetail map[string]any) {
	run, err := s.restoreRuns.ActiveRestoreRunForSnapshot(ctx, tenantID, snapshotID)
	if err != nil {
		// domain.NotFound is expected when no restore is active — not a bug.
		return
	}

	// Extract message from phase_detail (the agent sets phase_detail.message).
	message := ""
	if m, ok := phaseDetail["message"]; ok {
		if ms, ok := m.(string); ok {
			message = ms
		}
	}

	// Derive the event status from the phase name; terminal phases carry their
	// own status, others are "running".
	evStatus := "running"
	if isTerminalRestorePhase(phase) {
		evStatus = phase // completed / failed / rolled_back double as status
	}

	// Marshal the phase_detail for detail storage.
	var detailJSON []byte
	if phaseDetail != nil {
		if b, merr := json.Marshal(phaseDetail); merr == nil {
			detailJSON = b
		}
	}

	// Append the event row.
	_ = func() error {
		_, rerr := s.restoreRuns.AppendRestoreEvent(ctx, AppendRestoreEventInput{
			TenantID:     tenantID,
			RestoreRunID: run.ID,
			Phase:        phase,
			Status:       evStatus,
			Message:      message,
			Detail:       detailJSON,
		})
		return rerr
	}()

	// Advance current_phase.
	_ = s.restoreRuns.UpdateRestoreRunPhase(ctx, tenantID, run.ID, phase)

	// Finalize on terminal phases — idempotent (repo guards with WHERE status
	// NOT IN terminal).
	if isTerminalRestorePhase(phase) {
		errMsg := ""
		if phase == "failed" {
			if e, ok := phaseDetail["error"]; ok {
				if es, ok := e.(string); ok {
					errMsg = es
				}
			}
		}
		terminalStatus := phase // "completed" / "failed" / "rolled_back"
		_ = s.restoreRuns.MarkRestoreRunStatus(ctx, MarkRestoreRunStatusInput{
			TenantID:    tenantID,
			RunID:       run.ID,
			Status:      terminalStatus,
			Error:       errMsg,
			SetFinished: true,
		})
	}
}

// ListStalledRunningSnapshots is the watchdog feeder: cross-tenant enumeration
// of running snapshots whose runner has gone quiet for longer than `threshold`.
// The caller (ProgressWatchdogWorker) marks each failed.
func (s *Service) ListStalledRunningSnapshots(ctx context.Context, threshold time.Duration) ([]StalledSnapshot, error) {
	return s.repo.ListStalledRunningSnapshots(ctx, threshold)
}

// SiteForSnapshot returns the snapshot's site info (used by the backup worker to
// target the agent command).
func (s *Service) SiteForSnapshot(ctx context.Context, tenantID uuid.UUID, snap Snapshot) (SiteInfo, error) {
	return s.sites.GetBackupSiteInfo(ctx, tenantID, snap.SiteID)
}

// GetSchedule returns a site's backup schedule.
func (s *Service) GetSchedule(ctx context.Context, tenantID, siteID uuid.UUID) (Schedule, error) {
	if tenantID == uuid.Nil {
		return Schedule{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	sched, err := s.repo.GetSchedule(ctx, tenantID, siteID)
	if err != nil {
		return Schedule{}, err
	}
	// Populate read-only Timezone + GmtOffset from the site's WordPress settings
	// (the scheduler resolves these at fire time; the response DTO surfaces them
	// so the UI can display run times in the site's local timezone). Default to
	// UTC so the field is never empty even if the site lookup fails or the WP
	// timezone has not been reported via diagnostics yet.
	sched.Timezone = "UTC"
	if si, siErr := s.sites.GetBackupSiteInfo(ctx, tenantID, siteID); siErr == nil {
		loc := resolveLocation(si.WpTimezone, si.WpGmtOffset)
		sched.Timezone = loc.String()
		sched.GmtOffset = si.WpGmtOffset
	}
	return sched, nil
}

// PutScheduleInput is the validated schedule input.
type PutScheduleInput struct {
	TenantID           uuid.UUID
	SiteID             uuid.UUID
	Cadence            string
	Kind               string
	Enabled            bool
	RetentionDays      int32
	MonthlyArchiveKeep int32
	// Timing fields (M17).
	RunHour        int32
	RunMinute      int32
	DayOfWeek      *int32
	DayOfMonth     *int32
	FrequencyHours *int32
	KeepLast       int32
}

// PutSchedule creates/updates a site's backup schedule.
//
// next_run_at is only (re)computed when the row is NEW or a timing field
// (cadence/run_hour/run_minute/day_of_week/day_of_month/frequency_hours)
// changed. Otherwise, the existing next_run_at is preserved so a non-timing
// edit (e.g. changing retention_days) does not push the next run a full cycle
// forward.
func (s *Service) PutSchedule(ctx context.Context, in PutScheduleInput) (Schedule, error) {
	if in.TenantID == uuid.Nil {
		return Schedule{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	cadence := strings.TrimSpace(strings.ToLower(in.Cadence))
	if cadence == "" {
		cadence = CadenceDaily
	}
	if !validCadence(cadence) {
		return Schedule{}, domain.Validation("invalid_cadence", "cadence must be one of hourly, every_n_hours, daily, weekly, or monthly")
	}
	kind := strings.TrimSpace(strings.ToLower(in.Kind))
	if kind == "" {
		kind = KindFull
	}
	if !validKind(kind) {
		return Schedule{}, domain.Validation("invalid_kind", "kind must be files, db, or full")
	}
	// Validate cadence–field consistency.
	if err := validateSchedule(cadence, in.DayOfWeek, in.DayOfMonth, in.FrequencyHours); err != nil {
		return Schedule{}, domain.Validation("invalid_schedule", err.Error())
	}
	// Bound every timing field to its DB CHECK range so out-of-range input is a
	// clean 422 (not a Postgres CHECK 500) and the int32→int16 cast in the repo
	// can never wrap (e.g. run_hour=98304 silently wrapping to hour 0).
	if in.RunHour < 0 || in.RunHour > 23 {
		return Schedule{}, domain.Validation("invalid_run_hour", "run_hour must be between 0 and 23")
	}
	if in.RunMinute < 0 || in.RunMinute > 59 {
		return Schedule{}, domain.Validation("invalid_run_minute", "run_minute must be between 0 and 59")
	}
	if in.DayOfWeek != nil && (*in.DayOfWeek < 0 || *in.DayOfWeek > 6) {
		return Schedule{}, domain.Validation("invalid_day_of_week", "day_of_week must be between 0 and 6")
	}
	if in.DayOfMonth != nil && (*in.DayOfMonth < 1 || *in.DayOfMonth > 28) {
		return Schedule{}, domain.Validation("invalid_day_of_month", "day_of_month must be between 1 and 28")
	}
	if in.FrequencyHours != nil && (*in.FrequencyHours < 1 || *in.FrequencyHours > 24) {
		return Schedule{}, domain.Validation("invalid_frequency_hours", "frequency_hours must be between 1 and 24")
	}
	retention := in.RetentionDays
	if retention <= 0 {
		retention = int32(s.retentionDays)
	}
	archive := in.MonthlyArchiveKeep
	if archive < 0 {
		archive = int32(s.monthlyArchiveKeep)
	}
	keepLast := in.KeepLast
	if keepLast < 0 {
		keepLast = 7 // sane default
	}

	// Resolve the site's timezone from wp_timezone/wp_gmt_offset (NOT operator input).
	si, err := s.sites.GetBackupSiteInfo(ctx, in.TenantID, in.SiteID)
	if err != nil {
		return Schedule{}, err
	}
	loc := resolveLocation(si.WpTimezone, si.WpGmtOffset)

	// Determine whether to recompute next_run_at. Load the existing row to
	// compare timing fields; treat "not found" as a new row.
	nextRunAt := s.clock.Now() // will be replaced below
	existing, getErr := s.repo.GetSchedule(ctx, in.TenantID, in.SiteID)
	isNew := getErr != nil // domain.NotFound → new row; any error → treat as new (safe)
	if isNew {
		// New row: compute first occurrence.
		jitter := SiteJitter(in.SiteID)
		dow := optInt32ToInt(in.DayOfWeek)
		dom := optInt32ToInt(in.DayOfMonth)
		fh := optInt32ToInt(in.FrequencyHours)
		nextRunAt = nextOccurrence(s.clock.Now(), cadence,
			int(in.RunHour), int(in.RunMinute),
			dow, dom, fh,
			jitter, loc)
	} else {
		// Existing row: recompute only when any timing field changed.
		timingChanged := existing.Cadence != cadence ||
			existing.RunHour != in.RunHour ||
			existing.RunMinute != in.RunMinute ||
			!optInt32Equal(existing.DayOfWeek, in.DayOfWeek) ||
			!optInt32Equal(existing.DayOfMonth, in.DayOfMonth) ||
			!optInt32Equal(existing.FrequencyHours, in.FrequencyHours)
		if timingChanged {
			jitter := SiteJitter(in.SiteID)
			dow := optInt32ToInt(in.DayOfWeek)
			dom := optInt32ToInt(in.DayOfMonth)
			fh := optInt32ToInt(in.FrequencyHours)
			nextRunAt = nextOccurrence(s.clock.Now(), cadence,
				int(in.RunHour), int(in.RunMinute),
				dow, dom, fh,
				jitter, loc)
		} else {
			// Preserve the current next_run_at.
			nextRunAt = existing.NextRunAt
		}
	}

	out, upsertErr := s.repo.UpsertSchedule(ctx, UpsertScheduleInput{
		TenantID:           in.TenantID,
		SiteID:             in.SiteID,
		Cadence:            cadence,
		Kind:               kind,
		Enabled:            in.Enabled,
		RetentionDays:      retention,
		MonthlyArchiveKeep: archive,
		NextRunAt:          nextRunAt,
		RunHour:            in.RunHour,
		RunMinute:          in.RunMinute,
		DayOfWeek:          in.DayOfWeek,
		DayOfMonth:         in.DayOfMonth,
		FrequencyHours:     in.FrequencyHours,
		KeepLast:           keepLast,
	})
	if upsertErr != nil {
		return Schedule{}, upsertErr
	}
	// Populate the read-only Timezone and GmtOffset fields from the resolved site zone.
	out.Timezone = loc.String()
	out.GmtOffset = si.WpGmtOffset
	return out, nil
}

// DueSchedules returns enabled, due schedules across all tenants (scheduler).
func (s *Service) DueSchedules(ctx context.Context, limit int32) ([]Schedule, error) {
	if limit <= 0 {
		limit = 100
	}
	return s.repo.ListDueSchedules(ctx, s.clock.Now(), limit)
}

// EnqueueScheduledBackup records a pending snapshot for a due schedule, enqueues
// the backup job, advances the schedule's next_run_at, and (when the schedule
// run store is wired) materializes the M17 schedule run row. Used by the
// scheduler periodic job. It resolves the site's recipient at enqueue time.
//
// Transaction flow (M17):
//  1. AgentUpsertScheduleRun(scheduled_for=sched.NextRunAt) → 'queued'
//  2. CreateSnapshot (tenant-scoped)
//  3. SetScheduleRunSnapshot (link run → snapshot)
//  4. EnqueueBackup (River)
//  5. AdvanceScheduleRun (next_run_at = nextOccurrence)
//  6. Pre-insert next 'scheduled' run row
//
// Un-enrollable / no-recipient site → run marked 'skipped' (visible in history)
// and schedule still advances (no busy-loop).
func (s *Service) EnqueueScheduledBackup(ctx context.Context, sched Schedule) error {
	si, err := s.sites.GetBackupSiteInfo(ctx, sched.TenantID, sched.SiteID)
	if err != nil {
		return err
	}
	// Resolve the site's timezone for next-occurrence computation.
	loc := resolveLocation(si.WpTimezone, si.WpGmtOffset)
	jitter := SiteJitter(sched.SiteID)
	dow := optInt32ToInt(sched.DayOfWeek)
	dom := optInt32ToInt(sched.DayOfMonth)
	fh := optInt32ToInt(sched.FrequencyHours)
	nextAt := nextOccurrence(s.clock.Now(), sched.Cadence,
		int(sched.RunHour), int(sched.RunMinute),
		dow, dom, fh,
		jitter, loc)

	// Un-enrollable / no-recipient path: mark run skipped and advance.
	if !si.Enrolled || si.AgeRecipient == "" {
		reason := "site not enrolled"
		if si.Enrolled && si.AgeRecipient == "" {
			reason = "no age recipient configured"
		}
		skipReason := reason
		if s.scheduleRuns != nil {
			triggeredBy := "schedule"
			_, _ = s.scheduleRuns.AgentUpsertScheduleRun(ctx, UpsertScheduleRunInput{
				TenantID:     sched.TenantID,
				SiteID:       sched.SiteID,
				ScheduleID:   sched.ID,
				ScheduledFor: sched.NextRunAt,
				Status:       ScheduleRunStatusSkipped,
				Kind:         sched.Kind,
				TriggeredBy:  &triggeredBy,
			})
			// Pre-insert next scheduled row.
			_, _ = s.scheduleRuns.AgentUpsertScheduleRun(ctx, UpsertScheduleRunInput{
				TenantID:     sched.TenantID,
				SiteID:       sched.SiteID,
				ScheduleID:   sched.ID,
				ScheduledFor: nextAt,
				Status:       ScheduleRunStatusScheduled,
				Kind:         sched.Kind,
				TriggeredBy:  &triggeredBy,
			})
		}
		_ = s.repo.AdvanceScheduleRun(ctx, sched.TenantID, sched.ID, nextAt)
		return fmt.Errorf("scheduled backup skipped: %s", skipReason)
	}

	// Happy path. Advance next_run_at and pre-insert the next scheduled row
	// FIRST so a crash mid-fire can never re-fire this same slot — worst case is
	// one missed backup, never a duplicate storm. The firing row uses the slot
	// we are firing (firingSlot) while the pre-insert uses nextAt; the
	// UNIQUE(schedule_id, scheduled_for) keeps the two rows distinct.
	triggeredBy := "schedule"
	firingSlot := sched.NextRunAt
	_ = s.repo.AdvanceScheduleRun(ctx, sched.TenantID, sched.ID, nextAt)
	if s.scheduleRuns != nil {
		_, _ = s.scheduleRuns.AgentUpsertScheduleRun(ctx, UpsertScheduleRunInput{
			TenantID:     sched.TenantID,
			SiteID:       sched.SiteID,
			ScheduleID:   sched.ID,
			ScheduledFor: nextAt,
			Status:       ScheduleRunStatusScheduled,
			Kind:         sched.Kind,
			TriggeredBy:  &triggeredBy,
		})
	}

	// Materialize the run row for the slot we are firing. An upsert error here
	// is fatal: never create a snapshot with no linked run row to reconcile.
	var runID uuid.UUID
	if s.scheduleRuns != nil {
		run, rerr := s.scheduleRuns.AgentUpsertScheduleRun(ctx, UpsertScheduleRunInput{
			TenantID:     sched.TenantID,
			SiteID:       sched.SiteID,
			ScheduleID:   sched.ID,
			ScheduledFor: firingSlot,
			Status:       ScheduleRunStatusQueued,
			Kind:         sched.Kind,
			TriggeredBy:  &triggeredBy,
		})
		if rerr != nil {
			return fmt.Errorf("materialize schedule run: %w", rerr)
		}
		runID = run.ID
	}

	snap, err := s.repo.CreateSnapshot(ctx, CreateSnapshotInput{
		TenantID:     sched.TenantID,
		SiteID:       sched.SiteID,
		Kind:         sched.Kind,
		AgeRecipient: si.AgeRecipient,
	})
	if err != nil {
		return err
	}

	// Link snapshot to the schedule run.
	if s.scheduleRuns != nil && runID != uuid.Nil {
		_, _ = s.scheduleRuns.SetScheduleRunSnapshot(ctx, sched.TenantID, runID, snap.ID)
	}

	// Enqueue the backup job. If this fails the snapshot exists but no worker
	// will ever run it — mark the (now snapshot-linked) run failed so history
	// does not stick on "queued".
	if err := s.enqueuer.EnqueueBackup(ctx, sched.TenantID, snap.ID); err != nil {
		if s.scheduleRuns != nil && runID != uuid.Nil {
			msg := "enqueue failed: " + err.Error()
			_, _ = s.scheduleRuns.SetScheduleRunStatusBySnapshot(ctx, sched.TenantID, snap.ID, SetScheduleRunStatusInput{
				TenantID:    sched.TenantID,
				Status:      ScheduleRunStatusFailed,
				Error:       &msg,
				SetFinished: true,
			})
		}
		return err
	}

	return nil
}

// presignTTLSeconds is the presign TTL in seconds (advisory, surfaced to the
// agent alongside the presigned URLs).
func (s *Service) presignTTLSeconds() int { return int(s.presignTTL.Seconds()) }

// selectEntries resolves a RestoreSelection against a manifest, returning the
// matching entries. Full selects everything; Paths selects file entries by
// exact path; DBTables selects db entries by table name. When Components is
// non-empty the result is further filtered to entries whose EntryKind matches
// one of the requested components (M6 / Track 2). Returns a 400 if a requested
// component has no corresponding entries in the snapshot, and a 422 if the
// resolved selection matches nothing.
func selectEntries(entries []ManifestEntry, sel RestoreSelection) ([]ManifestEntry, error) {
	// Component pre-validation: if the caller asked for a component that the
	// snapshot doesn't actually carry, fail fast with an operator-readable
	// message rather than silently dropping back to whatever the manifest
	// happened to contain.
	//
	// Track 5 nuance: "files" is a broad-strokes alias for
	// {plugin, theme, upload, wp-content}. A snapshot with ANY of those (OR
	// the legacy 'file' kind) satisfies a "files" request — we don't require
	// every fine-grained file component to be present.
	//
	// Pre-Track-5 snapshots ONLY carry the legacy 'file' kind on their files
	// parts. componentOfEntryKind maps 'file' to "files", so a "files"
	// request satisfies for them; a fine-grained "plugin" request against a
	// legacy snapshot will FAIL here — there's no way for the CP to tell
	// which sub-bucket a legacy lumped entry belongs to.
	if len(sel.Components) > 0 {
		havePresent := map[string]bool{}
		hasLegacyFile := false
		for _, e := range entries {
			havePresent[componentOfEntryKind(e.EntryKind)] = true
			if e.EntryKind == EntryKindFile {
				hasLegacyFile = true
			}
		}
		for _, c := range sel.Components {
			if c == KindFiles {
				// "files" alias: satisfied if the snapshot has ANY file
				// component OR a legacy 'file' entry.
				anyPresent := hasLegacyFile
				for _, fc := range fileComponentKinds {
					if havePresent[fc] {
						anyPresent = true
						break
					}
				}
				if !anyPresent {
					return nil, domain.Validation(
						"component_not_in_snapshot",
						"Snapshot does not contain files data; choose a different component.",
					)
				}
				continue
			}
			if !havePresent[c] {
				return nil, domain.Validation(
					"component_not_in_snapshot",
					"Snapshot does not contain "+c+" data; choose a different component.",
				)
			}
		}
	}

	allowComponent := componentAllowFn(sel.Components)

	if sel.Full || (len(sel.Paths) == 0 && len(sel.DBTables) == 0) {
		if len(entries) == 0 {
			return nil, domain.Validation("empty_manifest", "the snapshot has no manifest entries to restore")
		}
		if len(sel.Components) == 0 {
			return entries, nil
		}
		var out []ManifestEntry
		for _, e := range entries {
			if allowComponent(e.EntryKind) {
				out = append(out, e)
			}
		}
		if len(out) == 0 {
			return nil, domain.Validation("no_matching_entries", "the restore selection matched no manifest entries")
		}
		return out, nil
	}
	wantPath := map[string]bool{}
	for _, p := range sel.Paths {
		wantPath[p] = true
	}
	wantTable := map[string]bool{}
	for _, t := range sel.DBTables {
		wantTable[t] = true
	}
	var out []ManifestEntry
	for _, e := range entries {
		if !allowComponent(e.EntryKind) {
			continue
		}
		switch {
		case e.EntryKind == EntryKindDB && wantTable[e.TableName]:
			out = append(out, e)
		case e.EntryKind != EntryKindDB && wantPath[e.Path]:
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return nil, domain.Validation("no_matching_entries", "the restore selection matched no manifest entries")
	}
	return out, nil
}

// fileComponentKinds is the closed set of fine-grained "file" components
// introduced in Track 5. The legacy broad-strokes "files" component is an
// alias for ALL of these. Order matches the agent's COMPONENT_PARTITIONS so
// audit trails read consistently across the stack.
var fileComponentKinds = []string{
	EntryKindPlugin,
	EntryKindTheme,
	EntryKindUpload,
	EntryKindWPContent,
}

// componentOfEntryKind maps a manifest EntryKind to its component name on the
// public RestoreCreate wire.
//
// Track 5 split — the wire vocabulary is now:
//   - "files"      (broad-strokes, alias for {plugin, theme, upload, wp-content})
//   - "db"         (database)
//   - "plugin"     (fine-grained, plugins.partNNN.zip)
//   - "theme"      (fine-grained, themes.partNNN.zip)
//   - "upload"     (fine-grained, uploads.partNNN.zip)
//   - "wp-content" (fine-grained, the catch-all bucket)
//
// Each EntryKind maps to its EXACT wire component:
//
//	"db"         -> "db"
//	"plugin"     -> "plugin"
//	"theme"      -> "theme"
//	"upload"     -> "upload"
//	"wp-content" -> "wp-content"
//	"file"       -> "files"   (legacy fallback — pre-Track-5 entries)
//	anything else -> "files"  (defensive default; manifest validation upstream
//	                            already rejects empty entry_kind)
func componentOfEntryKind(entryKind string) string {
	switch entryKind {
	case EntryKindDB:
		return KindDB
	case EntryKindPlugin:
		return EntryKindPlugin
	case EntryKindTheme:
		return EntryKindTheme
	case EntryKindUpload:
		return EntryKindUpload
	case EntryKindWPContent:
		return EntryKindWPContent
	}
	// EntryKindFile (legacy) and any unknown kind: bucket under the broad
	// "files" component so a "files"-selecting restore still picks them up.
	return KindFiles
}

// expandComponentAliases resolves the broad-strokes "files" alias into the four
// fine-grained file components. After expansion the set is the closed union of
// {db, plugin, theme, upload, wp-content}. The same component appearing both
// as an alias parent and explicit child is deduped.
//
// This is the canonical "what does the operator actually want" set used by
// every downstream decision (selectEntries / deriveWireKind).
func expandComponentAliases(components []string) []string {
	if len(components) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(components)+len(fileComponentKinds))
	add := func(c string) {
		if seen[c] {
			return
		}
		seen[c] = true
		out = append(out, c)
	}
	for _, c := range components {
		if c == KindFiles {
			// Broad-strokes alias: expand to the four file components.
			for _, fc := range fileComponentKinds {
				add(fc)
			}
			continue
		}
		add(c)
	}
	return out
}

// componentAllowFn returns a predicate that reports whether a manifest entry's
// EntryKind passes the requested component filter. Empty Components allows all
// kinds (component filter is opt-in).
//
// Legacy 'file' entry_kind ALWAYS passes when ANY of the four file components
// is selected. Pre-Track-5 snapshots don't tag entries with their bucket, so
// the only safe semantics for "operator asked for plugin only" against a
// legacy snapshot is "include the lumped wp-content parts" — the agent then
// extracts the whole tree as it always did. (For brand-new snapshots that
// carry fine-grained kinds, this clause is unreachable.)
func componentAllowFn(components []string) func(string) bool {
	if len(components) == 0 {
		return func(string) bool { return true }
	}
	expanded := expandComponentAliases(components)
	want := map[string]bool{}
	for _, c := range expanded {
		want[c] = true
	}
	// Detect whether any file component was requested — if so, legacy
	// 'file' entries pass through.
	anyFileComponent := false
	for _, fc := range fileComponentKinds {
		if want[fc] {
			anyFileComponent = true
			break
		}
	}
	return func(entryKind string) bool {
		if entryKind == EntryKindFile && anyFileComponent {
			return true
		}
		return want[componentOfEntryKind(entryKind)]
	}
}

// deriveWireKind picks the agent-wire `kind` based on the requested components,
// falling back to the snapshot's own kind when Components is empty.
//
// Track 5 / 0.9.6 rules:
//   - Components empty                                   → snapshotKind
//     (legacy behaviour: take whatever the snapshot itself was taken as).
//   - Components covers all 4 file components AND db     → "full".
//   - Components covers all 4 file components, no db     → "files".
//   - Components covers only "db"                        → "db".
//   - Components is a 1-3 subset of file components,
//     no db                                              → "files"
//     (the agent will receive ONLY the entry list for the selected
//     components — partial-restore-over-files-wire).
//   - Components mixes db + some-but-not-all file
//     components                                         → "full"
//     (the agent will receive entries for both halves).
//
// The "files" + "db" aliases (broad-strokes selectors used by the legacy UI)
// are expanded into the fine-grained set first; "files" expands to all four
// file components. Net behaviour: the V1 UI's `["files","db"]` and the
// Track-5 explicit `["plugin","theme","upload","wp-content","db"]` produce
// the same wire kind ("full").
//
// The snapshot's own kind is the upper bound — selectEntries already 400s if
// a component is requested that the snapshot doesn't carry, so by the time we
// get here we know the snapshot has matching entries for every requested
// component.
func deriveWireKind(snapshotKind string, components []string) string {
	if len(components) == 0 {
		return snapshotKind
	}
	expanded := expandComponentAliases(components)
	want := map[string]bool{}
	for _, c := range expanded {
		want[c] = true
	}
	haveDB := want[KindDB]
	fileCount := 0
	for _, fc := range fileComponentKinds {
		if want[fc] {
			fileCount++
		}
	}
	allFiles := fileCount == len(fileComponentKinds)
	switch {
	case allFiles && haveDB:
		return KindFull
	case allFiles && !haveDB:
		return KindFiles
	case fileCount > 0 && haveDB:
		// Mixed partial file + db -> agent runs both halves.
		return KindFull
	case fileCount > 0 && !haveDB:
		// Partial file-only restore. Agent's runner uses "files" for both
		// 1/3/4-component subsets; the entry list narrows the actual work.
		return KindFiles
	case haveDB:
		return KindDB
	}
	return snapshotKind
}

// optInt32ToInt converts a nullable int32 pointer to a nullable int pointer.
// Returns nil when the input is nil.
func optInt32ToInt(p *int32) *int {
	if p == nil {
		return nil
	}
	v := int(*p)
	return &v
}

// optInt32Equal reports whether two nullable int32 pointers hold the same value.
func optInt32Equal(a, b *int32) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func isHexHash(s string) bool {
	if len(s) < 16 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func normalizePage(limit, offset int32) (int32, int32) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// archiveIDs computes the set of snapshot IDs to keep as monthly archives: the
// newest completed snapshot in each of the most recent keep calendar months.
// Used by the retention GC to flag archives before pruning the rolling window.
func archiveIDs(metas []SnapshotMeta, keep int) map[uuid.UUID]bool {
	out := map[uuid.UUID]bool{}
	if keep <= 0 {
		return out
	}
	// metas are newest-first. Take the first (newest) snapshot per month key.
	seenMonth := map[string]bool{}
	var months []string
	newestPerMonth := map[string]uuid.UUID{}
	for _, m := range metas {
		key := m.CreatedAt.UTC().Format("2006-01")
		if !seenMonth[key] {
			seenMonth[key] = true
			months = append(months, key)
			newestPerMonth[key] = m.ID
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(months)))
	for i, key := range months {
		if i >= keep {
			break
		}
		out[newestPerMonth[key]] = true
	}
	return out
}
