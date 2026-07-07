package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	// ListSiteIDs returns all site IDs for the tenant. Used by the fleet
	// endpoints to build the full candidate set for org-scoped principals.
	ListSiteIDs(ctx context.Context, tenantID uuid.UUID) ([]uuid.UUID, error)
}

// BackupMailer enqueues a durable transactional email for a backup event.
// The concrete implementation is *mailer.Enqueuer. Optional: when nil, backup
// notification emails are silently suppressed (deploy without SMTP configured).
type BackupMailer interface {
	Enqueue(ctx context.Context, tenantID uuid.UUID, recipients []string, template string, data map[string]any) error
}

// Enqueuer schedules the background backup/restore/GC jobs (River, wired in
// main).
type Enqueuer interface {
	EnqueueBackup(ctx context.Context, tenantID, snapshotID uuid.UUID) error
	// EnqueueBackupWithChain enqueues a backup job with ADR-048 incremental
	// chain fields pre-populated in BackupArgs. Used when CreateSnapshot already
	// resolved is_incremental/generation/chain_id at enqueue time.
	EnqueueBackupWithChain(ctx context.Context, snap Snapshot) error
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

// IndexPutter writes raw bytes to object storage at an arbitrary key. Used for
// the per-snapshot manifest index object written at SubmitManifest time. It is
// a narrower interface than the full blobstore.Store so tests can wire a simple
// stub without providing a real S3 client.
//
// Implementations: *blobstore.Store satisfies this interface via its Put method.
type IndexPutter interface {
	Put(ctx context.Context, key string, body io.Reader, size int64) error
}

// IndexDeleter removes an object from object storage at an arbitrary key. Used
// to delete the per-snapshot manifest.json index object when a snapshot is
// deleted. It is the narrow complement to IndexPutter so tests can stub it
// independently.
//
// Implementations: *blobstore.Store satisfies this interface via its Delete
// method, which treats 404 as success (idempotent delete).
type IndexDeleter interface {
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

// Destination kind wire values (GH #146 / ADR-036 P1). Mirrored from
// sitedestination.Kind as plain strings so this package does not import
// sitedestination directly — see DestinationLookup.
const (
	DestinationKindCP       = "cp"
	DestinationKindLocal    = "local"
	DestinationKindS3Compat = "s3_compat"
)

// DestinationInfo is the destination-routing metadata resolved for an
// already-chosen (non-nil) destination id: the wire Kind ("cp" | "local" |
// "s3_compat") and, for "local", the operator-configured on-disk path prefix
// the agent should write/read chunks under. cp/s3_compat destinations leave
// PathPrefix empty on the wire — the CP still presigns PUT/GET for both via
// the registry, so the agent needs no extra config for them (and, critically,
// never sees the s3_compat customer's credentials).
type DestinationInfo struct {
	ID         uuid.UUID
	Kind       string
	PathPrefix string
}

// BillingGate gates a backup RUN (manual or scheduled) that resolves to the
// CP-managed destination behind the M16 Phase B managed-backup-storage
// entitlement. Implemented by *internal/billing.Service (wired in
// cmd/wpmgr); a nil value disables the gate entirely (self-host / back-compat
// / any test that does not wire it) and every call site treats that exactly
// like an unlimited plan — mirrors site.BillingGate's nil-disables-the-gate
// convention exactly. This local interface keeps the backup package free of a
// direct billing import.
//
// It is deliberately NOT consulted on any restore/download path: a customer
// must always be able to get existing data back, even after losing the
// entitlement (see DestinationInfoForSnapshot's restore-routing callers,
// which never call this gate).
type BillingGate interface {
	CheckManagedBackupStorage(ctx context.Context, tenantID uuid.UUID) error
}

// DestinationLookup bridges the ADR-036 P1 site-destination domain into the
// backup service without an import cycle (sitedestination has no reason to
// import backup, but keeping the dependency direction explicit and narrow
// keeps both packages independently testable — mirrors PresignerForSnapshot).
// Wired via SetDestinationLookup; nil is a fully supported, tested state:
// every call site treats a nil destLookup exactly like "no destination
// configured", i.e. every new snapshot's DestinationID stays uuid.Nil and
// every backup/restore dispatch reports DestinationKind="cp" — the
// pre-existing, always-managed-storage behaviour, byte for byte.
type DestinationLookup interface {
	// DefaultDestinationForSite resolves the site's default destination id, or
	// uuid.Nil (no error) when the site has none configured — the common case.
	DefaultDestinationForSite(ctx context.Context, tenantID, siteID uuid.UUID) (uuid.UUID, error)
	// DestinationInfo resolves the kind + local-path metadata for an already-
	// chosen (non-nil) destination id.
	DestinationInfo(ctx context.Context, tenantID, destinationID uuid.UUID) (DestinationInfo, error)
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
	// indexPutter writes the per-snapshot manifest index object (M5.7 P4).
	// Optional: when nil the index write is skipped (best-effort by design).
	indexPutter IndexPutter
	// indexDeleter removes the per-snapshot manifest index object on delete.
	// Optional: when nil the manifest object is not explicitly deleted (it will
	// remain orphaned in object storage until a lifecycle rule reclaims it).
	indexDeleter IndexDeleter
	// mailer sends backup-completion/failure notification emails (Track B, m49).
	// Optional: when nil, notification emails are silently suppressed.
	mailer BackupMailer
	// destLookup resolves ADR-036 P1 site-destination routing metadata: the
	// site's configured default destination at backup-creation time, and an
	// already-chosen destination's kind/local-path at dispatch/restore time.
	// Optional: see DestinationLookup's doc for the graceful-degradation
	// contract when nil.
	destLookup DestinationLookup
	// billing is the M16 Phase B managed-backup-storage gate. Optional: nil
	// disables the gate (self-host / not-yet-wired tests), matching
	// BillingGate's doc-comment contract exactly.
	billing BillingGate
}

// SetBillingGate wires the M16 Phase B managed-backup-storage gate. Call once
// at startup (after NewService), alongside SetDestinationLookup. Optional:
// when never called, every backup run is ungated exactly as before this
// feature existed.
func (s *Service) SetBillingGate(b BillingGate) { s.billing = b }

// SetRegistry wires the ADR-036 P1 storage-adapter router. Calling this AFTER
// NewService routes every subsequent presign through the registry; callers
// that haven't migrated keep the legacy single-store path. Safe to call once
// at startup before serving traffic.
func (s *Service) SetRegistry(r PresignerForSnapshot) { s.registry = r }

// SetIndexPutter wires the M5.7 P4 manifest-index writer. When set, every
// successful SubmitManifest writes a per-snapshot JSON index object at
// tenant/<tenantID>/site/<siteID>/backup/<snapshotID>/manifest.json via p.
// A write failure is best-effort: it is logged but never fails the backup.
// Call once at startup before serving traffic; safe to omit in tests.
func (s *Service) SetIndexPutter(p IndexPutter) { s.indexPutter = p }

// SetIndexDeleter wires the manifest-index deleter. When set, every snapshot
// deletion (DeleteSnapshotForUser and the retention GC metadata prune) issues
// a best-effort delete of the snapshot's manifest.json object so no orphaned
// manifest keys accumulate in object storage. A delete failure (including an
// already-absent object) is logged as a warning and never fails the delete.
// Call once at startup before serving traffic; safe to omit in tests.
func (s *Service) SetIndexDeleter(d IndexDeleter) { s.indexDeleter = d }

// SetMailer wires the transactional-email enqueuer for backup-completion
// notifications (Track B, m49). Call once at startup after River is started.
// When not called, backup email notifications are silently suppressed.
func (s *Service) SetMailer(m BackupMailer) { s.mailer = m }

// SetDestinationLookup wires the ADR-036 P1 site-destination resolver. Call
// once at startup (after NewService), before serving traffic — alongside
// SetRegistry, which routes the actual presigns once a destination is chosen.
// Optional: when never called, every backup/restore uses the managed (cp)
// destination exactly as before this feature existed.
func (s *Service) SetDestinationLookup(d DestinationLookup) { s.destLookup = d }

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

// logger returns the default structured logger for package-level service events.
// The service does not carry an injected logger to keep NewService simple; it
// uses the default slog logger which is configured at boot via slog.SetDefault.
func (s *Service) logger() *slog.Logger { return slog.Default() }

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

// resolveDefaultDestination resolves the site's default destination id for a
// NEW backup run. Returns uuid.Nil (never an error) when destLookup is not
// wired, the site has no default configured, or the lookup itself fails — a
// destination-routing hiccup must never block a backup from being created; it
// simply falls back to the managed bucket exactly as every snapshot did
// before this feature existed. A lookup failure is logged so the degrade is
// visible to operators.
func (s *Service) resolveDefaultDestination(ctx context.Context, tenantID, siteID uuid.UUID) uuid.UUID {
	if s.destLookup == nil {
		return uuid.Nil
	}
	id, err := s.destLookup.DefaultDestinationForSite(ctx, tenantID, siteID)
	if err != nil {
		slog.WarnContext(ctx, "backup: default destination lookup failed; falling back to managed storage",
			slog.String("tenant_id", tenantID.String()),
			slog.String("site_id", siteID.String()),
			slog.Any("error", err))
		return uuid.Nil
	}
	return id
}

// destinationIsManaged reports whether destinationID routes a NEW run to the
// CP-managed bucket — either because no destination is configured (uuid.Nil,
// the legacy default-fallback resolved by resolveDefaultDestination) or
// because an explicitly-chosen destination is itself of kind "cp". Mirrors
// DestinationInfoForSnapshot's resolution rule, but is called BEFORE a
// snapshot exists (at run-creation time in CreateBackup/EnqueueScheduledBackup),
// so it takes the destination id directly rather than a Snapshot.
//
// A destLookup failure degrades to "not managed" (the gate is skipped) rather
// than "managed" (the gate would fire): mirrors resolveDefaultDestination's
// identical fail-open rationale — a destination-routing hiccup must never
// block a backup from being created. This is a soft billing gate, not a
// security boundary, so failing open here is the right trade.
func (s *Service) destinationIsManaged(ctx context.Context, tenantID, destinationID uuid.UUID) bool {
	if destinationID == uuid.Nil {
		return true
	}
	if s.destLookup == nil {
		// A destination_id somehow resolved without a lookup wired — cannot
		// happen operationally (SetDestinationLookup pairs with
		// SetBillingGate), but degrade to "not managed" (gate skipped) per
		// this function's fail-open rationale above.
		return false
	}
	info, err := s.destLookup.DestinationInfo(ctx, tenantID, destinationID)
	if err != nil {
		slog.WarnContext(ctx, "backup: destination-kind lookup failed for the managed-storage billing gate; skipping the gate",
			slog.String("tenant_id", tenantID.String()),
			slog.String("destination_id", destinationID.String()),
			slog.Any("error", err))
		return false
	}
	return info.Kind == DestinationKindCP
}

// checkManagedBackupStorageGate enforces the M16 Phase B managed-storage
// entitlement for a NEW run resolving to destinationID. No-op (nil error)
// when no billing gate is wired, or when destinationID does not route to
// managed storage (a BYO local/s3_compat destination is always plan-agnostic).
func (s *Service) checkManagedBackupStorageGate(ctx context.Context, tenantID, destinationID uuid.UUID) error {
	if s.billing == nil {
		return nil
	}
	if !s.destinationIsManaged(ctx, tenantID, destinationID) {
		return nil
	}
	return s.billing.CheckManagedBackupStorage(ctx, tenantID)
}

// DestinationInfoForSnapshot resolves the wire-facing destination kind (and,
// for "local", the path-prefix config) for an ALREADY-CREATED snapshot.
//
// CRITICAL INVARIANT: snap.DestinationID == uuid.Nil short-circuits to
// {Kind: DestinationKindCP} WITHOUT ever calling destLookup. This is what
// keeps the managed backup/restore path completely ungated by destLookup
// wiring or errors — every snapshot created before this feature, and every
// snapshot for a site with no configured destination, always takes this
// branch and behaves exactly as it did before ADR-036 P1 shipped.
func (s *Service) DestinationInfoForSnapshot(ctx context.Context, snap Snapshot) (DestinationInfo, error) {
	if snap.DestinationID == uuid.Nil {
		return DestinationInfo{Kind: DestinationKindCP}, nil
	}
	if s.destLookup == nil {
		// A destination_id is stamped on the row but no resolver is wired. This
		// should not happen operationally (SetDestinationLookup is wired
		// alongside SetRegistry) — degrade to the managed kind rather than error.
		return DestinationInfo{Kind: DestinationKindCP}, nil
	}
	return s.destLookup.DestinationInfo(ctx, snap.TenantID, snap.DestinationID)
}

// presignerForSnapshot resolves the Presigner that should mint URLs for a
// snapshot's chunks: the ADR-036 P1 per-destination registry route when
// wired, falling back to the legacy single CP-global Store. Shared by the
// backup-upload path (PresignChunks) and every restore-download path so a
// destination-routed snapshot presigns consistently on both sides — an
// s3_compat snapshot always presigns against ITS OWN bucket, never the CP's.
func (s *Service) presignerForSnapshot(ctx context.Context, snap Snapshot) (Presigner, error) {
	presigner := s.store
	if s.registry != nil {
		p, err := s.registry.PresignerForSnapshot(ctx, snap)
		if err != nil {
			return nil, domain.Internal("backup_presign_route_failed", "failed to resolve presigner for snapshot").WithCause(err)
		}
		if p != nil {
			presigner = p
		}
	}
	return presigner, nil
}

// restoreDestinationRoute resolves how a restore should fetch a snapshot's
// chunks: the wire DestinationKind to report to the agent, the local path
// prefix (kind=="local" only — empty otherwise), and the Presigner to mint GET
// URLs with (nil for kind=="local": the agent reads its own local disk
// instead, so no GET is ever minted for those chunks).
//
// CRITICAL INVARIANT: snap.DestinationID == uuid.Nil is the managed/legacy
// path — DestinationInfoForSnapshot short-circuits for it without touching
// destLookup, so this function's behaviour for every snapshot created before
// this feature (and every site with no configured destination) is IDENTICAL
// to calling presignerForSnapshot alone: kind="cp", no local prefix, a
// registry-or-store presigner. destLookup being unwired or erroring can never
// gate that path.
func (s *Service) restoreDestinationRoute(ctx context.Context, snap Snapshot) (kind, localPathPrefix string, presigner Presigner, err error) {
	info, ierr := s.DestinationInfoForSnapshot(ctx, snap)
	if ierr != nil {
		return "", "", nil, ierr
	}
	kind = info.Kind
	if kind == "" {
		kind = DestinationKindCP
	}
	if kind == DestinationKindLocal {
		// The agent reads its own local disk; no presign is possible (or
		// needed) for a local destination.
		return kind, info.PathPrefix, nil, nil
	}
	presigner, err = s.presignerForSnapshot(ctx, snap)
	return kind, "", presigner, err
}

// mintRestoreChunks resolves the GET-able agentcmd.RestoreChunk for every
// stored chunk in `chunks`. When presigner is nil (destination kind ==
// "local") no presign is attempted at all — the agent reads the chunk from
// its own local disk keyed by hash, so only Hash+Size are populated and URL
// stays empty. Otherwise a presigned GET is minted after verifying the stored
// s3_key sits inside this tenant's content-addressed prefix (defense-in-
// depth: a presign can never address another tenant's chunk).
func (s *Service) mintRestoreChunks(ctx context.Context, tenantID uuid.UUID, presigner Presigner, chunks map[string]Chunk) (map[string]agentcmd.RestoreChunk, error) {
	out := make(map[string]agentcmd.RestoreChunk, len(chunks))
	for h, c := range chunks {
		if presigner == nil {
			out[h] = agentcmd.RestoreChunk{Hash: h, Size: c.Size}
			continue
		}
		expected := chunkS3Key(tenantID, h)
		if c.S3Key != expected {
			return nil, domain.Internal("backup_chunk_key_mismatch", "stored chunk key is outside the tenant prefix")
		}
		url, perr := presigner.PresignGet(ctx, c.S3Key, s.presignTTL)
		if perr != nil {
			return nil, domain.Internal("backup_presign_get_failed", "failed to presign chunk").WithCause(perr)
		}
		out[h] = agentcmd.RestoreChunk{Hash: h, URL: url, Size: c.Size}
	}
	return out, nil
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

	// In-flight guard: reject a manual backup if one is already pending/running.
	// This mirrors the scheduled path's belt check and is backed by the partial
	// unique index (backup_snapshots_one_inflight_per_site) as the suspenders.
	inFlight, ifErr := s.repo.CountInFlightSnapshots(ctx, tenantID, siteID)
	if ifErr != nil {
		return Snapshot{}, fmt.Errorf("in-flight guard check failed: %w", ifErr)
	}
	if inFlight > 0 {
		return Snapshot{}, domain.Validation("backup_already_in_flight",
			"a backup is already pending or running for this site; wait for it to complete before starting another")
	}

	// ADR-036 P1: resolve the site's configured default destination ONCE up
	// front so both the incremental and full paths below stamp it identically.
	// uuid.Nil (destLookup unwired, no default configured, or a lookup
	// failure) routes to the legacy CP-managed bucket — unchanged behaviour.
	destinationID := s.resolveDefaultDestination(ctx, tenantID, siteID)

	// M16 Phase B: a free-plan tenant may not create a NEW run against the
	// CP-managed destination — no-op when unwired/self-host/BYO destination
	// (see checkManagedBackupStorageGate). Checked before either CreateSnapshot
	// branch below so a gated manual backup never mints a pending snapshot row.
	if err := s.checkManagedBackupStorageGate(ctx, tenantID, destinationID); err != nil {
		return Snapshot{}, err
	}

	// ADR-048 P5: run-now honours the same per-schedule incremental toggle as the
	// scheduled path, so an operator can enable the toggle and then drive the
	// base→increment→restore QA flow via run-now. A site with no schedule row (or
	// the toggle off) keeps the full-backup path byte-for-byte: a zero-value
	// CreateSnapshotInput + EnqueueBackup, exactly as before.
	if sched, serr := s.repo.GetSchedule(ctx, tenantID, siteID); serr == nil && sched.IncrementalEnabled {
		res, rerr := s.resolveChainForSiteWithWindow(ctx, tenantID, siteID, baseWindowDaysOr(sched.BaseWindowDays))
		if rerr != nil {
			// Degrade to a full base rather than fail the whole backup.
			res = ChainResolution{}
		}
		snap, err := s.repo.CreateSnapshot(ctx, CreateSnapshotInput{
			TenantID:         tenantID,
			SiteID:           siteID,
			CreatedBy:        createdBy,
			Kind:             kind,
			AgeRecipient:     si.AgeRecipient,
			IsIncremental:    res.IsIncremental,
			ParentSnapshotID: res.ParentSnapshotID,
			BaseSnapshotID:   res.BaseSnapshotID,
			ChainID:          res.ChainID,
			Generation:       res.Generation,
			DestinationID:    destinationID,
		})
		if err != nil {
			if isUniqueViolation(err) {
				return Snapshot{}, domain.Validation("backup_already_in_flight",
					"a backup is already pending or running for this site")
			}
			return Snapshot{}, err
		}
		if err := s.enqueuer.EnqueueBackupWithChain(ctx, snap); err != nil {
			return snap, err
		}
		return snap, nil
	}

	snap, err := s.repo.CreateSnapshot(ctx, CreateSnapshotInput{
		TenantID:      tenantID,
		SiteID:        siteID,
		CreatedBy:     createdBy,
		Kind:          kind,
		AgeRecipient:  si.AgeRecipient,
		DestinationID: destinationID,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return Snapshot{}, domain.Validation("backup_already_in_flight",
				"a backup is already pending or running for this site")
		}
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

// FleetSiteIDs returns the set of site IDs that this principal may access for
// fleet queries. For org-scoped principals it returns all site IDs in the
// tenant; for site-scoped principals it returns p.AllowedSiteIDs.
func (s *Service) FleetSiteIDs(ctx context.Context, tenantID uuid.UUID, p domain.Principal) ([]uuid.UUID, error) {
	if p.Scope == domain.ScopeSite {
		return p.AllowedSiteIDs, nil
	}
	return s.sites.ListSiteIDs(ctx, tenantID)
}

// FleetListSnapshots returns a paginated, filtered list of snapshots across the
// principal's accessible sites. For org-scoped principals allSiteIDs must be the
// full tenant site list (passed from the caller); for site-scoped principals it
// must be p.AllowedSiteIDs. The explicit site IDs passed as the query ?sites= CSV
// are intersected with the principal's access list before being forwarded to the
// repo.
//
// Fail-closed: if p is site-scoped and the resolved f.SiteIDs slice is empty
// (the principal has no granted sites), no query is issued and an empty page
// is returned immediately. This prevents an empty site-filter from becoming
// an unscoped scan of the tenant's snapshots.
func (s *Service) FleetListSnapshots(ctx context.Context, p domain.Principal, tenantID uuid.UUID, f FleetListFilter) (FleetSnapshotPage, error) {
	if tenantID == uuid.Nil {
		return FleetSnapshotPage{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	// Fail-closed: site-scoped principal with zero accessible sites → empty page.
	if p.Scope == domain.ScopeSite && len(f.SiteIDs) == 0 {
		return FleetSnapshotPage{Items: []Snapshot{}}, nil
	}
	const (
		maxLimit     = 200
		defaultLimit = 50
	)
	if f.Limit <= 0 {
		f.Limit = defaultLimit
	}
	if f.Limit > maxLimit {
		f.Limit = maxLimit
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	return s.repo.FleetListSnapshots(ctx, p, tenantID, f)
}

// FleetBackupHealth returns one FleetBackupHealthItem per requested site with a
// server-derived health classification. siteIDs must already be limited to the
// principal's accessible sites.
//
// Fail-closed: if p is site-scoped and siteIDs is empty (the principal has no
// granted sites), no query is issued and an empty list is returned immediately.
func (s *Service) FleetBackupHealth(ctx context.Context, p domain.Principal, tenantID uuid.UUID, siteIDs []uuid.UUID) ([]FleetBackupHealthItem, error) {
	if tenantID == uuid.Nil {
		return nil, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	// Fail-closed: site-scoped principal with zero accessible sites → empty list.
	if p.Scope == domain.ScopeSite && len(siteIDs) == 0 {
		return []FleetBackupHealthItem{}, nil
	}
	if len(siteIDs) == 0 {
		return []FleetBackupHealthItem{}, nil
	}
	return s.repo.FleetBackupHealth(ctx, p, tenantID, siteIDs)
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
//
// ADR-049: when the target snapshot has a non-nil chain_id the chain-planner
// path is taken; otherwise the existing single-snapshot path runs unchanged.
func (s *Service) PlanRestore(ctx context.Context, tenantID, snapshotID uuid.UUID, sel RestoreSelection, restoreID, progressEndpoint string) (agentcmd.RestoreRequest, Snapshot, SiteInfo, error) {
	// M5.7 P4: use scoped snapshot lookup so RLS denies non-granted sites for
	// site-scoped principals before any presigned GET URL is minted.
	snap, err := s.getSnapshotForPresign(ctx, tenantID, snapshotID)
	if err != nil {
		return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, err
	}
	si, err := s.sites.GetBackupSiteInfo(ctx, tenantID, snap.SiteID)
	if err != nil {
		return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, err
	}

	// ADR-049: detect chain path. A non-nil chain_id means this snapshot belongs
	// to an incremental chain. Generation 0 with is_incremental=false is the
	// full-base anchor — treat it like a non-chain restore (manifest entries only,
	// no file-index walk) for correctness: the base was taken as a full zip backup
	// and its restore path is unchanged.
	if snap.ChainID != nil && !(snap.Generation == 0 && !snap.IsIncremental) {
		return s.planRestoreChain(ctx, tenantID, snap, si, sel, restoreID, progressEndpoint)
	}

	// --- Non-chain (original) path ---
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

	// ADR-036 P1: resolve destination routing BEFORE minting any chunk URL.
	// destKind=="local" skips presigning entirely (the agent reads its own
	// local disk instead); every other kind (cp — the pre-existing default —
	// or s3_compat) mints a GET exactly as before, routed to the CUSTOMER
	// bucket for s3_compat via the registry.
	destKind, destLocalPrefix, presigner, drerr := s.restoreDestinationRoute(ctx, snap)
	if drerr != nil {
		return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, drerr
	}
	getURLs, err := s.mintRestoreChunks(ctx, tenantID, presigner, chunks)
	if err != nil {
		return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, err
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
		// ADR-036 P1 storage adapter (GH #146): tells the agent which backend
		// to read chunks from. "local" carries no chunk URLs (see
		// mintRestoreChunks) — the agent reads its own disk instead.
		DestinationKind:   destKind,
		DestinationConfig: agentcmd.DestinationConfig{LocalPathPrefix: destLocalPrefix},
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

// reachableChunks is the PURE reachability oracle (ADR-050 STEP 3): it returns
// the set of blake3 chunk hashes that `snap` depends on, with NO presign and NO
// integrity-abort. Both the restore planner (planRestoreChain) and the retention
// GC mark pass call it so the two can never disagree about what is reachable.
//
// Cases:
//
//	(a) base / legacy — chain_id == nil, OR (generation == 0 && !is_incremental):
//	    a full-base anchor / legacy full backup is restored MANIFEST-ONLY, so its
//	    reachable chunks are exactly the manifest entries' chunk_hashes.
//	(b) chain increment — walk generations 0..retainedMaxGen building the
//	    latest-version-wins winMap (tombstones remove a path), pick the
//	    highest-gen DB-dump over 0..retainedMaxGen, and union the chunk_hashes of
//	    the winning file entries + that DB-dump.
//
// retainedMaxGen is the highest generation that should be considered reachable.
// For restore it is snap.Generation (the target tip). For the GC it is the
// chain's highest RETAINED generation — which pins a carry-forward chunk whose
// origin file_index row lives in an older generation under a live tip.
func (s *Service) reachableChunks(ctx context.Context, tenantID uuid.UUID, snap Snapshot, retainedMaxGen int) (map[string]struct{}, error) {
	out := map[string]struct{}{}

	// Case (a): base / legacy — manifest-only reachability.
	if snap.ChainID == nil || (snap.Generation == 0 && !snap.IsIncremental) {
		entries, err := s.repo.ListManifest(ctx, tenantID, snap.ID)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			for _, h := range e.ChunkHashes {
				out[h] = struct{}{}
			}
		}
		return out, nil
	}

	// Case (b): chain increment — walk 0..retainedMaxGen.
	chainID := *snap.ChainID
	if retainedMaxGen < 0 {
		retainedMaxGen = 0
	}
	chainSnaps, err := s.repo.ListChainSnapshots(ctx, tenantID, chainID, retainedMaxGen)
	if err != nil {
		return nil, err
	}

	// Index by generation so the walk is robust to gaps (the GC may have already
	// pruned an older non-pinned generation; the mark pass only needs the
	// generations that survive). We walk in ascending generation order.
	byGen := map[int]Snapshot{}
	maxGen := -1
	for _, cs := range chainSnaps {
		byGen[cs.Generation] = cs
		if cs.Generation > maxGen {
			maxGen = cs.Generation
		}
	}

	// MODEL SELECT (parity with planRestoreChain): an ADR-051 archive-delta chain's
	// gen-0 base carries a `files-list` manifest entry; a legacy per-file-index
	// chain does not. The reachability unit differs — archive PARTS vs per-file
	// chunk_hashes — but the same oracle feeds GC + restore so they never disagree.
	if base, ok := byGen[0]; ok {
		baseEntries, berr := s.repo.ListManifest(ctx, tenantID, base.ID)
		if berr != nil {
			return nil, berr
		}
		if manifestHasKind(baseEntries, EntryKindFilesList) {
			// Archive-delta: reachable = union over retained gens of every zip-PART
			// entry's chunk_hashes + each gen's files-list + tombstones chunk_hashes
			// + the highest-gen DB dump's chunk_hashes. No winMap dedup is needed —
			// a chunk referenced by ANY retained generation's part is reachable, and
			// carry-forward is automatic (an unchanged file's part lives in the older
			// generation that is still retained).
			dbSnapID := uuid.Nil
			for gen := 0; gen <= maxGen; gen++ {
				cs, ok := byGen[gen]
				if !ok {
					continue
				}
				entries, derr := s.repo.ListManifest(ctx, tenantID, cs.ID)
				if derr != nil {
					return nil, derr
				}
				for _, e := range entries {
					switch {
					case isArchivePartKind(e.EntryKind), e.EntryKind == EntryKindFilesList, e.EntryKind == EntryKindTombstones:
						for _, h := range e.ChunkHashes {
							out[h] = struct{}{}
						}
					case e.EntryKind == EntryKindDB:
						dbSnapID = cs.ID
					}
				}
			}
			if dbSnapID != uuid.Nil {
				dbEntries, derr := s.repo.ListManifest(ctx, tenantID, dbSnapID)
				if derr != nil {
					return nil, derr
				}
				for _, e := range dbEntries {
					if e.EntryKind == EntryKindDB {
						for _, h := range e.ChunkHashes {
							out[h] = struct{}{}
						}
					}
				}
			}
			return out, nil
		}
	}

	// LEGACY: build the winning-entry map (latest-version-wins over the file index).
	winMap := map[string]*FileIndexEntry{}
	for gen := 0; gen <= maxGen; gen++ {
		cs, ok := byGen[gen]
		if !ok {
			continue
		}
		streamErr := s.repo.StreamFileIndex(ctx, tenantID, cs.ID, func(e FileIndexEntry) error {
			eCopy := e
			if e.IsTombstone {
				delete(winMap, e.FilePath)
			} else {
				winMap[e.FilePath] = &eCopy
			}
			return nil
		})
		if streamErr != nil {
			return nil, streamErr
		}
	}
	for _, e := range winMap {
		for _, h := range e.ChunkHashes {
			out[h] = struct{}{}
		}
	}

	// Pick the highest-gen DB-dump over 0..maxGen and union its chunk_hashes.
	dbSnapID := uuid.Nil
	for gen := 0; gen <= maxGen; gen++ {
		cs, ok := byGen[gen]
		if !ok {
			continue
		}
		dbEntries, derr := s.repo.ListManifest(ctx, tenantID, cs.ID)
		if derr != nil {
			return nil, derr
		}
		for _, e := range dbEntries {
			if e.EntryKind == EntryKindDB {
				dbSnapID = cs.ID
				break
			}
		}
	}
	if dbSnapID != uuid.Nil {
		dbEntries, derr := s.repo.ListManifest(ctx, tenantID, dbSnapID)
		if derr != nil {
			return nil, derr
		}
		for _, e := range dbEntries {
			if e.EntryKind == EntryKindDB {
				for _, h := range e.ChunkHashes {
					out[h] = struct{}{}
				}
			}
		}
	}

	return out, nil
}

// planRestoreChain is the ADR-049 chain-planner path for PlanRestore. It runs
// when the target snapshot has a non-nil chain_id and is not the full-base
// anchor (generation 0, is_incremental=false). The caller (PlanRestore) has
// already loaded `snap` and `si`.
func (s *Service) planRestoreChain(ctx context.Context, tenantID uuid.UUID, snap Snapshot, si SiteInfo, sel RestoreSelection, restoreID, progressEndpoint string) (agentcmd.RestoreRequest, Snapshot, SiteInfo, error) {
	chainID := *snap.ChainID
	targetGen := snap.Generation

	// STEP 2 — load all chain snapshots 0..targetGen.
	chainSnaps, err := s.repo.ListChainSnapshots(ctx, tenantID, chainID, targetGen)
	if err != nil {
		return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, err
	}

	// STEP 3 — strict chain integrity gate (before any presign or destructive work).

	// CHECK 1: no missing generations.
	if len(chainSnaps) != targetGen+1 {
		return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, domain.Validation(
			"chain_integrity_violation",
			fmt.Sprintf("chain %s is missing generations: expected %d snapshots (0..%d) but found %d",
				chainID, targetGen+1, targetGen, len(chainSnaps)),
		)
	}
	// Verify each entry's generation equals its index (guards middle gaps).
	for i, cs := range chainSnaps {
		if cs.Generation != i {
			return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, domain.Validation(
				"chain_integrity_violation",
				fmt.Sprintf("chain generation sequence broken at index %d: got generation %d", i, cs.Generation),
			)
		}
	}

	// CHECK 2: all generations completed.
	for _, cs := range chainSnaps {
		if cs.Status != StatusCompleted {
			return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, domain.Validation(
				"chain_integrity_violation",
				fmt.Sprintf("chain generation %d (snapshot %s) has status %q: only completed snapshots can be used in a chain restore",
					cs.Generation, cs.ID, cs.Status),
			)
		}
	}

	// STEP 4 — MODEL SELECT. ADR-051 archive-delta chains overlay whole zip PARTS
	// in generation order (newest-wins by ZipArchive extract order); LEGACY
	// per-file-index chains reconstruct a winMap of individual files. The gen-0
	// base of an archive-delta chain carries a `files-list` manifest entry; a
	// legacy chain does not. Detecting on the base keeps both paths correct during
	// the migration window — a legacy chain ages out via retention.
	baseEntries, err := s.repo.ListManifest(ctx, tenantID, chainSnaps[0].ID)
	if err != nil {
		return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, err
	}
	if manifestHasKind(baseEntries, EntryKindFilesList) {
		return s.planRestoreChainOverlay(ctx, tenantID, snap, si, sel, restoreID, progressEndpoint, chainSnaps)
	}
	return s.planRestoreChainFileIndex(ctx, tenantID, snap, si, sel, restoreID, progressEndpoint, chainSnaps)
}

// planRestoreChainOverlay is the ADR-051 archive-delta restore: base parts +
// each increment's parts overlaid in generation order (the agent extracts in
// Manifest.Entries order; ZipArchive extractTo overwrites, so a later
// generation's file wins) + a tombstone-delete pass (newest-wins un-delete).
// No per-file winMap — a chunk referenced by ANY retained generation's part is
// reachable, and carry-forward is automatic because an unchanged file's part
// stays in the older retained generation.
func (s *Service) planRestoreChainOverlay(ctx context.Context, tenantID uuid.UUID, snap Snapshot, si SiteInfo, sel RestoreSelection, restoreID, progressEndpoint string, chainSnaps []Snapshot) (agentcmd.RestoreRequest, Snapshot, SiteInfo, error) {
	chainID := *snap.ChainID
	targetGen := snap.Generation

	// STEP 4a — walk generations 0..targetGen ONCE, collecting each generation's
	// zip PARTS (in stored order), the highest-gen DB dump, and resolving the
	// final deleted set with NEWEST-WINS un-delete. A `tombstones` entry is a
	// per-path delta carrying its state in Mode: Delete marks the path deleted,
	// Readd cancels an earlier delete (the file was repacked). Latest mention per
	// path wins, so a deleted-then-re-added path ends up live.
	byGenParts := make([][]ManifestEntry, targetGen+1)
	dbSnapGen := -1
	var dbSelected []ManifestEntry
	deletedPaths := map[string]bool{} // file_path -> currently tombstoned at this point in the walk

	for gen := 0; gen <= targetGen; gen++ {
		entries, derr := s.repo.ListManifest(ctx, tenantID, chainSnaps[gen].ID)
		if derr != nil {
			return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, derr
		}
		var hasDB bool
		var dbForGen []ManifestEntry
		for _, e := range entries {
			switch {
			case isArchivePartKind(e.EntryKind):
				byGenParts[gen] = append(byGenParts[gen], e)
			case e.EntryKind == EntryKindTombstones:
				// Mode is the delta state: Delete sets deleted, Readd clears it.
				if int(e.Mode) == agentcmd.TombstoneModeReadd {
					delete(deletedPaths, e.Path)
				} else {
					deletedPaths[e.Path] = true
				}
			case e.EntryKind == EntryKindDB:
				hasDB = true
				dbForGen = append(dbForGen, e)
			default:
				// files-list / inspection / unknown: not part of the restore overlay.
			}
		}
		if hasDB {
			dbSnapGen = gen
			dbSelected = dbForGen
		}
	}

	// STEP 4b — DB dump default: if no generation carried a DB dump, fall back to
	// gen-0 (a chain base is a full backup and always has one).
	if dbSnapGen < 0 {
		dbSnapGen = 0
		entries, derr := s.repo.ListManifest(ctx, tenantID, chainSnaps[0].ID)
		if derr != nil {
			return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, derr
		}
		for _, e := range entries {
			if e.EntryKind == EntryKindDB {
				dbSelected = append(dbSelected, e)
			}
		}
	}

	// STEP 5 — reachable chunks via the SHARED oracle (GC + restore parity).
	allHashes, err := s.reachableChunks(ctx, tenantID, snap, targetGen)
	if err != nil {
		return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, err
	}
	hashSlice := make([]string, 0, len(allHashes))
	for h := range allHashes {
		hashSlice = append(hashSlice, h)
	}
	chunks, err := s.repo.ExistingChunkHashes(ctx, tenantID, hashSlice)
	if err != nil {
		return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, err
	}

	// CHECK 3 — chunk resolvability over every part we will send + the DB dump.
	for gen := 0; gen <= targetGen; gen++ {
		for _, e := range byGenParts[gen] {
			for _, h := range e.ChunkHashes {
				if _, ok := chunks[h]; !ok {
					return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, domain.Internal(
						"chain_chunk_missing",
						fmt.Sprintf("archive part %q (gen %d) references chunk %s which is no longer in object storage: chain restore aborted (possible GC before restore)",
							e.Path, gen, h[:min16(h)]),
					)
				}
			}
		}
	}
	for _, e := range dbSelected {
		for _, h := range e.ChunkHashes {
			if _, ok := chunks[h]; !ok {
				return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, domain.Internal(
					"chain_chunk_missing",
					fmt.Sprintf("db dump entry %q references chunk %s which is no longer in object storage: chain restore aborted (possible GC before restore)",
						e.Path, h[:min16(h)]),
				)
			}
		}
	}

	// STEP 6 — ADR-036 P1 destination routing + chunk GET URLs (skipped for a
	// local destination — the agent reads its own local disk instead).
	destKind, destLocalPrefix, presigner, drerr := s.restoreDestinationRoute(ctx, snap)
	if drerr != nil {
		return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, drerr
	}
	getURLs, err := s.mintRestoreChunks(ctx, tenantID, presigner, chunks)
	if err != nil {
		return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, err
	}

	// CP-side tombstone path sanitization (defense-in-depth; agent re-sanitizes).
	tombstonePaths := make([]string, 0, len(deletedPaths))
	for p := range deletedPaths {
		if sanitizeTombstonePathCP(p) {
			tombstonePaths = append(tombstonePaths, p)
		} else {
			slog.WarnContext(ctx, "backup: chain restore: tombstone path failed CP sanitization, excluded",
				slog.String("path", p),
				slog.String("chain_id", chainID.String()))
		}
	}
	sort.Strings(tombstonePaths) // stable order for tests + log readability

	wireKind := deriveWireKind(snap.Kind, sel.Components)
	targetSiteURL := strings.TrimRight(si.URL, "/")
	targetHomeURL := targetSiteURL

	// EstimatedBytes: sum of distinct chunk sizes (advisory disk-check hint).
	var estimatedBytes int64
	for _, c := range chunks {
		estimatedBytes += c.Size
	}

	out := agentcmd.RestoreRequest{
		SnapshotID:       snap.ID.String(),
		RestoreID:        restoreID,
		Kind:             wireKind,
		ProgressEndpoint: progressEndpoint,
		ChunkBytes:       agentcmd.ChunkBytes,
		KeepOldFiles:     sel.KeepOldFiles,
		TargetSiteURL:    targetSiteURL,
		TargetHomeURL:    targetHomeURL,
		SourceSiteURL:    snap.SourceSiteURL,
		SourceHomeURL:    snap.SourceHomeURL,
		SourceContentURL: snap.SourceContentURL,
		SourceUploadURL:  snap.SourceUploadURL,
		IsChainRestore:   true,
		TargetGeneration: targetGen,
		EstimatedBytes:   estimatedBytes,
		TombstonePaths:   tombstonePaths,
		// ADR-036 P1 storage adapter (GH #146): see PlanRestore's non-chain
		// path for the field contract.
		DestinationKind:   destKind,
		DestinationConfig: agentcmd.DestinationConfig{LocalPathPrefix: destLocalPrefix},
	}

	// OVERLAY ORDER — append each generation's parts ASCENDING by generation so a
	// later generation's file wins on extract (ZipArchive extractTo overwrites).
	// THIS ORDER IS LOAD-BEARING: a wrong order yields a silently-stale restore.
	for gen := 0; gen <= targetGen; gen++ {
		for _, e := range byGenParts[gen] {
			re := agentcmd.RestoreEntry{LogicalPath: e.Path}
			for _, h := range e.ChunkHashes {
				rc, ok := getURLs[h]
				if !ok {
					return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, domain.Internal("backup_chunk_missing", "archive part references a chunk that is no longer stored")
				}
				re.Chunks = append(re.Chunks, rc)
			}
			out.Manifest.Entries = append(out.Manifest.Entries, re)
		}
	}

	// DB entries from the highest-gen DB dump snapshot, appended last.
	for _, e := range dbSelected {
		re := agentcmd.RestoreEntry{LogicalPath: e.Path}
		for _, h := range e.ChunkHashes {
			rc, ok := getURLs[h]
			if !ok {
				return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, domain.Internal("backup_chunk_missing", "db manifest references a chunk that is no longer stored")
			}
			re.Chunks = append(re.Chunks, rc)
		}
		out.Manifest.Entries = append(out.Manifest.Entries, re)
	}

	// Stash dbSnapGen for the worker's SSE phase_detail (read-only convention; see
	// the file-index path for the same trick).
	snap.CycleFilesScanned = int64(dbSnapGen)
	return out, snap, si, nil
}

// planRestoreChainFileIndex is the LEGACY (pre-ADR-051) per-file-index chain
// restore. Retained verbatim so chains created under the chunk engine still
// restore correctly until they age out via retention. New chains use the
// archive-delta overlay (planRestoreChainOverlay).
func (s *Service) planRestoreChainFileIndex(ctx context.Context, tenantID uuid.UUID, snap Snapshot, si SiteInfo, sel RestoreSelection, restoreID, progressEndpoint string, chainSnaps []Snapshot) (agentcmd.RestoreRequest, Snapshot, SiteInfo, error) {
	chainID := *snap.ChainID
	targetGen := snap.Generation

	// STEP 4 — build the winning-entry map (latest-version-wins over file-index).
	winMap := map[string]*FileIndexEntry{} // file_path -> winning entry
	deletedPaths := map[string]bool{}      // file_path -> true if tombstoned at targetGen

	for gen := 0; gen <= targetGen; gen++ {
		snapG := chainSnaps[gen]
		streamErr := s.repo.StreamFileIndex(ctx, tenantID, snapG.ID, func(e FileIndexEntry) error {
			eCopy := e // copy to heap so pointer is stable
			if e.IsTombstone {
				delete(winMap, e.FilePath)
				deletedPaths[e.FilePath] = true
			} else {
				winMap[e.FilePath] = &eCopy
				delete(deletedPaths, e.FilePath) // un-delete if re-added after tombstone
			}
			return nil
		})
		if streamErr != nil {
			return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, streamErr
		}
	}

	// STEP 5 — pick the DB dump snapshot (highest generation with DB entries).
	dbSnapID := chainSnaps[0].ID
	dbSnapGen := 0
	for gen := 0; gen <= targetGen; gen++ {
		dbEntries, derr := s.repo.ListManifest(ctx, tenantID, chainSnaps[gen].ID)
		if derr != nil {
			return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, derr
		}
		for _, e := range dbEntries {
			if e.EntryKind == EntryKindDB {
				dbSnapID = chainSnaps[gen].ID
				dbSnapGen = gen
				break
			}
		}
	}
	dbEntries, err := s.repo.ListManifest(ctx, tenantID, dbSnapID)
	if err != nil {
		return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, err
	}
	var dbSelected []ManifestEntry
	for _, e := range dbEntries {
		if e.EntryKind == EntryKindDB {
			dbSelected = append(dbSelected, e)
		}
	}

	// STEP 6 — validate winning entries have resolvable chunks (GC-awareness gate).
	// The set of chunks this snapshot depends on is computed by the SHARED
	// reachableChunks oracle (ADR-050) so the restore planner and the retention
	// GC can never disagree about reachability. retainedMaxGen == targetGen here:
	// restore always reaches up to the requested tip.
	allHashes, err := s.reachableChunks(ctx, tenantID, snap, targetGen)
	if err != nil {
		return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, err
	}
	hashSlice := make([]string, 0, len(allHashes))
	for h := range allHashes {
		hashSlice = append(hashSlice, h)
	}
	chunks, err := s.repo.ExistingChunkHashes(ctx, tenantID, hashSlice)
	if err != nil {
		return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, err
	}

	// CHECK 3: chunk resolvability — file-index entries.
	for path, e := range winMap {
		for _, h := range e.ChunkHashes {
			if _, ok := chunks[h]; !ok {
				return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, domain.Internal(
					"chain_chunk_missing",
					fmt.Sprintf("file %q references chunk %s which is no longer in object storage: chain restore aborted (possible GC before restore)",
						path, h[:min16(h)]),
				)
			}
		}
	}
	// CHECK 3 continued: DB dump entries.
	for _, e := range dbSelected {
		for _, h := range e.ChunkHashes {
			if _, ok := chunks[h]; !ok {
				return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, domain.Internal(
					"chain_chunk_missing",
					fmt.Sprintf("db dump entry %q references chunk %s which is no longer in object storage: chain restore aborted (possible GC before restore)",
						e.Path, h[:min16(h)]),
				)
			}
		}
	}

	// STEP 7 — ADR-036 P1 destination routing + chunk GET URLs (skipped for a
	// local destination — the agent reads its own local disk instead).
	destKind, destLocalPrefix, presigner, drerr := s.restoreDestinationRoute(ctx, snap)
	if drerr != nil {
		return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, drerr
	}
	getURLs, err := s.mintRestoreChunks(ctx, tenantID, presigner, chunks)
	if err != nil {
		return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, err
	}

	// STEP 8 — assemble RestoreRequest.
	var estimatedBytes int64
	for _, e := range winMap {
		estimatedBytes += e.FileSize
	}

	// CP-side tombstone path sanitization (defense-in-depth; agent re-sanitizes).
	tombstonePaths := make([]string, 0, len(deletedPaths))
	for p := range deletedPaths {
		if sanitizeTombstonePathCP(p) {
			tombstonePaths = append(tombstonePaths, p)
		} else {
			slog.WarnContext(ctx, "backup: chain restore: tombstone path failed CP sanitization, excluded",
				slog.String("path", p),
				slog.String("chain_id", chainID.String()))
		}
	}

	wireKind := deriveWireKind(snap.Kind, sel.Components)
	targetSiteURL := strings.TrimRight(si.URL, "/")
	targetHomeURL := targetSiteURL

	out := agentcmd.RestoreRequest{
		SnapshotID:       snap.ID.String(),
		RestoreID:        restoreID,
		Kind:             wireKind,
		ProgressEndpoint: progressEndpoint,
		ChunkBytes:       agentcmd.ChunkBytes,
		KeepOldFiles:     sel.KeepOldFiles,
		TargetSiteURL:    targetSiteURL,
		TargetHomeURL:    targetHomeURL,
		SourceSiteURL:    snap.SourceSiteURL,
		SourceHomeURL:    snap.SourceHomeURL,
		SourceContentURL: snap.SourceContentURL,
		SourceUploadURL:  snap.SourceUploadURL,
		// ADR-049 chain fields.
		IsChainRestore:   true,
		TargetGeneration: targetGen,
		EstimatedBytes:   estimatedBytes,
		TombstonePaths:   tombstonePaths,
		// ADR-036 P1 storage adapter (GH #146): see PlanRestore's non-chain
		// path for the field contract.
		DestinationKind:   destKind,
		DestinationConfig: agentcmd.DestinationConfig{LocalPathPrefix: destLocalPrefix},
	}

	// File entries from winMap (non-tombstone, each as a RestoreEntry).
	for _, e := range winMap {
		re := agentcmd.RestoreEntry{LogicalPath: e.FilePath}
		for _, h := range e.ChunkHashes {
			rc, ok := getURLs[h]
			if !ok {
				return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, domain.Internal("backup_chunk_missing", "file-index references a chunk that is no longer stored")
			}
			re.Chunks = append(re.Chunks, rc)
		}
		out.Manifest.Entries = append(out.Manifest.Entries, re)
	}

	// DB entries from the highest-gen DB dump snapshot.
	for _, e := range dbSelected {
		re := agentcmd.RestoreEntry{LogicalPath: e.Path}
		for _, h := range e.ChunkHashes {
			rc, ok := getURLs[h]
			if !ok {
				return agentcmd.RestoreRequest{}, Snapshot{}, SiteInfo{}, domain.Internal("backup_chunk_missing", "db manifest references a chunk that is no longer stored")
			}
			re.Chunks = append(re.Chunks, rc)
		}
		out.Manifest.Entries = append(out.Manifest.Entries, re)
	}

	// Stash dbSnapGen for the worker's SSE phase_detail (read-only convention).
	snap.CycleFilesScanned = int64(dbSnapGen)
	return out, snap, si, nil
}

// isArchivePartKind reports whether a manifest entry kind is a zip PART that the
// archive-delta restore overlay extracts. Excludes db / files-list / tombstones
// / inspection.
func isArchivePartKind(kind string) bool {
	switch kind {
	case EntryKindFile, EntryKindPlugin, EntryKindTheme, EntryKindUpload, EntryKindWPContent, EntryKindCore:
		return true
	default:
		return false
	}
}

// manifestHasKind reports whether any entry has the given kind.
func manifestHasKind(entries []ManifestEntry, kind string) bool {
	for _, e := range entries {
		if e.EntryKind == kind {
			return true
		}
	}
	return false
}

// hasNonRestorableKind reports whether any entry is an ADR-051 bookkeeping kind
// (files-list / tombstones) that must be excluded from a non-chain restore.
func hasNonRestorableKind(entries []ManifestEntry) bool {
	for _, e := range entries {
		if e.EntryKind == EntryKindFilesList || e.EntryKind == EntryKindTombstones {
			return true
		}
	}
	return false
}

// sanitizeTombstonePathCP validates a tombstone path on the CP side before
// adding it to TombstonePaths. This is defense-in-depth: the agent MUST also
// sanitize. Any path that fails this check is excluded from the wire request.
//
// Rules:
//
//	a. Non-empty
//	b. Does not start with "/" or "\"
//	c. No component equals ".."
//	d. No NUL byte
func sanitizeTombstonePathCP(p string) bool {
	if p == "" {
		return false
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") {
		return false
	}
	if strings.ContainsRune(p, 0) {
		return false
	}
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return false
		}
	}
	return true
}

// min16 returns min(16, len(s)) for truncating hash strings in error messages.
func min16(s string) int {
	if len(s) < 16 {
		return len(s)
	}
	return 16
}

// PresignChunks is the agent-facing dedup step: given candidate ciphertext chunk
// hashes for an in-flight snapshot, it returns presigned PUT URLs for ONLY the
// hashes NOT already stored for the tenant. The s3 key is content-addressed and
// tenant-namespaced so a presign can never target another tenant's prefix.
//
// M5.7 P4: the gating snapshot lookup runs inside GetSnapshotScoped so a
// site-scoped principal (Scope=="site") activates InScopedTenantTx and RLS
// denies access to non-granted sites before any presigned URL is minted.
// Worker and agent callers (no principal on ctx) fall back to InTenantTx.
func (s *Service) PresignChunks(ctx context.Context, tenantID, snapshotID uuid.UUID, hashes []string) (map[string]string, error) {
	// The snapshot must exist in this tenant and be in progress. Use the
	// scoped lookup so RLS enforces site-scope for outside collaborators.
	snap, err := s.getSnapshotForPresign(ctx, tenantID, snapshotID)
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
	presigner, perr := s.presignerForSnapshot(ctx, snap)
	if perr != nil {
		return nil, perr
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
	// A cancelled (status==failed) snapshot must not be resurrected by a late
	// agent submit that finished after the operator cancelled the run.
	if snap.Status == StatusFailed {
		return 0, 0, domain.Conflict("snapshot_canceled", "the snapshot was cancelled and no longer accepts a manifest")
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
	// ADR-051: an archive-delta increment reuses THIS path and carries its cycle
	// telemetry as optional top-level fields. Stamp them best-effort (the snapshot
	// is already completed). A full backup sends zeros, leaving the row untouched.
	if req.CycleFilesScanned != 0 || req.CycleFilesChanged != 0 || req.CycleFilesDeleted != 0 || req.CycleBytesUploaded != 0 {
		if serr := s.repo.UpdateSnapshotCycleStats(ctx, tenantID, snapshotID, CycleStatsInput{
			CycleFilesScanned:  req.CycleFilesScanned,
			CycleFilesChanged:  req.CycleFilesChanged,
			CycleFilesDeleted:  req.CycleFilesDeleted,
			CycleBytesUploaded: req.CycleBytesUploaded,
		}); serr != nil {
			slog.WarnContext(ctx, "backup: cycle stats update failed (best-effort)",
				slog.String("snapshot_id", snapshotID.String()),
				slog.Any("error", serr))
		}
	}
	// Publish a terminal completed event so live SSE subscribers see the
	// final state without waiting for a poll. Best-effort: a fetch error
	// here only loses live smoothness (the handler re-reads from DB).
	if completed, gerr := s.repo.GetSnapshot(ctx, tenantID, snapshotID); gerr == nil {
		// M5.7 P4: write the per-snapshot manifest index object. Best-effort:
		// a failure here MUST NOT fail the backup (the snapshot is already
		// completed in the DB). We use the freshly-read snapshot for the
		// site_id so the key is accurate even for scheduled backups.
		if s.indexPutter != nil {
			s.writeManifestIndex(ctx, completed, in)
		}
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
	// Track B (m49): send backup-completion notification email (best-effort).
	if completed, gerr := s.repo.GetSnapshot(ctx, tenantID, snapshotID); gerr == nil {
		s.sendBackupEmail(ctx, completed, "backup_completed")
	}
	return chunkRefs, stored, nil
}

// PresignParentFilesList resolves the PARENT snapshot's files-list manifest
// entry (ADR-051) and mints a presigned GET per chunk so the dispatching worker
// can hand the agent the prev[rel]=>{size,mtime} map source for change detection.
// It reuses the same presigned-GET transport as chunk fetch — no agent-facing
// route. Returns an error when the parent carries no files-list (a chain that
// can't be diffed) or a chunk is no longer stored, so the worker can surface a
// retryable infra failure rather than silently re-pack the whole tree.
//
// ADR-036 P1 (GH #146): the files-list chunks were stored under the PARENT
// snapshot's OWN destination — routing must key off the PARENT's
// destination_id, not the increment currently being dispatched (in practice a
// chain shares one destination, but this is correct even if it doesn't).
// restoreDestinationRoute is reused here even though this isn't a restore: it
// is exactly the same operation — reading already-stored chunks back — so the
// same cp/local/s3_compat routing decision applies. For a "local" parent no
// presign is minted (mirrors the restore path): the agent reads the parent's
// files-list from its own local disk, keyed by hash, using the returned
// Hash/Size (URL empty).
func (s *Service) PresignParentFilesList(ctx context.Context, tenantID, parentSnapshotID uuid.UUID) ([]agentcmd.RestoreChunk, error) {
	entries, err := s.repo.ListManifest(ctx, tenantID, parentSnapshotID)
	if err != nil {
		return nil, err
	}
	var hashes []string
	for _, e := range entries {
		if e.EntryKind == EntryKindFilesList {
			hashes = append(hashes, e.ChunkHashes...)
		}
	}
	if len(hashes) == 0 {
		return nil, domain.Validation("parent_files_list_missing",
			"the parent snapshot has no files-list manifest entry to diff against")
	}
	chunks, err := s.repo.ExistingChunkHashes(ctx, tenantID, hashes)
	if err != nil {
		return nil, err
	}

	parentSnap, err := s.repo.GetSnapshot(ctx, tenantID, parentSnapshotID)
	if err != nil {
		return nil, err
	}
	_, _, presigner, drerr := s.restoreDestinationRoute(ctx, parentSnap)
	if drerr != nil {
		return nil, drerr
	}
	urls, err := s.mintRestoreChunks(ctx, tenantID, presigner, chunks)
	if err != nil {
		return nil, err
	}

	out := make([]agentcmd.RestoreChunk, 0, len(hashes))
	// Preserve chunk order (a files.list is a stream; concat order matters).
	for _, h := range hashes {
		rc, ok := urls[h]
		if !ok {
			return nil, domain.Internal("parent_files_list_chunk_missing",
				"a files-list chunk for the parent snapshot is no longer stored")
		}
		out = append(out, rc)
	}
	return out, nil
}

// manifestIndexKey returns the per-snapshot index object key:
// tenant/<tenantID>/site/<siteID>/backup/<snapshotID>/manifest.json
// This is the M5.7 P4 browseable layout — entirely separate from the
// chunk keys (chunks/<tenantID>/<blake3>) which are content-addressed.
func manifestIndexKey(tenantID, siteID, snapshotID uuid.UUID) string {
	return fmt.Sprintf("tenant/%s/site/%s/backup/%s/manifest.json",
		tenantID, siteID, snapshotID)
}

// manifestIndexPayload is the JSON shape written to the index object.
// It mirrors the structure of the assembled manifest so downstream tooling
// (lifecycle rules, auditors, the future sharing UI) can read it without
// querying the DB.
type manifestIndexPayload struct {
	SnapshotID string                   `json:"snapshot_id"`
	TenantID   string                   `json:"tenant_id"`
	SiteID     string                   `json:"site_id"`
	Kind       string                   `json:"kind"`
	Entries    []agentcmd.ManifestEntry `json:"entries"`
}

// writeManifestIndex writes the per-snapshot manifest.json index object.
// BEST-EFFORT: any error is logged and silently swallowed so a storage
// failure never propagates back to the agent and never fails the backup.
func (s *Service) writeManifestIndex(ctx context.Context, snap Snapshot, in RecordManifestInput) {
	payload := manifestIndexPayload{
		SnapshotID: snap.ID.String(),
		TenantID:   snap.TenantID.String(),
		SiteID:     snap.SiteID.String(),
		Kind:       snap.Kind,
		Entries:    make([]agentcmd.ManifestEntry, 0, len(in.Entries)),
	}
	// Reconstruct the ManifestEntry slice from the validated input so the
	// index carries the same shape the agent originally submitted.
	for _, e := range in.Entries {
		chunks := make([]agentcmd.ChunkRef, 0, len(e.ChunkHashes))
		for _, h := range e.ChunkHashes {
			up, ok := in.Chunks[h]
			if !ok {
				continue
			}
			chunks = append(chunks, agentcmd.ChunkRef{Blake3: h, Size: up.Size})
		}
		payload.Entries = append(payload.Entries, agentcmd.ManifestEntry{
			Path:      e.Path,
			EntryKind: e.EntryKind,
			TableName: e.TableName,
			Size:      e.Size,
			Mode:      uint32(e.Mode),
			Chunks:    chunks,
		})
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		slog.WarnContext(ctx, "backup: manifest index marshal failed (best-effort, backup not affected)",
			slog.String("snapshot_id", snap.ID.String()),
			slog.String("tenant_id", snap.TenantID.String()),
			slog.Any("error", err))
		return
	}
	key := manifestIndexKey(snap.TenantID, snap.SiteID, snap.ID)
	if err := s.indexPutter.Put(ctx, key, bytes.NewReader(raw), int64(len(raw))); err != nil {
		slog.WarnContext(ctx, "backup: manifest index write failed (best-effort, backup not affected)",
			slog.String("snapshot_id", snap.ID.String()),
			slog.String("key", key),
			slog.Any("error", err))
	}
}

// deleteManifestIndex removes the per-snapshot manifest.json index object from
// object storage. It is the mirror of writeManifestIndex and carries the same
// best-effort contract: any error (including a missing object, which the
// blobstore already treats as success) is logged as a warning and never fails
// the caller — the snapshot row is already gone and the orphaned manifest object
// is harmless (a lifecycle rule or the next explicit cleanup can reclaim it).
//
// tenantID, siteID, snapshotID must be known before calling; typically the
// caller already holds the Snapshot value fetched before deleting the DB row.
func (s *Service) deleteManifestIndex(ctx context.Context, tenantID, siteID, snapshotID uuid.UUID) {
	if s.indexDeleter == nil {
		return
	}
	key := manifestIndexKey(tenantID, siteID, snapshotID)
	if err := s.indexDeleter.Delete(ctx, key); err != nil {
		slog.WarnContext(ctx, "backup: manifest index delete failed (best-effort, snapshot delete unaffected)",
			slog.String("snapshot_id", snapshotID.String()),
			slog.String("tenant_id", tenantID.String()),
			slog.String("key", key),
			slog.Any("error", err))
	}
}

// getSnapshotForPresign is the M5.7 P4 helper used by PresignChunks and
// PlanRestore. It runs the gating snapshot lookup inside the correct scoped
// transaction based on the principal found (or not) on the request context:
//
//   - Principal with Scope=="site" on ctx → repo.GetSnapshotScoped (routes
//     through InScopedTenantTx; RLS enforces the site allowlist).
//   - Principal with Scope=="org" or "" on ctx → repo.GetSnapshotScoped
//     (routes through InTenantTxAsUser; same RLS as GetSnapshot but with
//     user GUC set, which is the correct behaviour for org members).
//   - No principal on ctx (workers, agent callbacks) → repo.GetSnapshot
//     (InTenantTx; service GUC is already set by the worker context).
func (s *Service) getSnapshotForPresign(ctx context.Context, tenantID, snapshotID uuid.UUID) (Snapshot, error) {
	p, ok := domain.PrincipalFromContext(ctx)
	if !ok {
		// Worker / agent path: no user principal. Fall back to the existing
		// InTenantTx path; RLS still applies via the connecting role.
		return s.repo.GetSnapshot(ctx, tenantID, snapshotID)
	}
	// Principal available: use the scoped-tx variant so RunTenantTx picks
	// the right tx helper (InScopedTenantTx for site-scoped principals).
	return s.repo.GetSnapshotScoped(ctx, p, tenantID, snapshotID)
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
	// Track B (m49): send backup-failure notification email (best-effort).
	// Operator-cancels notify too — an alert that a backup did not complete is
	// honest, and the recipient can ignore one they triggered themselves.
	s.sendBackupEmail(ctx, snap, "backup_failed")
	return snap, nil
}

// cancelByOperatorMsg is the error message stamped on a snapshot a user cancels.
// It is also the marker the submit guards look for is purely cosmetic — the
// status==failed transition is what rejects a late agent submit. Kept as a
// constant so the UI label and the audit metadata stay in sync.
const cancelByOperatorMsg = "cancelled by operator"

// CancelSnapshot marks a RUNNING (or PENDING) snapshot failed at the operator's
// request. There is no separate "cancelled" status: a cancel is a fail with a
// well-known error message, which (a) makes the snapshot deletable via
// DeleteSnapshotForUser and (b) auto-rejects a late agent manifest submit — both
// SubmitManifest and SubmitIncrementalManifest reject status==failed rows. The
// progress watchdog only catches STALLED runs, so this is the only path that can
// stop an actively-running-but-unwanted backup immediately.
//
// A snapshot that is already in a terminal state (completed/failed) is rejected
// with a Conflict so the caller can surface "nothing to cancel".
func (s *Service) CancelSnapshot(ctx context.Context, tenantID, snapshotID uuid.UUID) (Snapshot, error) {
	if tenantID == uuid.Nil {
		return Snapshot{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	snap, err := s.repo.GetSnapshot(ctx, tenantID, snapshotID)
	if err != nil {
		return Snapshot{}, err
	}
	if snap.Status != StatusRunning && snap.Status != StatusPending {
		return Snapshot{}, domain.Conflict("snapshot_not_cancelable",
			"only a running or pending backup can be cancelled")
	}
	return s.FailSnapshot(ctx, tenantID, snapshotID, cancelByOperatorMsg)
}

// SetSnapshotLocked sets or clears the per-snapshot lock flag (Track C, m49).
// A locked snapshot is never auto-pruned by the retention GC regardless of
// retention_days or keep_last. The snapshot must be terminal (completed or
// failed); locking a running/pending backup is rejected as a 409 to prevent
// confusion (the GC does not prune non-terminal rows anyway, so locking an
// in-flight backup is semantically redundant and likely a UI bug).
func (s *Service) SetSnapshotLocked(ctx context.Context, tenantID, snapshotID uuid.UUID, locked bool) (Snapshot, error) {
	if tenantID == uuid.Nil {
		return Snapshot{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	snap, err := s.repo.GetSnapshot(ctx, tenantID, snapshotID)
	if err != nil {
		return Snapshot{}, err
	}
	if snap.Status == StatusRunning || snap.Status == StatusPending {
		return Snapshot{}, domain.Conflict("snapshot_in_progress",
			"a running or pending backup cannot be locked or unlocked; wait for it to complete")
	}
	return s.repo.SetSnapshotLocked(ctx, tenantID, snapshotID, locked)
}

// DeleteSnapshotForUser removes a snapshot at the operator's request and
// reclaims any now-unreferenced chunks. It is CHAIN-SAFE: deleting a base or a
// mid-chain increment that still has dependent later-generation increments would
// break planRestoreChain CHECK 1 (which requires every generation 0..tip to
// exist) and orphan the dependents, so this REFUSES with a domain.Validation
// error in that case. A leaf increment, the whole-chain case (the snapshot IS
// the highest generation), and a standalone non-chained full backup are all safe
// to delete on their own.
//
// Chunk reclamation reuses the proven ADR-050 mark-and-sweep: after removing the
// snapshot row we run RunRetentionGC for the tenant, which recomputes the live
// set over the SURVIVING snapshots and sweeps only chunks no survivor can reach.
// Because the live oracle is reachability (not refcount), a chunk a surviving
// snapshot still needs is never deleted, and the in-flight grace floor protects
// chunks a concurrent backup re-references. GC reclamation is best-effort: a
// sweep error does not fail the delete (the row is already gone; the next
// periodic GC reclaims the orphans).
//
// Only TERMINAL snapshots (completed/failed) may be deleted. A running/pending
// snapshot must be cancelled first (CancelSnapshot) — deleting an in-flight row
// would race the agent's chunk uploads and the grace floor.
//
// DeleteSnapshotForUser is a thin wrapper around deleteSnapshotCore (the shared
// guard-and-delete primitive issue #115 also drives from BulkDeleteSnapshots):
// it resolves the single snapshot, runs the core guards + delete, and — only on
// a successful delete — runs the retention GC exactly once. This is also where
// the single-delete path picks up the restore_in_progress guard (previously
// missing here): deleteSnapshotCore refuses to delete a snapshot any active
// restore is currently reading, chain-wide.
func (s *Service) DeleteSnapshotForUser(ctx context.Context, tenantID, snapshotID uuid.UUID) error {
	if tenantID == uuid.Nil {
		return domain.Forbidden("tenant_required", "a tenant context is required")
	}
	snap, err := s.repo.GetSnapshot(ctx, tenantID, snapshotID)
	if err != nil {
		return err
	}
	code, cerr := s.deleteSnapshotCore(ctx, tenantID, snap, nil)
	if cerr != nil {
		return cerr
	}
	if code != "" {
		return domain.Validation(code, skipMessage(code))
	}

	// Reclaim orphaned chunks via the reachability-based GC over the survivors.
	// Best-effort: the row is already gone; a sweep error is logged, not fatal.
	if _, _, gerr := s.RunRetentionGC(ctx, tenantID); gerr != nil {
		slog.WarnContext(ctx, "backup delete: post-delete GC sweep failed (orphans reclaimed on next periodic GC)",
			slog.String("tenant_id", tenantID.String()),
			slog.String("snapshot_id", snapshotID.String()), slog.Any("error", gerr))
	}
	return nil
}

// evaluateDeleteGuards runs every delete guard against an already-resolved
// snapshot WITHOUT mutating anything, so it doubles as the dry-run planner for
// BulkDeleteSnapshots. Guards, in order:
//
//  1. in_progress — a running/pending snapshot must be cancelled first.
//  2. locked — an operator lock exempts a snapshot from any delete (manual or
//     GC) until explicitly unlocked.
//  3. restore_in_progress — an active (queued|running) restore_runs row reads
//     this snapshot's WHOLE chain (see restoreGroupKey), so deleting any member
//     while a restore runs would pull the rug out from under the agent mid-read.
//  4. chain_has_dependents — refuses to orphan a later-generation increment.
//     Re-checked against LIVE chain membership (repo.ListChainSnapshots) rather
//     than a snapshot list cached before this call. deletedThisReq lets a caller
//     that is deleting an entire chain in ONE bulk request treat siblings it has
//     ALREADY deleted earlier in the SAME request as gone, so a newest-first
//     walk down a whole chain doesn't trip this guard on its own already-doomed
//     tip. Pass a nil/empty map for a single, isolated delete.
//
// Returns ("", nil) when every guard passes (the snapshot IS deletable); a
// non-empty skip code (and nil error) when a guard refuses; a non-nil err only
// for a genuine infrastructure failure.
func (s *Service) evaluateDeleteGuards(ctx context.Context, tenantID uuid.UUID, snap Snapshot, deletedThisReq map[uuid.UUID]bool) (skipCode string, err error) {
	if snap.Status == StatusRunning || snap.Status == StatusPending {
		return SkipSnapshotInProgress, nil
	}

	// Lock guard (m49): a locked snapshot is exempt from retention GC AND from
	// manual deletion. The operator must explicitly unlock before deleting, so a
	// lock genuinely protects the backup (not just from the auto-pruner).
	if snap.Locked {
		return SkipSnapshotLocked, nil
	}

	active, aerr := s.hasActiveRestore(ctx, tenantID, snap)
	if aerr != nil {
		return "", aerr
	}
	if active {
		return SkipRestoreInProgress, nil
	}

	// Chain-safety: never orphan a dependent increment. If this snapshot anchors
	// or sits mid-chain (i.e. a sibling exists at a HIGHER generation in the same
	// chain), refuse — the dependents would become unrestorable.
	if snap.ChainID != nil {
		siblings, lerr := s.repo.ListChainSnapshots(ctx, tenantID, *snap.ChainID, maxChainEnumGeneration)
		if lerr != nil {
			return "", lerr
		}
		for _, sib := range siblings {
			if sib.Generation > snap.Generation && !deletedThisReq[sib.ID] {
				return SkipChainHasDependents, nil
			}
		}
	}
	return "", nil
}

// deleteSnapshotCore is the shared guard-and-delete primitive behind both
// DeleteSnapshotForUser (single delete) and BulkDeleteSnapshots (issue #115).
// It applies every evaluateDeleteGuards check and, only if all of them pass,
// removes the snapshot row (metadata-only — chunks are freed later by the
// mark-and-sweep GC) plus the best-effort manifest-index object. It
// deliberately does NOT run the retention GC: callers own that, and
// BulkDeleteSnapshots relies on running it exactly ONCE for the whole batch
// rather than once per deleted id (each GC pass re-marks every retained
// manifest tenant-wide, so a per-id call would be wasteful, not merely
// suboptimal, at any real batch size).
func (s *Service) deleteSnapshotCore(ctx context.Context, tenantID uuid.UUID, snap Snapshot, deletedThisReq map[uuid.UUID]bool) (skipCode string, err error) {
	code, gerr := s.evaluateDeleteGuards(ctx, tenantID, snap, deletedThisReq)
	if gerr != nil || code != "" {
		return code, gerr
	}

	// Remove the snapshot row (cascades file_index + manifest via FK). Chunks are
	// NOT freed here (post-ADR-050 only the mark-and-sweep frees objects).
	if derr := s.repo.DeleteSnapshot(ctx, tenantID, snap.ID); derr != nil {
		return "", derr
	}

	// Delete the per-snapshot manifest.json index object. Best-effort: the DB row
	// is already gone; a storage error (including an absent manifest on a snapshot
	// that never completed its write) is logged as a warning and does not fail the
	// delete. We call this after the DB delete so a transient storage failure can
	// never strand the DB row in an inconsistent state.
	s.deleteManifestIndex(ctx, tenantID, snap.SiteID, snap.ID)
	return "", nil
}

// restoreGroupKey is the key HasActiveRestore groups by: a chained snapshot's
// chain_id (shared by every generation in the chain, including the base — whose
// own id IS the chain_id), or a standalone snapshot's own id.
func restoreGroupKey(snap Snapshot) uuid.UUID {
	if snap.ChainID != nil {
		return *snap.ChainID
	}
	return snap.ID
}

// hasActiveRestore reports whether an active restore currently reads snap or,
// when snap is chained, any sibling sharing its restoreGroupKey.
func (s *Service) hasActiveRestore(ctx context.Context, tenantID uuid.UUID, snap Snapshot) (bool, error) {
	key := restoreGroupKey(snap)
	active, err := s.repo.HasActiveRestore(ctx, tenantID, []uuid.UUID{key})
	if err != nil {
		return false, err
	}
	return active[key], nil
}

// Bulk-delete outcome + skip-code vocabulary (issue #115). Code is a closed
// set the frontend switches on to render a per-row message; Message mirrors
// the wording DeleteSnapshotForUser's equivalent single-delete refusal uses
// (see skipMessage) so the two entry points never drift.
const (
	BulkDeleteOutcomeDeleted = "deleted"
	BulkDeleteOutcomeSkipped = "skipped"

	SkipSnapshotNotFound   = "snapshot_not_found"
	SkipSnapshotInProgress = "snapshot_in_progress"
	SkipSnapshotLocked     = "snapshot_locked"
	SkipChainHasDependents = "chain_has_dependents"
	SkipRestoreInProgress  = "restore_in_progress"
)

// BulkDeleteMaxIDs caps the number of snapshot ids accepted per bulk-delete
// call (issue #115).
const BulkDeleteMaxIDs = 100

// skipMessage returns the human-readable explanation for a skip code. Shared
// by DeleteSnapshotForUser's single-delete error path (domain.Validation(code,
// skipMessage(code))) and BulkDeleteSnapshots' per-id result rows.
func skipMessage(code string) string {
	switch code {
	case SkipSnapshotNotFound:
		return "backup snapshot not found"
	case SkipSnapshotInProgress:
		return "this backup is still running; cancel it before deleting"
	case SkipSnapshotLocked:
		return "this backup is locked; unlock it before deleting"
	case SkipRestoreInProgress:
		return "a restore is currently reading this backup's chain; wait for it to finish before deleting"
	case SkipChainHasDependents:
		return "this backup is part of an incremental chain and later increments depend on it; " +
			"delete the newer increments first"
	default:
		return ""
	}
}

// BulkDeleteResult is the per-id outcome reported by BulkDeleteSnapshots.
// Kind/Status are NOT part of the wire response (see toBulkDeleteResponse) —
// they are carried here purely so the HTTP handler can audit each actually-
// deleted row (recordBackupAction) without an extra per-id fetch of a row that
// (by the time the handler sees it) has already been removed.
type BulkDeleteResult struct {
	ID uuid.UUID
	// Outcome is BulkDeleteOutcomeDeleted or BulkDeleteOutcomeSkipped. In
	// dry-run mode, "deleted" means "would be deleted".
	Outcome string
	// Code is the skip reason; empty when Outcome == BulkDeleteOutcomeDeleted.
	Code string
	// Message is the human-readable explanation of Code; empty when
	// Outcome == BulkDeleteOutcomeDeleted.
	Message string
	// Kind and Status are copied from the resolved Snapshot for every
	// candidate (i.e. every id that was NOT a snapshot_not_found skip). Both
	// are zero-value for a snapshot_not_found result, since no row was ever
	// resolved.
	Kind   string
	Status string
}

// BulkDeleteOutput is the aggregate result of a BulkDeleteSnapshots call.
type BulkDeleteOutput struct {
	DryRun                 bool
	Requested              int
	Deleted                int
	Skipped                int
	Results                []BulkDeleteResult
	ReclaimedBytesEstimate int64
}

// BulkDeleteSnapshots deletes (or, when dryRun, plans the deletion of) up to
// BulkDeleteMaxIDs snapshots in one call. It is chain-aware and ALWAYS
// partial-success: an id that fails a guard is reported as a "skipped" result
// row rather than aborting the whole request — one locked or in-flight backup
// in a batch never blocks the rest from being cleaned up, and the method
// returns a nil error (200 to the caller) even when every id is skipped.
//
// ids are deduplicated (first occurrence wins) before the 1..BulkDeleteMaxIDs
// count check runs, so a client that double-submits the same id is not
// penalized. A id count of zero after dedup, or a count over BulkDeleteMaxIDs,
// is rejected with a domain.Validation error (missing_ids / too_many_ids) and
// no partial work is attempted.
//
// Delete ORDER matters for chain safety: within each incremental chain,
// requested snapshots are deleted newest-generation-first (tip → base).
// Deleting in this order means every surviving snapshot's chain stays
// contiguous 0..tip at every intermediate point of the request — the same
// invariant planRestoreChain's CHECK 1 and the retention GC's chain-aware
// pinning rely on — so a crash or error partway through the batch can never
// strand an unrestorable mid-chain gap. evaluateDeleteGuards' chain_has_dependents
// guard is re-checked against LIVE DB state (via repo.ListChainSnapshots)
// immediately before each row would be deleted, treating ids already deleted
// (or, in dry-run, already "would-delete"d) EARLIER in this same request as
// gone; this is what makes it safe to request an entire chain (base + every
// increment) in one call, and what makes "base+mid requested, tip omitted"
// correctly skip BOTH (the live tip — untouched — still counts as a dependent).
//
// The retention GC runs at most ONCE, after every requested id has been
// processed — never per-id (see deleteSnapshotCore).
//
// dryRun computes the exact same plan (guards + would-be delete order) via
// evaluateDeleteGuards but performs NO mutation whatsoever: no row delete, no
// manifest-index delete, no GC, no audit (the audit call lives in the HTTP
// handler, which the caller is expected to skip when dryRun is set; this
// method itself never touches the DB in dry-run mode regardless).
func (s *Service) BulkDeleteSnapshots(ctx context.Context, tenantID, siteID uuid.UUID, ids []uuid.UUID, dryRun bool) (BulkDeleteOutput, error) {
	if tenantID == uuid.Nil {
		return BulkDeleteOutput{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}

	// Dedupe while preserving first-occurrence order.
	seen := make(map[uuid.UUID]bool, len(ids))
	uniqueIDs := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if id == uuid.Nil || seen[id] {
			continue
		}
		seen[id] = true
		uniqueIDs = append(uniqueIDs, id)
	}
	if len(uniqueIDs) == 0 {
		return BulkDeleteOutput{}, domain.Validation("missing_ids", "ids must contain at least one entry")
	}
	if len(uniqueIDs) > BulkDeleteMaxIDs {
		return BulkDeleteOutput{}, domain.Validation("too_many_ids",
			fmt.Sprintf("ids may contain at most %d entries per call", BulkDeleteMaxIDs))
	}

	// Resolve every requested id in ONE query.
	found, ferr := s.repo.GetSnapshotsByIDs(ctx, tenantID, uniqueIDs)
	if ferr != nil {
		return BulkDeleteOutput{}, ferr
	}

	// resultByID accumulates the outcome for every requested id, regardless of
	// which phase decided it, so the final Results slice can be assembled in the
	// caller's requested order at the end.
	resultByID := make(map[uuid.UUID]BulkDeleteResult, len(uniqueIDs))

	// candidates holds the ids that passed the not-found check (right tenant —
	// guaranteed by GetSnapshotsByIDs — AND right site), preserving request order.
	candidates := make([]Snapshot, 0, len(uniqueIDs))
	for _, id := range uniqueIDs {
		snap, ok := found[id]
		if !ok || snap.SiteID != siteID {
			resultByID[id] = BulkDeleteResult{
				ID: id, Outcome: BulkDeleteOutcomeSkipped,
				Code: SkipSnapshotNotFound, Message: skipMessage(SkipSnapshotNotFound),
			}
			continue
		}
		candidates = append(candidates, snap)
	}

	// Bucket candidates by chain (a standalone snapshot is its own singleton
	// "chain" keyed by its own id), preserving first-seen chain order.
	chains := make(map[uuid.UUID][]Snapshot, len(candidates))
	chainOrder := make([]uuid.UUID, 0, len(candidates))
	for _, snap := range candidates {
		key := restoreGroupKey(snap)
		if _, ok := chains[key]; !ok {
			chainOrder = append(chainOrder, key)
		}
		chains[key] = append(chains[key], snap)
	}

	deletedThisReq := make(map[uuid.UUID]bool, len(candidates))
	var reclaimed int64

	for _, key := range chainOrder {
		members := chains[key]
		// Newest-generation-first within the chain (tip before base).
		sort.Slice(members, func(i, j int) bool { return members[i].Generation > members[j].Generation })
		for _, snap := range members {
			var (
				code string
				gerr error
			)
			if dryRun {
				code, gerr = s.evaluateDeleteGuards(ctx, tenantID, snap, deletedThisReq)
			} else {
				code, gerr = s.deleteSnapshotCore(ctx, tenantID, snap, deletedThisReq)
			}
			if gerr != nil {
				return BulkDeleteOutput{}, gerr
			}
			if code == "" {
				deletedThisReq[snap.ID] = true
				reclaimed += snap.TotalSize
				resultByID[snap.ID] = BulkDeleteResult{
					ID: snap.ID, Outcome: BulkDeleteOutcomeDeleted,
					Kind: snap.Kind, Status: snap.Status,
				}
			} else {
				resultByID[snap.ID] = BulkDeleteResult{
					ID: snap.ID, Outcome: BulkDeleteOutcomeSkipped,
					Code: code, Message: skipMessage(code),
					Kind: snap.Kind, Status: snap.Status,
				}
			}
		}
	}

	out := BulkDeleteOutput{
		DryRun:                 dryRun,
		Requested:              len(uniqueIDs),
		Results:                make([]BulkDeleteResult, 0, len(uniqueIDs)),
		ReclaimedBytesEstimate: reclaimed,
	}
	for _, id := range uniqueIDs {
		res := resultByID[id]
		out.Results = append(out.Results, res)
		if res.Outcome == BulkDeleteOutcomeDeleted {
			out.Deleted++
		} else {
			out.Skipped++
		}
	}

	// Retention GC runs exactly ONCE for the whole batch, and only when the
	// batch actually deleted something and we are NOT in dry-run mode.
	if !dryRun && out.Deleted > 0 {
		if _, _, gerr := s.RunRetentionGC(ctx, tenantID); gerr != nil {
			slog.WarnContext(ctx, "bulk backup delete: post-delete GC sweep failed (orphans reclaimed on next periodic GC)",
				slog.String("tenant_id", tenantID.String()),
				slog.Int("deleted", out.Deleted), slog.Any("error", gerr))
		}
	}
	return out, nil
}

// maxChainEnumGeneration bounds ListChainSnapshots when we want EVERY generation
// in a chain (the dependent-detection walk). BackupMaxChainDepth caps real chain
// length; this is comfortably above any chain the scheduler will build.
const maxChainEnumGeneration = 1 << 30

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
// (compressing_files / encrypting / uploading) and the ADR-033 task-runner
// phases (archiving_files / encrypting_uploading). The set is the union
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
	// ADR-033 task-runner phases (backup side).
	"queued":               {},
	"archiving_files":      {},
	"encrypting_uploading": {},
	// ADR-048 incremental backup phases.
	"fetching_file_index":   {},
	"scanning_files":        {},
	"uploading_incremental": {},
	"incremental_fallback":  {},
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

	// When the agent reports "failed" (e.g. a PHP exception or out-of-disk
	// mid-backup), extract the agent's reason and immediately mark the snapshot
	// failed so the dashboard shows the real cause rather than waiting for the
	// stall watchdog to stamp a generic "stalled" message.
	//
	// Guard: only call FailSnapshot when the snapshot is not already in a
	// terminal state (failed/completed/cancelled). This prevents double-failing
	// a row that was concurrently cancelled by the operator or already failed by
	// the watchdog before this progress POST arrived.
	if phase == "failed" && snap.Status != StatusFailed && snap.Status != StatusCompleted {
		reason := agentFailReason(phaseDetail)
		if reason != "" {
			// FailSnapshot persists the terminal state AND publishes its own
			// terminal SSE event. On success, return early so the trailing
			// s.publish below does not emit a second event with the stale
			// pre-failure status (which would regress the SSE stream from
			// "failed" back to "running"/"pending").
			if failedSnap, ferr := s.FailSnapshot(ctx, tenantID, snapshotID, reason); ferr != nil {
				slog.Warn("RecordProgress: agent-reported failure could not be persisted",
					slog.String("snapshot_id", snapshotID.String()),
					slog.String("tenant_id", tenantID.String()),
					slog.Any("error", ferr))
			} else {
				_ = failedSnap
				return raw, nil
			}
		}
	}

	// Fan out the validated progress to live SSE subscribers. The Status mirrors
	// the snapshot's status as returned by UpdateSnapshotProgress (the
	// FailSnapshot path above short-circuits before this point when it succeeds,
	// so this publish only fires for non-failure phases or when FailSnapshot
	// itself errored).
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

// agentFailReason extracts the operator-safe failure reason from a "failed"
// phase_detail map. The agent sends the scrubbed reason under "message" (the
// primary field); "error" is accepted as a fallback for older agents. Returns
// an empty string when neither key is present or both are empty.
func agentFailReason(phaseDetail map[string]any) string {
	for _, key := range []string{"message", "error"} {
		if v, ok := phaseDetail[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				// Defense in depth: the agent already scrubs and truncates this
				// reason, but the CP must not rely on a (possibly compromised)
				// agent honoring its own budget before persisting it to the
				// snapshot.error text column. Clamp to a sane operator-readable
				// length on a rune boundary.
				const maxReasonLen = 512
				if len(s) > maxReasonLen {
					s = strings.ToValidUTF8(s[:maxReasonLen], "")
				}
				return s
			}
		}
	}
	return ""
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

// PutScheduleInput is the validated schedule input. Track-A and Track-B fields
// (backup scope + notifications) were removed in m50 — they now live in
// site_backup_settings and are accessed via PutBackupContents /
// PutBackupNotifications.
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
	// ADR-048 P5: per-schedule incremental opt-in + optional base-window override.
	IncrementalEnabled bool
	BaseWindowDays     *int32
}

// PutBackupContentsInput is the input for updating Track-A backup-scope settings.
type PutBackupContentsInput struct {
	TenantID          uuid.UUID
	SiteID            uuid.UUID
	BackupComponents  []string // nil = all components (full backup)
	IncludeCore       bool
	ExcludePaths      []string
	ExcludeExtensions []string
	ExcludeFileSizeMB int32 // 0 = no filter
}

// PutBackupNotificationsInput is the input for updating Track-B notification settings.
type PutBackupNotificationsInput struct {
	TenantID           uuid.UUID
	SiteID             uuid.UUID
	NotifyOnCompletion string   // "always"|"on_failure"|"never"; empty defaults to "never"
	NotifyRecipients   []string // max 20, each a valid email
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
	if in.BaseWindowDays != nil && (*in.BaseWindowDays < 1 || *in.BaseWindowDays > 365) {
		return Schedule{}, domain.Validation("invalid_base_window_days", "base_window_days must be between 1 and 365")
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
		// Existing row: recompute when timing changed, when a disabled schedule is
		// re-enabled (enableTransition), or when next_run_at is already in the past
		// (overdue) — any of these cases means the stored slot is stale and the next
		// future occurrence must be computed fresh (skip-missed policy: advance to
		// the next slot, do NOT immediately run a backup).
		timingChanged := existing.Cadence != cadence ||
			existing.RunHour != in.RunHour ||
			existing.RunMinute != in.RunMinute ||
			!optInt32Equal(existing.DayOfWeek, in.DayOfWeek) ||
			!optInt32Equal(existing.DayOfMonth, in.DayOfMonth) ||
			!optInt32Equal(existing.FrequencyHours, in.FrequencyHours)
		enableTransition := !existing.Enabled && in.Enabled
		overdue := !existing.NextRunAt.After(s.clock.Now())
		if timingChanged || enableTransition || (in.Enabled && overdue) {
			jitter := SiteJitter(in.SiteID)
			dow := optInt32ToInt(in.DayOfWeek)
			dom := optInt32ToInt(in.DayOfMonth)
			fh := optInt32ToInt(in.FrequencyHours)
			nextRunAt = nextOccurrence(s.clock.Now(), cadence,
				int(in.RunHour), int(in.RunMinute),
				dow, dom, fh,
				jitter, loc)
		} else {
			// A non-timing edit on an already-enabled, still-future schedule:
			// preserve the current next_run_at so we do not push the run forward.
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
		IncrementalEnabled: in.IncrementalEnabled,
		BaseWindowDays:     in.BaseWindowDays,
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
// Kept for backward compatibility; new callers should use ClaimDueSchedules for
// the atomic claim-and-advance path.
func (s *Service) DueSchedules(ctx context.Context, limit int32) ([]Schedule, error) {
	if limit <= 0 {
		limit = 100
	}
	return s.repo.ListDueSchedules(ctx, s.clock.Now(), limit)
}

// ClaimDueSchedules atomically selects due schedules, pre-computes their next
// occurrence (tz-aware, jittered), advances next_run_at in the SAME DB
// transaction (FOR UPDATE SKIP LOCKED), and returns the claimed slots. Schedules
// locked by a concurrent CP pass are silently skipped — a schedule is claimed at
// most once per tick regardless of how many CP instances are running.
func (s *Service) ClaimDueSchedules(ctx context.Context) ([]Schedule, error) {
	now := s.clock.Now()
	// First enumerate candidates without locking so we can do the tz lookup
	// (network call to sites service) outside the DB transaction.
	candidates, err := s.repo.ListDueSchedules(ctx, now, 200)
	if err != nil {
		return nil, err
	}
	s.logger().Info("backup_scheduler: due candidates found",
		slog.Int("due_candidates", len(candidates)),
		slog.Time("now", now))
	if len(candidates) == 0 {
		return nil, nil
	}

	// Pre-compute nextAt for each candidate using the site's tz.
	nextAt := make(map[uuid.UUID]time.Time, len(candidates))
	for _, sched := range candidates {
		si, serr := s.sites.GetBackupSiteInfo(ctx, sched.TenantID, sched.SiteID)
		if serr != nil {
			// Site lookup failed: log so this is never silent, then leave this
			// schedule out of nextAt so the claim loop skips it (it stays due
			// and will be retried on the next scheduler tick). A missing nextAt
			// entry means ClaimAndAdvanceDueSchedules will not advance the row,
			// so the schedule remains claimable and no run is lost.
			s.logger().Warn("backup_scheduler: site lookup failed for due schedule, skipping this tick",
				slog.String("schedule_id", sched.ID.String()),
				slog.String("site_id", sched.SiteID.String()),
				slog.String("tenant_id", sched.TenantID.String()),
				slog.Any("error", serr))
			continue
		}
		loc := resolveLocation(si.WpTimezone, si.WpGmtOffset)
		jitter := SiteJitter(sched.SiteID)
		dow := optInt32ToInt(sched.DayOfWeek)
		dom := optInt32ToInt(sched.DayOfMonth)
		fh := optInt32ToInt(sched.FrequencyHours)
		nextAt[sched.ID] = nextOccurrence(now, sched.Cadence,
			int(sched.RunHour), int(sched.RunMinute),
			dow, dom, fh,
			jitter, loc)
	}

	// Atomic claim: FOR UPDATE SKIP LOCKED + advance in one tx.
	return s.repo.ClaimAndAdvanceDueSchedules(ctx, now, nextAt)
}

// HealOverdueSchedulesAndSnapshots is the issue #68 boot-time data-heal task.
// It runs two one-shot passes:
//  1. Reconcile duplicate pending/running snapshots (mark older ones failed)
//     so the partial-unique index added by migration m75 can be created.
//  2. Advance every enabled schedule whose next_run_at is already past to its
//     next future occurrence (per-site tz/jitter math via nextOccurrence).
//
// Called in a goroutine at boot, after River starts and the enqueuer is wired.
// Errors are logged but never fatal — the scheduler still works without the heal
// (the FOR UPDATE SKIP LOCKED and in-flight guard handle ongoing fires).
func (s *Service) HealOverdueSchedulesAndSnapshots(ctx context.Context, logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}) {
	// Step 1: reconcile duplicate in-flight snapshots.
	failed, err := s.repo.ReconcileDuplicateInflightSnapshots(ctx)
	if err != nil {
		logger.Warn("backup issue#68 heal: reconcile duplicate in-flight snapshots failed", "error", err.Error())
	} else if failed > 0 {
		logger.Info("backup issue#68 heal: duplicate in-flight snapshots reconciled", "count", failed)
	}

	// Step 2: heal overdue enabled schedules.
	now := s.clock.Now()
	compute := func(sched Schedule, now time.Time) time.Time {
		// Resolve tz from sites service; fall back to UTC on error.
		si, serr := s.sites.GetBackupSiteInfo(ctx, sched.TenantID, sched.SiteID)
		loc := time.UTC
		if serr == nil {
			loc = resolveLocation(si.WpTimezone, si.WpGmtOffset)
		}
		jitter := SiteJitter(sched.SiteID)
		dow := optInt32ToInt(sched.DayOfWeek)
		dom := optInt32ToInt(sched.DayOfMonth)
		fh := optInt32ToInt(sched.FrequencyHours)
		return nextOccurrence(now, sched.Cadence,
			int(sched.RunHour), int(sched.RunMinute),
			dow, dom, fh,
			jitter, loc)
	}
	healed, herr := s.repo.HealOverdueSchedules(ctx, now, compute)
	if herr != nil {
		logger.Warn("backup issue#68 heal: advance overdue schedules failed", "error", herr.Error())
	} else if healed > 0 {
		logger.Info("backup issue#68 heal: overdue schedules advanced to next future slot", "count", healed)
	}
}

// EnqueueScheduledBackup records a pending snapshot for a due schedule, enqueues
// the backup job, and (when the schedule run store is wired) materializes the M17
// schedule run row. Used by the scheduler periodic job (ScheduleWorker.Work).
//
// IMPORTANT: next_run_at is NOT advanced here. The caller (ClaimDueSchedules /
// ClaimAndAdvanceDueSchedules) already advanced it atomically via FOR UPDATE SKIP
// LOCKED before calling this function. Any caller that bypasses ClaimDueSchedules
// is responsible for advancing before calling EnqueueScheduledBackup.
//
// Transaction flow (M17):
//  1. In-flight guard: if a pending/running snapshot already exists → skip (no-op).
//  2. AgentUpsertScheduleRun(scheduled_for=sched.NextRunAt) → 'queued'
//  3. CreateSnapshot (tenant-scoped) — unique_violation treated as in-flight → skip.
//  4. SetScheduleRunSnapshot (link run → snapshot)
//  5. EnqueueBackup (River)
//
// Un-enrollable / no-recipient site → run marked 'skipped' (visible in history).
func (s *Service) EnqueueScheduledBackup(ctx context.Context, sched Schedule) error {
	si, err := s.sites.GetBackupSiteInfo(ctx, sched.TenantID, sched.SiteID)
	if err != nil {
		return err
	}

	// Un-enrollable / no-recipient path: record skip in history; schedule advance
	// already happened in ClaimAndAdvanceDueSchedules.
	if !si.Enrolled || si.AgeRecipient == "" {
		reason := "site not enrolled"
		if si.Enrolled && si.AgeRecipient == "" {
			reason = "no age recipient configured"
		}
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
		}
		return fmt.Errorf("scheduled backup skipped: %s", reason)
	}

	// In-flight guard (belt): skip if a pending/running snapshot already exists.
	// The partial-unique index is the suspenders; this is the belt.
	inFlight, ifErr := s.repo.CountInFlightSnapshots(ctx, sched.TenantID, sched.SiteID)
	if ifErr != nil {
		// Log and skip — a broken guard must not block other schedules.
		s.logger().Warn("backup_scheduler in-flight guard failed, skipping schedule",
			"schedule_id", sched.ID.String(),
			"site_id", sched.SiteID.String(),
			"error", ifErr.Error())
		return fmt.Errorf("in-flight guard failed: %w", ifErr)
	}
	if inFlight > 0 {
		s.logger().Info("backup_scheduler skipping due schedule: snapshot already in flight",
			"schedule_id", sched.ID.String(),
			"site_id", sched.SiteID.String(),
			"in_flight_count", inFlight)
		return nil
	}

	// Materialize the run row for the slot we are firing.
	triggeredBy := "schedule"
	firingSlot := sched.NextRunAt
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

	// ADR-036 P1: resolve the site's configured default destination ONCE up
	// front so both branches below stamp it identically (see CreateBackup for
	// the identical run-now-path rationale).
	destinationID := s.resolveDefaultDestination(ctx, sched.TenantID, sched.SiteID)

	// M16 Phase B: a free-plan tenant may not create a NEW run against the
	// CP-managed destination. Unlike CreateBackup's manual path, a scheduled
	// run must NEVER throw — the schedule's own advance already happened in
	// ClaimAndAdvanceDueSchedules, so an error here would just look like an
	// unexplained scheduler failure in logs. Instead flip the just-materialized
	// 'queued' run row to 'skipped' with a caller-actionable reason, exactly
	// like the un-enrollable/no-recipient skip path above, so the gap is
	// visible in the site's run history rather than a silent no-op.
	if gerr := s.checkManagedBackupStorageGate(ctx, sched.TenantID, destinationID); gerr != nil {
		if s.scheduleRuns != nil && runID != uuid.Nil {
			msg := "byo_destination_required: " + gerr.Error()
			_, _ = s.scheduleRuns.SetScheduleRunStatusByID(ctx, SetScheduleRunStatusInput{
				TenantID:    sched.TenantID,
				RunID:       runID,
				Status:      ScheduleRunStatusSkipped,
				Error:       &msg,
				SetFinished: true,
			})
		}
		s.logger().Info("backup_scheduler skipping due schedule: managed backup storage requires a paid plan",
			"schedule_id", sched.ID.String(),
			"site_id", sched.SiteID.String())
		return nil
	}

	// ADR-048 P5: when the schedule opts into incremental backups, consult the
	// auto-base chain rule and stamp the snapshot's chain fields. When the toggle
	// is OFF this takes the existing full path (zero-value CreateSnapshotInput +
	// EnqueueBackup) byte-for-byte. resolveChainForSite encodes first-run / stale
	// / depth>=max → full base, so a toggle-ON full base is identical to today's
	// full backup; only a resolved increment diverges.
	var snap Snapshot
	if sched.IncrementalEnabled {
		res, rerr := s.resolveChainForSiteWithWindow(ctx, sched.TenantID, sched.SiteID, baseWindowDaysOr(sched.BaseWindowDays))
		if rerr != nil {
			// Degrade to a full base rather than fail the whole scheduled run.
			res = ChainResolution{}
		}
		snap, err = s.repo.CreateSnapshot(ctx, CreateSnapshotInput{
			TenantID:         sched.TenantID,
			SiteID:           sched.SiteID,
			Kind:             sched.Kind,
			AgeRecipient:     si.AgeRecipient,
			IsIncremental:    res.IsIncremental,
			ParentSnapshotID: res.ParentSnapshotID,
			BaseSnapshotID:   res.BaseSnapshotID,
			ChainID:          res.ChainID,
			Generation:       res.Generation,
			DestinationID:    destinationID,
		})
	} else {
		snap, err = s.repo.CreateSnapshot(ctx, CreateSnapshotInput{
			TenantID:      sched.TenantID,
			SiteID:        sched.SiteID,
			Kind:          sched.Kind,
			AgeRecipient:  si.AgeRecipient,
			DestinationID: destinationID,
		})
	}
	if err != nil {
		// A unique-constraint violation on (site_id) WHERE status IN ('pending','running')
		// means a concurrent pass raced past the in-flight guard and won. Treat as
		// a skip (already in flight), not an error.
		if isUniqueViolation(err) {
			s.logger().Info("backup_scheduler CreateSnapshot unique-violation: already in flight",
				"schedule_id", sched.ID.String(),
				"site_id", sched.SiteID.String())
			return nil
		}
		return err
	}

	// Link snapshot to the schedule run.
	if s.scheduleRuns != nil && runID != uuid.Nil {
		_, _ = s.scheduleRuns.SetScheduleRunSnapshot(ctx, sched.TenantID, runID, snap.ID)
	}

	// Enqueue the backup job. If this fails the snapshot exists but no worker
	// will ever run it — mark the run failed so history does not stick on "queued".
	var enqueueErr error
	if sched.IncrementalEnabled {
		enqueueErr = s.enqueuer.EnqueueBackupWithChain(ctx, snap)
	} else {
		enqueueErr = s.enqueuer.EnqueueBackup(ctx, sched.TenantID, snap.ID)
	}
	if enqueueErr != nil {
		if s.scheduleRuns != nil && runID != uuid.Nil {
			msg := "enqueue failed: " + enqueueErr.Error()
			_, _ = s.scheduleRuns.SetScheduleRunStatusBySnapshot(ctx, sched.TenantID, snap.ID, SetScheduleRunStatusInput{
				TenantID:    sched.TenantID,
				Status:      ScheduleRunStatusFailed,
				Error:       &msg,
				SetFinished: true,
			})
		}
		return enqueueErr
	}

	return nil
}

// presignTTLSeconds is the presign TTL in seconds (advisory, surfaced to the
// agent alongside the presigned URLs).
func (s *Service) presignTTLSeconds() int { return int(s.presignTTL.Seconds()) }

// GetBackupSettings returns the site's backup settings. Safe defaults
// (never/empty/false) are returned when no settings row exists yet.
func (s *Service) GetBackupSettings(ctx context.Context, tenantID, siteID uuid.UUID) (SiteBackupSettings, error) {
	settings, err := s.repo.GetBackupSettings(ctx, tenantID, siteID)
	if err != nil {
		var de *domain.Error
		if errors.As(err, &de) && de.Kind == domain.KindNotFound {
			return SiteBackupSettings{
				SiteID:             siteID,
				NotifyOnCompletion: "never",
			}, nil
		}
		return SiteBackupSettings{}, err
	}
	return settings, nil
}

// PutBackupContents validates and persists the Track-A backup-scope settings.
// It merges with the existing notification settings so that only content fields
// are overwritten.
func (s *Service) PutBackupContents(ctx context.Context, in PutBackupContentsInput) (SiteBackupSettings, error) {
	if in.TenantID == uuid.Nil {
		return SiteBackupSettings{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	// Validate backup_components against the canonical singular enum.
	for _, comp := range in.BackupComponents {
		if !validBackupComponent(comp) {
			return SiteBackupSettings{}, domain.Validation("invalid_backup_component",
				fmt.Sprintf("backup_components contains an unknown value %q; valid values are: plugin, theme, upload, wp-content, db, core", comp))
		}
	}
	// Validate exclude_file_size_mb: must be >= 0 and <= 102400 MiB (100 GiB).
	const maxExcludeFileSizeMB = 102400
	if in.ExcludeFileSizeMB < 0 {
		return SiteBackupSettings{}, domain.Validation("invalid_exclude_file_size_mb",
			"exclude_file_size_mb must be 0 or greater (0 = no filter)")
	}
	if in.ExcludeFileSizeMB > maxExcludeFileSizeMB {
		return SiteBackupSettings{}, domain.Validation("invalid_exclude_file_size_mb",
			fmt.Sprintf("exclude_file_size_mb must not exceed %d MiB (100 GiB)", maxExcludeFileSizeMB))
	}
	// Validate exclude_paths and exclude_extensions length caps.
	const maxExcludePaths = 100
	const maxExcludeExtensions = 50
	if len(in.ExcludePaths) > maxExcludePaths {
		return SiteBackupSettings{}, domain.Validation("too_many_exclude_paths",
			fmt.Sprintf("exclude_paths may contain at most %d entries", maxExcludePaths))
	}
	if len(in.ExcludeExtensions) > maxExcludeExtensions {
		return SiteBackupSettings{}, domain.Validation("too_many_exclude_extensions",
			fmt.Sprintf("exclude_extensions may contain at most %d entries", maxExcludeExtensions))
	}
	// Load existing notification settings to preserve them during this content-only update.
	existing, _ := s.GetBackupSettings(ctx, in.TenantID, in.SiteID)
	merged := SiteBackupSettings{
		SiteID:            in.SiteID,
		BackupComponents:  in.BackupComponents,
		IncludeCore:       in.IncludeCore,
		ExcludePaths:      in.ExcludePaths,
		ExcludeExtensions: in.ExcludeExtensions,
		ExcludeFileSizeMB: in.ExcludeFileSizeMB,
		// Preserve existing notification fields.
		NotifyOnCompletion: existing.NotifyOnCompletion,
		NotifyRecipients:   existing.NotifyRecipients,
	}
	return s.repo.UpsertBackupSettings(ctx, in.TenantID, merged)
}

// PutBackupNotifications validates and persists the Track-B notification settings.
// It merges with the existing content settings so that only notification fields
// are overwritten.
func (s *Service) PutBackupNotifications(ctx context.Context, in PutBackupNotificationsInput) (SiteBackupSettings, error) {
	if in.TenantID == uuid.Nil {
		return SiteBackupSettings{}, domain.Forbidden("tenant_required", "a tenant context is required")
	}
	// Validate notify_on_completion (default "never" when empty).
	notifyOnCompletion := strings.TrimSpace(strings.ToLower(in.NotifyOnCompletion))
	if notifyOnCompletion == "" {
		notifyOnCompletion = "never"
	}
	if notifyOnCompletion != "always" && notifyOnCompletion != "on_failure" && notifyOnCompletion != "never" {
		return SiteBackupSettings{}, domain.Validation("invalid_notify_on_completion",
			"notify_on_completion must be 'always', 'on_failure', or 'never'")
	}
	// Validate notify_recipients: well-formed emails, capped at 20 addresses.
	const maxNotifyRecipients = 20
	if len(in.NotifyRecipients) > maxNotifyRecipients {
		return SiteBackupSettings{}, domain.Validation("too_many_notify_recipients",
			fmt.Sprintf("notify_recipients may contain at most %d addresses", maxNotifyRecipients))
	}
	for _, addr := range in.NotifyRecipients {
		if !isValidEmail(addr) {
			return SiteBackupSettings{}, domain.Validation("invalid_notify_recipient",
				fmt.Sprintf("notify_recipients contains an invalid email address: %q", addr))
		}
	}
	// Load existing content settings to preserve them during this notification-only update.
	existing, _ := s.GetBackupSettings(ctx, in.TenantID, in.SiteID)
	merged := SiteBackupSettings{
		SiteID:            in.SiteID,
		BackupComponents:  existing.BackupComponents,
		IncludeCore:       existing.IncludeCore,
		ExcludePaths:      existing.ExcludePaths,
		ExcludeExtensions: existing.ExcludeExtensions,
		ExcludeFileSizeMB: existing.ExcludeFileSizeMB,
		NotifyOnCompletion: notifyOnCompletion,
		NotifyRecipients:   in.NotifyRecipients,
	}
	return s.repo.UpsertBackupSettings(ctx, in.TenantID, merged)
}

// BackupScopeConfig carries the Track-A (m50) selective-component + exclusion
// fields resolved from a site's backup settings at backup dispatch time. The
// worker calls scheduleBackupScope to fetch these once and threads them into
// the agent command. When no settings row exists the zero value produces "all
// components, no exclusions" — identical to the pre-m49 behaviour.
type BackupScopeConfig struct {
	Components        []string
	IncludeCore       bool
	ExcludePaths      []string
	ExcludeExtensions []string
	ExcludeFileSizeMB int32
}

// scheduleBackupScope resolves the Track-A backup-scope settings from the site's
// backup settings (site_backup_settings table, m50). Returns the zero
// BackupScopeConfig when no settings row exists (or the lookup fails), which
// produces "all components, no exclusions" — identical to the pre-m49 default.
// This is the single dispatch point for BOTH manual and scheduled backup runs.
func (s *Service) scheduleBackupScope(ctx context.Context, tenantID, siteID uuid.UUID) BackupScopeConfig {
	settings, err := s.repo.GetBackupSettings(ctx, tenantID, siteID)
	if err != nil {
		return BackupScopeConfig{} // no settings row → no filter (all components)
	}
	return BackupScopeConfig{
		Components:        settings.BackupComponents,
		IncludeCore:       settings.IncludeCore,
		ExcludePaths:      settings.ExcludePaths,
		ExcludeExtensions: settings.ExcludeExtensions,
		ExcludeFileSizeMB: settings.ExcludeFileSizeMB,
	}
}

// sendBackupEmail enqueues a backup-notification email (Track B, m50) when the
// site's backup settings have notify_on_completion configured and a mailer is
// wired. Reads from site_backup_settings (not backup_schedules). Fires for
// BOTH manual and scheduled backup completions/failures because SubmitManifest
// and FailSnapshot both call this function regardless of how the run was
// triggered. Always best-effort: any failure is logged and silently swallowed.
//
// template is "backup_completed" or "backup_failed".
func (s *Service) sendBackupEmail(ctx context.Context, snap Snapshot, template string) {
	if s.mailer == nil {
		return
	}
	settings, serr := s.repo.GetBackupSettings(ctx, snap.TenantID, snap.SiteID)
	if serr != nil {
		// No settings row (or lookup error): no notification configured.
		return
	}
	notify := settings.NotifyOnCompletion
	if notify == "" || notify == "never" {
		return
	}
	if notify == "on_failure" && template != "backup_failed" {
		return
	}
	if len(settings.NotifyRecipients) == 0 {
		return
	}

	data := map[string]any{
		"SiteURL":    "",
		"Kind":       snap.Kind,
		"SnapshotID": snap.ID.String(),
		"SizeBytes":  snap.TotalSize,
		"FinishedAt": snap.FinishedAt,
		"Error":      snap.Error,
	}
	if si, sierr := s.sites.GetBackupSiteInfo(ctx, snap.TenantID, snap.SiteID); sierr == nil {
		data["SiteURL"] = si.URL
	}

	if err := s.mailer.Enqueue(ctx, snap.TenantID, settings.NotifyRecipients, template, data); err != nil {
		slog.WarnContext(ctx, "backup: notification email enqueue failed (best-effort)",
			slog.String("template", template),
			slog.String("snapshot_id", snap.ID.String()),
			slog.Any("error", err))
	}
}

// selectEntries resolves a RestoreSelection against a manifest, returning the
// matching entries. Full selects everything; Paths selects file entries by
// exact path; DBTables selects db entries by table name. When Components is
// non-empty the result is further filtered to entries whose EntryKind matches
// one of the requested components (M6 / Track 2). Returns a 400 if a requested
// component has no corresponding entries in the snapshot, and a 422 if the
// resolved selection matches nothing.
func selectEntries(entries []ManifestEntry, sel RestoreSelection) ([]ManifestEntry, error) {
	// ADR-051: files-list and tombstones are internal change-detection bookkeeping
	// (the relpath\tsize\tmtime seed + delete deltas), NOT restorable artifacts.
	// Drop them up-front so a base-alone (non-chain) restore never extracts a
	// stray files.list or an empty tombstone-path file. The chain overlay path
	// handles them separately (tombstones drive the delete pass).
	if hasNonRestorableKind(entries) {
		filtered := make([]ManifestEntry, 0, len(entries))
		for _, e := range entries {
			if e.EntryKind == EntryKindFilesList || e.EntryKind == EntryKindTombstones {
				continue
			}
			filtered = append(filtered, e)
		}
		entries = filtered
	}

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

// deriveIncludeDB computes the explicit DB-inclusion signal from a components
// allowlist. Returns nil when the list is empty (no filter; the agent uses its
// own kind-based default). When the list is non-empty, returns a pointer to
// true if "db" is in the list, or false if it is absent.
//
// This is the AUTHORITATIVE rule for the #187 contract: DB inclusion is driven
// by the components list, not the snapshot kind alone. When the CP sends a
// non-nil IncludeDB the agent MUST respect it: skip runDumpDatabase when false,
// include it when true — regardless of the snapshot kind field.
func deriveIncludeDB(components []string) *bool {
	if len(components) == 0 {
		return nil // no filter; let the agent follow the snapshot kind
	}
	for _, c := range components {
		if c == KindDB {
			v := true
			return &v
		}
	}
	v := false
	return &v
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

// baseWindowDaysOr returns the schedule's base-window override as a plain int,
// or 0 when unset. resolveChainForSiteWithWindow treats a non-positive value as
// "use the BackupBaseWindowDays constant".
func baseWindowDaysOr(p *int32) int {
	if p == nil {
		return 0
	}
	return int(*p)
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

// ----------------------------------------------------------------------------
// ADR-048 — Incremental Backup V1
// ----------------------------------------------------------------------------

// ChainResolution is the result of resolveChainForSite: it describes whether
// the next backup for a site should be incremental or a full base.
type ChainResolution struct {
	IsIncremental    bool
	ParentSnapshotID *uuid.UUID
	BaseSnapshotID   *uuid.UUID
	ChainID          *uuid.UUID
	Generation       int
}

// resolveChainForSite implements the AUTO-BASE rule from ADR-048.
// It queries the most recently completed snapshot for (tenantID, siteID) and
// decides whether the next run should be incremental or a full base.
//
// AUTO-BASE conditions (returns IsIncremental=false):
//
//	a. No prior completed snapshot → full base.
//	b. Prior snapshot chain_id is NULL and is_incremental=false but finished_at
//	   is within the base window → start a new chain (generation=1).
//	c. Prior snapshot's finished_at < now() - 7 days → stale chain, full base.
//	d. Prior snapshot generation >= BackupMaxChainDepth → force new full base.
//	e. Prior snapshot has no backup_file_index rows (pre-m44 full zip backup)
//	   AND is NOT incremental → full base (no index to diff against).
func (s *Service) resolveChainForSite(ctx context.Context, tenantID, siteID uuid.UUID) (ChainResolution, error) {
	return s.resolveChainForSiteWithWindow(ctx, tenantID, siteID, BackupBaseWindowDays)
}

// resolveChainForSiteWithWindow is resolveChainForSite with an explicit
// base-window (in days) for the stale-chain check. A non-positive window falls
// back to the BackupBaseWindowDays constant so callers can pass a schedule's
// optional override (nil → constant) safely.
func (s *Service) resolveChainForSiteWithWindow(ctx context.Context, tenantID, siteID uuid.UUID, baseWindowDays int) (ChainResolution, error) {
	if baseWindowDays <= 0 {
		baseWindowDays = BackupBaseWindowDays
	}
	// baseIncrement is the gen-0 BASE bootstrap resolution: an incremental run
	// with NO parent. The agent treats an empty file_index_endpoint as a base
	// scan (every file is new) and uploads a full backup_file_index; repo.
	// CreateSnapshot self-anchors chain_id to the snapshot's own id for a
	// nil-chain gen-0. This is what bootstraps a chain so the NEXT run finds a
	// usable file index and produces a real gen-1 increment. This branch is
	// only reached when incremental is ENABLED (the caller gates on the
	// schedule flag), so toggle-off still gets a plain full backup.
	baseIncrement := ChainResolution{IsIncremental: true, Generation: 0}

	prev, err := s.repo.GetLatestCompletedSnapshot(ctx, tenantID, siteID)
	if err != nil {
		var de *domain.Error
		if errors.As(err, &de) && de.Kind == domain.KindNotFound {
			// No prior snapshot → gen-0 base-increment (bootstrap the chain).
			return baseIncrement, nil
		}
		return ChainResolution{}, err
	}

	now := s.clock.Now()

	// Stale chain (base window elapsed): re-base as a gen-0 base-increment.
	if prev.FinishedAt != nil && now.Sub(*prev.FinishedAt) > time.Duration(baseWindowDays)*24*time.Hour {
		return baseIncrement, nil
	}

	// Chain too deep: re-base as a gen-0 base-increment.
	if prev.Generation >= BackupMaxChainDepth {
		return baseIncrement, nil
	}

	// The prior snapshot must be DIFFABLE or we re-base. ADR-051: an archive-delta
	// snapshot is diffable iff it carries a `files-list` manifest entry (the
	// per-snapshot relpath\tsize\tmtime list the next increment diffs against).
	// A LEGACY chunk-engine snapshot is diffable iff it has backup_file_index rows.
	// Accept EITHER during the migration window. Re-base when the parent carries
	// neither — this guards the 24-min full-re-hash bug an empty/undiffable base
	// would cause (the next increment would scan the whole tree as new).
	diffable, derr := s.repo.HasFilesList(ctx, tenantID, prev.ID)
	if derr != nil {
		return baseIncrement, nil
	}
	if !diffable {
		count, cerr := s.repo.CountFileIndex(ctx, tenantID, prev.ID)
		if cerr != nil || count == 0 {
			return baseIncrement, nil
		}
	}

	// We have a usable prior snapshot. Build the chain fields.
	parentID := prev.ID
	var baseID uuid.UUID
	var chainID uuid.UUID
	generation := prev.Generation + 1

	if prev.IsIncremental && prev.ChainID != nil {
		// Continue an existing chain.
		chainID = *prev.ChainID
		if prev.BaseSnapshotID != nil {
			baseID = *prev.BaseSnapshotID
		} else {
			// prev is the chain's gen-0 base-increment: its own base_snapshot_id
			// is NULL (nothing anchors above it), so the base of THIS increment is
			// prev itself. Without this, baseID stays the zero UUID and the
			// base_snapshot_id FK stamp fails (the 500 on the first increment).
			baseID = prev.ID
		}
	} else {
		// Prior snapshot was a full base. Start a new chain anchored there.
		baseID = prev.ID
		chainID = prev.ID // chain_id = base_snapshot_id per spec.
		generation = 1
	}

	return ChainResolution{
		IsIncremental:    true,
		ParentSnapshotID: &parentID,
		BaseSnapshotID:   &baseID,
		ChainID:          &chainID,
		Generation:       generation,
	}, nil
}

// ADR-051: SubmitIncrementalManifest (the per-file backup_file_index recorder)
// is RETIRED. An archive-delta increment submits the SAME SubmitManifestRequest
// as a full backup — zip parts + DB dump + files-list + tombstones manifest
// entries — and is recorded through SubmitManifest/RecordManifest, inheriting
// its ADR-050 atomic completion for free.
