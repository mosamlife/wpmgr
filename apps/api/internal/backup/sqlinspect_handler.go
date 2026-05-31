package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/restore/sqlinspect"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// Manifest entry kinds for sql-inspection sources. The "inspection" entry kind
// is the agent-supplied path — a manifest entry whose path is
// "sql-inspection.json" carrying the report bytes. The CP can also recognise
// an entry kind of literal "inspection" if/when the agent moves to a typed
// classification; both are accepted by inspectionEntry below.
const (
	// EntryKindInspection is the M6 manifest entry kind for an agent-supplied
	// SQL inspection JSON. Older agents that pre-date this typing carry the
	// payload as a regular file entry with path="sql-inspection.json"; the
	// handler accepts both. Defined here (not in model.go) so the agent-side
	// renaming/migration stays local to the inspection feature.
	EntryKindInspection = "inspection"
	// InspectionLogicalPath is the conventional logical path for an agent-
	// supplied inspection artifact.
	InspectionLogicalPath = "sql-inspection.json"
)

// InspectionEnqueuer enqueues the SqlInspectLegacy River job for snapshots
// whose manifest does not carry an agent-supplied inspection artifact and
// whose CP-side cache is empty. Implemented by RiverEnqueuer (see enqueuer.go);
// kept as an interface so unit tests can substitute a stub.
type InspectionEnqueuer interface {
	EnqueueSqlInspectLegacy(ctx context.Context, tenantID, snapshotID uuid.UUID) error
}

// InspectionCache fetches a previously-cached legacy inspection report by
// snapshot ID. Backed by the blobstore in production (the JSON object lives
// at inspection-cache/{snapshot_id}.json); abstracted so tests can wire a
// memory cache.
type InspectionCache interface {
	// Get returns the cached report bytes (a JSON-marshalled
	// sqlinspect.Report) or (nil, nil) when the cache is empty.
	Get(ctx context.Context, tenantID, snapshotID uuid.UUID) ([]byte, error)
}

// ManifestInspectionFetcher fetches an agent-supplied inspection artifact
// from the chunk store. Implementations stream the manifest entry's ordered
// chunks (decrypting if the agent encrypted them — V0 agents do NOT encrypt
// the inspection artifact because the CP needs to read it) and return the
// concatenated bytes. Returns (nil, nil) when no inspection entry exists.
type ManifestInspectionFetcher interface {
	Fetch(ctx context.Context, tenantID uuid.UUID, snap Snapshot, entry ManifestEntry) ([]byte, error)
}

// InspectionDeps groups the optional collaborators the inspection handler
// needs. Each may be nil — the handler degrades gracefully (returns a 503
// pointing at the missing tier) so the endpoint is reachable on a partial
// rollout where, e.g., the cache backend is wired but the River job is not.
type InspectionDeps struct {
	Enqueuer      InspectionEnqueuer
	Cache         InspectionCache
	ManifestFetch ManifestInspectionFetcher
	Logger        *slog.Logger
	// PollLocation, when non-empty, is the URL template the handler emits in
	// the 202 Accepted Location header. Defaults to the request URL.
	PollLocation string
}

// RegisterInspection mounts the sql-inspection route on the /api/v1 router
// group. Kept separate from Register so callers that don't have the optional
// blobstore + River-enabled deps can mount the rest of the backup API
// without this endpoint.
func (h *Handler) RegisterInspection(r *gin.RouterGroup, deps InspectionDeps) {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	r.GET(
		"/backups/:snapshotId/sql-inspection",
		authz.RequirePermission(authz.PermSiteRead),
		h.sqlInspectionHandler(deps),
	)
}

