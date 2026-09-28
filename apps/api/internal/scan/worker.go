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
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/uptime"
)

// Audit action names for the scan lifecycle.
const (
	ActionScanStarted   = "scan.started"
	ActionScanCompleted = "scan.completed"
	ActionScanFailed    = "scan.failed"
	ActionFileFetched   = "scan.file_fetched"

	// Phase 2: file-integrity audit actions.
	ActionFileBaselineEstablished = "scan.baseline_established"
	ActionFileChangeDetected      = "scan.file_change_detected"
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

// SecurityAlerter resolves and dispatches a security alert for a tenant.
// Implemented in cmd/wpmgr via the uptime Dispatcher + Repo. Declared here as
// an interface to keep the scan package free of a direct uptime import for
// the concrete Repo type (which would require DB-pool access at construction).
type SecurityAlerter interface {
	FireFileIntegrityAlert(ctx context.Context, tenantID, siteID uuid.UUID, summary string)
}

// ScanRunWorker drives the multi-step scan loop. Each River job invocation:
//  1. Re-reads the run's current state from the DB.
//  2. Transitions queued → scanning (idempotent).
//  3. Calls scan on the agent (DoOnce via signed JWT).
//  4. Inserts the hash batch (ON CONFLICT DO NOTHING).
//  5. Persists the new cursor BEFORE re-enqueueing (retry correctness).
//  6. If status=partial → re-enqueue self with a fresh JWT (new River job).
//     If status=done   → diffCore/diffFiles → UpsertFindings → MarkDone → PurgeHashes.
type ScanRunWorker struct {
	river.WorkerDefaults[ScanRunArgs]
	repo      *Repo
	checksums *ChecksumProvider
	cmd       AgentScanClient
	sites     SiteLookup
	enqueuer  Reenqueuer
	audit     *audit.Recorder
	logger    *slog.Logger
	// Phase 2: optional dependencies; absent = no alerts/SSE (graceful).
	alerter   SecurityAlerter
	publisher site.EventPublisher
	uptimeRepo uptime.Repo
	dispatcher *uptime.Dispatcher
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

// SetFileIntegrityDeps wires the Phase 2 alert and SSE dependencies.
// Called once from main after all services are constructed. Both are optional;
// absent = no alerts/SSE (safe to omit in tests).
func (w *ScanRunWorker) SetFileIntegrityDeps(pub site.EventPublisher, uptimeRepo uptime.Repo, dispatcher *uptime.Dispatcher) {
	w.publisher = pub
	w.uptimeRepo = uptimeRepo
	w.dispatcher = dispatcher
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
		// A redirect means the site's saved address is wrong. It is terminal
		// (a retry is refused the same way) and it is not an old agent.
		if re, ok := agentcmd.AsRedirect(err); ok {
			return w.fail(ctx, run, re.OperatorMessage("Scan"))
		}
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

// finish dispatches to the correct diff implementation based on the run kind.
func (w *ScanRunWorker) finish(ctx context.Context, run Run, si ScanSiteInfo) error {
	switch run.Kind {
	case KindFull, KindFiles:
		return w.finishFiles(ctx, run, si)
	default:
		// KindCore and any future kinds use the original core-checksum diff.
		return w.finishCore(ctx, run, si)
	}
}

// finishCore runs diffCore and completes the run (status=done). Used for
// kind=core (and any unrecognised kind as a safe fallback).
func (w *ScanRunWorker) finishCore(ctx context.Context, run Run, si ScanSiteInfo) error {
	version := si.WPVersion
	locale := "en_US"

	hashes, err := w.repo.ListHashes(ctx, run.TenantID, run.ID)
	if err != nil {
		return w.fail(ctx, run, "failed to load hashes: "+err.Error())
	}

	checksums, err := w.checksums.Core(ctx, version, locale)
	if err != nil {
		w.logger.Warn("scan checksums fetch failed — proceeding without diff",
			slog.String("run_id", run.ID.String()),
			slog.String("version", version),
			slog.Any("error", err))
		checksums = map[string]string{}
	}

	findings := diffCore(run.ID, run.TenantID, run.SiteID, hashes, checksums)

	if err := w.repo.UpsertFindings(ctx, run.TenantID, findings); err != nil {
		return w.fail(ctx, run, "failed to upsert findings: "+err.Error())
	}

	counts := map[string]int{}
	for _, f := range findings {
		counts[f.FindingType]++
	}

	doneRun, err := w.repo.MarkDone(ctx, run.TenantID, run.ID, version, locale, counts)
	if err != nil {
		return fmt.Errorf("scan worker: mark done: %w", err)
	}

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

// finishFiles runs the Phase 2 diffFiles classifier for kind=full/files:
//  1. Load staged hashes, baseline, managed-file registry.
//  2. Fetch core + plugin checksums (SSRF-safe, Postgres-cached).
//  3. diffFiles → UpsertFindings.
//  4. PromoteBaseline (replace old baseline with this run's hashes).
//  5. Emit audit, alert, SSE.
func (w *ScanRunWorker) finishFiles(ctx context.Context, run Run, si ScanSiteInfo) error {
	version := si.WPVersion
	locale := "en_US"

	// --- 1. Load all inputs for diffFiles ---
	hashes, err := w.repo.ListHashes(ctx, run.TenantID, run.ID)
	if err != nil {
		return w.fail(ctx, run, "failed to load hashes: "+err.Error())
	}

	baseline, err := w.repo.GetBaseline(ctx, run.TenantID, run.SiteID)
	if err != nil {
		w.logger.Warn("failed to load baseline — treating as cold start",
			slog.String("run_id", run.ID.String()), slog.Any("error", err))
		baseline = nil
	}

	managedFiles, err := w.repo.GetManagedFiles(ctx, run.TenantID, run.SiteID)
	if err != nil {
		w.logger.Warn("failed to load managed files — proceeding without suppression",
			slog.String("run_id", run.ID.String()), slog.Any("error", err))
		managedFiles = nil
	}

	// --- 2. Fetch core checksums ---
	coreChecksums, err := w.checksums.Core(ctx, version, locale)
	if err != nil || coreChecksums == nil {
		w.logger.Warn("core checksums fetch failed — proceeding without core diff",
			slog.String("run_id", run.ID.String()), slog.Any("error", err))
		coreChecksums = map[string]string{}
	}

	// Fetch plugin/theme checksums for each installed plugin/theme in the
	// site inventory. We parse the inventory from ScanSiteInfo via a separate
	// lookup (SiteLookup provides components for sites that have them).
	pluginChecksums := make(map[string]map[string][]string) // slug → path → []md5
	if w.sites != nil {
		if components, ok := w.sites.(ComponentLookup); ok {
			plugins, themes := components.GetComponents(ctx, run.TenantID, run.SiteID)
			pluginChecksums = w.fetchAllPluginChecksums(ctx, plugins, themes)
		}
	}

	// --- 3. Run diffFiles ---
	coldStart := len(baseline) == 0
	findings := diffFiles(run.ID, run.TenantID, run.SiteID, hashes, baseline, coreChecksums, pluginChecksums, managedFiles, coldStart)

	if err := w.repo.UpsertFindings(ctx, run.TenantID, findings); err != nil {
		return w.fail(ctx, run, "failed to upsert findings: "+err.Error())
	}

	counts := map[string]int{}
	for _, f := range findings {
		counts[f.FindingType]++
	}

	// --- 4. Promote baseline (path-selective — never advance past an unreviewed change) ---
	//
	// Build the set of paths that have an active (non-ignored) finding from this
	// run. The baseline must NOT advance for those paths; the next scan will
	// re-compare against the prior known-good hash and re-flag them until an
	// operator explicitly accepts the change via IgnoreFinding.
	activeFindingPaths := make(map[string]bool, len(findings))
	for _, f := range findings {
		switch f.FindingType {
		case FindingFileChanged, FindingPluginModified, FindingPluginUnknown,
			FindingFileAdded, FindingFileRemoved:
			activeFindingPaths[f.Path] = true
		}
	}
	if promErr := w.repo.PromoteBaselineSelective(ctx, run.TenantID, run.SiteID, run.ID, coldStart, activeFindingPaths); promErr != nil {
		// Non-fatal: the diff already ran; just log and continue.
		w.logger.Warn("baseline promotion failed",
			slog.String("run_id", run.ID.String()), slog.Any("error", promErr))
	}

	// --- 5. Mark done ---
	doneRun, err := w.repo.MarkDone(ctx, run.TenantID, run.ID, version, locale, counts)
	if err != nil {
		return fmt.Errorf("scan worker: mark done: %w", err)
	}

	_ = w.repo.PurgeHashes(ctx, run.TenantID, run.ID)

	// --- 6. Audit ---
	auditAction := ActionScanCompleted
	if coldStart {
		auditAction = ActionFileBaselineEstablished
	}
	w.recordAudit(ctx, doneRun, auditAction, map[string]any{
		"files_scanned": doneRun.FilesScanned,
		"findings":      counts,
		"cold_start":    coldStart,
	})

	// Separate audit record when high-severity file-change findings were found.
	highCount := counts[FindingFileChanged] + counts[FindingPluginModified] + counts[FindingPluginUnknown]
	if highCount > 0 {
		w.recordAudit(ctx, doneRun, ActionFileChangeDetected, map[string]any{
			"file_changed":    counts[FindingFileChanged],
			"file_added":      counts[FindingFileAdded],
			"file_removed":    counts[FindingFileRemoved],
			"plugin_modified": counts[FindingPluginModified],
			"plugin_unknown":  counts[FindingPluginUnknown],
		})
	}

	// --- 7. Alert (high-severity findings only) ---
	if !coldStart && highCount > 0 && w.uptimeRepo != nil && w.dispatcher != nil {
		alertCfg, alertFound, alertErr := w.uptimeRepo.GetAlertConfig(ctx, run.TenantID)
		if alertErr == nil && alertFound && alertCfg.Enabled && alertCfg.NotifySecurity {
			summary := fmt.Sprintf("%d changed / %d added / %d removed files on site %s",
				counts[FindingFileChanged]+counts[FindingPluginModified],
				counts[FindingFileAdded],
				counts[FindingFileRemoved],
				run.SiteID)
			w.dispatcher.FireSecurityEvent(ctx, alertCfg, uptime.SecurityEvent{
				TenantID:  run.TenantID,
				SiteID:    run.SiteID,
				Summary:   summary,
				EventType: "file_integrity",
				Severity:  SeverityHigh,
				FiredAt:   time.Now(),
			})
		}
	}

	// --- 8. SSE live push (push is a hint; the dashboard polls useScanRun) ---
	if w.publisher != nil {
		// ID is intentionally left empty — Publisher mints the ULID (SSE ULID contract).
		_ = w.publisher.Publish(ctx, site.ConnectionEvent{
			Type:     "scan.finding",
			TenantID: run.TenantID,
			SiteID:   run.SiteID,
			Data: map[string]any{
				"run_id":          run.ID.String(),
				"file_changed":    counts[FindingFileChanged],
				"file_added":      counts[FindingFileAdded],
				"file_removed":    counts[FindingFileRemoved],
				"plugin_modified": counts[FindingPluginModified],
				"plugin_unknown":  counts[FindingPluginUnknown],
				"cold_start":      coldStart,
			},
		})
	}

	w.logger.Info("file-integrity scan run completed",
		slog.String("run_id", run.ID.String()),
		slog.String("site_id", run.SiteID.String()),
		slog.Int64("files_scanned", doneRun.FilesScanned),
		slog.Bool("cold_start", coldStart),
		slog.Any("findings", counts))
	return nil
}

// fetchAllPluginChecksums fetches and merges plugin+theme checksums for every
// installed component. Returns slug → (plugin-relative path → []md5 variants).
// Non-fatal: missing checksums for a slug mean it falls through to baseline.
func (w *ScanRunWorker) fetchAllPluginChecksums(ctx context.Context, plugins, themes []site.Component) map[string]map[string][]string {
	result := make(map[string]map[string][]string)
	for _, p := range plugins {
		slug := pluginDirSlug(p.Slug)
		if slug == "" {
			continue
		}
		cs, err := w.checksums.Plugin(ctx, "plugin", slug, p.Version)
		if err != nil || len(cs) == 0 {
			continue
		}
		result[slug] = cs
	}
	for _, t := range themes {
		// Theme slug from inventory is already the stylesheet dir = the wp.org slug.
		slug := t.Slug
		if slug == "" {
			continue
		}
		cs, err := w.checksums.Plugin(ctx, "theme", slug, t.Version)
		if err != nil || len(cs) == 0 {
			continue
		}
		result[slug] = cs
	}
	return result
}

// pluginDirSlug extracts the directory slug from a plugin file path such as
// "akismet/akismet.php" → "akismet". Handles bare slugs (no slash) unchanged.
func pluginDirSlug(pluginFilePath string) string {
	if idx := strings.Index(pluginFilePath, "/"); idx > 0 {
		return pluginFilePath[:idx]
	}
	return pluginFilePath
}

// ComponentLookup is an optional extension of SiteLookup that provides the
// installed plugin/theme inventory. Implemented by the site adapter in main.
// When SiteLookup does not implement this interface, plugin checksums are skipped
// and the diff falls through to baseline-only for all plugin/theme paths.
type ComponentLookup interface {
	GetComponents(ctx context.Context, tenantID, siteID uuid.UUID) (plugins, themes []site.Component)
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
	"index.php":            true,
	"wp-activate.php":      true,
	"wp-blog-header.php":   true,
	"wp-comments-post.php": true,
	"wp-config-sample.php": true,
	"wp-cron.php":          true,
	"wp-links-opml.php":    true,
	"wp-load.php":          true,
	"wp-login.php":         true,
	"wp-mail.php":          true,
	"wp-settings.php":      true,
	"wp-signup.php":        true,
	"wp-trackback.php":     true,
	"xmlrpc.php":           true,
}

// allowedRootFiles is the operator-managed / WordPress-generated root files
// that are NOT in the wp.org manifest (so they never trigger unknown_injected).
// .htaccess is also excluded from core_modified detection since WordPress
// rewrites it constantly.
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
// diffFiles — Phase 2 full-filesystem diff classifier
// ---------------------------------------------------------------------------

// diffFiles classifies each path in a full/files scan run against:
//  1. site_managed_files (suppress or detect managed-file tampering).
//  2. WordPress core checksums (reuse core detection for core paths).
//  3. wp.org plugin/theme checksums (plugin_modified / plugin_unknown).
//  4. site_file_baseline (file_added / file_changed / file_removed).
//
// The function is pure (no I/O) for unit testing.
//
// pluginChecksums is a map from directory slug → (plugin-relative path → []md5
// variants). A path under plugins/<slug>/ is looked up as a plugin; a path
// under themes/<slug>/ as a theme.
//
// When coldStart is true (no baseline rows) the function emits only
// core/plugin-checksum findings; no Added/Changed/Removed findings are emitted
// for non-core/plugin paths. This prevents a first-scan flood and sets up the
// baseline for next time.
func diffFiles(
	runID, tenantID, siteID uuid.UUID,
	hashes []HashRow,
	baseline []BaselineRow,
	coreChecksums map[string]string,
	pluginChecksums map[string]map[string][]string, // slug → path → []md5
	managed []ManagedFileRow,
	coldStart bool,
) []Finding {
	// Build lookup maps.
	managedMap := make(map[string]ManagedFileRow, len(managed))
	for _, m := range managed {
		managedMap[m.Path] = m
	}

	baselineMap := make(map[string]BaselineRow, len(baseline))
	for _, b := range baseline {
		baselineMap[b.Path] = b
	}

	// Track paths present in this run for the Removed pass.
	thisRun := make(map[string]bool, len(hashes))
	for _, h := range hashes {
		thisRun[h.Path] = true
	}

	var findings []Finding

	// -----------------------------------------------------------------------
	// Pass 1: classify each path in the current run.
	// -----------------------------------------------------------------------
	for _, h := range hashes {
		path := h.Path
		actualMD5 := strings.ToLower(h.MD5)

		// (a) Check managed-file registry first.
		if m, ok := managedMap[path]; ok {
			if m.MD5 == "" {
				// Suppress: this path is WPMgr-managed, churn-tolerant.
				continue
			}
			if strings.EqualFold(m.MD5, actualMD5) {
				// Matches expected managed hash: OK.
				continue
			}
			// Hash differs from the expected managed hash: managed-file tampering.
			findings = append(findings, makeFinding(runID, tenantID, siteID,
				FindingFileChanged, path, SeverityHigh, m.MD5, actualMD5))
			continue
		}

		// (b) Core paths: delegate to core checksums (same logic as diffCore).
		if isCorePath(path) {
			expectedMD5, inManifest := coreChecksums[path]
			if path == ".htaccess" || allowedRootFiles[path] {
				continue // allow-listed
			}
			if inManifest {
				if actualMD5 == "" || !strings.EqualFold(actualMD5, expectedMD5) {
					findings = append(findings, makeFinding(runID, tenantID, siteID,
						FindingCoreModified, path, SeverityHigh, expectedMD5, actualMD5))
				}
				continue
			}
			// In a core dir but not in manifest (injected file).
			if !strings.HasPrefix(path, "wp-content/") {
				findings = append(findings, makeFinding(runID, tenantID, siteID,
					FindingCoreUnknownInjected, path, SeverityHigh, "", actualMD5))
			}
			continue
		}

		// (c) Plugin / theme paths: compare against wp.org checksums.
		if slug, relPath, kind := pluginOrThemePath(path); slug != "" {
			_ = kind
			slugChecksums, hasPlugin := pluginChecksums[slug]
			if hasPlugin {
				variants, inManifest := slugChecksums[relPath]
				if inManifest {
					if md5MatchesAny(actualMD5, variants) {
						// Matches an official variant: OK.
						continue
					}
					findings = append(findings, makeFinding(runID, tenantID, siteID,
						FindingPluginModified, path, SeverityHigh,
						strings.Join(variants, "|"), actualMD5))
					continue
				}
				// File not in manifest but in a known-wp.org plugin dir.
				findings = append(findings, makeFinding(runID, tenantID, siteID,
					FindingPluginUnknown, path, SeverityHigh, "", actualMD5))
				continue
			}
			// No wp.org checksums for this slug: fall through to baseline.
		}

		// (d) Baseline diff (for non-core, non-plugin/theme, non-managed paths).
		if coldStart {
			// Cold start: no baseline yet; establish baseline only, no A/C/R.
			continue
		}
		baseRow, inBaseline := baselineMap[path]
		if inBaseline {
			if !strings.EqualFold(baseRow.MD5, actualMD5) {
				findings = append(findings, makeFinding(runID, tenantID, siteID,
					FindingFileChanged, path, SeverityHigh, baseRow.MD5, actualMD5))
			}
			// else: unchanged
		} else {
			// New path not in baseline: file_added.
			findings = append(findings, makeFinding(runID, tenantID, siteID,
				FindingFileAdded, path, SeverityMedium, "", actualMD5))
		}
	}

	// -----------------------------------------------------------------------
	// Pass 2: detect removed files (in baseline but absent from this run).
	// -----------------------------------------------------------------------
	if !coldStart {
		for _, b := range baseline {
			if thisRun[b.Path] {
				continue // still present
			}
			if _, managed := managedMap[b.Path]; managed {
				continue // managed path removal is not reported
			}
			// Core-path removals are reported by diffCore (core_missing); skip here
			// to avoid double-reporting when both kinds are run.
			if isCorePath(b.Path) {
				continue
			}
			findings = append(findings, makeFinding(runID, tenantID, siteID,
				FindingFileRemoved, b.Path, SeverityLow, b.MD5, ""))
		}
	}

	return findings
}

// pluginOrThemePath checks whether path is under plugins/<slug>/... or
// themes/<slug>/... (relative to the WordPress root). Returns (slug,
// plugin-relative path, "plugin"|"theme"). Returns ("","","") for other paths.
func pluginOrThemePath(path string) (slug, relPath, kind string) {
	for _, prefix := range []struct{ dir, kind string }{
		{"wp-content/plugins/", "plugin"},
		{"wp-content/themes/", "theme"},
	} {
		if !strings.HasPrefix(path, prefix.dir) {
			continue
		}
		rest := path[len(prefix.dir):]
		slash := strings.Index(rest, "/")
		if slash <= 0 {
			return "", "", "" // bare file directly under plugins/ (no slug dir)
		}
		return rest[:slash], rest[slash+1:], prefix.kind
	}
	return "", "", ""
}

// md5MatchesAny returns true when actual (lowercase hex) matches any of the
// given accepted variants (case-insensitive). Used for the multi-variant edge
// case in wp.org plugin checksums.
func md5MatchesAny(actual string, variants []string) bool {
	for _, v := range variants {
		if strings.EqualFold(actual, v) {
			return true
		}
	}
	return false
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
