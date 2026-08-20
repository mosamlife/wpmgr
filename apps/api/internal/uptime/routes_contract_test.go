package uptime

import (
	"sort"
	"testing"

	"github.com/gin-gonic/gin"
)

// canonicalUptimeRoutes is the EXACT set of method+path tuples the uptime
// domain must register on the authenticated /api/v1 group. If you add,
// remove or rename a route, update this list AND packages/openapi/openapi.yaml
// in the same change.
var canonicalUptimeRoutes = []string{
	// Per-site uptime dashboard.
	"GET    /api/v1/sites/:siteId/uptime",
	// Tenant-wide uptime summary (existing).
	"GET    /api/v1/uptime/summary",
	// Tenant-level alert config.
	"GET    /api/v1/alert-config",
	"PUT    /api/v1/alert-config",
	// Fleet uptime status + incidents.
	"GET    /api/v1/fleet/status",
	// Fleet daily availability strip (GH #460).
	"GET    /api/v1/fleet/uptime-history",
	"GET    /api/v1/fleet/incidents",
	"GET    /api/v1/fleet/incidents/:incidentId",
	// Per-site app-health settings (GH #291 Phase 3).
	"GET    /api/v1/sites/:siteId/app-health-settings",
	"PUT    /api/v1/sites/:siteId/app-health-settings",
}

func TestUptimeRoutesContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	v1 := engine.Group("/api/v1")

	h := NewHandler(&Service{}, nil)
	h.Register(v1)

	got := make([]string, 0, len(engine.Routes()))
	for _, r := range engine.Routes() {
		got = append(got, uptimeFormatRoute(r.Method, r.Path))
	}

	want := make([]string, len(canonicalUptimeRoutes))
	copy(want, canonicalUptimeRoutes)
	sort.Strings(want)
	sort.Strings(got)

	if len(got) != len(want) {
		t.Fatalf("uptime route count = %d, want %d\n got: %v\nwant: %v",
			len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("uptime route mismatch at index %d:\n got: %q\nwant: %q\n\nfull got: %v\nfull want: %v",
				i, got[i], want[i], got, want)
		}
	}
}

func uptimeFormatRoute(method, path string) string {
	pad := method
	for len(pad) < 6 {
		pad += " "
	}
	return pad + " " + path
}
