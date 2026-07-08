package perf

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agent"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/objectcache"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Body limits for the agent endpoints. The RUCSS HTML/CSS limits are ENFORCED
// BEFORE buffering (the multipart reader stops at the cap → 413), never after.
const (
	maxStatsBody  = 64 << 10 // 64 KiB — small gauge JSON
	maxConfigAck  = 16 << 10 // 16 KiB — install-state ack JSON
	maxRucssHTML  = 10 << 20 // 10 MiB
	maxRucssCSS   = 5 << 20  // 5 MiB (total across all css parts)
	maxRucssMeta  = 8 << 10  // 8 KiB meta JSON
	maxRucssTotal = 16 << 20 // hard overall request ceiling (HTML+CSS+meta+framing)
)

// AgentHandler serves the agent-authenticated Performance Suite callbacks under
// /agent/v1/cache/* and /agent/v1/perf/* and the RUCSS ingest at
// /agent/v1/rucss. Authentication is the Ed25519 signed-request middleware on
// the agent group; the site + tenant are taken from the VERIFIED identity, never
// from the body. The RUCSS endpoint additionally asserts the body's site_id
// matches the JWT-bound site (no cross-site).
// objectCacheSvc is the subset of *objectcache.Service used by AgentHandler.
// Defined as an interface so tests can stub it without a live DB.
type objectCacheSvc interface {
	IngestStats(ctx context.Context, input objectcache.IngestStatsInput) error
	IngestHeartbeat(ctx context.Context, tenantID, siteID uuid.UUID, block *objectcache.HeartbeatBlock) error
}

type AgentHandler struct {
	svc   *Service
	rucss *RucssIngestService
	// ocSvc is the Object Cache service for the optional object_cache ingest block
	// that the agent appends to its stats-report push. May be nil when the Object
	// Cache feature is not wired.
	ocSvc objectCacheSvc
}

// NewAgentHandler wires the agent handler. rucss and ocSvc may be nil.
// rucss: RUCSS endpoint reports unavailable so the agent keeps serving full CSS.
// ocSvc: object_cache block in stats-report is silently skipped.
func NewAgentHandler(svc *Service, rucss *RucssIngestService, ocSvc *objectcache.Service) *AgentHandler {
	return &AgentHandler{svc: svc, rucss: rucss, ocSvc: ocSvc}
}

// Register mounts the routes on the agent-authenticated group.
func (h *AgentHandler) Register(r *gin.RouterGroup) {
	r.POST("/cache/stats-report", h.statsReport)
	r.POST("/perf/config-ack", h.configAck)
	r.POST("/rucss", h.rucssIngest)
	r.POST("/db-clean/progress", h.dbCleanProgress)
	// P3.8 — async orphan-delete progress from the agent.
	// POST /agent/v1/db-orphan-delete/progress
	r.POST("/db-orphan-delete/progress", h.dbOrphanDeleteProgress)
}

// ---------------------------------------------------------------------------
// cache stats report
// ---------------------------------------------------------------------------

type statsReportBody struct {
	CachedPagesCount int    `json:"cached_pages_count"`
	CacheSizeBytes   int64  `json:"cache_size_bytes"`
	LastPurgedAt     int64  `json:"last_purged_at,omitempty"` // unix seconds
	LastPurgeKind    string `json:"last_purge_kind,omitempty"`
	LastPreloadAt    int64  `json:"last_preload_at,omitempty"`
	PreloadPending   int    `json:"preload_pending"`
	PreloadTotal     int    `json:"preload_total"`
	// M52 / #162 — window DELTA hit/miss counts since the agent's last
	// emission. Both optional: when absent or zero the history row is skipped.
	CacheHitCount  int64 `json:"cache_hit_count,omitempty"`
	CacheMissCount int64 `json:"cache_miss_count,omitempty"`
	// M53 / #169 — WooCommerce theme fragments-support probe result (agent-reported,
	// read-only from the operator API). The agent reports this on its regular cache
	// stats heartbeat so the dashboard can gate the shell-cache toggle. Optional:
	// pre-M53 agents omit it; when absent the CP leaves the stored value unchanged.
	WooThemeFragmentsSupported *bool `json:"woo_theme_fragments_supported,omitempty"`
	// M68 — optional object_cache block emitted by the agent alongside the page
	// cache stats. Pre-M68 agents omit it; the block is silently dropped when
	// absent or when the Object Cache service is not wired. All fields are
	// attacker-controlled (a compromised site can forge them), so the handler
	// clamps numeric ranges and validates string enums before forwarding.
	//
	// Stored as json.RawMessage so that a malformed block (e.g. the PHP agent
	// emitting ops_per_sec as a float before we typed it correctly, or any
	// future schema drift) does NOT fail the whole-body Unmarshal and cause a
	// 422 for the page-cache stats portion. The block is decoded separately,
	// best-effort: a decode failure is WARN-logged and the handler continues.
	ObjectCache json.RawMessage `json:"object_cache,omitempty"`
}

