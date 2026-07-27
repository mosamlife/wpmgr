package vuln

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Handler serves the vulnerability-scanner routes.
//
// Per-site group (under /api/v1/sites/:siteId, gated by RequireSiteAccess):
//
//	GET  /vulnerabilities          — open findings for a site (severity-sorted)
//	POST /vulnerabilities/rescan   — enqueue a per-site rescan
//	POST /vulnerabilities/:id/dismiss — dismiss a finding (PermSecurityManage)
//	POST /vulnerabilities/:id/restore — restore a dismissed finding (PermSecurityManage)
//	POST /vulnerabilities/:id/remediate — trigger an update run for the fix
//
// Tenant fleet route (under /api/v1, tenant-scoped):
//
//	GET  /vulnerabilities          — cross-site counts + prioritized list
type Handler struct {
	svc    *Service
	enq    RescanEnqueuer
	audit  *audit.Recorder
}

// NewHandler builds the vulnerability handler.
func NewHandler(svc *Service, enq RescanEnqueuer, rec *audit.Recorder) *Handler {
	return &Handler{svc: svc, enq: enq, audit: rec}
}

// Register mounts the routes on the authenticated /api/v1 group.
func (h *Handler) Register(r *gin.RouterGroup) {
	// Fleet rollup (tenant-scoped, not per-site).
	// RequireOrgScope blocks site-scoped collaborators (fleet aggregation is
	// tenant-level; site-scoped principals use the per-site
	// /sites/:siteId/vulnerabilities endpoint instead, which RequireSiteAccess
	// already scopes to their allowed sites). This matches the perf fleet
	// convention (/perf/db/fleet-health, /perf/rum/fleet).
	r.GET("/vulnerabilities",
		authz.RequireOrgScope(),
		authz.RequirePermission(authz.PermSiteRead),
		h.fleetSummary,
	)

	// Per-site routes, gated by RequireSiteAccess.
	g := r.Group("/sites/:siteId/vulnerabilities", authz.RequireSiteAccess("siteId"))
	g.GET("", authz.RequirePermission(authz.PermSiteRead), h.listFindings)
	g.POST("/rescan", authz.RequirePermission(authz.PermSiteWrite), h.rescan)
	g.POST("/:id/dismiss", authz.RequirePermission(authz.PermSecurityManage), h.dismiss)
	g.POST("/:id/restore", authz.RequirePermission(authz.PermSecurityManage), h.restore)
	g.POST("/:id/remediate", authz.RequirePermission(authz.PermSiteWrite), h.remediate)
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

// The precise GET /api/v1/sites/:siteId/vulnerabilities response shape.
// Web builder contract (v0.52.x):
//
//	{
//	  "items": [ <findingDTO>, ... ],
//	  "attribution": {
//	    "defiant_notice":  "...",
//	    "defiant_license": "...",
//	    "mitre_notice":    "..."
//	  },
//	  "feed_ok":     true,
//	  "feed_synced": "2026-06-20T12:00:00Z",
//	  "enrichment_available": true,
//	  "last_enrichment_at":   "2026-06-20T12:00:00Z"
//	}
type siteVulnsResponseDTO struct {
	Items               []findingDTO   `json:"items"`
	Attribution         attributionDTO `json:"attribution"`
	FeedOK              bool           `json:"feed_ok"`
	FeedSynced          *string        `json:"feed_synced,omitempty"`
	EnrichmentAvailable bool           `json:"enrichment_available"`
	LastEnrichmentAt    *string        `json:"last_enrichment_at,omitempty"`
}

// attributionDTO carries the Wordfence Intelligence attribution notices.
// Defiant copyright/license is displayed in the UI footer; MITRE notice is
// displayed on any finding row that shows a CVE (Gate 0 DoD, §1).
type attributionDTO struct {
	DefiantNotice  string `json:"defiant_notice"`
	DefiantLicense string `json:"defiant_license"`
	MitreNotice    string `json:"mitre_notice"`
}

type findingDTO struct {
	ID               string   `json:"id"`
	SiteID           string   `json:"site_id"`
	VulnID           string   `json:"vuln_id"`
	Kind             string   `json:"kind"`
	Slug             string   `json:"slug"`
	Name             string   `json:"name"`
	InstalledVersion string   `json:"installed_version"`
	FixedVersion     *string  `json:"fixed_version,omitempty"`
	Severity         string   `json:"severity"`
	CVSSScore        *float64 `json:"cvss_score,omitempty"`
	CVE              *string  `json:"cve,omitempty"`
	CVELink          *string  `json:"cve_link,omitempty"`
	Title            string   `json:"title"`
	Status           string   `json:"status"`
	FirstSeen        string   `json:"first_seen"`
	LastSeen         string   `json:"last_seen"`
	References       []string `json:"references"`
	// WordfenceLink is the deterministic per-record Wordfence Intelligence
	// vulnerability-database URL (see wordfenceLink below), satisfying the
	// Defiant per-record attribution requirement independently of whatever
	// the feed's own references[] array happens to contain this run.
	WordfenceLink string `json:"wordfence_link"`
}

// The GET /api/v1/vulnerabilities fleet response shape:
//
//	{
//	  "total_open": 42,
//	  "critical":   3,
//	  "high":       8,
//	  "medium":     20,
//	  "low":        11,
//	  "unknown":    0,
//	  "items":      [ <fleetFindingDTO>, ... ],
//	  "attribution": { ... },
//	  "feed_ok":     true,
//	  "feed_synced": "2026-06-20T12:00:00Z",
//	  "enrichment_available": true,
//	  "last_enrichment_at":   "2026-06-20T12:00:00Z"
//	}
type fleetVulnsResponseDTO struct {
	TotalOpen           int               `json:"total_open"`
	Critical            int               `json:"critical"`
	High                int               `json:"high"`
	Medium              int               `json:"medium"`
	Low                 int               `json:"low"`
	Unknown             int               `json:"unknown"`
	Items               []fleetFindingDTO `json:"items"`
	Attribution         attributionDTO    `json:"attribution"`
	FeedOK              bool              `json:"feed_ok"`
	FeedSynced          *string           `json:"feed_synced,omitempty"`
	EnrichmentAvailable bool              `json:"enrichment_available"`
	LastEnrichmentAt    *string           `json:"last_enrichment_at,omitempty"`
}

type fleetFindingDTO struct {
	SiteID   string     `json:"site_id"`
	SiteName string     `json:"site_name"`
	SiteURL  string     `json:"site_url"`
	Finding  findingDTO `json:"finding"`
}

type rescanResponseDTO struct {
	OK bool `json:"ok"`
}

type remediateResponseDTO struct {
	RunID string `json:"run_id"`
}

// ---------------------------------------------------------------------------
// DTO mapping helpers
// ---------------------------------------------------------------------------

func toFindingDTO(f Finding) findingDTO {
	dto := findingDTO{
		ID:               f.ID.String(),
		SiteID:           f.SiteID.String(),
		VulnID:           f.VulnID,
		Kind:             f.Kind,
		Slug:             f.Slug,
		Name:             f.Name,
		InstalledVersion: f.InstalledVersion,
		Severity:         f.Severity,
		CVSSScore:        f.CVSSScore,
		Title:            f.Title,
		Status:           f.Status,
		FirstSeen:        f.FirstSeen.UTC().Format(time.RFC3339),
		LastSeen:         f.LastSeen.UTC().Format(time.RFC3339),
		References:       refsFromJSON(f.References),
		WordfenceLink:    wordfenceLink(f.VulnID),
	}
	if f.FixedVersion != "" {
		dto.FixedVersion = &f.FixedVersion
	}
	if f.CVE != "" {
		dto.CVE = &f.CVE
	}
	if f.CVELink != "" {
		dto.CVELink = &f.CVELink
	}
	return dto
}

func toAttributionDTO(meta FeedMeta) attributionDTO {
	return attributionDTO{
		DefiantNotice:  meta.DefiantNotice,
		DefiantLicense: meta.DefiantLicense,
		MitreNotice:    meta.MitreNotice,
	}
}

func feedSyncedStr(meta FeedMeta) *string {
	if meta.FetchedAt == nil {
		return nil
	}
	s := meta.FetchedAt.UTC().Format(time.RFC3339)
	return &s
}

// lastEnrichmentStr formats meta.LastEnrichmentAt for the wire, or nil when
// Production has never successfully enriched the feed yet.
func lastEnrichmentStr(meta FeedMeta) *string {
	if meta.LastEnrichmentAt == nil {
		return nil
	}
	s := meta.LastEnrichmentAt.UTC().Format(time.RFC3339)
	return &s
}

// refsFromJSON parses the raw JSONB references column into a string slice of
// URLs for the DTO.  Non-fatal: returns nil on parse error.
func refsFromJSON(raw []byte) []string {
	if len(raw) == 0 {
		return []string{}
	}
	// The references field may be an array of strings (URLs) or an array of
	// objects with a "url" key — handle both.
	var asStrings []string
	if json.Unmarshal(raw, &asStrings) == nil {
		return asStrings
	}
	var asObjects []struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(raw, &asObjects) == nil {
		urls := make([]string, 0, len(asObjects))
		for _, o := range asObjects {
			if o.URL != "" {
				urls = append(urls, o.URL)
			}
		}
		return urls
	}
	return []string{}
}

// wordfenceLink builds the deterministic per-record Wordfence Intelligence
// vulnerability-database URL for a finding.
//
// The Defiant per-record licence requires that any copy of a vulnerability
// record include a hyperlink to that specific record. Prior to this, WPMgr
// relied INDIRECTLY on the feed's own reference_urls[] array happening to
// contain a Wordfence link at some position, which is not guaranteed for
// every record/position, and empty for records the feed sends with no
// references at all. wordfenceLink instead derives the link deterministically
// from a stable feed-supplied field the finding always carries: vuln_id,
// which is the UUID key of the root JSON object in both the Scanner and
// Production Wordfence Intelligence feeds (see FeedRecord.VulnID / wfRecord
// in worker.go, the feed's own record identifier, not something WPMgr
// invents). That same identifier is what Wordfence's own public vulnerability
// database uses in its own per-record detail URL:
//
//	https://www.wordfence.com/threat-intel/vulnerabilities/id/<vuln_id>
//
// This is also the exact URL shape the Wordfence Intelligence feed itself
// commonly supplies as a reference for a record (see the realistic fixture
// data in apps/web/src/features/security/use-vuln.test.ts). Building it
// deterministically here, rather than reading references[0], guarantees
// every finding carries a valid, always-present per-record link-back
// regardless of what the feed's references array contains for that record.
//
// The vuln_id segment is percent-encoded (url.PathEscape) as defense in
// depth: it originates from external feed data and, while normally a plain
// UUID, is never assumed to be safe to concatenate unescaped into a URL path.
// Returns "" only when vulnID is empty (never expected in practice: vuln_id
// is a NOT NULL primary-key component of every finding and feed record).
func wordfenceLink(vulnID string) string {
	if vulnID == "" {
		return ""
	}
	return "https://www.wordfence.com/threat-intel/vulnerabilities/id/" + url.PathEscape(vulnID)
}

// ---------------------------------------------------------------------------
// Route handlers
// ---------------------------------------------------------------------------

func (h *Handler) listFindings(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	findings, meta, err := h.svc.GetSiteFindings(c.Request.Context(), p.TenantID, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]findingDTO, 0, len(findings))
	for _, f := range findings {
		items = append(items, toFindingDTO(f))
	}
	c.JSON(http.StatusOK, siteVulnsResponseDTO{
		Items:               items,
		Attribution:         toAttributionDTO(meta),
		FeedOK:              meta.OK,
		FeedSynced:          feedSyncedStr(meta),
		EnrichmentAvailable: meta.EnrichmentOK,
		LastEnrichmentAt:    lastEnrichmentStr(meta),
	})
}

