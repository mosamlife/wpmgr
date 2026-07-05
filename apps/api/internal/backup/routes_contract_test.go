package backup

import (
	"sort"
	"testing"

	"github.com/gin-gonic/gin"
)

// canonicalBackupRoutes is the EXACT set of method+path tuples the backup
// domain must register on the authenticated /api/v1 group.
//
// Three handler types contribute:
//   - Handler          (core backup/snapshot/schedule/settings/SSE)
//   - RestoreRunHandler (restore-run history + phase log)
//   - ScheduleRunHandler (schedule-run history)
//
// This list is the Go-side pin on the backup operator paths described in
// packages/openapi/openapi.yaml. The three MUST stay in lock-step:
//
//   - openapi.yaml  → documents the contract; generates the @wpmgr/api SDK fns.
//   - this list     → fails the build if the registered gin routes drift.
//
// A path mismatch (e.g. a route renamed without updating the spec) will break
// the TS client silently; this test surfaces that breakage on the Go side.
// If you intentionally add/rename/remove an operator route, update BOTH this
// list AND openapi.yaml in the same change.
//
// Paths use gin's :param form (the registered form).
var canonicalBackupRoutes = []string{
	// --- Handler.Register ---
	// Per-site backup CRUD.
	"POST   /api/v1/sites/:siteId/backups",
	"GET    /api/v1/sites/:siteId/backups",
	// issue #115: chain-aware bulk delete.
	"POST   /api/v1/sites/:siteId/backups/bulk-delete",
	// Backup schedule (CP-owned cron config).
	"GET    /api/v1/sites/:siteId/backup-schedule",
	"PUT    /api/v1/sites/:siteId/backup-schedule",
	// m50: per-site backup settings (Track-A content scope).
	"GET    /api/v1/sites/:siteId/backup-settings/contents",
	"PUT    /api/v1/sites/:siteId/backup-settings/contents",
	// m50: per-site backup settings (Track-B notifications).
	"GET    /api/v1/sites/:siteId/backup-settings/notifications",
	"PUT    /api/v1/sites/:siteId/backup-settings/notifications",
	// Fleet endpoints (no :siteId, tenant-level backup health / list).
	"GET    /api/v1/backups/fleet",
	"GET    /api/v1/backups/health",
	// By-snapshotId routes (no :siteId; site gate via canReadSite).
	"GET    /api/v1/backups/:snapshotId",
	"DELETE /api/v1/backups/:snapshotId",
	"GET    /api/v1/backups/:snapshotId/events",
	"POST   /api/v1/backups/:snapshotId/restore",
	"POST   /api/v1/backups/:snapshotId/cancel",
	// Track C (m49): snapshot lock toggle.
	"PATCH  /api/v1/backups/:snapshotId/lock",
	"DELETE /api/v1/backups/:snapshotId/lock",
	// ADR-037: environment fingerprint (agent-supplied JSON pass-through).
	"GET    /api/v1/backups/:snapshotId/environment",
	// --- Handler.RegisterInspection ---
	// M6: SQL-inspection artifact (agent-supplied or CP legacy-parsed).
	"GET    /api/v1/backups/:snapshotId/sql-inspection",
	// --- RestoreRunHandler.Register ---
	// Restore-run history + phase log (m16).
	"GET    /api/v1/sites/:siteId/restores",
	"GET    /api/v1/restores/:restoreId",
	"GET    /api/v1/restores/:restoreId/events",
	// --- ScheduleRunHandler.Register ---
	// Schedule-run history (M17).
	"GET    /api/v1/sites/:siteId/schedule-runs",
	"GET    /api/v1/schedule-runs/:runId",
}

// TestBackupRoutesContract pins the registered backup route set to the
// canonical list above. All three handler types are mounted exactly as
// server.New mounts them (on a /api/v1 group), and the live gin route table is
// compared, so the test catches a renamed/dropped/added route, a wrong method,
// or a path-prefix slip. Middleware closures are not executed during
// registration, so bare *Service / nil hub / nil audit / zero InspectionDeps
// are sufficient.
func TestBackupRoutesContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	v1 := engine.Group("/api/v1")

	// Core backup handler (snapshots, schedule, settings, SSE, environment).
	h := NewHandler(&Service{}, nil, nil)
	h.Register(v1)

	// SQL-inspection endpoint (mounted separately so partial deployments can
	// omit the River + blobstore deps; the route still exists even with nil deps).
	h.RegisterInspection(v1, InspectionDeps{})

	// Restore-run history + phase log.
	rh := NewRestoreRunHandler(&Service{})
	rh.Register(v1)

	// Schedule-run history.
	sh := NewScheduleRunHandler(&Service{})
	sh.Register(v1)

	got := make([]string, 0, len(engine.Routes()))
	for _, r := range engine.Routes() {
		got = append(got, backupFormatRoute(r.Method, r.Path))
	}

	want := make([]string, len(canonicalBackupRoutes))
	copy(want, canonicalBackupRoutes)
	sort.Strings(want)
	sort.Strings(got)

	if len(got) != len(want) {
		t.Fatalf("backup route count = %d, want %d\n got: %v\nwant: %v",
			len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backup route mismatch at index %d:\n got: %q\nwant: %q\n\nfull got: %v\nfull want: %v",
				i, got[i], want[i], got, want)
		}
	}
}

// backupFormatRoute renders a method+path tuple in the canonical list's
// fixed-width form so the literals above line up and diff cleanly.
func backupFormatRoute(method, path string) string {
	pad := method
	for len(pad) < 6 {
		pad += " "
	}
	return pad + " " + path
}
