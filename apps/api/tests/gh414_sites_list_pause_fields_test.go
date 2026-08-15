// gh414_sites_list_pause_fields_test.go — GH #414 phase 4a: the sites list and
// detail endpoints carry the monitoring pause state AND the "as of" stamp for
// health_status.
//
// Asserts against the RAW JSON, by the exact wire names the web client reads,
// rather than against the Go DTO: a field renamed in openapi.yaml without a
// regeneration, or a mapping dropped in toAPI, both have to redden here. The
// engine, service, repo and Postgres are the production ones (see
// newMonitoringEngine), so every read runs through the tenant tx helper as
// wpmgr_app with the sites and site_uptime_status RLS policies live.
//
// The health_checked_at half is the point of the phase. Pause stops the uptime
// prober, so a paused site's health_status freezes at its last value — a site
// whose server died an hour ago keeps reporting "healthy". Serving that verdict
// without its age is the "lie to me" failure the whole feature exists to avoid,
// so the stamp travels with it on both endpoints.
package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

// seedUptimeStatus writes one site_uptime_status row through the SAME tenant tx
// helper the request path uses, so the table's tenant policy is in force for
// the write as well as the later read.
func seedUptimeStatus(t *testing.T, pool *db.Pool, tenant, siteID uuid.UUID, probedAt time.Time) {
	t.Helper()
	err := pool.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `
			INSERT INTO site_uptime_status (site_id, tenant_id, latest_up, last_probed_at)
			VALUES ($1, $2, true, $3)`, siteID, tenant, probedAt)
		return err
	})
	if err != nil {
		t.Fatalf("seed site_uptime_status: %v", err)
	}
}

// siteByID pulls one entry out of a GET /api/v1/sites body as a raw JSON
// object, so absent keys stay distinguishable from zero values.
func siteByID(t *testing.T, body []byte, id string) map[string]any {
	t.Helper()
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode site list: %v (body %s)", err, string(body))
	}
	for _, it := range list.Items {
		if it["id"] == id {
			return it
		}
	}
	t.Fatalf("site %s missing from list of %d: %s", id, len(list.Items), string(body))
	return nil
}

func rawSite(t *testing.T, body []byte) map[string]any {
	t.Helper()
	out := map[string]any{}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode site detail: %v (body %s)", err, string(body))
	}
	return out
}

// wantTime asserts a wire field is present and parses as the expected instant.
//
// Compared at second granularity because the generated encoder formats
// date-time as plain RFC 3339, which drops the fractional seconds Postgres'
// now() carries. That is the real contract the web client parses, so the
// assertion matches it rather than the DB's microseconds; the deliberate
// hour/minute gaps between the fixtures below are far wider than a second, so
// nothing here can pass by rounding into the wrong value.
func wantTime(t *testing.T, obj map[string]any, key string, want time.Time) {
	t.Helper()
	raw, ok := obj[key]
	if !ok {
		t.Fatalf("%s absent; got keys %v", key, keysOf(obj))
	}
	s, ok := raw.(string)
	if !ok {
		t.Fatalf("%s is %T, want an RFC 3339 string", key, raw)
	}
	got, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("%s = %q does not parse as RFC 3339: %v", key, s, err)
	}
	if !got.Truncate(time.Second).Equal(want.Truncate(time.Second)) {
		t.Fatalf("%s = %v, want %v (compared to the second)", key, got, want)
	}
}

func wantAbsent(t *testing.T, obj map[string]any, key string) {
	t.Helper()
	if v, ok := obj[key]; ok {
		t.Fatalf("%s should be absent for an unpaused site, got %v", key, v)
	}
}

func keysOf(obj map[string]any) []string {
	out := make([]string, 0, len(obj))
	for k := range obj {
		out = append(out, k)
	}
	return out
}

