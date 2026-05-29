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
// for the site are encrypted to.
type SiteInfo struct {
	ID           uuid.UUID
	URL          string
	Enrolled     bool
	AgeRecipient string
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
	EnqueueRestore(ctx context.Context, tenantID, snapshotID uuid.UUID, sel RestoreSelection) error
}

// Presigner mints presigned PUT/GET URLs over object storage and reports keys.
type Presigner interface {
	PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error)
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	Delete(ctx context.Context, key string) error
}

// Service holds the backup orchestration logic.
type Service struct {
	repo       Repo
	sites      SiteLookup
	enqueuer   Enqueuer
	store      Presigner
	clock      domain.Clock
	hub        *Hub
	presignTTL time.Duration
	// retention defaults (overridable per schedule).
	retentionDays      int
	monthlyArchiveKeep int
}

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
// by db table. Exactly one mode is used; full when both lists are empty.
type RestoreSelection struct {
	Full     bool
	Paths    []string
	DBTables []string
}

// CreateRestore validates a restore request against the snapshot's manifest and
// enqueues a background restore job. The selection is validated here so an
// invalid path/table fails fast with a 422 (the worker only assembles the
// presigned plan).
func (s *Service) CreateRestore(ctx context.Context, tenantID, snapshotID uuid.UUID, sel RestoreSelection) (Snapshot, error) {
	if tenantID == uuid.Nil {
		return Snapshot{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	snap, err := s.repo.GetSnapshot(ctx, tenantID, snapshotID)
	if err != nil {
		return Snapshot{}, err
	}
	if snap.Status != StatusCompleted {
		return Snapshot{}, domain.Validation("snapshot_not_restorable", "only a completed snapshot can be restored")
	}
	entries, err := s.repo.ListManifest(ctx, tenantID, snapshotID)
	if err != nil {
		return Snapshot{}, err
	}
	// Validate the selection resolves to >=1 entry.
	if _, err := selectEntries(entries, sel); err != nil {
		return Snapshot{}, err
	}
	if err := s.enqueuer.EnqueueRestore(ctx, tenantID, snapshotID, sel); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
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

	out := agentcmd.RestoreRequest{
		SnapshotID:       snapshotID.String(),
		RestoreID:        restoreID,
		Kind:             snap.Kind,
		ProgressEndpoint: progressEndpoint,
		ChunkBytes:       agentcmd.ChunkBytes,
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
	uploads := map[string]string{}
	for _, h := range hashes {
		if _, ok := existing[h]; ok {
			continue // dedup: already stored; skip.
		}
		key := chunkS3Key(tenantID, h)
		url, perr := s.store.PresignPut(ctx, key, s.presignTTL)
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
	return chunkRefs, stored, nil
}

// MarkRunning transitions a snapshot to running (called by the backup worker).
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
	return snap, nil
}

// FailSnapshot marks a snapshot failed (called by the backup worker on error).
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
	"swap_db":            {},
	"post_hooks":         {},
	"maintenance_off":    {},
	"cleanup":            {},
	"rolled_back":        {},
}

// RecordProgress validates and persists a single agent progress POST. Returns
// the bytes that were stored (canonical JSON) for logging/debugging — NEVER
// log the raw agent input verbatim, only the validated shape.
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
	return raw, nil
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
	return s.repo.GetSchedule(ctx, tenantID, siteID)
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
}

// PutSchedule creates/updates a site's backup schedule. next_run_at is computed
// from now per the cadence so the first run fires on the next boundary.
func (s *Service) PutSchedule(ctx context.Context, in PutScheduleInput) (Schedule, error) {
	if in.TenantID == uuid.Nil {
		return Schedule{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	cadence := strings.TrimSpace(strings.ToLower(in.Cadence))
	if cadence == "" {
		cadence = CadenceDaily
	}
	if !validCadence(cadence) {
		return Schedule{}, domain.Validation("invalid_cadence", "cadence must be daily, weekly, or monthly")
	}
	kind := strings.TrimSpace(strings.ToLower(in.Kind))
	if kind == "" {
		kind = KindFull
	}
	if !validKind(kind) {
		return Schedule{}, domain.Validation("invalid_kind", "kind must be files, db, or full")
	}
	retention := in.RetentionDays
	if retention <= 0 {
		retention = int32(s.retentionDays)
	}
	archive := in.MonthlyArchiveKeep
	if archive < 0 {
		archive = int32(s.monthlyArchiveKeep)
	}

	// Verify the site exists in this tenant before scheduling.
	if _, err := s.sites.GetBackupSiteInfo(ctx, in.TenantID, in.SiteID); err != nil {
		return Schedule{}, err
	}

	return s.repo.UpsertSchedule(ctx, UpsertScheduleInput{
		TenantID:           in.TenantID,
		SiteID:             in.SiteID,
		Cadence:            cadence,
		Kind:               kind,
		Enabled:            in.Enabled,
		RetentionDays:      retention,
		MonthlyArchiveKeep: archive,
		NextRunAt:          nextRun(s.clock.Now(), cadence),
	})
}

// DueSchedules returns enabled, due schedules across all tenants (scheduler).
func (s *Service) DueSchedules(ctx context.Context, limit int32) ([]Schedule, error) {
	if limit <= 0 {
		limit = 100
	}
	return s.repo.ListDueSchedules(ctx, s.clock.Now(), limit)
}

// EnqueueScheduledBackup records a pending snapshot for a due schedule, enqueues
// the backup job, and advances the schedule's next_run_at. Used by the scheduler
// periodic job. It resolves the site's recipient at enqueue time.
func (s *Service) EnqueueScheduledBackup(ctx context.Context, sched Schedule) error {
	si, err := s.sites.GetBackupSiteInfo(ctx, sched.TenantID, sched.SiteID)
	if err != nil {
		return err
	}
	// Always advance the schedule so a non-enrollable/recipient-less site does not
	// busy-loop the scheduler.
	defer func() {
		_ = s.repo.AdvanceScheduleRun(ctx, sched.TenantID, sched.ID, nextRun(s.clock.Now(), sched.Cadence))
	}()
	if !si.Enrolled || si.AgeRecipient == "" {
		return fmt.Errorf("scheduled backup skipped: site not enrolled or no age recipient")
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
	return s.enqueuer.EnqueueBackup(ctx, sched.TenantID, snap.ID)
}

// presignTTLSeconds is the presign TTL in seconds (advisory, surfaced to the
// agent alongside the presigned URLs).
func (s *Service) presignTTLSeconds() int { return int(s.presignTTL.Seconds()) }

// selectEntries resolves a RestoreSelection against a manifest, returning the
// matching entries. Full selects everything; Paths selects file entries by exact
// path; DBTables selects db entries by table name. Returns a 422 if the
// selection matches nothing.
func selectEntries(entries []ManifestEntry, sel RestoreSelection) ([]ManifestEntry, error) {
	if sel.Full || (len(sel.Paths) == 0 && len(sel.DBTables) == 0) {
		if len(entries) == 0 {
			return nil, domain.Validation("empty_manifest", "the snapshot has no manifest entries to restore")
		}
		return entries, nil
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
