package scan

import (
	"context"
	"crypto/md5" //nolint:gosec // used only for dedup_key computation, not security
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
)

// Audit action names for the scan lifecycle.
const (
	ActionScanStarted   = "scan.started"
	ActionScanCompleted = "scan.completed"
	ActionScanFailed    = "scan.failed"
	ActionFileFetched   = "scan.file_fetched"
)

// ScanRunQueue is the dedicated River queue for scan run driver jobs.
const ScanRunQueue = "scan_run"

// ScanRunArgs is the River job payload for one scan run iteration. The worker
// re-reads authoritative state (tenant-scoped) from the DB on every attempt.
type ScanRunArgs struct {
	TenantID uuid.UUID `json:"tenant_id"`
	SiteID   uuid.UUID `json:"site_id"`
	RunID    uuid.UUID `json:"run_id"`
}

// Kind implements river.JobArgs.
func (ScanRunArgs) Kind() string { return "scan_run" }

// Reenqueuer inserts a new scan_run River job (used by the worker to re-enqueue
// itself for the next partial iteration). *RiverEnqueuer satisfies it.
type Reenqueuer interface {
	EnqueueScanRun(ctx context.Context, args ScanRunArgs) error
}

// ScanRunWorker drives the multi-step scan loop. Each River job invocation:
//  1. Re-reads the run's current state from the DB.
//  2. Transitions queued → scanning (idempotent).
//  3. Calls scan on the agent (DoOnce via signed JWT).
//  4. Inserts the hash batch (ON CONFLICT DO NOTHING).
//  5. Persists the new cursor BEFORE re-enqueueing (retry correctness).
//  6. If status=partial → re-enqueue self with a fresh JWT (new River job).
//     If status=done   → diffCore → UpsertFindings → MarkDone → PurgeHashes.
type ScanRunWorker struct {
	river.WorkerDefaults[ScanRunArgs]
	repo      *Repo
	checksums *ChecksumProvider
	cmd       AgentScanClient
	sites     SiteLookup
	enqueuer  Reenqueuer
	audit     *audit.Recorder
	logger    *slog.Logger
}

// NewScanRunWorker builds the scan worker.
func NewScanRunWorker(
	repo *Repo,
	checksums *ChecksumProvider,
	cmd AgentScanClient,
	sites SiteLookup,
	enqueuer Reenqueuer,
	rec *audit.Recorder,
	logger *slog.Logger,
) *ScanRunWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &ScanRunWorker{
		repo:      repo,
		checksums: checksums,
		cmd:       cmd,
		sites:     sites,
		enqueuer:  enqueuer,
		audit:     rec,
		logger:    logger,
	}
}

// SetEnqueuer wires the Reenqueuer after River has started (the River client is
// not available when the worker is built, so wiring happens in two phases).
func (w *ScanRunWorker) SetEnqueuer(e Reenqueuer) { w.enqueuer = e }

// Timeout overrides River's per-job context deadline. 90s gives 12s agent
// budget + 30s network latency headroom + 48s for DB inserts.
func (w *ScanRunWorker) Timeout(*river.Job[ScanRunArgs]) time.Duration {
	return 90 * time.Second
}