func (h *Handler) rescan(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_site_id", "siteId is not a valid UUID"))
		return
	}
	if h.enq == nil {
		httpx.Error(c, domain.ServiceUnavailable("vuln_enqueuer_not_wired", "vulnerability rescan is not available"))
		return
	}
	if err := h.enq.EnqueueRescanSite(c.Request.Context(), RescanSiteArgs{
		TenantID: p.TenantID,
		SiteID:   siteID,
	}); err != nil {
		httpx.Error(c, domain.Internal("enqueue_rescan_failed", "failed to enqueue vulnerability rescan").WithCause(err))
		return
	}
	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   p.TenantID,
		ActorType:  actorTypeFromPrincipal(p),
		ActorID:    p.ActorID(),
		Action:     "site_vuln.rescan",
		TargetType: "site",
		TargetID:   siteID.String(),
	})
	c.JSON(http.StatusOK, rescanResponseDTO{OK: true})
}

func (h *Handler) dismiss(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, findingID, err := parseSiteAndFinding(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	if err := h.svc.Dismiss(c.Request.Context(), p.TenantID, siteID, findingID, p.UserID); err != nil {
		httpx.Error(c, err)
		return
	}
	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   p.TenantID,
		ActorType:  actorTypeFromPrincipal(p),
		ActorID:    p.ActorID(),
		Action:     "site_vuln.dismiss",
		TargetType: "site_vulnerability",
		TargetID:   findingID.String(),
		Metadata:   map[string]any{"site_id": siteID.String()},
	})
	c.Status(http.StatusNoContent)
}