// agentObjectCacheBlock is the optional object_cache sub-object inside a
// stats-report push. It carries both heartbeat state (live status pill) and
// a stats delta (hit/miss counts and server metrics). Every field is optional;
// a missing or partially-missing block is a tolerant-ingest no-op.
// The fields mirror the agent's class-object-cache-heartbeat.php build() output.
type agentObjectCacheBlock struct {
	// Heartbeat fields (state pill).
	State           string  `json:"state"`
	LatencyMs       float64 `json:"latency_ms"`
	LastErrorClass  string  `json:"last_error_class,omitempty"`
	HitRatioPct     float64 `json:"hit_ratio_window_pct"`
	UsedMemoryBytes int64   `json:"used_memory_bytes"`
	// EngineVersion is the agent-reported version of the object-cache engine
	// code actually executing on the site (0.41.3+). Logged for observability.
	EngineVersion string `json:"engine_version,omitempty"`
	// ConfigHash is the sha256 hex of the config file the drop-in is reading
	// (added in 0.42.0 per M11). Pre-0.42.0 agents omit this field; the drift
	// check is skipped when absent or empty.
	ConfigHash string `json:"config_hash,omitempty"`

	// Stats delta fields (time-series history).
	HitCount  int64   `json:"hit_count,omitempty"`
	MissCount int64   `json:"miss_count,omitempty"`
	AvgWaitMs float64 `json:"avg_wait_ms,omitempty"`
	// OpsPerSec is typed float64 because the PHP agent emits round($ops/$elapsed, 2)
	// which produces a fractional JSON number (e.g. 35.25). An int target would
	// cause encoding/json to reject the whole block. The value is rounded to the
	// nearest integer at the DB-bound service boundary (ops_per_sec column is integer).
	OpsPerSec        float64 `json:"ops_per_sec,omitempty"`
	EvictedKeysDelta int64   `json:"evicted_keys_delta,omitempty"`
	ConnectedClients int     `json:"connected_clients,omitempty"`
}