// Work drives one scan iteration.
func (w *ScanRunWorker) Work(ctx context.Context, job *river.Job[ScanRunArgs]) error {
	a := job.Args

	// 1. Re-read authoritative state.
	run, err := w.repo.GetRun(ctx, a.TenantID, a.RunID)
	if err != nil {
		return fmt.Errorf("scan worker: get run: %w", err)
	}
	if run.Status == StatusDone || run.Status == StatusFailed {
		return nil // already terminal (dup delivery)
	}

	// 2. Resolve site.
	si, err := w.sites.GetScanSiteInfo(ctx, a.TenantID, a.SiteID)
	if err != nil {
		return w.fail(ctx, run, "site unresolved: "+err.Error())
	}
	if !si.Enrolled {
		return w.fail(ctx, run, "site is not enrolled")
	}
	if w.cmd == nil {
		return w.fail(ctx, run, "scan agent client is not wired")
	}

	// 3. Transition to scanning (idempotent — run may already be scanning on retry).
	if run.Status == StatusQueued {
		run, err = w.repo.MarkScanning(ctx, a.TenantID, a.RunID)
		if err != nil {
			return fmt.Errorf("scan worker: mark scanning: %w", err)
		}
		w.recordAudit(ctx, run, ActionScanStarted, nil)
	}

	// 4. Build scan request with resume cursor.
	var cursor *agentcmd.ScanCursor
	if len(run.Cursor) > 0 && string(run.Cursor) != "null" {
		var c agentcmd.ScanCursor
		if err := json.Unmarshal(run.Cursor, &c); err == nil {
			cursor = &c
		}
	}

	req := agentcmd.ScanRequest{
		RunID:                 run.ID.String(),
		Kind:                  run.Kind,
		IncludeMD5:            true,
		TimeBudgetS:           12,
		PathsLimit:            4000,
		BatchSize:             512,
		TraversalStackMaxSize: 100,
		ResumeCursor:          cursor,
	}

	// 5. Call agent (DoOnce — JWT jti single-use; River retries mint fresh JWT).
	resp, err := w.cmd.Scan(ctx, a.SiteID, si.URL, req)
	if err != nil {
		errMsg := err.Error()
		// A 404 from the REST layer means the agent is too old to have the scan route.
		if strings.Contains(errMsg, "status 404") {
			return w.fail(ctx, run, "agent_too_old")
		}
		// Other transport errors: return for River retry with a fresh JWT.
		return fmt.Errorf("scan command to agent failed: %w", err)
	}
	if !resp.OK {
		return w.fail(ctx, run, "agent refused scan: "+resp.Status)
	}

	// 6. Insert hash batch (ON CONFLICT DO NOTHING = idempotent).
	hashRows := make([]HashRow, 0, len(resp.Hashes))
	for _, h := range resp.Hashes {
		hashRows = append(hashRows, HashRow{
			TenantID: a.TenantID,
			RunID:    a.RunID,
			Path:     h.Path,
			Size:     h.Size,
			MD5:      h.MD5,
			Mtime:    h.Mtime,
			IsLink:   h.IsLink,
		})
	}
	if err := w.repo.InsertHashBatch(ctx, a.TenantID, hashRows); err != nil {
		return fmt.Errorf("scan worker: insert hashes: %w", err)
	}

	// 7. Persist cursor BEFORE re-enqueueing (retry correctness: if re-enqueue
	// fails the next attempt will re-read the correct cursor from the DB).
	var newCursorJSON json.RawMessage
	if resp.NextCursor != nil {
		if b, jerr := json.Marshal(resp.NextCursor); jerr == nil {
			newCursorJSON = b
		}
	}
	if err := w.repo.UpdateCursor(ctx, a.TenantID, a.RunID, newCursorJSON, resp.FilesScanned); err != nil {
		return fmt.Errorf("scan worker: update cursor: %w", err)
	}

	// 8. Dispatch based on scan status.
	if resp.Status == "partial" {
		// Re-enqueue self for the next partial batch.
		if w.enqueuer != nil {
			if err := w.enqueuer.EnqueueScanRun(ctx, a); err != nil {
				// If enqueue fails return the error so River retries this job
				// (cursor is already persisted so the next attempt picks up correctly).
				return fmt.Errorf("scan worker: re-enqueue: %w", err)
			}
		}
		return nil
	}

	// status == "done": run diffCore and write findings.
	return w.finish(ctx, run, si)
}

// finish runs diffCore and completes the run (status=done).
func (w *ScanRunWorker) finish(ctx context.Context, run Run, si ScanSiteInfo) error {
	// Use the site's wp_version for checksums lookup; locale defaults to en_US
	// (no per-site locale field exists in the current schema).
	version := si.WPVersion
	locale := "en_US"

	// Load staged hashes.
	hashes, err := w.repo.ListHashes(ctx, run.TenantID, run.ID)
	if err != nil {
		return w.fail(ctx, run, "failed to load hashes: "+err.Error())
	}

	// Fetch checksums (Postgres-cached; SSRF-safe via httpclient).
	checksums, err := w.checksums.Core(ctx, version, locale)
	if err != nil {
		// Non-fatal: proceed without checksums (no findings this run).
		w.logger.Warn("scan checksums fetch failed — proceeding without diff",
			slog.String("run_id", run.ID.String()),
			slog.String("version", version),
			slog.Any("error", err))
		checksums = map[string]string{}
	}

	// Run diff.
	findings := diffCore(run.ID, run.TenantID, run.SiteID, hashes, checksums)

	// Upsert deduplicated findings.
	if err := w.repo.UpsertFindings(ctx, run.TenantID, findings); err != nil {
		return w.fail(ctx, run, "failed to upsert findings: "+err.Error())
	}

	// Count findings by type for the summary.
	counts := map[string]int{}
	for _, f := range findings {
		counts[f.FindingType]++
	}

	// Mark done.
	doneRun, err := w.repo.MarkDone(ctx, run.TenantID, run.ID, version, locale, counts)
	if err != nil {
		return fmt.Errorf("scan worker: mark done: %w", err)
	}

	// Purge staging hashes.
	_ = w.repo.PurgeHashes(ctx, run.TenantID, run.ID)

	w.recordAudit(ctx, doneRun, ActionScanCompleted, map[string]any{
		"files_scanned": doneRun.FilesScanned,
		"findings":      counts,
	})
	w.logger.Info("scan run completed",
		slog.String("run_id", run.ID.String()),
		slog.String("site_id", run.SiteID.String()),
		slog.Int64("files_scanned", doneRun.FilesScanned),
		slog.Any("findings", counts))
	return nil
}

