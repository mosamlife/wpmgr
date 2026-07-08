package perf

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the operator-facing Performance Suite routes under
// /api/v1/sites/{siteId}/perf|cache|db|rucss/... plus the portfolio bulk routes
// under /api/v1/cache/*.
type Handler struct {
	svc       *Service
	rucss     *RucssResultsReader
	fonts     *FontResultsReader
	rum       *RumResultsReader // M56 — nil degrades to empty response
	audit     *audit.Recorder
	corpus    CorpusSource // P3.5 — nil degrades orphans to no-scan-found
	cpBaseURL string       // forwarded to Service.DBClean for progress_endpoint
}

// RucssResultsReader is the subset of the rucss repo the operator results route
// needs. *rucss/repo.Repo satisfies it via ListForSite; a thin adapter maps the
// rucss model.Result to the DTO. Declared here as func seams so the handler does
// not import the rucss model directly.
type RucssResultsReader struct {
	List  func(ctx context.Context, tenantID, siteID uuid.UUID, limit, offset int32) ([]RucssResultDTO, error)
	Clear func(ctx context.Context, tenantID, siteID uuid.UUID) (int, error)
}

// NewHandler builds the operator handler. rucss may be nil (RUCSS results route
// degrades to an empty list). cpBaseURL is the CP public base URL forwarded to
// the agent as the progress_endpoint host (may be empty in test).
func NewHandler(svc *Service, rucss *RucssResultsReader, rec *audit.Recorder) *Handler {
	return &Handler{svc: svc, rucss: rucss, audit: rec}
}

// SetCorpusSource wires the corpus reader used by the orphans classification
// endpoint (P3.5). When nil the endpoint returns a domain.NotFound response
// (same as when no scan exists).
func (h *Handler) SetCorpusSource(c CorpusSource) { h.corpus = c }

// SetFontResultsReader wires the font results list reader (M55). When nil the
// /perf/fonts endpoint returns an empty list.
func (h *Handler) SetFontResultsReader(f *FontResultsReader) { h.fonts = f }

// SetCPBaseURL sets the CP public base URL used when constructing the
// progress_endpoint for db_clean commands.
func (h *Handler) SetCPBaseURL(u string) { h.cpBaseURL = u }

// Register mounts the routes on the authenticated /api/v1 group. RequireSiteAccess
// is applied on the per-site group so every sub-route inherits the collaborator
// allowlist gate (belt-and-braces in front of the m36 RLS).
func (h *Handler) Register(r *gin.RouterGroup) {
	g := r.Group("/sites/:siteId", authz.RequireSiteAccess("siteId"))

	g.GET("/perf/config", authz.RequirePermission(authz.PermSitePerfConfig), h.getConfig)
	g.PUT("/perf/config", authz.RequirePermission(authz.PermSitePerfConfig), h.putConfig)

	// All per-site perf routes live under the /perf/ namespace (the dashboard
	// builds every URL as /sites/{id}/perf + suffix).
	g.GET("/perf/cache/stats", authz.RequirePermission(authz.PermSiteRead), h.getCacheStats)
	g.POST("/perf/cache/purge", authz.RequirePermission(authz.PermSiteCachePurge), h.purge)
	g.POST("/perf/cache/preload", authz.RequirePermission(authz.PermSiteCachePurge), h.preload)
	g.POST("/perf/cache/enable", authz.RequirePermission(authz.PermSiteCacheManage), h.enable)
	g.POST("/perf/cache/disable", authz.RequirePermission(authz.PermSiteCacheManage), h.disable)

	g.POST("/perf/db/clean", authz.RequirePermission(authz.PermSiteCacheManage), h.dbClean)
	g.GET("/perf/db/clean", authz.RequirePermission(authz.PermSiteRead), h.getDbClean)
	g.POST("/perf/db/scan", authz.RequirePermission(authz.PermSiteCacheManage), h.dbScan)
	g.GET("/perf/db/scan", authz.RequirePermission(authz.PermSiteRead), h.getDbScan)
	// M42 Phase 3.4 — DB-size trend history + growth summary.
	g.GET("/perf/db/health", authz.RequirePermission(authz.PermSiteRead), h.getDBHealth)
	// M52 / #162 — cache hit-ratio history + avg.
	g.GET("/perf/cache/health", authz.RequirePermission(authz.PermSiteRead), h.getCacheHealth)
	// P3.5 — on-demand orphan classification report (READ-ONLY).
	g.GET("/perf/db/orphans", authz.RequirePermission(authz.PermSiteRead), h.getOrphansReport)
	// P3.8 — destructive orphan deletion (options / cron / tables from UNINSTALLED
	// plugins only). Route-level gate: PermSiteCacheManage (operator+). The handler
	// body enforces PermSiteCacheDeleteAll (admin+) + type-to-confirm token +
	// CP-side re-classify before signing. The agent performs live re-verification
	// of every item independently.
	g.POST("/perf/db/orphan-delete", authz.RequirePermission(authz.PermSiteCacheManage), h.dbOrphanDelete)
	// Phase 2.2/2.5 — per-table DDL actions (optimize/repair/drop/empty/analyze/convert_innodb).
	// Route-level gate: PermSiteCacheManage (operator+). Destructive actions
	// (drop/empty) additionally require PermSiteCacheDeleteAll (admin+), enforced
	// inside the handler body (mirrors the purge delete-everything pattern).
	// Non-destructive actions (analyze/convert_innodb) require only PermSiteCacheManage.
	g.POST("/perf/db/table-action", authz.RequirePermission(authz.PermSiteCacheManage), h.dbTableAction)

	// #188 — serialization-safe search-replace tool. PermSiteWrite (operator+)
	// is the right gate: this is a data-mutation tool on par with a site edit,
	// not a cache-management action. The handler enforces a dry_run preview step
	// (UI) and emits an advisory backup warning header when dry_run=false.
	g.POST("/perf/db/search-replace", authz.RequirePermission(authz.PermSiteWrite), h.dbSearchReplace)

	// #189 — local database snapshot tool.
	// GET list: PermSiteRead (viewer+) — no side effects.
	// POST create/revert/delete: PermSiteWrite (operator+) — data-mutation operations;
	//   revert additionally requires the "REVERT" confirm token (enforced on the agent).
	g.GET("/perf/db/snapshots", authz.RequirePermission(authz.PermSiteRead), h.dbSnapshotList)
	g.POST("/perf/db/snapshots", authz.RequirePermission(authz.PermSiteWrite), h.dbSnapshotCreate)
	g.POST("/perf/db/snapshots/:snapshotId/revert", authz.RequirePermission(authz.PermSiteWrite), h.dbSnapshotRevert)
	g.DELETE("/perf/db/snapshots/:snapshotId", authz.RequirePermission(authz.PermSiteWrite), h.dbSnapshotDelete)

	g.GET("/perf/rucss/results", authz.RequirePermission(authz.PermSiteRead), h.rucssResults)
	g.POST("/perf/rucss/clear", authz.RequirePermission(authz.PermSitePerfConfig), h.rucssClear)
	g.POST("/perf/rucss/compute", authz.RequirePermission(authz.PermSitePerfConfig), h.rucssCompute)

	// M55 — Font results catalog (dashboard list, operator read-only).
	g.GET("/perf/fonts", authz.RequirePermission(authz.PermSiteRead), h.fontResults)

	// M56 — RUM Core Web Vitals read endpoints (operator read-only).
	// /perf/rum/summary returns site-level p75 aggregates over a configurable window,
	//   including the good/NI/poor distribution bar for each metric slice.
	// /perf/rum/trend returns per-metric daily p75 trend series (default 28 days).
	// /perf/rum returns per-URL/metric/device breakdown rows for the dashboard table.
	// All three enforce the min_sample_count suppression floor server-side.
	g.GET("/perf/rum/summary", authz.RequirePermission(authz.PermSiteRead), h.rumSummary)
	g.GET("/perf/rum/trend", authz.RequirePermission(authz.PermSiteRead), h.rumTrend)
	g.GET("/perf/rum", authz.RequirePermission(authz.PermSiteRead), h.rumResults)
	// GH #174 — deterministic operator recovery for a beacon key stuck empty on
	// the agent (the one best-effort mint+push on first RUM-enable was lost).
	// Same permission gate as putConfig — this is a perf-config-class mutation.
	g.POST("/perf/rum/rotate-key", authz.RequirePermission(authz.PermSitePerfConfig), h.rotateRumBeaconKey)

	// #190 — Media Cleaner tool.
	// Scan is read-only (PermMediaCleanScan = viewer+); isolate/restore are
	// reversible mutations (PermMediaCleanWrite = operator+); permanent delete
	// requires PermMediaCleanWrite at route level and PermMediaCleanDelete in
	// the handler body (admin+), mirroring the dbTableAction drop/empty pattern.
	g.GET("/media/clean/scan", authz.RequirePermission(authz.PermMediaCleanScan), h.mediaCleanScan)
	g.GET("/media/clean/quarantine", authz.RequirePermission(authz.PermMediaCleanScan), h.mediaCleanQuarantine)
	g.POST("/media/clean/isolate", authz.RequirePermission(authz.PermMediaCleanWrite), h.mediaCleanIsolate)
	g.POST("/media/clean/restore", authz.RequirePermission(authz.PermMediaCleanWrite), h.mediaCleanRestore)
	g.POST("/media/clean/delete", authz.RequirePermission(authz.PermMediaCleanWrite), h.mediaCleanDelete)

	// Portfolio bulk routes. RequireSiteAccess is enforced PER-SITE inside the
	// handlers (each site_id is checked against the principal's allowlist) since
	// the route param is a body array, not a path :siteId.
	r.POST("/cache/bulk-purge", authz.RequirePermission(authz.PermSiteCachePurge), h.bulkPurge)
	r.PUT("/cache/bulk-config", authz.RequirePermission(authz.PermSitePerfConfig), h.bulkConfig)

	// P3.7 — tenant-level (no :siteId) fleet DB health aggregate.
	// RequireOrgScope blocks site-scoped collaborators; PermSiteRead is the
	// minimum read permission (viewer+), matching the sites-list and update-list
	// portfolio endpoints.
	r.GET("/perf/db/fleet-health",
		authz.RequireOrgScope(),
		authz.RequirePermission(authz.PermSiteRead),
		h.getFleetDbHealth,
	)
	// Fleet RUM aggregate (cross-site Core Web Vitals).
	// RequireOrgScope blocks site-scoped collaborators (fleet aggregation is
	// tenant-level; site-scoped principals use the per-site /perf/rum/summary).
	r.GET("/perf/rum/fleet",
		authz.RequireOrgScope(),
		authz.RequirePermission(authz.PermSiteRead),
		h.rumFleet,
	)
}

