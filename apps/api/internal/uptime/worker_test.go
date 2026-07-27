package uptime

import (
	"testing"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// appAlertEligible - GH #291 Phase 3 Fix 2: the fire path and the ratio
// query's WHERE clause must use ONE definition of "eligible", never two
// that can drift apart.
// ---------------------------------------------------------------------------

// TestAppAlertEligibleMatchesRatioQueryPredicate pins appAlertEligible's
// three input combinations against the LITERAL predicate
// db/query/alerts.sql's GetTenantAppAlertRatio enforces server-side:
//
//	sas.tenant_id = @tenant_id
//	AND sas.ever_app_up = true
//	AND s.app_alerts_disabled = false
//	AND s.connection_state NOT IN ('revoked', 'archived')
//
// appAlertEligible deliberately covers only the app_alerts_disabled and
// connection_state legs (ever_app_up is enforced one layer down, inside
// EvaluateApp itself - see appAlertEligible's own doc comment for why that
// third leg is not restated here). If GetTenantAppAlertRatio's WHERE clause
// ever changes, this test's expectations must change with it - that
// coupling is the point: it is the mechanism that keeps this Go predicate
// and the SQL predicate from silently drifting apart again.
func TestAppAlertEligibleMatchesRatioQueryPredicate(t *testing.T) {
	cases := []struct {
		name              string
		appAlertsDisabled bool
		connectionState   string
		want              bool
	}{
		{"connected, alerting on", false, "connected", true},
		{"degraded, alerting on", false, "degraded", true},
		{"disconnected, alerting on", false, "disconnected", true},
		{"empty connection_state (legacy/never-set row), alerting on", false, "", true},
		{"connected, alerting disabled", true, "connected", false},
		{"revoked, alerting on", false, "revoked", false},
		{"archived, alerting on", false, "archived", false},
		{"revoked AND alerting disabled", true, "revoked", false},
		{"archived AND alerting disabled", true, "archived", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := EnrolledSite{
				ID:                uuid.New(),
				TenantID:          uuid.New(),
				AppAlertsDisabled: tc.appAlertsDisabled,
				ConnectionState:   tc.connectionState,
			}
			if got := appAlertEligible(s); got != tc.want {
				t.Errorf("appAlertEligible(AppAlertsDisabled=%v, ConnectionState=%q) = %v, want %v",
					tc.appAlertsDisabled, tc.connectionState, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// appFireDownOnly - GH #291 Phase 3 Fix 1: on the tick an aggregate speaks
// for the RECOVERY set, a same-tick NEW incident must still be dispatched
// individually.
// ---------------------------------------------------------------------------

func TestAppFireDownOnlyFiltersRecoveries(t *testing.T) {
	down1 := pendingAppFire{site: EnrolledSite{ID: uuid.New()}, tr: AppTransition{FireDown: true}}
	down2 := pendingAppFire{site: EnrolledSite{ID: uuid.New()}, tr: AppTransition{FireDown: true}}
	recovery1 := pendingAppFire{site: EnrolledSite{ID: uuid.New()}, tr: AppTransition{FireRecovery: true}}
	recovery2 := pendingAppFire{site: EnrolledSite{ID: uuid.New()}, tr: AppTransition{FireRecovery: true}}

	got := appFireDownOnly([]pendingAppFire{recovery1, down1, recovery2, down2})
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 FireDown-typed fires, got %d: %+v", len(got), got)
	}
	for _, p := range got {
		if !p.tr.FireDown || p.tr.FireRecovery {
			t.Fatalf("appFireDownOnly leaked a non-FireDown entry: %+v", p)
		}
	}

	// Nil/empty input must not panic and must return an empty (non-nil)
	// slice - fireAppIndividually ranges over it unconditionally.
	if out := appFireDownOnly(nil); len(out) != 0 {
		t.Fatalf("appFireDownOnly(nil) = %+v, want empty", out)
	}
	// All-recovery input filters down to nothing.
	if out := appFireDownOnly([]pendingAppFire{recovery1, recovery2}); len(out) != 0 {
		t.Fatalf("appFireDownOnly(all recoveries) = %+v, want empty", out)
	}
}