// fail marks the run as failed (terminal River success — the job itself
// completed; it recorded the failure in the DB).
func (w *ScanRunWorker) fail(ctx context.Context, run Run, msg string) error {
	failed, err := w.repo.MarkFailed(ctx, run.TenantID, run.ID, msg)
	if err != nil {
		return err
	}
	_ = w.repo.PurgeHashes(ctx, run.TenantID, run.ID)
	w.recordAudit(ctx, failed, ActionScanFailed, map[string]any{"error": msg})
	w.logger.Warn("scan run failed",
		slog.String("run_id", run.ID.String()),
		slog.String("site_id", run.SiteID.String()),
		slog.String("error", msg))
	return nil // terminal
}

func (w *ScanRunWorker) recordAudit(ctx context.Context, run Run, action string, extra map[string]any) {
	if w.audit == nil {
		return
	}
	meta := map[string]any{
		"site_id": run.SiteID.String(),
		"kind":    run.Kind,
		"status":  run.Status,
	}
	for k, v := range extra {
		meta[k] = v
	}
	_, _ = w.audit.Record(ctx, audit.Event{
		TenantID:   run.TenantID,
		ActorType:  audit.ActorSystem,
		Action:     action,
		TargetType: "scan_run",
		TargetID:   run.ID.String(),
		Metadata:   meta,
	})
}

// ---------------------------------------------------------------------------
// diffCore — the classification engine
// ---------------------------------------------------------------------------

// coreDirs is the set of prefixes that constitute the WordPress "core" area.
// Files outside these paths are not subject to unknown_injected detection to
// avoid false positives (wp-content/, plugins, themes, uploads are all
// operator-modified by design).
var coreDirs = []string{
	"wp-admin/",
	"wp-includes/",
}

// coreRootPHPFiles is the set of root-level PHP files shipped with WordPress
// core that are subject to core_modified / core_missing detection.
var coreRootPHPFiles = map[string]bool{
	"index.php":              true,
	"wp-activate.php":        true,
	"wp-blog-header.php":     true,
	"wp-comments-post.php":   true,
	"wp-config-sample.php":   true,
	"wp-cron.php":            true,
	"wp-links-opml.php":      true,
	"wp-load.php":            true,
	"wp-login.php":           true,
	"wp-mail.php":            true,
	"wp-settings.php":        true,
	"wp-signup.php":          true,
	"wp-trackback.php":       true,
	"xmlrpc.php":             true,
}

// allowedRootFiles is the operator-managed / WordPress-generated root files
// that are NOT in the wp.org manifest (so they never trigger unknown_injected).
// Seeded from wpremote $cwAllowedFiles. .htaccess is also excluded from
// core_modified detection since WordPress rewrites it constantly.
var allowedRootFiles = map[string]bool{
	"wp-config.php":      true,
	".htaccess":          true,
	".user.ini":          true,
	".maintenance":       true,
	"object-cache.php":   true,
	"advanced-cache.php": true,
}

// diffCore compares the staged file hashes against the known-good checksums
// and returns the classified findings.
//
// Classification rules:
//
//	core_missing          — a manifest entry has no corresponding hash row.
//	                        Severity: medium.
//	core_modified         — a manifest entry has a hash row but the MD5 differs.
//	                        Exception: .htaccess is allow-listed.
//	                        Severity: high.
//	core_unknown_injected — a hash row in a core dir (wp-admin/, wp-includes/,
//	                        or a root *.php) is NOT in the manifest AND is NOT
//	                        in the allow-list. Severity: high.
//
// The function is pure (no I/O) for easy unit testing.
func diffCore(runID, tenantID, siteID uuid.UUID, hashes []HashRow, checksums map[string]string) []Finding {
	if len(checksums) == 0 {
		return nil
	}

	// Build a path→HashRow lookup.
	hashMap := make(map[string]HashRow, len(hashes))
	for _, h := range hashes {
		hashMap[h.Path] = h
	}

	var findings []Finding

	// --- Pass 1: check manifest entries against hash rows ---
	for manifestPath, expectedMD5 := range checksums {
		if !isCorePath(manifestPath) {
			continue
		}
		// .htaccess: allow-listed (WordPress rewrites it constantly).
		if manifestPath == ".htaccess" {
			continue
		}

		hashRow, found := hashMap[manifestPath]
		if !found {
			findings = append(findings, makeFinding(runID, tenantID, siteID,
				FindingCoreMissing, manifestPath, SeverityMedium, expectedMD5, ""))
			continue
		}
		// Unreadable files (md5="") — treated as modified.
		if hashRow.MD5 == "" {
			findings = append(findings, makeFinding(runID, tenantID, siteID,
				FindingCoreModified, manifestPath, SeverityHigh, expectedMD5, ""))
			continue
		}
		if !strings.EqualFold(hashRow.MD5, expectedMD5) {
			findings = append(findings, makeFinding(runID, tenantID, siteID,
				FindingCoreModified, manifestPath, SeverityHigh, expectedMD5, hashRow.MD5))
		}
	}

	// --- Pass 2: detect injected files in core dirs ---
	for _, h := range hashes {
		if !isCorePath(h.Path) {
			continue
		}
		if _, inManifest := checksums[h.Path]; inManifest {
			continue // already handled above
		}
		if allowedRootFiles[h.Path] {
			continue // operator-managed file
		}
		if strings.HasPrefix(h.Path, "wp-content/") {
			continue // operator territory
		}
		findings = append(findings, makeFinding(runID, tenantID, siteID,
			FindingCoreUnknownInjected, h.Path, SeverityHigh, "", h.MD5))
	}

	return findings
}