// ---------------------------------------------------------------------------
// per-site config
// ---------------------------------------------------------------------------

func (h *Handler) getConfig(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	cfg, err := h.svc.GetConfig(c.Request.Context(), p.TenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toConfigDTO(cfg))
}

func (h *Handler) putConfig(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	var body perfConfigDTO
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	cfg := fromConfigDTO(body, p.TenantID, siteID)
	in := UpdateConfigInput{Config: cfg}
	if body.CDNCredentials != nil {
		in.CDNCredentialsRaw = &CDNCredentials{
			Provider: body.CDNProvider,
			APIToken: body.CDNCredentials.APIToken,
			ZoneID:   body.CDNCredentials.ZoneID,
			Zone:     body.CDNCredentials.Zone,
		}
	}
	saved, err := h.svc.UpdateConfig(c.Request.Context(), p.TenantID, siteID, in)
	if err != nil {
		if _, isDomain := domain.AsDomain(err); isDomain {
			httpx.Error(c, err)
			return
		}
		// Non-domain = agent push failure after a successful store. Return 200 with
		// the stored config; surface the warning in a header (mirrors security).
		c.Header("X-Agent-Push-Warning", err.Error())
		c.JSON(http.StatusOK, toConfigDTO(saved))
		return
	}
	h.record(c, p, audit.ActionPerfConfigUpdated, siteID, map[string]any{
		"config_version": saved.ConfigVersion,
		"cache_enabled":  saved.CacheEnabled,
	})
	c.JSON(http.StatusOK, toConfigDTO(saved))
}

// ---------------------------------------------------------------------------
// cache stats / purge / preload / enable / disable
// ---------------------------------------------------------------------------

func (h *Handler) getCacheStats(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	stats, err := h.svc.GetCacheStats(c.Request.Context(), p.TenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toCacheStatsDTO(stats))
}

func (h *Handler) purge(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	var body purgeBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	scope := PurgeKind(body.Scope)
	deleteEverything := body.Scope == "all" && body.DeleteEverything

	// The destructive "delete everything" flavour requires the higher admin-gated
	// permission. We re-check it here rather than on the route because the route
	// allows the normal purge permission.
	if deleteEverything {
		if !h.allows(c, authz.PermSiteCacheDeleteAll) {
			httpx.Error(c, domain.Forbidden("insufficient_permission", "delete-everything requires the cache delete-all permission"))
			return
		}
	}

	in := PurgeInput{
		Scope:            scope,
		URLs:             body.URLs,
		InitiatorID:      p.GetUserID(),
		DeleteEverything: deleteEverything,
	}
	if body.URL != "" {
		in.URLs = append(in.URLs, body.URL)
	}
	entry, detail, err := h.svc.Purge(c.Request.Context(), p.TenantID, siteID, in)
	if err != nil {
		if _, isDomain := domain.AsDomain(err); isDomain {
			httpx.Error(c, err)
			return
		}
		// Agent rejection: surface as 200 ok=false (mirrors security.unblockIP).
		c.JSON(http.StatusOK, gin.H{"ok": false, "detail": err.Error()})
		return
	}
	action := audit.ActionCachePurged
	if deleteEverything {
		action = audit.ActionCacheDeleteEverything
	}
	h.record(c, p, action, siteID, map[string]any{
		"kind":       string(scope),
		"urls_count": entry.URLsCount,
	})
	c.JSON(http.StatusOK, gin.H{"ok": true, "detail": detail, "purge_id": entry.ID.String()})
}

