package perf

import (
	"sort"
	"testing"

	"github.com/gin-gonic/gin"
)

// canonicalOperatorRoutes is the EXACT set of method+path tuples the operator
// Performance Suite handler must register on the authenticated /api/v1 group.
//
// This list is the Go-side mirror of the perf operator paths described in
// packages/openapi/openapi.yaml (the canonical contract the hey-api TS client is
// generated from) and the perf-paths.ts constant the dashboard imports. The
// three MUST stay in lock-step:
//
//   - openapi.yaml  → generates the @wpmgr/api SDK fns the frontend calls.
//   - perf-paths.ts → the single place the frontend builds these URLs.
//   - this list     → fails the build if the registered gin routes drift.
//
// A path mismatch (frontend calling /perf/cache/* while the backend served
// /cache/*) shipped once because nothing pinned these together; this test is
// that pin on the backend side. If you intentionally add/rename/remove an
// operator route, update ALL THREE in the same change.
//
// Paths use gin's :param form (the registered form). The portfolio bulk routes
// live at the /api/v1 root (no :siteId path param — the site ids are a body
// array, checked per-id inside the handler).
var canonicalOperatorRoutes = []string{
	"GET    /api/v1/sites/:siteId/perf/config",
	"PUT    /api/v1/sites/:siteId/perf/config",
	"GET    /api/v1/sites/:siteId/perf/cache/stats",
	"POST   /api/v1/sites/:siteId/perf/cache/purge",
	"POST   /api/v1/sites/:siteId/perf/cache/preload",
	"POST   /api/v1/sites/:siteId/perf/cache/enable",
	"POST   /api/v1/sites/:siteId/perf/cache/disable",
	"POST   /api/v1/sites/:siteId/perf/db/clean",
	// M71 — db_clean pull-truth endpoint (watchdog state + last result).
	"GET    /api/v1/sites/:siteId/perf/db/clean",
	// M39 Phase 2 — db_scan (trigger + latest result).
	"POST   /api/v1/sites/:siteId/perf/db/scan",
	"GET    /api/v1/sites/:siteId/perf/db/scan",
	// Phase 2.2 — per-table DDL actions (optimize/repair/drop/empty).
	"POST   /api/v1/sites/:siteId/perf/db/table-action",
	// M42 Phase 3.4 — DB-size trend history + growth summary.
	"GET    /api/v1/sites/:siteId/perf/db/health",
	// M52 / #162 — cache hit-ratio history + avg.
	"GET    /api/v1/sites/:siteId/perf/cache/health",
	// P3.5 — on-demand orphan classification report (read-only).
	"GET    /api/v1/sites/:siteId/perf/db/orphans",
	// P3.8 — destructive orphan deletion (options/cron/tables, UNINSTALLED plugins only).
	"POST   /api/v1/sites/:siteId/perf/db/orphan-delete",
	// #188 — serialization-safe search-replace tool (dry-run + live).
	"POST   /api/v1/sites/:siteId/perf/db/search-replace",
	// #189 — local database snapshot tool (create/list/revert/delete).
	"GET    /api/v1/sites/:siteId/perf/db/snapshots",
	"POST   /api/v1/sites/:siteId/perf/db/snapshots",
	"POST   /api/v1/sites/:siteId/perf/db/snapshots/:snapshotId/revert",
	"DELETE /api/v1/sites/:siteId/perf/db/snapshots/:snapshotId",
	"GET    /api/v1/sites/:siteId/perf/rucss/results",
	"POST   /api/v1/sites/:siteId/perf/rucss/clear",
	"POST   /api/v1/sites/:siteId/perf/rucss/compute",
	// M55 — Font results catalog (dashboard list).
	"GET    /api/v1/sites/:siteId/perf/fonts",
	// M56 — RUM Core Web Vitals read endpoints.
	// Dashboard redesign: /summary gains distribution field; /trend is new.
	"GET    /api/v1/sites/:siteId/perf/rum/summary",
	"GET    /api/v1/sites/:siteId/perf/rum/trend",
	"GET    /api/v1/sites/:siteId/perf/rum",
	// GH #174 — deterministic operator recovery for a beacon key stuck empty
	// on the agent.
	"POST   /api/v1/sites/:siteId/perf/rum/rotate-key",
	"POST   /api/v1/cache/bulk-purge",
	"PUT    /api/v1/cache/bulk-config",
	// P3.7 — tenant-level (no :siteId) fleet DB health aggregate.
	"GET    /api/v1/perf/db/fleet-health",
	// Fleet RUM aggregate (cross-site Core Web Vitals, org-scoped only).
	"GET    /api/v1/perf/rum/fleet",
	// #190 — unused media library cleaner (scan/quarantine/isolate/restore/delete).
	"GET    /api/v1/sites/:siteId/media/clean/scan",
	"GET    /api/v1/sites/:siteId/media/clean/quarantine",
	"POST   /api/v1/sites/:siteId/media/clean/isolate",
	"POST   /api/v1/sites/:siteId/media/clean/restore",
	"POST   /api/v1/sites/:siteId/media/clean/delete",
}

// TestOperatorRoutesContract pins the registered operator route set to the
// canonical list above. The handler is mounted exactly as server.New mounts it
// (on a /api/v1 group), and the live gin route table is compared, so the test
// catches a renamed/dropped/added route, a wrong method, or a path-prefix slip
// (the bug that shipped). The middleware closures are not executed during
// registration, so a bare *Service / nil rucss / nil audit is sufficient.
func TestOperatorRoutesContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	v1 := engine.Group("/api/v1")

	h := NewHandler(&Service{}, nil, nil)
	h.Register(v1)

	got := make([]string, 0, len(engine.Routes()))
	for _, r := range engine.Routes() {
		got = append(got, formatRoute(r.Method, r.Path))
	}

	want := make([]string, len(canonicalOperatorRoutes))
	copy(want, canonicalOperatorRoutes)
	sort.Strings(want)
	sort.Strings(got)

	if len(got) != len(want) {
		t.Fatalf("operator route count = %d, want %d\n got: %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("operator route mismatch at index %d:\n got: %q\nwant: %q\n\nfull got: %v\nfull want: %v",
				i, got[i], want[i], got, want)
		}
	}
}

// formatRoute renders a method+path tuple in the canonical list's fixed-width
// form so the literals above line up and diff cleanly.
func formatRoute(method, path string) string {
	pad := method
	for len(pad) < 6 {
		pad += " "
	}
	return pad + " " + path
}