// isCorePath returns true when the path is within a WordPress core directory
// subject to integrity checking: wp-admin/, wp-includes/, or a root-level
// PHP file that is part of core.
func isCorePath(path string) bool {
	for _, prefix := range coreDirs {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	// Root-level PHP files: must be in coreRootPHPFiles to be checked.
	if !strings.Contains(path, "/") && strings.HasSuffix(path, ".php") {
		return coreRootPHPFiles[path]
	}
	return false
}

// makeFinding constructs a Finding with a stable dedup_key.
func makeFinding(runID, tenantID, siteID uuid.UUID, findingType, path, severity, expectedMD5, actualMD5 string) Finding {
	h := md5.New() //nolint:gosec
	_, _ = fmt.Fprintf(h, "%s:%s:%s:%s", siteID, findingType, path, tenantID)
	deduKey := fmt.Sprintf("%x", h.Sum(nil))

	return Finding{
		TenantID:    tenantID,
		SiteID:      siteID,
		RunID:       runID,
		FindingType: findingType,
		Path:        path,
		Severity:    severity,
		ExpectedMD5: expectedMD5,
		ActualMD5:   actualMD5,
		DeduKey:     deduKey,
		LastSeenRun: runID,
	}
}

// ---------------------------------------------------------------------------
// RiverEnqueuer
// ---------------------------------------------------------------------------

// RiverEnqueuer inserts scan_run jobs into River. Satisfies Reenqueuer.
type RiverEnqueuer struct {
	client *river.Client[pgx.Tx]
}

// NewRiverEnqueuer builds an enqueuer around the River client.
func NewRiverEnqueuer(client *river.Client[pgx.Tx]) *RiverEnqueuer {
	return &RiverEnqueuer{client: client}
}

// EnqueueScanRun inserts a new scan_run job.
func (e *RiverEnqueuer) EnqueueScanRun(ctx context.Context, args ScanRunArgs) error {
	if _, err := e.client.Insert(ctx, args, &river.InsertOpts{Queue: ScanRunQueue}); err != nil {
		return fmt.Errorf("enqueue scan_run: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Hash GC worker (periodic)
// ---------------------------------------------------------------------------

// HashGCArgs is the River job payload for the periodic orphan-hash GC.
type HashGCArgs struct{}

// Kind implements river.JobArgs.
func (HashGCArgs) Kind() string { return "scan_hash_gc" }

// HashGCWorker sweeps orphan scan_run_hashes rows: hashes whose run has been
// in staging (not done/failed) for >24h, indicating the River job died
// mid-flight.
type HashGCWorker struct {
	river.WorkerDefaults[HashGCArgs]
	repo   *Repo
	age    time.Duration
	logger *slog.Logger
}

// NewHashGCWorker builds the hash GC worker.
func NewHashGCWorker(repo *Repo, age time.Duration, logger *slog.Logger) *HashGCWorker {
	if logger == nil {
		logger = slog.Default()
	}
	if age <= 0 {
		age = 24 * time.Hour
	}
	return &HashGCWorker{repo: repo, age: age, logger: logger}
}

// Work runs one GC pass.
func (w *HashGCWorker) Work(ctx context.Context, _ *river.Job[HashGCArgs]) error {
	deleted, err := w.repo.PurgeOrphanHashes(ctx, w.age)
	if err != nil {
		w.logger.Warn("scan hash GC error", slog.Any("error", err))
		return err
	}
	if deleted > 0 {
		w.logger.Info("scan hash GC",
			slog.Int64("rows_deleted", deleted),
			slog.Duration("age", w.age))
	}
	return nil
}