func (h *AgentHandler) statsReport(c *gin.Context) {
	id, ok := agent.IdentityFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("agent_unauthenticated", "agent identity required"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxStatsBody))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "could not read request body"))
		return
	}
	var in statsReportBody
	if err := json.Unmarshal(body, &in); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON: "+err.Error()))
		return
	}
	stats := CacheStats{
		SiteID:           id.SiteID,
		TenantID:         id.TenantID,
		CachedPagesCount: in.CachedPagesCount,
		CacheSizeBytes:   in.CacheSizeBytes,
		LastPurgeKind:    in.LastPurgeKind,
		PreloadPending:   in.PreloadPending,
		PreloadTotal:     in.PreloadTotal,
		// Clamp to non-negative: a buggy/rogue agent could send a negative
		// counter, which would skew the computed hit ratio (e.g. above 100%).
		CacheHitCount:  max(0, in.CacheHitCount),
		CacheMissCount: max(0, in.CacheMissCount),
	}
	if in.LastPurgedAt > 0 {
		t := time.Unix(in.LastPurgedAt, 0).UTC()
		stats.LastPurgedAt = &t
	}
	if in.LastPreloadAt > 0 {
		t := time.Unix(in.LastPreloadAt, 0).UTC()
		stats.LastPreloadAt = &t
	}
	if _, err := h.svc.ReportCacheStats(c.Request.Context(), stats); err != nil {
		httpx.Error(c, err)
		return
	}
	// M53 / #169: persist the agent's WooCommerce theme fragments-support probe
	// result when present. Best-effort: a failure must not fail the stats response
	// (the agent re-reports it on every heartbeat).
	if in.WooThemeFragmentsSupported != nil {
		if wooErr := h.svc.MarkWooFragmentsSupported(c.Request.Context(), id.SiteID, *in.WooThemeFragmentsSupported); wooErr != nil {
			_ = wooErr
		}
	}
	// M68 — object_cache block: best-effort, tolerant. ObjectCache is a
	// json.RawMessage: the outer Unmarshal captures the raw bytes without
	// attempting to decode the sub-object, so a malformed block (wrong field
	// types, future schema drift) CANNOT fail the whole stats-report and MUST
	// NOT 4xx this response (the email-log 422 lesson, this handler's own
	// comment promise). Decode the sub-object separately here.
	// The block is attacker-controlled (a compromised site can forge any value)
	// so every field is clamped/validated before forwarding. siteID and tenantID
	// come STRICTLY from the verified agent identity (id), never from the body.
	var ocBlockDecoded *agentObjectCacheBlock
	if len(in.ObjectCache) > 0 && h.ocSvc != nil {
		var parsed agentObjectCacheBlock
		if decErr := json.Unmarshal(in.ObjectCache, &parsed); decErr != nil {
			// A malformed block is a WARN (observability) not an error: the page-cache
			// stats were already ingested above; the response is still 200.
			slog.Warn("objectcache: block malformed, skipping",
				slog.String("site_id", id.SiteID.String()),
				slog.Any("error", decErr),
			)
		} else {
			ocBlockDecoded = &parsed
		}
	}
	if ocBlockDecoded != nil && h.ocSvc != nil {
		ocBlock := ocBlockDecoded
		// Validate the state enum. The agent legitimately emits 'disabled' when
		// the drop-in is configured but not serving. Unknown values are coerced to
		// "" (OCStateUnknown) and the heartbeat update is skipped entirely so we
		// never overwrite a good stored state with a junk value.
		validOCState := func(s string) bool {
			switch s {
			case "", "disabled", "connected", "degraded", "down":
				return true
			}
			return false
		}
		state := ocBlock.State
		rawStateValid := validOCState(state)
		if !rawStateValid {
			state = ""
		}
		// Clamp numerics: latency and ratio must be non-negative and sane.
		latencyMs := ocBlock.LatencyMs
		if latencyMs < 0 {
			latencyMs = 0
		}
		if latencyMs > 60000 { // 60 s max
			latencyMs = 60000
		}
		hitRatioPct := ocBlock.HitRatioPct
		if hitRatioPct < 0 {
			hitRatioPct = 0
		}
		if hitRatioPct > 100 {
			hitRatioPct = 100
		}
		usedMemoryBytes := ocBlock.UsedMemoryBytes
		if usedMemoryBytes < 0 {
			usedMemoryBytes = 0
		}
		// Bound the error class string to prevent unbounded memory/storage.
		lastErrorClass := ocBlock.LastErrorClass
		if len(lastErrorClass) > 128 {
			lastErrorClass = lastErrorClass[:128]
		}
		engineVersion := ocBlock.EngineVersion
		if len(engineVersion) > 32 {
			engineVersion = engineVersion[:32]
		}
		// Bound the config_hash field: contract is 64-char sha256 hex but the
		// field is attacker-controlled on a compromised site.
		configHash := ocBlock.ConfigHash
		if len(configHash) > 64 {
			configHash = configHash[:64]
		}

		// Observability: one INFO line per received block so on-site engine
		// behavior (which code runs, what state it reports) is visible in the
		// CP logs without DB access. All values are clamped above.
		slog.Info("objectcache: block received",
			slog.String("site_id", id.SiteID.String()),
			slog.String("state", state),
			slog.String("engine_version", engineVersion),
			slog.String("last_error_class", lastErrorClass),
			slog.Int64("hit_count", max(int64(0), ocBlock.HitCount)),
			slog.Int64("miss_count", max(int64(0), ocBlock.MissCount)),
		)

		// IngestHeartbeat: updates the live status pill and emits SSE.
		// tenantID and siteID come from the verified agent identity.
		// Skip the update entirely when the agent sent an unrecognised state
		// (clamped to "") to preserve whatever valid state is already stored.
		if !rawStateValid {
			// Cap before logging: state is agent-controlled (up to maxStatsBody).
			rawState := ocBlock.State
			if len(rawState) > 128 {
				rawState = rawState[:128]
			}
			slog.Warn("objectcache: unknown state from agent, skipping heartbeat update",
				slog.String("site_id", id.SiteID.String()),
				slog.String("raw_state", rawState),
			)
		} else {
			hbBlock := &objectcache.HeartbeatBlock{
				State:           objectcache.OCState(state),
				LatencyMs:       int(latencyMs),
				LastErrorClass:  lastErrorClass,
				UsedMemoryBytes: usedMemoryBytes,
				HitRatioPct:     hitRatioPct,
				ConfigHash:      configHash,
			}
			if hbErr := h.ocSvc.IngestHeartbeat(c.Request.Context(), id.TenantID, id.SiteID, hbBlock); hbErr != nil {
				slog.Warn("objectcache: heartbeat ingest error",
					slog.String("site_id", id.SiteID.String()),
					slog.Any("error", hbErr),
				)
			}
		}

		// IngestStats: appends a time-series data point when hit/miss counts are
		// non-zero. Clamp to non-negative to prevent ratio skew from a rogue agent.
		hitCount := max(int64(0), ocBlock.HitCount)
		missCount := max(int64(0), ocBlock.MissCount)
		if hitCount > 0 || missCount > 0 {
			// Upper clamps match the columns' representable ranges: a forged
			// out-of-range value would otherwise fail the INSERT and silently
			// drop the site's own stats row (avg_wait_ms is numeric(8,3);
			// ops_per_sec and connected_clients are integer columns).
			avgWaitMs := ocBlock.AvgWaitMs
			if avgWaitMs < 0 {
				avgWaitMs = 0
			} else if avgWaitMs > 60000 {
				avgWaitMs = 60000
			}
			opsPerSec := ocBlock.OpsPerSec
			if opsPerSec < 0 {
				opsPerSec = 0
			} else if opsPerSec > math.MaxInt32 {
				opsPerSec = math.MaxInt32
			}
			evictedKeysDelta := ocBlock.EvictedKeysDelta
			if evictedKeysDelta < 0 {
				evictedKeysDelta = 0
			}
			connectedClients := ocBlock.ConnectedClients
			if connectedClients < 0 {
				connectedClients = 0
			} else if connectedClients > math.MaxInt32 {
				connectedClients = math.MaxInt32
			}
			if statsErr := h.ocSvc.IngestStats(c.Request.Context(), objectcache.IngestStatsInput{
				TenantID:         id.TenantID,
				SiteID:           id.SiteID,
				HitCount:         hitCount,
				MissCount:        missCount,
				UsedMemoryBytes:  usedMemoryBytes,
				AvgWaitMs:        avgWaitMs,
				OpsPerSec:        opsPerSec,
				EvictedKeysDelta: evictedKeysDelta,
				ConnectedClients: connectedClients,
			}); statsErr != nil {
				slog.Warn("objectcache: stats ingest error",
					slog.String("site_id", id.SiteID.String()),
					slog.Any("error", statsErr),
				)
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---------------------------------------------------------------------------
// perf config ack (install-state report)
// ---------------------------------------------------------------------------

// configAckBody is the POST /agent/v1/perf/config-ack body. rum_beacon_present
// is the GH #174 PINNED contract field: a plain boolean, true iff the agent
// currently holds a non-empty rum_beacon_key. It is a *bool (not bool) so a
// pre-#174 agent that omits the field entirely is distinguishable from a
// current agent explicitly reporting false — mirrors the identical
// WooThemeFragmentsSupported *bool precedent above (statsReportBody). The
// agent MUST NEVER send the plaintext key back here; only this boolean.
type configAckBody struct {
	ConfigVersion      int    `json:"config_version"`
	ServerSoftware     string `json:"server_software"`
	DropinInstalled    bool   `json:"dropin_installed"`
	WPCacheConstantSet bool   `json:"wp_cache_constant_set"`
	HtaccessManaged    bool   `json:"htaccess_managed"`
	// RumBeaconPresent reports whether the agent holds a non-empty
	// rum_beacon_key at ack time (GH #174). Optional: nil = the agent did not
	// report it (no signal, not "absent" — never treated as a reconcile
	// trigger). See Service.MarkConfigApplied for the self-heal this drives.
	RumBeaconPresent *bool `json:"rum_beacon_present,omitempty"`
}

func (h *AgentHandler) configAck(c *gin.Context) {
	id, ok := agent.IdentityFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("agent_unauthenticated", "agent identity required"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxConfigAck))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "could not read request body"))
		return
	}
	var in configAckBody
	if err := json.Unmarshal(body, &in); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON: "+err.Error()))
		return
	}
	if err := h.svc.MarkConfigApplied(c.Request.Context(), id.TenantID, id.SiteID, in.ServerSoftware, in.DropinInstalled, in.WPCacheConstantSet, in.HtaccessManaged, in.RumBeaconPresent); err != nil {
		// A missing config row (the operator never saved one yet) is non-fatal: the
		// agent will re-ack after the next config push. Return ok=true.
		if err == ErrNotFound {
			c.JSON(http.StatusOK, gin.H{"ok": true, "detail": "no config row yet"})
			return
		}
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---------------------------------------------------------------------------
// RUCSS ingest (multipart)
// ---------------------------------------------------------------------------

// rucssMeta is the `meta` JSON part of the multipart RUCSS request.
type rucssMeta struct {
	SiteID        string   `json:"site_id"`
	URL           string   `json:"url"`
	StructureHash string   `json:"structure_hash"`
	Safelist      []string `json:"safelist,omitempty"`
	// Reheat is set by the agent when this render is a CP-initiated post-compute
	// re-warm self-fetch; it propagates to the River job so the worker does not
	// re-trigger another reheat (loop guard against structure_hash drift).
	Reheat bool `json:"reheat,omitempty"`
}

// rucssIngest accepts a multipart body: a `meta` JSON part + one `html` part +
// one or more `css` parts. Body limits are enforced as we stream each part (we
// never buffer past the cap → 413).
//
// RUCSS AGENT ROUND-TRIP CONTRACT (authoritative — match this exactly, agent eng):
//
//   - Endpoint: POST {cp_base}/agent/v1/rucss
//   - Auth: Ed25519 signed-request middleware (agent group). Site + tenant come
//     from the VERIFIED identity, NEVER the body.
//   - Request: Content-Type multipart/form-data with parts —
//     meta (application/json) {"site_id","url","structure_hash","safelist"?};
//     site_id, when present, MUST equal the authenticated site (else 403);
//     structure_hash is REQUIRED; html (required, ≤10 MiB); css (one-or-more,
//     optional, ≤5 MiB total). Hard overall request ceiling 16 MiB; a part over
//     its cap → 413.
//
// Responses:
//   - 200 OK — CACHE HIT. Body is the used-CSS CONTENT (NOT a key); the agent
//     inlines it directly, no S3 access. Headers: Content-Type: text/css;
//     Content-Encoding: gzip (the bytes ARE gzip-compressed); X-Rucss-Reduction-Pct
//     (float, informational); X-Rucss-Used-Bytes (int, uncompressed used-CSS size).
//     An HTTP client sending Accept-Encoding: gzip inflates transparently; a raw
//     client must gunzip the body.
//   - 202 Accepted — CACHE MISS (or RUCSS degraded/unavailable). JSON
//     {"status":"processing","job_id":"..."} or {"status":"unavailable"}. The agent
//     serves FULL CSS this render and NEVER blocks; the used CSS becomes available
//     on a later request once the job runs.
//   - 401 — no verified agent identity.
//   - 403 — meta.site_id does not match the authenticated site.
//   - 413 — a part (or the whole request) exceeded its size limit.
//   - 422 — missing/invalid meta (no structure_hash) or missing html part.
func (h *AgentHandler) rucssIngest(c *gin.Context) {
	id, ok := agent.IdentityFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("agent_unauthenticated", "agent identity required"))
		return
	}

	mediaType, params, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		httpx.Error(c, domain.Validation("invalid_content_type", "expected multipart/form-data"))
		return
	}
	boundary := params["boundary"]
	if boundary == "" {
		httpx.Error(c, domain.Validation("invalid_multipart", "missing multipart boundary"))
		return
	}

	// Hard ceiling on the whole request so a malicious framing flood can't OOM us
	// before the per-part caps engage.
	mr := multipart.NewReader(io.LimitReader(c.Request.Body, maxRucssTotal+1), boundary)

	var (
		meta    rucssMeta
		gotMeta bool
		html    []byte
		gotHTML bool
		css     []byte
	)

	for {
		part, perr := mr.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			httpx.Error(c, domain.Validation("invalid_multipart", "could not read multipart part: "+perr.Error()))
			return
		}
		switch part.FormName() {
		case "meta":
			raw, rerr := readCapped(part, maxRucssMeta)
			if rerr != nil {
				_ = part.Close()
				httpx.Error(c, rerr)
				return
			}
			if jerr := json.Unmarshal(raw, &meta); jerr != nil {
				_ = part.Close()
				httpx.Error(c, domain.Validation("invalid_meta", "meta part is not valid JSON"))
				return
			}
			gotMeta = true
		case "html":
			raw, rerr := readCapped(part, maxRucssHTML)
			if rerr != nil {
				_ = part.Close()
				httpx.Error(c, rerr)
				return
			}
			html = raw
			gotHTML = true
		case "css":
			// CSS may arrive in multiple parts; enforce the CAP across the running
			// total so the sum cannot exceed maxRucssCSS.
			remaining := maxRucssCSS - len(css)
			if remaining <= 0 {
				_ = part.Close()
				httpx.Error(c, domain.TooLarge("rucss_css_too_large", "css total exceeds 5MB limit"))
				return
			}
			raw, rerr := readCapped(part, remaining)
			if rerr != nil {
				_ = part.Close()
				httpx.Error(c, rerr)
				return
			}
			css = append(css, raw...)
		}
		_ = part.Close()
	}

	if !gotMeta || meta.StructureHash == "" {
		httpx.Error(c, domain.Validation("invalid_meta", "meta part with structure_hash is required"))
		return
	}
	if !gotHTML {
		httpx.Error(c, domain.Validation("missing_html", "html part is required"))
		return
	}

	// SECURITY: the body-supplied site_id MUST match the JWT-bound site. Reject
	// any cross-site attempt (a compromised/buggy agent must not compute or read
	// another site's RUCSS).
	if meta.SiteID != "" {
		bodySite, perr := uuid.Parse(meta.SiteID)
		if perr != nil || bodySite != id.SiteID {
			httpx.Error(c, domain.Forbidden("site_mismatch", "meta.site_id does not match the authenticated site"))
			return
		}
	}

	if h.rucss == nil {
		// RUCSS not wired: tell the agent to keep serving full CSS.
		c.JSON(http.StatusAccepted, gin.H{"status": "unavailable"})
		return
	}

	res, ierr := h.rucss.Ingest(c.Request.Context(), RucssIngestInput{
		TenantID:      id.TenantID,
		SiteID:        id.SiteID,
		StructureHash: meta.StructureHash,
		URL:           meta.URL,
		HTML:          html,
		CSS:           css,
		Safelist:      meta.Safelist,
		Reheat:        meta.Reheat,
	})
	if ierr != nil {
		httpx.Error(c, ierr)
		return
	}
	// CACHE HIT: return the used-CSS CONTENT (not a key) so the agent can inline
	// it without S3 access. Only when we actually read the object bytes — if the
	// read degraded (res.UsedCSS empty) we fall through to the 202 miss so the
	// agent keeps serving full CSS this render. See the contract comment above.
	if res.Cached && len(res.UsedCSS) > 0 {
		c.Header("X-Rucss-Reduction-Pct", strconv.FormatFloat(res.ReductionPct, 'f', 2, 64))
		c.Header("X-Rucss-Used-Bytes", strconv.Itoa(res.UsedCSSBytes))
		if res.UsedCSSGzip {
			c.Header("Content-Encoding", "gzip")
		}
		c.Data(http.StatusOK, "text/css", res.UsedCSS)
		return
	}
	// Miss (or degraded): 202 processing. The agent serves full CSS now.
	c.JSON(http.StatusAccepted, gin.H{"status": "processing", "job_id": res.JobID})
}

