package uptime

import (
	"context"
	"testing"
	"time"

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

// ---------------------------------------------------------------------------
// ProbeWorker.Timeout / SetJobTimeout - the job-timeout mismatch fix. The
// uptime_probe River job otherwise inherits River's own 60s default
// (river.Config.JobTimeout is unset in cmd/wpmgr/main.go), which only covers
// the reachability pass up to ~40 sites at the production defaults
// (ProbeConcurrency=10, ProbeTimeout=15s); past that, River silently cancels
// the sweep mid-flight with no error explaining which sites were skipped.
// ---------------------------------------------------------------------------

// TestProbeWorker_Timeout mirrors update.TestWorker_Timeout /
// backup.BackupWorker.Timeout's own test coverage: SetJobTimeout threads a
// positive duration straight through to Timeout(), and the zero value (every
// existing NewProbeWorker call site, since none of them call SetJobTimeout)
// reports back as exactly 0 - the documented signal for "fall back to
// river.Config.JobTimeout" (see river's job executor, which uses cmp.Or) -
// rather than an accidental instant timeout.
func TestProbeWorker_Timeout(t *testing.T) {
	t.Run("SetJobTimeout threads a positive duration through to Timeout()", func(t *testing.T) {
		w := &ProbeWorker{}
		w.SetJobTimeout(42 * time.Minute)
		if got := w.Timeout(nil); got != 42*time.Minute {
			t.Fatalf("Timeout() = %v, want 42m (the duration passed to SetJobTimeout)", got)
		}
	})

	t.Run("never calling SetJobTimeout falls back to River's default, not an instant timeout", func(t *testing.T) {
		w := &ProbeWorker{}
		if got := w.Timeout(nil); got != 0 {
			t.Fatalf("Timeout() = %v, want 0 (river falls back to river.Config.JobTimeout on exactly 0)", got)
		}
	})

	t.Run("SetJobTimeout(0) is indistinguishable from never calling it", func(t *testing.T) {
		w := &ProbeWorker{}
		w.SetJobTimeout(42 * time.Minute)
		w.SetJobTimeout(0)
		if got := w.Timeout(nil); got != 0 {
			t.Fatalf("Timeout() = %v, want 0 after SetJobTimeout(0)", got)
		}
	})
}

// TestDeriveProbeJobTimeout pins the arithmetic DeriveProbeJobTimeout uses to
// size the probe sweep's River Timeout(): ceil(maxFleetSize/concurrency) *
// probeTimeout (reachability) + ceil(ceil(maxFleetSize/ratio)/concurrency) *
// appProbeTimeout (app-health, only when wired) + a fixed 2-minute headroom,
// and the documented fallback behavior for each unset/invalid input.
func TestDeriveProbeJobTimeout(t *testing.T) {
	const (
		concurrency      = 10
		probeTimeout     = 15 * time.Second
		probeInterval    = 60 * time.Second
		appProbeInterval = 300 * time.Second
		appProbeTimeout  = 10 * time.Second
	)

	t.Run("production defaults at the default fleet-size ceiling: 50m reachability + 6m40s app + 2m headroom = 58m40s", func(t *testing.T) {
		got := DeriveProbeJobTimeout(concurrency, probeTimeout, probeInterval, appProbeInterval, appProbeTimeout, DefaultMaxFleetSizeForProbeTimeout)
		want := 58*time.Minute + 40*time.Second
		if got != want {
			t.Fatalf("DeriveProbeJobTimeout(...) = %v, want %v", got, want)
		}
	})

	t.Run("maxFleetSize <= 0 falls back to DefaultMaxFleetSizeForProbeTimeout", func(t *testing.T) {
		want := DeriveProbeJobTimeout(concurrency, probeTimeout, probeInterval, appProbeInterval, appProbeTimeout, DefaultMaxFleetSizeForProbeTimeout)
		if got := DeriveProbeJobTimeout(concurrency, probeTimeout, probeInterval, appProbeInterval, appProbeTimeout, 0); got != want {
			t.Fatalf("DeriveProbeJobTimeout(maxFleetSize=0) = %v, want %v (same as the explicit default)", got, want)
		}
		if got := DeriveProbeJobTimeout(concurrency, probeTimeout, probeInterval, appProbeInterval, appProbeTimeout, -1); got != want {
			t.Fatalf("DeriveProbeJobTimeout(maxFleetSize=-1) = %v, want %v (same as the explicit default)", got, want)
		}
	})

	t.Run("reachability-only term matches the documented ceil(N/concurrency)*probeTimeout formula at N=500", func(t *testing.T) {
		// The motivating worked example: at the production defaults a 500-site
		// fleet's reachability pass alone already costs ceil(500/10)*15s =
		// 750s, twelve and a half times River's 60s default.
		got := DeriveProbeJobTimeout(concurrency, probeTimeout, probeInterval, appProbeInterval, 0, 500)
		want := 750*time.Second + 2*time.Minute // + the fixed headroom
		if got != want {
			t.Fatalf("DeriveProbeJobTimeout(maxFleetSize=500, app disabled) = %v, want %v", got, want)
		}
	})

	t.Run("appProbeTimeout <= 0 (app probe never wired via SetAppProber) omits the app pass entirely", func(t *testing.T) {
		withApp := DeriveProbeJobTimeout(concurrency, probeTimeout, probeInterval, appProbeInterval, appProbeTimeout, DefaultMaxFleetSizeForProbeTimeout)
		withoutApp := DeriveProbeJobTimeout(concurrency, probeTimeout, probeInterval, appProbeInterval, 0, DefaultMaxFleetSizeForProbeTimeout)
		if withoutApp >= withApp {
			t.Fatalf("DeriveProbeJobTimeout(appProbeTimeout=0) = %v, want < %v (the app pass with it wired)", withoutApp, withApp)
		}
		wantWithoutApp := 50*time.Minute + 2*time.Minute
		if withoutApp != wantWithoutApp {
			t.Fatalf("DeriveProbeJobTimeout(appProbeTimeout=0) = %v, want %v (reachability + headroom only)", withoutApp, wantWithoutApp)
		}
	})

	t.Run("zero concurrency falls back to 0, not a misleadingly small nonzero budget", func(t *testing.T) {
		if got := DeriveProbeJobTimeout(0, probeTimeout, probeInterval, appProbeInterval, appProbeTimeout, DefaultMaxFleetSizeForProbeTimeout); got != 0 {
			t.Fatalf("DeriveProbeJobTimeout(concurrency=0) = %v, want 0", got)
		}
	})

	t.Run("zero probeTimeout falls back to 0, not a misleadingly small nonzero budget", func(t *testing.T) {
		if got := DeriveProbeJobTimeout(concurrency, 0, probeInterval, appProbeInterval, appProbeTimeout, DefaultMaxFleetSizeForProbeTimeout); got != 0 {
			t.Fatalf("DeriveProbeJobTimeout(probeTimeout=0) = %v, want 0", got)
		}
	})

	t.Run("zero probeInterval/appProbeInterval mirror appProbeDue's own defaults (60s/300s) rather than dividing by zero", func(t *testing.T) {
		got := DeriveProbeJobTimeout(concurrency, probeTimeout, 0, 0, appProbeTimeout, DefaultMaxFleetSizeForProbeTimeout)
		want := DeriveProbeJobTimeout(concurrency, probeTimeout, probeInterval, appProbeInterval, appProbeTimeout, DefaultMaxFleetSizeForProbeTimeout)
		if got != want {
			t.Fatalf("DeriveProbeJobTimeout(probeInterval=0, appProbeInterval=0) = %v, want %v (same as explicit 60s/300s)", got, want)
		}
	})
}

// ---------------------------------------------------------------------------
// Sweep's admission-control backstop - a fleet larger than the configured/
// default ceiling degrades to a partial-but-recorded sweep instead of an
// abrupt River cancellation. See ProbeWorker.Sweep and DeriveProbeJobTimeout.
// ---------------------------------------------------------------------------

func TestSweepBudgetExhausted(t *testing.T) {
	t.Run("no context deadline never trips it (every existing test/call site)", func(t *testing.T) {
		if sweepBudgetExhausted(context.Background(), time.Hour) {
			t.Fatal("sweepBudgetExhausted(no deadline) = true, want false")
		}
	})

	t.Run("admissionBudget <= 0 never trips it even with an imminent deadline", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Millisecond))
		defer cancel()
		if sweepBudgetExhausted(ctx, 0) {
			t.Fatal("sweepBudgetExhausted(admissionBudget=0) = true, want false")
		}
	})

	t.Run("deadline comfortably beyond the admission budget is false", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Hour))
		defer cancel()
		if sweepBudgetExhausted(ctx, 25*time.Second) {
			t.Fatal("sweepBudgetExhausted(deadline 1h away, budget 25s) = true, want false")
		}
	})

	t.Run("deadline within the admission budget is true", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(5*time.Second))
		defer cancel()
		if !sweepBudgetExhausted(ctx, 25*time.Second) {
			t.Fatal("sweepBudgetExhausted(deadline 5s away, budget 25s) = false, want true")
		}
	})
}

func TestProbeWorker_ProbeAdmissionBudget(t *testing.T) {
	t.Run("reachability timeout only when no app prober is wired", func(t *testing.T) {
		w := &ProbeWorker{prober: NewProber(nil, 15*time.Second)}
		if got := w.probeAdmissionBudget(); got != 15*time.Second {
			t.Fatalf("probeAdmissionBudget() = %v, want 15s", got)
		}
	})

	t.Run("reachability + app timeout when the app prober is wired (SetAppProber)", func(t *testing.T) {
		w := &ProbeWorker{prober: NewProber(nil, 15*time.Second), appProber: NewAppProber(nil, 10*time.Second)}
		if got := w.probeAdmissionBudget(); got != 25*time.Second {
			t.Fatalf("probeAdmissionBudget() = %v, want 25s (15s reachability + 10s app)", got)
		}
	})

	t.Run("nil prober is guarded defensively", func(t *testing.T) {
		w := &ProbeWorker{}
		if got := w.probeAdmissionBudget(); got != 0 {
			t.Fatalf("probeAdmissionBudget() = %v, want 0", got)
		}
	})
}