// sqlInspectionHandler returns the gin.HandlerFunc serving
// GET /api/v1/backups/{snapshotId}/sql-inspection.
//
// Resolution order (mirrors the OpenAPI doc):
//
//  1. Manifest carries an inspection entry (agent-supplied JSON) — fetch,
//     stamp source="agent", return 200.
//  2. CP cache holds a legacy report at inspection-cache/{snapshot_id}.json
//     — fetch, stamp source="cp-legacy" (already on disk), return 200.
//  3. Otherwise enqueue the SqlInspectLegacy River job and return 202
//     Accepted with a Location header pointing at the polling URL.
//
// The handler is intentionally read-only on the manifest side (no writes
// even when it materialises an agent-supplied artifact for the first time);
// caching that path is the future-work item, today's request hits the chunk
// store every call. Cheap enough for the operator-poll cadence.
func (h *Handler) sqlInspectionHandler(deps InspectionDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID, ok := tenantOf(c)
		if !ok {
			return
		}
		snapshotID, ok := uuidParam(c, "snapshotId", "invalid_snapshot_id")
		if !ok {
			return
		}

		snap, entries, err := h.svc.GetSnapshot(c.Request.Context(), tenantID, snapshotID)
		if err != nil {
			httpx.Error(c, err)
			return
		}
		if !canReadSite(c, snap.SiteID) {
			httpx.Error(c, domain.Forbidden("forbidden", "you do not have access to this site"))
			return
		}
		// A snapshot that doesn't carry any DB content has nothing to inspect.
		// Distinct from "no inspection cached" — this is a permanent 404 because
		// no run of the legacy parser will ever find a SQL dump in a
		// files-only snapshot.
		if !snapshotHasDB(snap, entries) {
			httpx.Error(c, domain.NotFound(
				"sql_inspection_not_applicable",
				"snapshot has no database artifact to inspect",
			))
			return
		}

		// Tier 1: agent-supplied inspection artifact in the manifest.
		if inspectionEntry, ok := findInspectionEntry(entries); ok {
			if deps.ManifestFetch == nil {
				h.respondInspectionUnavailable(c, "manifest_fetch_unwired",
					"agent supplied an inspection artifact but the CP cannot fetch it (manifest fetcher unwired)")
				return
			}
			raw, ferr := deps.ManifestFetch.Fetch(c.Request.Context(), tenantID, snap, inspectionEntry)
			if ferr != nil {
				deps.Logger.Warn("sql-inspection manifest fetch failed; falling through to legacy",
					slog.String("snapshot_id", snapshotID.String()),
					slog.Any("error", ferr))
				// Fall through to cache/enqueue tiers — a transient chunk-fetch
				// failure on a single request shouldn't deny the operator the
				// 503 "try again later" surface that the lower tiers provide.
				// (Until 2026-05-29 this hard-500'd, which surfaced an unhelpful
				// server-error toast in the restore dialog instead of the more
				// useful "inspection unavailable" muted note.)
			} else {
				rep, perr := decodeInspectionReport(raw, sqlinspect.SourceAgent)
				if perr != nil {
					deps.Logger.Warn("agent inspection artifact unparseable; falling back to legacy",
						slog.String("snapshot_id", snapshotID.String()),
						slog.Any("error", perr))
					// Fall through to the cache/enqueue path rather than 500ing —
					// a malformed agent artifact shouldn't deny the operator a
					// CP-side inspection.
				} else {
					c.JSON(http.StatusOK, rep)
					return
				}
			}
		}

		// Tier 2: CP legacy cache.
		if deps.Cache != nil {
			cached, cerr := deps.Cache.Get(c.Request.Context(), tenantID, snapshotID)
			if cerr != nil {
				deps.Logger.Warn("sql-inspection cache get failed",
					slog.String("snapshot_id", snapshotID.String()),
					slog.Any("error", cerr))
				// Don't 500 — fall through to enqueue.
			} else if len(cached) > 0 {
				rep, perr := decodeInspectionReport(cached, sqlinspect.SourceCPLegacy)
				if perr != nil {
					deps.Logger.Warn("cached inspection JSON unparseable; re-enqueueing",
						slog.String("snapshot_id", snapshotID.String()),
						slog.Any("error", perr))
				} else {
					c.JSON(http.StatusOK, rep)
					return
				}
			}
		}

		// Tier 3: enqueue the legacy parser job and return 202.
		if deps.Enqueuer == nil {
			h.respondInspectionUnavailable(c, "inspection_unwired",
				"this control-plane is not configured to run the legacy SQL inspector; rebuild the snapshot with an inspection-aware agent")
			return
		}
		if eerr := deps.Enqueuer.EnqueueSqlInspectLegacy(c.Request.Context(), tenantID, snapshotID); eerr != nil {
			httpx.Error(c, domain.Internal("sql_inspection_enqueue_failed",
				"failed to enqueue the SQL inspection job").WithCause(eerr))
			return
		}
		loc := deps.PollLocation
		if loc == "" {
			// Reuse the request URL so the client can blindly follow Location.
			loc = c.Request.URL.RequestURI()
		}
		c.Header("Location", loc)
		c.Header("Retry-After", "5") // operator-poll cadence hint; cheap to honour.
		c.Status(http.StatusAccepted)
	}
}

// snapshotHasDB reports whether the snapshot looks like it has at least one
// DB-kind manifest entry. The snapshot.Kind field is the primary signal (DB or
// Full); the manifest scan is a defence in depth for snapshots that pre-date
// the typed kind field.
func snapshotHasDB(snap Snapshot, entries []ManifestEntry) bool {
	if snap.Kind == KindDB || snap.Kind == KindFull {
		return true
	}
	for _, e := range entries {
		if e.EntryKind == EntryKindDB {
			return true
		}
	}
	return false
}

// findInspectionEntry locates the agent-supplied SQL inspection artifact in a
// manifest, if present. Two locator strategies (in priority order):
//  1. EntryKind == "inspection" — the typed-after-M6 agent path.
//  2. Path == "sql-inspection.json" — the un-typed agent path used before
//     EntryKindInspection landed on the contract.
func findInspectionEntry(entries []ManifestEntry) (ManifestEntry, bool) {
	for _, e := range entries {
		if e.EntryKind == EntryKindInspection {
			return e, true
		}
	}
	for _, e := range entries {
		if e.Path == InspectionLogicalPath {
			return e, true
		}
	}
	return ManifestEntry{}, false
}

// decodeInspectionReport unmarshals a JSON-encoded sqlinspect.Report from raw
// bytes and stamps the source. Treats unknown JSON fields permissively (the
// SchemaVersion field on the report itself is the negotiation lever).
func decodeInspectionReport(raw []byte, source string) (*sqlinspect.Report, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("sql inspection report is empty")
	}
	var rep sqlinspect.Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, fmt.Errorf("decode inspection JSON: %w", err)
	}
	if rep.GeneratedAt.IsZero() {
		rep.GeneratedAt = time.Now().UTC()
	}
	if rep.SchemaVersion == 0 {
		rep.SchemaVersion = sqlinspect.ReportSchemaVersion
	}
	rep.Source = source
	return &rep, nil
}

// respondInspectionUnavailable is the 503 path for a CP that cannot serve the
// inspection because optional plumbing is unwired. The operator-readable
// message points at the specific missing tier so the operator can act.
// Until 2026-05-29 this incorrectly routed through domain.Internal (HTTP 500),
// which surfaced as a hard server error in the UI for legacy snapshots and for
// CP builds without the legacy inspector wired. Now correctly maps to 503.
func (h *Handler) respondInspectionUnavailable(c *gin.Context, code, msg string) {
	httpx.Error(c, domain.ServiceUnavailable(code, msg))
}