// TestSitesCarryMonitoringPauseAndHealthAsOf_Phase4a pauses one of two sites,
// stamps both with a last_probed_at, and then asserts the wire contract on the
// list and the detail endpoint in turn.
func TestSitesCarryMonitoringPauseAndHealthAsOf_Phase4a(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "gh414-list-"+uuid.NewString()[:8])
	actor := seedMonitoringUser(t, pool, "gh414-list-"+uuid.NewString()[:8]+"@example.com")
	engine := newMonitoringEngine(t, pool, monitoringPrincipal(tenant, actor))

	pausedID := seedSite(t, pool, tenant, "https://gh414-paused-"+uuid.NewString()[:8]+".example.com")
	activeID := seedSite(t, pool, tenant, "https://gh414-active-"+uuid.NewString()[:8]+".example.com")

	// The paused site's health verdict is an hour old and will never be
	// refreshed again while the pause holds; the active site's is a minute old.
	// Truncated to the second so the RFC 3339 round-trip is exact.
	pausedProbedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	activeProbedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	seedUptimeStatus(t, pool, tenant, pausedID, pausedProbedAt)
	seedUptimeStatus(t, pool, tenant, activeID, activeProbedAt)

	resumeAt := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	w := doJSON(t, engine, http.MethodPost, "/api/v1/sites/monitoring/pause", map[string]any{
		"site_ids":  []string{pausedID.String()},
		"reason":    "database migration window",
		"resume_at": resumeAt.Format(time.RFC3339),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("pause: status %d, body %s", w.Code, w.Body.String())
	}
	pausedAt := readPausedAt(t, pool, tenant, pausedID)

	t.Run("list carries pause state and the health as-of for the paused site", func(t *testing.T) {
		w := doJSON(t, engine, http.MethodGet, "/api/v1/sites?limit=100", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("list: status %d, body %s", w.Code, w.Body.String())
		}
		got := siteByID(t, w.Body.Bytes(), pausedID.String())

		wantTime(t, got, "monitoring_paused_at", pausedAt)
		wantTime(t, got, "monitoring_resume_at", resumeAt)
		if got["monitoring_paused_by"] != actor.String() {
			t.Fatalf("monitoring_paused_by = %v, want %s", got["monitoring_paused_by"], actor)
		}
		if got["monitoring_paused_reason"] != "database migration window" {
			t.Fatalf("monitoring_paused_reason = %v", got["monitoring_paused_reason"])
		}
		// The whole point: health_status is still served, and it now travels
		// with the timestamp that says how old it is.
		if _, ok := got["health_status"]; !ok {
			t.Fatalf("health_status absent; got keys %v", keysOf(got))
		}
		wantTime(t, got, "health_checked_at", pausedProbedAt)
	})

	t.Run("list omits the pause fields for an active site but still stamps health", func(t *testing.T) {
		w := doJSON(t, engine, http.MethodGet, "/api/v1/sites?limit=100", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("list: status %d, body %s", w.Code, w.Body.String())
		}
		got := siteByID(t, w.Body.Bytes(), activeID.String())

		// Absent monitoring_paused_at IS "monitoring is active" — there is no
		// separate boolean that could disagree with the timestamp.
		wantAbsent(t, got, "monitoring_paused_at")
		wantAbsent(t, got, "monitoring_paused_by")
		wantAbsent(t, got, "monitoring_paused_reason")
		wantAbsent(t, got, "monitoring_resume_at")
		wantTime(t, got, "health_checked_at", activeProbedAt)
	})

	t.Run("detail carries the same fields", func(t *testing.T) {
		w := doJSON(t, engine, http.MethodGet, "/api/v1/sites/"+pausedID.String(), nil)
		if w.Code != http.StatusOK {
			t.Fatalf("get: status %d, body %s", w.Code, w.Body.String())
		}
		got := rawSite(t, w.Body.Bytes())

		wantTime(t, got, "monitoring_paused_at", pausedAt)
		wantTime(t, got, "monitoring_resume_at", resumeAt)
		if got["monitoring_paused_by"] != actor.String() {
			t.Fatalf("monitoring_paused_by = %v, want %s", got["monitoring_paused_by"], actor)
		}
		if got["monitoring_paused_reason"] != "database migration window" {
			t.Fatalf("monitoring_paused_reason = %v", got["monitoring_paused_reason"])
		}
		wantTime(t, got, "health_checked_at", pausedProbedAt)

		w = doJSON(t, engine, http.MethodGet, "/api/v1/sites/"+activeID.String(), nil)
		if w.Code != http.StatusOK {
			t.Fatalf("get active: status %d, body %s", w.Code, w.Body.String())
		}
		active := rawSite(t, w.Body.Bytes())
		wantAbsent(t, active, "monitoring_paused_at")
		wantTime(t, active, "health_checked_at", activeProbedAt)
	})

	t.Run("a never-probed site omits health_checked_at rather than implying now", func(t *testing.T) {
		unprobed := seedSite(t, pool, tenant, "https://gh414-unprobed-"+uuid.NewString()[:8]+".example.com")
		w := doJSON(t, engine, http.MethodGet, "/api/v1/sites/"+unprobed.String(), nil)
		if w.Code != http.StatusOK {
			t.Fatalf("get: status %d, body %s", w.Code, w.Body.String())
		}
		wantAbsent(t, rawSite(t, w.Body.Bytes()), "health_checked_at")
	})

	t.Run("pausing does not bump updated_at", func(t *testing.T) {
		// sites.updated_at is the INVENTORY freshness stamp (served as `as_of`
		// on GET /sites/{id}/updates). Phase 1 deliberately left it alone on a
		// pause write; if that regressed, pausing a site would make its
		// inventory look freshly synced at the moment monitoring stopped.
		before := readSiteUpdatedAtGH414(t, pool, tenant, activeID)
		w := doJSON(t, engine, http.MethodPost, "/api/v1/sites/monitoring/pause", map[string]any{
			"site_ids": []string{activeID.String()},
			"reason":   "second window",
		})
		if w.Code != http.StatusOK {
			t.Fatalf("pause: status %d, body %s", w.Code, w.Body.String())
		}
		if after := readSiteUpdatedAtGH414(t, pool, tenant, activeID); !after.Equal(before) {
			t.Fatalf("updated_at moved on a pause write: %v -> %v", before, after)
		}
	})
}

// readPausedAt returns the persisted monitoring_paused_at, read through the
// tenant tx helper as wpmgr_app, and fails when the site is not paused.
func readPausedAt(t *testing.T, pool *db.Pool, tenant, siteID uuid.UUID) time.Time {
	t.Helper()
	at, _, _, _, found := readPauseRow(t, pool, tenant, siteID)
	if !found {
		t.Fatalf("site %s not visible under tenant %s", siteID, tenant)
	}
	if at == nil {
		t.Fatalf("site %s is not paused", siteID)
	}
	return at.UTC()
}

func readSiteUpdatedAtGH414(t *testing.T, pool *db.Pool, tenant, siteID uuid.UUID) time.Time {
	t.Helper()
	var out time.Time
	err := pool.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT updated_at FROM sites WHERE tenant_id = $1 AND id = $2`, tenant, siteID).Scan(&out)
	})
	if err != nil {
		t.Fatalf("read updated_at: %v", err)
	}
	return out.UTC()
}