func (h *Handler) preload(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	detail, err := h.svc.Preload(c.Request.Context(), p.TenantID, siteID, p.GetUserID())
	if err != nil {
		if _, isDomain := domain.AsDomain(err); isDomain {
			httpx.Error(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": false, "detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "detail": detail})
}

func (h *Handler) enable(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	detail, err := h.svc.EnableCache(c.Request.Context(), p.TenantID, siteID)
	if err != nil {
		if _, isDomain := domain.AsDomain(err); isDomain {
			httpx.Error(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": false, "detail": err.Error()})
		return
	}
	h.record(c, p, audit.ActionCacheEnabled, siteID, nil)
	c.JSON(http.StatusOK, gin.H{"ok": true, "detail": detail})
}

func (h *Handler) disable(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	detail, err := h.svc.DisableCache(c.Request.Context(), p.TenantID, siteID)
	if err != nil {
		if _, isDomain := domain.AsDomain(err); isDomain {
			httpx.Error(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": false, "detail": err.Error()})
		return
	}
	h.record(c, p, audit.ActionCacheDisabled, siteID, nil)
	c.JSON(http.StatusOK, gin.H{"ok": true, "detail": detail})
}

func (h *Handler) dbClean(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	jobID, err := h.svc.DBClean(c.Request.Context(), p.TenantID, siteID, h.cpBaseURL)
	if err != nil {
		if _, isDomain := domain.AsDomain(err); isDomain {
			httpx.Error(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": false, "detail": err.Error()})
		return
	}
	h.record(c, p, audit.ActionDbCleaned, siteID, map[string]any{"job_id": jobID})
	c.JSON(http.StatusOK, gin.H{"ok": true, "job_id": jobID})
}

// ---------------------------------------------------------------------------
// db clean GET (M71) — pull-truth endpoint
// ---------------------------------------------------------------------------

// getDbClean returns the watchdog state and the latest stored db_clean result
// for a site (M71). It mirrors getDbScan exactly:
//
//   - clean_active / active_job_id / active_started_at come from the watchdog
//     columns on site_perf_config (stamped by DBClean / cleared by HandleDBCleanProgress).
//   - last_result is the most-recently persisted clean result, or null when no
//     clean has completed yet.
//
// JSON field names (web builds against these; do not rename):
//
//	clean_active       bool
//	active_job_id      string | null
//	active_started_at  RFC3339 | null
//	last_result        object | null
//	  job_id           string
//	  rows_deleted     number
//	  bytes_freed      number
//	  result           object  (per-category {rows_deleted,bytes_freed,state} map)
//	  cleaned_at       RFC3339
func (h *Handler) getDbClean(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	result, err := h.svc.GetLatestClean(c.Request.Context(), p.TenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	// Read active-clean watchdog state so the web can show/hide the spinner on
	// page load without relying solely on SSE delivery. ErrNotFound (no perf
	// config row yet) is treated as "not active" — not an error.
	activeState, stateErr := h.svc.GetActiveDBCleanState(c.Request.Context(), p.TenantID, siteID)
	if stateErr != nil && stateErr != ErrNotFound {
		httpx.Error(c, stateErr)
		return
	}

	var activeJobID any     // null when not active
	var activeStartedAt any // null when not active
	if activeState.Active {
		activeJobID = activeState.JobID
		if !activeState.StartedAt.IsZero() {
			activeStartedAt = activeState.StartedAt.UTC().Format(time.RFC3339)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"clean_active":      activeState.Active,
		"active_job_id":     activeJobID,
		"active_started_at": activeStartedAt,
		"last_result":       dbCleanResultToGinH(result),
	})
}

// dbCleanResultToGinH converts a DBCleanResult into a gin.H map for the wire
// response. Returns nil when result is nil (no clean run yet — serialises as
// JSON null). Shared by getDbClean.
func dbCleanResultToGinH(r *DBCleanResult) gin.H {
	if r == nil {
		return nil
	}
	var resultMap any = map[string]any{}
	if len(r.ResultJSON) > 0 {
		var m map[string]any
		if jerr := json.Unmarshal(r.ResultJSON, &m); jerr == nil {
			resultMap = m
		}
	}
	return gin.H{
		"job_id":       r.JobID,
		"rows_deleted": r.RowsDeleted,
		"bytes_freed":  r.BytesFreed,
		"result":       resultMap,
		"cleaned_at":   r.CleanedAt.UTC().Format(time.RFC3339),
	}
}

// ---------------------------------------------------------------------------
// db scan (M39 Phase 2)
// ---------------------------------------------------------------------------

// dbScanBody is the optional request body for POST /perf/db/scan. An empty
// categories list means scan all 14 categories.
type dbScanBody struct {
	Categories []string `json:"categories"`
}

// dbScan triggers a synchronous read-only database scan. The agent returns the
// full per-category result in the ACK body; the CP stores it, emits SSE, and
// returns the job_id plus the full result payload in the 200 body. SSE is a
// hint only — the ACK body is the truth so the UI never hangs on a missed event.
func (h *Handler) dbScan(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	var body dbScanBody
	// Body is optional — an absent body means scan all categories.
	if c.Request.ContentLength != 0 {
		if err := bindJSON(c, &body); err != nil {
			httpx.Error(c, err)
			return
		}
	}

	jobID, result, err := h.svc.DBScan(c.Request.Context(), p.TenantID, siteID, body.Categories)
	if err != nil {
		if _, isDomain := domain.AsDomain(err); isDomain {
			httpx.Error(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": false, "job_id": jobID, "detail": err.Error()})
		return
	}
	// Embed the full scan result in the ACK so the browser can render
	// immediately even if the SSE db.scan.completed frame was missed.
	c.JSON(http.StatusOK, gin.H{
		"ok":     true,
		"job_id": jobID,
		"result": dbScanResultToGinH(result),
	})
}

// getDbScan returns the latest stored db_scan result for a site.
// Returns null result when no scan has been run yet.
// Phase 2.1: the response includes both `categories` and `tables` so the web
// layer can render the Tables tab on page reload (hydration path) without
// waiting for an SSE db.scan.completed event.
// The response also includes scan_active/active_job_id/active_started_at so
// the web can derive its "Scanning..." spinner state on page load.
func (h *Handler) getDbScan(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	result, err := h.svc.GetLatestScan(c.Request.Context(), p.TenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	// Read active-scan watchdog state so the web can show/hide the spinner on
	// page load without relying solely on SSE delivery. ErrNotFound (no perf
	// config row yet) is treated as "not active" — not an error.
	activeState, stateErr := h.svc.GetActiveDBScanState(c.Request.Context(), p.TenantID, siteID)
	if stateErr != nil && stateErr != ErrNotFound {
		httpx.Error(c, stateErr)
		return
	}

	var activeJobID any     // null when not active
	var activeStartedAt any // null when not active
	if activeState.Active {
		activeJobID = activeState.JobID
		if !activeState.StartedAt.IsZero() {
			activeStartedAt = activeState.StartedAt.UTC().Format(time.RFC3339)
		}
	}

	if result == nil {
		c.JSON(http.StatusOK, gin.H{
			"result":            nil,
			"scan_active":       activeState.Active,
			"active_job_id":     activeJobID,
			"active_started_at": activeStartedAt,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"result":            dbScanResultToGinH(result),
		"scan_active":       activeState.Active,
		"active_job_id":     activeJobID,
		"active_started_at": activeStartedAt,
	})
}

// dbScanResultToGinH converts a DBScanResult (with raw JSONB blobs) into a
// gin.H map with clean JSON values for the wire response. It is shared by both
// the POST ACK (dbScan) and the GET response (getDbScan) so the JSON key names
// are identical on both paths.
func dbScanResultToGinH(r *DBScanResult) gin.H {
	if r == nil {
		return nil
	}
	// Unmarshal JSONB blobs back to typed values. Fall through to zero/empty
	// values on parse error so the response is always well-formed.
	var categories any
	if len(r.CategoriesJSON) > 0 {
		var m map[string]any
		if jerr := json.Unmarshal(r.CategoriesJSON, &m); jerr == nil {
			categories = m
		}
	}
	var tables any = []any{}
	if len(r.TablesJSON) > 0 {
		var arr []any
		if jerr := json.Unmarshal(r.TablesJSON, &arr); jerr == nil {
			tables = arr
		}
	}
	var orphanedOptions any = []any{}
	if len(r.OrphanedOptionsJSON) > 0 {
		var arr []any
		if jerr := json.Unmarshal(r.OrphanedOptionsJSON, &arr); jerr == nil {
			orphanedOptions = arr
		}
	}
	var orphanedCron any = []any{}
	if len(r.OrphanedCronJSON) > 0 {
		var arr []any
		if jerr := json.Unmarshal(r.OrphanedCronJSON, &arr); jerr == nil {
			orphanedCron = arr
		}
	}
	var installedPlugins any = []any{}
	if len(r.InstalledPluginsJSON) > 0 {
		var arr []any
		if jerr := json.Unmarshal(r.InstalledPluginsJSON, &arr); jerr == nil {
			installedPlugins = arr
		}
	}
	h := gin.H{
		"job_id":            r.JobID,
		"categories":        categories,
		"tables":            tables,
		"orphaned_options":  orphanedOptions,
		"orphaned_cron":     orphanedCron,
		"installed_plugins": installedPlugins,
		"db_size_bytes":     r.DBSizeBytes,
		"table_count":       r.TableCount,
		"scanned_at":        r.ScannedAt.Unix(),
	}
	// created_at is only populated from the DB path (GET); in the POST ACK path
	// CreatedAt is zero and we omit it to avoid a confusing epoch timestamp.
	if !r.CreatedAt.IsZero() {
		h["created_at"] = r.CreatedAt.Unix()
	}
	return h
}

// ---------------------------------------------------------------------------
// db health / size trend (M42 Phase 3.4)
// ---------------------------------------------------------------------------

// getDBHealth returns the 90-day DB-size trend and growth summary for a site.
// The `days` query parameter adjusts the lookback window (clamped to [7,365]).
func (h *Handler) getDBHealth(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	days := 90
	if s := c.Query("days"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			if n < 7 {
				n = 7
			} else if n > 365 {
				n = 365
			}
			days = n
		}
	}
	resp, err := h.svc.GetDBHealth(c.Request.Context(), p.TenantID, siteID, days)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// M52 / #162 — cache hit-ratio health trend
// ---------------------------------------------------------------------------

// getCacheHealth returns the 90-day cache hit-ratio trend and average for a
// site. The `days` query parameter adjusts the lookback window (clamped to
// [7,365], default 90).
func (h *Handler) getCacheHealth(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	days := 90
	if s := c.Query("days"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			if n < 7 {
				n = 7
			} else if n > 365 {
				n = 365
			}
			days = n
		}
	}
	resp, err := h.svc.GetCacheHealth(c.Request.Context(), p.TenantID, siteID, days)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// P3.7 — fleet / portfolio DB health aggregate (tenant-level, no :siteId)
// ---------------------------------------------------------------------------

// getFleetDbHealth returns the tenant-level aggregate of database health across
// all sites that have at least one completed scan. The `days` query parameter
// controls the growth lookback window (clamped to [7,365], default 90). This
// endpoint has no :siteId — it always aggregates the entire tenant.
func (h *Handler) getFleetDbHealth(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	days := 90
	if s := c.Query("days"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			if n < 7 {
				n = 7
			} else if n > 365 {
				n = 365
			}
			days = n
		}
	}
	resp, err := h.svc.GetFleetDbHealth(c.Request.Context(), p.TenantID, days)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// P3.5 — orphans classification report (read-only)
// ---------------------------------------------------------------------------

// getOrphansReport classifies the orphaned artefacts stored in the latest
// db_scan result and returns the structured report.  The classification runs
// on-demand against the live corpus so the report is always fresh relative to
// the current corpus version.  No destructive operation is performed; there is
// no delete path on this endpoint.
func (h *Handler) getOrphansReport(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	if h.corpus == nil {
		httpx.Error(c, domain.ServiceUnavailable("corpus_unwired", "corpus reader not configured"))
		return
	}
	report, err := h.svc.GetOrphansReport(c.Request.Context(), h.corpus, p.TenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, report)
}

// ---------------------------------------------------------------------------
// db table action (Phase 2.2)
// ---------------------------------------------------------------------------

// dbTableActionBody is the request body for POST /perf/db/table-action.
type dbTableActionBody struct {
	Action  string   `json:"action"`
	Tables  []string `json:"tables"`
	Confirm string   `json:"confirm,omitempty"`
}

// dbTableAction dispatches a per-table DDL operation
// (optimize/repair/drop/empty/analyze/convert_innodb) to the site's agent.
// Destructive actions (drop/empty) require the higher PermSiteCacheDeleteAll
// permission AND a type-to-confirm token in the request body. Non-destructive
// actions (optimize/repair/analyze/convert_innodb) require only
// PermSiteCacheManage. The backup-warning advisory is surfaced as an
// X-Backup-Warning header (non-blocking) when no recent backup is found.
func (h *Handler) dbTableAction(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	var body dbTableActionBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}

	// Destructive actions (drop/empty) require the admin-level permission.
	isDestructive := body.Action == "drop" || body.Action == "empty"
	if isDestructive {
		if !h.allows(c, authz.PermSiteCacheDeleteAll) {
			httpx.Error(c, domain.Forbidden("insufficient_permission",
				"drop and empty table actions require the cache delete-all permission (admin+)"))
			return
		}
	}

	out, err := h.svc.DBTableAction(c.Request.Context(), p.TenantID, siteID, DBTableActionInput{
		Action:  body.Action,
		Tables:  body.Tables,
		Confirm: body.Confirm,
	})
	if err != nil {
		if _, isDomain := domain.AsDomain(err); isDomain {
			httpx.Error(c, err)
			return
		}
		// Agent rejection: surface as 200 ok=false (mirrors security/purge pattern).
		c.JSON(http.StatusOK, gin.H{"ok": false, "detail": err.Error()})
		return
	}

	if out.BackupWarning != "" {
		c.Header("X-Backup-Warning", out.BackupWarning)
	}

	h.record(c, p, audit.ActionDbTableAction, siteID, map[string]any{
		"job_id":      out.JobID,
		"action":      body.Action,
		"table_count": len(body.Tables),
	})

	c.JSON(http.StatusOK, gin.H{
		"ok":             true,
		"job_id":         out.JobID,
		"action":         body.Action,
		"results":        out.Results,
		"backup_warning": out.BackupWarning,
	})
}

// ---------------------------------------------------------------------------
// P3.8 — orphan delete (destructive)
// ---------------------------------------------------------------------------

// orphanDeleteBody is the JSON body for POST /perf/db/orphan-delete.
type orphanDeleteBody struct {
	// Items is the set of orphan identifiers the operator wants deleted. Each
	// item carries kind ("option"|"cron"|"table"), name, and owner_slug as
	// reported by the P3.5 orphans endpoint. The CP re-classifies before signing;
	// any item that is no longer DeletableEligible or whose owner_slug drifted is
	// silently dropped from the signed command.
	Items []orphanDeleteBodyItem `json:"items"`
	// Confirm is the type-to-confirm token. See orphanDeleteExpectedConfirm for
	// the exact format spec.
	Confirm string `json:"confirm"`
}

// orphanDeleteBodyItem is one item in the orphanDeleteBody.
type orphanDeleteBodyItem struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	OwnerSlug string `json:"owner_slug"`
}

// dbOrphanDelete is the P3.8 destructive orphan-deletion handler.
//
// Permission model:
//   - Route-level gate: PermSiteCacheManage (operator+) — applied by Register.
//   - Handler-body gate: PermSiteCacheDeleteAll (admin+) — checked below before
//     re-classify, consistent with dbTableAction drop/empty.
//
// On success it returns {ok, job_id, accepted, dropped} plus an advisory
// X-Backup-Warning header when no recent backup is found.
func (h *Handler) dbOrphanDelete(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	// Inner admin-level gate (mirrors dbTableAction drop/empty pattern).
	if !h.allows(c, authz.PermSiteCacheDeleteAll) {
		httpx.Error(c, domain.Forbidden("insufficient_permission",
			"orphan deletion requires the cache delete-all permission (admin+)"))
		return
	}

	if h.corpus == nil {
		httpx.Error(c, domain.ServiceUnavailable("corpus_unwired", "corpus reader not configured"))
		return
	}

	var body orphanDeleteBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}

	// Convert the handler-layer DTO to the service-layer input.
	items := make([]OrphanDeleteRequestItem, 0, len(body.Items))
	for _, it := range body.Items {
		items = append(items, OrphanDeleteRequestItem{
			Kind:      it.Kind,
			Name:      it.Name,
			OwnerSlug: it.OwnerSlug,
		})
	}

	out, err := h.svc.DBOrphanDelete(c.Request.Context(), h.corpus, p.TenantID, siteID, h.cpBaseURL, OrphanDeleteInput{
		Items:   items,
		Confirm: body.Confirm,
	})
	if err != nil {
		if _, isDomain := domain.AsDomain(err); isDomain {
			httpx.Error(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": false, "detail": err.Error()})
		return
	}

	if out.BackupWarning != "" {
		c.Header("X-Backup-Warning", out.BackupWarning)
	}

	// Tally per-kind counts from the input items (for audit metadata).
	kindCounts := map[string]int{}
	for _, it := range body.Items {
		kindCounts[it.Kind]++
	}

	h.record(c, p, audit.ActionDbOrphanDelete, siteID, map[string]any{
		"job_id":     out.JobID,
		"accepted":   out.AcceptedCount,
		"dropped":    out.DroppedCount,
		"item_kinds": kindCounts,
	})

	c.JSON(http.StatusOK, gin.H{
		"ok":             true,
		"job_id":         out.JobID,
		"accepted_count": out.AcceptedCount,
		"dropped_count":  out.DroppedCount,
		"backup_warning": out.BackupWarning,
	})
}

// ---------------------------------------------------------------------------
// rucss results
// ---------------------------------------------------------------------------

func (h *Handler) rucssResults(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	limit, offset := pageParams(c)
	if h.rucss == nil {
		c.JSON(http.StatusOK, gin.H{"items": []RucssResultDTO{}})
		return
	}
	items, err := h.rucss.List(c.Request.Context(), p.TenantID, siteID, limit, offset)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	if items == nil {
		items = []RucssResultDTO{}
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *Handler) rucssClear(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	if h.rucss == nil || h.rucss.Clear == nil {
		c.JSON(http.StatusOK, gin.H{"ok": true, "cleared": 0})
		return
	}
	cleared, err := h.rucss.Clear(c.Request.Context(), p.TenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	// A destructive, tenant-scoped delete belongs in the audit trail (mirrors the
	// sibling rucssCompute record). cleared is the actual rows-deleted count.
	h.record(c, p, audit.ActionPerfConfigUpdated, siteID, map[string]any{"rucss_clear": true, "cleared": cleared})
	c.JSON(http.StatusOK, gin.H{"ok": true, "cleared": cleared})
}

// rucssComputeBody is the optional request body for POST /perf/rucss/compute. An
// empty body computes the home page.
type rucssComputeBody struct {
	URLs []string `json:"urls,omitempty"`
}

func (h *Handler) rucssCompute(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	var body rucssComputeBody
	if c.Request.ContentLength > 0 {
		if err := bindJSON(c, &body); err != nil {
			httpx.Error(c, err)
			return
		}
	}
	detail, err := h.svc.ComputeRucss(c.Request.Context(), p.TenantID, siteID, body.URLs)
	if err != nil {
		if _, isDomain := domain.AsDomain(err); isDomain {
			httpx.Error(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": false, "detail": err.Error()})
		return
	}
	h.record(c, p, audit.ActionPerfConfigUpdated, siteID, map[string]any{"rucss_compute": true})
	c.JSON(http.StatusOK, gin.H{"ok": true, "detail": detail})
}

// ---------------------------------------------------------------------------
// portfolio bulk
// ---------------------------------------------------------------------------

func (h *Handler) bulkPurge(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	var body bulkPurgeBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	results := make([]bulkResultDTO, 0, len(body.SiteIDs))
	for _, raw := range body.SiteIDs {
		siteID, perr := uuid.Parse(raw)
		if perr != nil {
			results = append(results, bulkResultDTO{SiteID: raw, OK: false, Detail: "invalid site id"})
			continue
		}
		if !p.CanAccessSite(siteID) {
			results = append(results, bulkResultDTO{SiteID: raw, OK: false, Detail: "forbidden"})
			continue
		}
		_, detail, err := h.svc.Purge(c.Request.Context(), p.TenantID, siteID, PurgeInput{
			Scope:       PurgeKindAll,
			InitiatorID: p.GetUserID(),
		})
		if err != nil {
			results = append(results, bulkResultDTO{SiteID: raw, OK: false, Detail: err.Error()})
			continue
		}
		h.record(c, p, audit.ActionCachePurged, siteID, map[string]any{"kind": "all", "bulk": true})
		results = append(results, bulkResultDTO{SiteID: raw, OK: true, Detail: detail})
	}
	c.JSON(http.StatusOK, gin.H{"results": results})
}

func (h *Handler) bulkConfig(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	var body bulkConfigBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	preset, ok := presetConfig(body.Preset)
	if !ok {
		httpx.Error(c, domain.Validation("invalid_preset", "preset must be one of: balanced, aggressive, safe"))
		return
	}
	results := make([]bulkResultDTO, 0, len(body.SiteIDs))
	for _, raw := range body.SiteIDs {
		siteID, perr := uuid.Parse(raw)
		if perr != nil {
			results = append(results, bulkResultDTO{SiteID: raw, OK: false, Detail: "invalid site id"})
			continue
		}
		if !p.CanAccessSite(siteID) {
			results = append(results, bulkResultDTO{SiteID: raw, OK: false, Detail: "forbidden"})
			continue
		}
		// Merge the preset toggles onto the site's existing config so the bulk
		// apply does not clobber per-site CDN/cache include lists.
		cur, gerr := h.svc.GetConfig(c.Request.Context(), p.TenantID, siteID)
		if gerr != nil {
			results = append(results, bulkResultDTO{SiteID: raw, OK: false, Detail: gerr.Error()})
			continue
		}
		applyPreset(&cur, preset)
		saved, err := h.svc.UpdateConfig(c.Request.Context(), p.TenantID, siteID, UpdateConfigInput{Config: cur})
		if err != nil {
			if _, isDomain := domain.AsDomain(err); isDomain {
				results = append(results, bulkResultDTO{SiteID: raw, OK: false, Detail: err.Error()})
				continue
			}
			// Agent push warning is non-fatal: config is stored.
			h.record(c, p, audit.ActionPerfConfigUpdated, siteID, map[string]any{"preset": body.Preset, "bulk": true})
			results = append(results, bulkResultDTO{SiteID: raw, OK: true, Detail: "stored; agent push warning: " + err.Error()})
			continue
		}
		h.record(c, p, audit.ActionPerfConfigUpdated, siteID, map[string]any{"preset": body.Preset, "bulk": true})
		results = append(results, bulkResultDTO{SiteID: raw, OK: true, Detail: "applied", ConfigVersion: saved.ConfigVersion})
	}
	c.JSON(http.StatusOK, gin.H{"results": results})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func parseSiteID(c *gin.Context) (uuid.UUID, bool) {
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return uuid.Nil, false
	}
	return siteID, true
}

func bindJSON(c *gin.Context, dst any) error {
	dec := json.NewDecoder(io.LimitReader(c.Request.Body, 1<<20)) // 1 MiB config/body cap
	if err := dec.Decode(dst); err != nil {
		return domain.Validation("invalid_body", "request body is not valid JSON: "+err.Error())
	}
	return nil
}

func pageParams(c *gin.Context) (limit, offset int32) {
	limit = 50
	if s := c.Query("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && n <= 500 {
			limit = int32(n)
		}
	}
	if s := c.Query("offset"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			offset = int32(n)
		}
	}
	return limit, offset
}

// ---------------------------------------------------------------------------
// #188 — search-replace tool (dry-run + live)
// ---------------------------------------------------------------------------

// dbSearchReplaceBody is the JSON body for POST /perf/db/search-replace.
type dbSearchReplaceBody struct {
	// Search is the exact string to find. Minimum 3 bytes (server enforces).
	Search string `json:"search"`
	// Replace is the replacement string. May be empty (removes occurrences).
	Replace string `json:"replace"`
	// DryRun when true scans and counts without writing. The UI MUST call with
	// dry_run=true first so the operator sees the preview before confirming.
	DryRun bool `json:"dry_run"`
	// Tables is an optional allowlist of full table names (including prefix,
	// e.g. "wp_options"). When absent all eligible tables are scanned.
	Tables []string `json:"tables,omitempty"`
}

// dbSearchReplace dispatches the serialization-safe search_replace command.
// dry_run=true returns counts without writing; dry_run=false applies and
// returns the actual rows_changed. An advisory X-Backup-Warning header is
// emitted when no recent backup is found and dry_run=false.
func (h *Handler) dbSearchReplace(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	var body dbSearchReplaceBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}

	out, err := h.svc.SearchReplace(c.Request.Context(), p.TenantID, siteID, SearchReplaceInput{
		Search:  body.Search,
		Replace: body.Replace,
		DryRun:  body.DryRun,
		Tables:  body.Tables,
	})
	if err != nil {
		if _, isDomain := domain.AsDomain(err); isDomain {
			httpx.Error(c, err)
			return
		}
		// Agent rejection: surface as 200 ok=false (mirrors dbTableAction pattern).
		c.JSON(http.StatusOK, gin.H{"ok": false, "detail": err.Error()})
		return
	}

	if out.BackupWarning != "" {
		c.Header("X-Backup-Warning", out.BackupWarning)
	}

	h.record(c, p, audit.ActionDbSearchReplace, siteID, map[string]any{
		"job_id":         out.JobID,
		"search_len":     len(body.Search), // length only — do not log the actual value
		"dry_run":        body.DryRun,
		"tables_scanned": out.TablesScanned,
		"rows_matched":   out.RowsMatched,
		"rows_changed":   out.RowsChanged,
	})

	c.JSON(http.StatusOK, gin.H{
		"ok":             true,
		"job_id":         out.JobID,
		"dry_run":        body.DryRun,
		"tables_scanned": out.TablesScanned,
		"rows_matched":   out.RowsMatched,
		"rows_changed":   out.RowsChanged,
		"backup_warning": out.BackupWarning,
	})
}

// ---------------------------------------------------------------------------
// #189 — db snapshot handlers (create / list / revert / delete)
// ---------------------------------------------------------------------------

// dbSnapshotCreateBody is the JSON body for POST /perf/db/snapshots.
type dbSnapshotCreateBody struct {
	// Label is an optional human-readable label for the snapshot (max 120 chars).
	Label string `json:"label,omitempty"`
	// Retention is the max number of snapshots to keep after this one (1-20, default 5).
	Retention int `json:"retention,omitempty"`
}

// dbSnapshotRevertBody is the JSON body for POST /perf/db/snapshots/:snapshotId/revert.
type dbSnapshotRevertBody struct {
	// Confirm MUST be the exact string "REVERT". This is the destructive-action gate:
	// the agent enforces it independently via hash_equals.
	Confirm string `json:"confirm"`
	// SkipSafetySnapshot when true suppresses the automatic pre-revert safety snapshot.
	SkipSafetySnapshot bool `json:"skip_safety_snapshot,omitempty"`
}

// dbSnapshotList handles GET /sites/:siteId/perf/db/snapshots.
// Returns the list of local snapshots for the site (read-only; PermSiteRead).
func (h *Handler) dbSnapshotList(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	out, err := h.svc.DbSnapshot(c.Request.Context(), p.TenantID, siteID, DbSnapshotInput{
		Action: "list",
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":        out.OK,
		"snapshots": out.Snapshots,
		"detail":    out.Detail,
	})
}

// dbSnapshotCreate handles POST /sites/:siteId/perf/db/snapshots.
// Takes a new local database snapshot (PermSiteWrite).
func (h *Handler) dbSnapshotCreate(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	var body dbSnapshotCreateBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	out, err := h.svc.DbSnapshot(c.Request.Context(), p.TenantID, siteID, DbSnapshotInput{
		Action:    "create",
		Label:     body.Label,
		Retention: body.Retention,
	})
	if err != nil {
		if _, isDomain := domain.AsDomain(err); isDomain {
			httpx.Error(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": false, "detail": err.Error()})
		return
	}
	h.record(c, p, audit.ActionDbSnapshot, siteID, map[string]any{
		"action":      "create",
		"snapshot_id": out.SnapshotID,
		"label":       body.Label,
	})
	c.JSON(http.StatusOK, gin.H{
		"ok":       out.OK,
		"snapshot": out.Snapshot,
		"detail":   out.Detail,
	})
}

// dbSnapshotRevert handles POST /sites/:siteId/perf/db/snapshots/:snapshotId/revert.
// Imports a snapshot SQL back into the live database (DESTRUCTIVE, PermSiteWrite).
// The operator MUST pass confirm="REVERT" in the body; the agent enforces this
// independently via hash_equals so a forged or mutated body cannot bypass the gate.
func (h *Handler) dbSnapshotRevert(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	snapshotID := c.Param("snapshotId")
	if snapshotID == "" {
		httpx.Error(c, domain.Validation("missing_snapshot_id", "snapshotId is required"))
		return
	}
	var body dbSnapshotRevertBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}
	out, err := h.svc.DbSnapshot(c.Request.Context(), p.TenantID, siteID, DbSnapshotInput{
		Action:             "revert",
		SnapshotID:         snapshotID,
		Confirm:            body.Confirm,
		SkipSafetySnapshot: body.SkipSafetySnapshot,
	})
	if err != nil {
		if _, isDomain := domain.AsDomain(err); isDomain {
			httpx.Error(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": false, "detail": err.Error()})
		return
	}
	h.record(c, p, audit.ActionDbSnapshot, siteID, map[string]any{
		"action":      "revert",
		"snapshot_id": snapshotID,
		"safety_id":   out.SafetyID,
	})
	c.JSON(http.StatusOK, gin.H{
		"ok":        out.OK,
		"detail":    out.Detail,
		"safety_id": out.SafetyID,
	})
}

// dbSnapshotDelete handles DELETE /sites/:siteId/perf/db/snapshots/:snapshotId.
// Removes a local snapshot from the WP server (PermSiteWrite).
func (h *Handler) dbSnapshotDelete(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}
	snapshotID := c.Param("snapshotId")
	if snapshotID == "" {
		httpx.Error(c, domain.Validation("missing_snapshot_id", "snapshotId is required"))
		return
	}
	out, err := h.svc.DbSnapshot(c.Request.Context(), p.TenantID, siteID, DbSnapshotInput{
		Action:     "delete",
		SnapshotID: snapshotID,
	})
	if err != nil {
		if _, isDomain := domain.AsDomain(err); isDomain {
			httpx.Error(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": false, "detail": err.Error()})
		return
	}
	h.record(c, p, audit.ActionDbSnapshot, siteID, map[string]any{
		"action":      "delete",
		"snapshot_id": snapshotID,
	})
	c.JSON(http.StatusOK, gin.H{
		"ok":     out.OK,
		"detail": out.Detail,
	})
}

// ---------------------------------------------------------------------------
// #190 — Media Cleaner handlers (scan / isolate / restore / delete)
// ---------------------------------------------------------------------------

// mediaCleanScan handles GET /sites/:siteId/media/clean/scan.
// Returns a paginated page of unused attachment candidates (READ-ONLY).
// Query params: offset (zero-based, default 0), limit (1–500, default 100).
func (h *Handler) mediaCleanScan(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	limit := 100
	if s := c.Query("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= 500 {
			limit = n
		}
	}
	offset := 0
	if s := c.Query("offset"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			offset = n
		}
	}

	out, err := h.svc.MediaCleanScan(c.Request.Context(), p.TenantID, siteID, MediaCleanScanInput{
		Offset: offset,
		Limit:  limit,
	})
	if err != nil {
		if _, isDomain := domain.AsDomain(err); isDomain {
			httpx.Error(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": false, "detail": err.Error()})
		return
	}

	h.record(c, p, audit.ActionMediaCleanScan, siteID, map[string]any{
		"candidate_count":   len(out.Candidates),
		"total":             out.Total,
		"has_more":          out.HasMore,
		"truncated":         out.Truncated,
		"total_attachments": out.TotalAttachments,
		"referenced_count":  out.ReferencedCount,
		"offset":            offset,
	})

	c.JSON(http.StatusOK, gin.H{
		"ok":                true,
		"total":             out.Total,
		"candidates":        out.Candidates,
		"has_more":          out.HasMore,
		"truncated":         out.Truncated,
		"total_attachments": out.TotalAttachments,
		"referenced_count":  out.ReferencedCount,
		"unused_count":      out.UnusedCount,
		"referenced":        out.Referenced,
	})
}

// mediaCleanQuarantine handles GET /sites/:siteId/media/clean/quarantine.
// Returns all quarantine manifests currently held on the site (READ-ONLY).
// The response is forwarded verbatim from the agent; field names are frozen.
func (h *Handler) mediaCleanQuarantine(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	out, err := h.svc.MediaCleanQuarantineList(c.Request.Context(), p.TenantID, siteID)
	if err != nil {
		if _, isDomain := domain.AsDomain(err); isDomain {
			httpx.Error(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": false, "detail": err.Error()})
		return
	}

	h.record(c, p, audit.ActionMediaCleanQuarantine, siteID, map[string]any{
		"manifest_count": len(out),
	})

	c.JSON(http.StatusOK, gin.H{
		"ok":        true,
		"manifests": out,
	})
}

// mediaCleanIsolateBody is the JSON body for POST /media/clean/isolate.
type mediaCleanIsolateBody struct {
	// JobID is a CP-minted UUID v4. Required; used by the agent for idempotency.
	JobID         string  `json:"job_id"`
	AttachmentIDs []int64 `json:"attachment_ids"`
}

// mediaCleanIsolate handles POST /sites/:siteId/media/clean/isolate.
// Moves the given attachments to the quarantine directory (REVERSIBLE).
// Emits X-Backup-Warning when no recent backup is found.
// Returns manifest_id which the client must store for restore/delete.
func (h *Handler) mediaCleanIsolate(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	var body mediaCleanIsolateBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}

	// Advisory backup warning — non-blocking, mirrors search-replace/db-table-action.
	if h.svc.backupChecker != nil {
		if hasBackup, bErr := h.svc.backupChecker.HasRecentBackup(c.Request.Context(), p.TenantID, siteID, 24*time.Hour*7); bErr == nil && !hasBackup {
			c.Header("X-Backup-Warning", "no backup found in the last 7 days; a backup before isolating media is strongly recommended")
		}
	}

	out, err := h.svc.MediaCleanIsolate(c.Request.Context(), p.TenantID, siteID, MediaCleanIsolateInput{
		JobID:         body.JobID,
		AttachmentIDs: body.AttachmentIDs,
	})
	if err != nil {
		if _, isDomain := domain.AsDomain(err); isDomain {
			httpx.Error(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": false, "detail": err.Error()})
		return
	}

	h.record(c, p, audit.ActionMediaCleanIsolate, siteID, map[string]any{
		"moved":            out.Moved,
		"manifest_id":      out.ManifestID,
		"entries_recorded": out.EntriesRecorded,
	})

	c.JSON(http.StatusOK, gin.H{
		"ok":               true,
		"job_id":           out.JobID,
		"moved":            out.Moved,
		"manifest_id":      out.ManifestID,
		"entries_recorded": out.EntriesRecorded,
		"per_attachment":   out.PerAttachment,
		"detail":           out.Detail,
	})
}

// mediaCleanRestoreBody is the JSON body for POST /media/clean/restore.
type mediaCleanRestoreBody struct {
	// JobID is a CP-minted UUID v4. Required.
	JobID string `json:"job_id"`
	// QuarantineIDs are the manifest IDs returned by prior isolate calls.
	QuarantineIDs []string `json:"quarantine_ids"`
}

// mediaCleanRestore handles POST /sites/:siteId/media/clean/restore.
// Reverses a prior isolate using the agent-side manifest records.
func (h *Handler) mediaCleanRestore(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	var body mediaCleanRestoreBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}

	out, err := h.svc.MediaCleanRestore(c.Request.Context(), p.TenantID, siteID, MediaCleanRestoreInput{
		JobID:         body.JobID,
		QuarantineIDs: body.QuarantineIDs,
	})
	if err != nil {
		if _, isDomain := domain.AsDomain(err); isDomain {
			httpx.Error(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": false, "detail": err.Error()})
		return
	}

	h.record(c, p, audit.ActionMediaCleanRestore, siteID, map[string]any{
		"restored": out.Restored,
	})

	c.JSON(http.StatusOK, gin.H{
		"ok":       true,
		"job_id":   out.JobID,
		"restored": out.Restored,
		"detail":   out.Detail,
	})
}

// mediaCleanDeleteBody is the JSON body for POST /media/clean/delete.
type mediaCleanDeleteBody struct {
	// JobID is a CP-minted UUID v4. Required.
	JobID string `json:"job_id"`
	// QuarantineIDs are the manifest IDs to permanently remove.
	QuarantineIDs []string `json:"quarantine_ids"`
	// Confirm MUST be the exact string "DELETE". The agent enforces this
	// independently via hash_equals. The CP validates here for belt-and-braces.
	Confirm string `json:"confirm"`
}

// mediaCleanDelete handles POST /sites/:siteId/media/clean/delete.
// PERMANENTLY removes quarantined attachments from the filesystem and
// force-deletes the attachment posts. IRREVERSIBLE.
// Permission model:
//   - Route-level gate: PermMediaCleanWrite (operator+) — applied by Register.
//   - Handler-body gate: PermMediaCleanDelete (admin+) — checked below.
//   - Confirm token: must equal "DELETE" (enforced on CP and agent).
func (h *Handler) mediaCleanDelete(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, ok := parseSiteID(c)
	if !ok {
		return
	}

	// Inner admin-level gate (mirrors dbTableAction drop/empty pattern).
	if !h.allows(c, authz.PermMediaCleanDelete) {
		httpx.Error(c, domain.Forbidden("insufficient_permission",
			"permanent media deletion requires admin or higher permission"))
		return
	}

	var body mediaCleanDeleteBody
	if err := bindJSON(c, &body); err != nil {
		httpx.Error(c, err)
		return
	}

	out, err := h.svc.MediaCleanDelete(c.Request.Context(), p.TenantID, siteID, MediaCleanDeleteInput{
		JobID:         body.JobID,
		QuarantineIDs: body.QuarantineIDs,
		Confirm:       body.Confirm,
	})
	if err != nil {
		if _, isDomain := domain.AsDomain(err); isDomain {
			httpx.Error(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": false, "detail": err.Error()})
		return
	}

	h.record(c, p, audit.ActionMediaCleanDelete, siteID, map[string]any{
		"deleted":           out.Deleted,
		"posts_deleted":     out.PostsDeleted,
		"posts_failed":      out.PostsFailed,
		"files_deleted":     out.FilesDeleted,
		"entries_processed": out.EntriesProcessed,
	})

	c.JSON(http.StatusOK, gin.H{
		"ok":                true,
		"job_id":            out.JobID,
		"deleted":           out.Deleted,
		"posts_deleted":     out.PostsDeleted,
		"posts_failed":      out.PostsFailed,
		"files_deleted":     out.FilesDeleted,
		"entries_processed": out.EntriesProcessed,
		"results":           out.Results,
		"detail":            out.Detail,
	})
}

func (h *Handler) allows(c *gin.Context, perm authz.Permission) bool {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		return false
	}
	return authz.Allows(authz.Role(p.Role), perm)
}

func (h *Handler) record(c *gin.Context, p domain.Principal, action string, siteID uuid.UUID, meta map[string]any) {
	if h.audit == nil {
		return
	}
	actorType := audit.ActorUser
	if p.Type == domain.PrincipalAPIKey {
		actorType = audit.ActorAPIKey
	}
	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   p.TenantID,
		ActorType:  actorType,
		ActorID:    p.ActorID(),
		Action:     action,
		TargetType: "site",
		TargetID:   siteID.String(),
		Metadata:   meta,
	})
}