// ---------------------------------------------------------------------------
// db-clean progress push (M38)
// ---------------------------------------------------------------------------

// maxDBCleanProgressBody is the size cap for per-category progress push bodies.
// 16 KiB matches the frozen contract's body_size_limit.
const maxDBCleanProgressBody = 16 << 10

// dbCleanProgressBody is the body the agent POSTs for each category result.
type dbCleanProgressBody struct {
	JobID       string `json:"job_id"`
	Category    string `json:"category"`
	RowsDeleted int    `json:"rows_deleted"`
	BytesFreed  int    `json:"bytes_freed"`
	State       string `json:"state"`
	Detail      string `json:"detail,omitempty"`
	// Done is true only on the FINAL push for this job (after the last category).
	// The CP emits db.clean.completed and advances next_db_clean_at.
	Done bool `json:"done"`
}

// dbCleanProgress handles per-category progress pushes from the agent at
// POST /agent/v1/db-clean/progress. Authentication is the same Ed25519 signed-
// request middleware as /agent/v1/cache/stats-report: the site + tenant come
// from the VERIFIED identity, never from the body.
//
// The frozen contract mandates:
//   - If job_id is unknown (CP restarted mid-job), still process — do NOT 404.
//   - The agent must tolerate a non-2xx response without halting the cleanup loop.
func (h *AgentHandler) dbCleanProgress(c *gin.Context) {
	id, ok := agent.IdentityFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("agent_unauthenticated", "agent identity required"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxDBCleanProgressBody))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "could not read request body"))
		return
	}
	var in dbCleanProgressBody
	if err := json.Unmarshal(body, &in); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON: "+err.Error()))
		return
	}
	if in.JobID == "" {
		httpx.Error(c, domain.Validation("missing_job_id", "job_id is required"))
		return
	}
	if in.Category == "" {
		httpx.Error(c, domain.Validation("missing_category", "category is required"))
		return
	}

	if err := h.svc.HandleDBCleanProgress(c.Request.Context(), DBCleanProgressInput{
		JobID:       in.JobID,
		Category:    in.Category,
		RowsDeleted: in.RowsDeleted,
		BytesFreed:  in.BytesFreed,
		State:       in.State,
		Detail:      in.Detail,
		Done:        in.Done,
		TenantID:    id.TenantID,
		SiteID:      id.SiteID,
	}); err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// readCapped reads up to limit bytes from r; if r has MORE than limit bytes it