func (h *Handler) restore(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, findingID, err := parseSiteAndFinding(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	if err := h.svc.Restore(c.Request.Context(), p.TenantID, siteID, findingID); err != nil {
		httpx.Error(c, err)
		return
	}
	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   p.TenantID,
		ActorType:  actorTypeFromPrincipal(p),
		ActorID:    p.ActorID(),
		Action:     "site_vuln.restore",
		TargetType: "site_vulnerability",
		TargetID:   findingID.String(),
		Metadata:   map[string]any{"site_id": siteID.String()},
	})
	c.Status(http.StatusNoContent)
}

func (h *Handler) remediate(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	siteID, findingID, err := parseSiteAndFinding(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	run, _, err := h.svc.Remediate(c.Request.Context(), p.TenantID, siteID, findingID, p.UserID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	_, _ = h.audit.Record(c.Request.Context(), audit.Event{
		TenantID:   p.TenantID,
		ActorType:  actorTypeFromPrincipal(p),
		ActorID:    p.ActorID(),
		Action:     "site_vuln.remediate",
		TargetType: "site_vulnerability",
		TargetID:   findingID.String(),
		Metadata:   map[string]any{"site_id": siteID.String(), "run_id": run.ID.String()},
	})
	c.JSON(http.StatusOK, remediateResponseDTO{RunID: run.ID.String()})
}

func (h *Handler) fleetSummary(c *gin.Context) {
	p, _ := domain.PrincipalFromContext(c.Request.Context())
	limit := 200
	if s := c.Query("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			limit = n
		}
	}
	summary, meta, err := h.svc.GetFleetSummary(c.Request.Context(), p.TenantID, limit)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]fleetFindingDTO, 0, len(summary.Findings))
	for _, ff := range summary.Findings {
		items = append(items, fleetFindingDTO{
			SiteID:   ff.SiteID.String(),
			SiteName: ff.SiteName,
			SiteURL:  ff.SiteURL,
			Finding:  toFindingDTO(ff.Finding),
		})
	}
	c.JSON(http.StatusOK, fleetVulnsResponseDTO{
		TotalOpen:           summary.TotalOpen,
		Critical:            summary.Critical,
		High:                summary.High,
		Medium:              summary.Medium,
		Low:                 summary.Low,
		Unknown:             summary.Unknown,
		Items:               items,
		Attribution:         toAttributionDTO(meta),
		FeedOK:              meta.OK,
		FeedSynced:          feedSyncedStr(meta),
		EnrichmentAvailable: meta.EnrichmentOK,
		LastEnrichmentAt:    lastEnrichmentStr(meta),
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func parseSiteAndFinding(c *gin.Context) (uuid.UUID, uuid.UUID, error) {
	siteID, err := uuid.Parse(c.Param("siteId"))
	if err != nil {
		return uuid.Nil, uuid.Nil, domain.Validation("invalid_site_id", "siteId is not a valid UUID")
	}
	findingID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return uuid.Nil, uuid.Nil, domain.Validation("invalid_finding_id", "finding id is not a valid UUID")
	}
	return siteID, findingID, nil
}

func actorTypeFromPrincipal(p domain.Principal) string {
	if p.Type == domain.PrincipalAPIKey {
		return audit.ActorAPIKey
	}
	return audit.ActorUser
}