// returns a 413 domain error (enforced BEFORE the excess is buffered: we read
// limit+1 and reject when the extra byte is present).
func readCapped(r io.Reader, limit int) ([]byte, error) {
	buf, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, domain.Validation("read_error", "could not read part")
	}
	if len(buf) > limit {
		return nil, domain.TooLarge("rucss_part_too_large", "a part exceeded its size limit")
	}
	return buf, nil
}

// ---------------------------------------------------------------------------
// db-orphan-delete progress push (P3.8)
// ---------------------------------------------------------------------------

// maxDBOrphanDeleteProgressBody is the size cap for orphan-delete progress
// push bodies. 512 KiB accommodates up to 500 items × ~1 KiB each.
const maxDBOrphanDeleteProgressBody = 512 << 10

// dbOrphanDeleteProgress handles batched progress pushes from the agent at
// POST /agent/v1/db-orphan-delete/progress. Authentication is the same
// Ed25519 signed-request middleware as all other /agent/v1/* routes: the site
// and tenant come from the VERIFIED identity, never from the body.
//
// The frozen contract requires:
//   - If job_id is unknown (CP restarted mid-job), still process — do NOT 404.
//   - The agent must tolerate a non-2xx response without halting the delete loop.
func (h *AgentHandler) dbOrphanDeleteProgress(c *gin.Context) {
	id, ok := agent.IdentityFromContext(c.Request.Context())
	if !ok {
		httpx.Error(c, domain.Unauthorized("agent_unauthenticated", "agent identity required"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxDBOrphanDeleteProgressBody))
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "could not read request body"))
		return
	}

	var in agentcmd.DBOrphanDeleteProgressBody
	if err := json.Unmarshal(body, &in); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid JSON: "+err.Error()))
		return
	}
	if in.JobID == "" {
		httpx.Error(c, domain.Validation("missing_job_id", "job_id is required"))
		return
	}

	if err := h.svc.HandleDBOrphanDeleteProgress(c.Request.Context(), DBOrphanDeleteProgressInput{
		JobID:          in.JobID,
		Results:        in.Results,
		DeletedOptions: in.DeletedOptions,
		DeletedCron:    in.DeletedCron,
		DeletedTables:  in.DeletedTables,
		Skipped:        in.Skipped,
		Done:           in.Done,
		TenantID:       id.TenantID,
		SiteID:         id.SiteID,
	}); err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
