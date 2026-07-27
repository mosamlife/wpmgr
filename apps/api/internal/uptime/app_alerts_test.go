package uptime

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// EvaluateApp - mirrors TestEvaluateTransitionDedupe/TestEvaluateThresholdOne
// (alerts_test.go) in shape, plus the two rules unique to the app-health
// machine: unknown never advances/resets, and ever_app_up gates FireDown.
// ---------------------------------------------------------------------------

// TestEvaluateAppNeverHealthySiteNeverFires is rule 2: a site that has never
// been conclusively observed healthy (EverAppUp=false) must NEVER fire an app
// alert, no matter how many consecutive conclusive-false verdicts it racks
// up - this is the core "almost always a blocked REST route, not a broken
// site" safeguard.
func TestEvaluateAppNeverHealthySiteNeverFires(t *testing.T) {
	const threshold = 3
	now := time.Now()
	st := AppAlertState{LastStatus: StatusUnknown}

	for i := 0; i < 10; i++ {
		tr := EvaluateApp(st, AppVerdictDown, threshold, now)
		if tr.FireDown {
			t.Fatalf("verdict %d: FireDown must never fire for a site with EverAppUp=false, got %+v", i, tr)
		}
		if tr.NewState.InIncident {
			t.Fatalf("verdict %d: InIncident must never become true for a site with EverAppUp=false, got %+v", i, tr.NewState)
		}
		st = tr.NewState
	}
	// The counter itself is still free to accumulate (harmless - it can
	// never gate a fire without EverAppUp) - just confirm it doesn't panic
	// or wrap unexpectedly.
	if st.ConsecutiveDown != 10 {
		t.Fatalf("expected consecutive_down=10, got %d", st.ConsecutiveDown)
	}
}

// TestEvaluateAppFiresOnceAfterEverHealthy proves the full lifecycle: a site
// first proves itself healthy (EverAppUp becomes sticky true), THEN crosses
// the down threshold (fires exactly once), stays de-duped while down, and
// recovers with exactly one recovery alert.
func TestEvaluateAppFiresOnceAfterEverHealthy(t *testing.T) {
	const threshold = 2
	now := time.Now()
	st := AppAlertState{LastStatus: StatusUnknown}

	// First ever verdict: conclusively up. Sets EverAppUp, no transition (no
	// prior incident to recover from).
	tr := EvaluateApp(st, AppVerdictUp, threshold, now)
	if tr.FireDown || tr.FireRecovery {
		t.Fatalf("first up verdict: expected no alert, got %+v", tr)
	}
	if !tr.NewState.EverAppUp {
		t.Fatal("first up verdict: expected EverAppUp=true")
	}
	st = tr.NewState

	// Down 1/2: below threshold, no alert.
	tr = EvaluateApp(st, AppVerdictDown, threshold, now)
	if tr.FireDown || tr.FireRecovery {
		t.Fatalf("down 1: expected no alert, got %+v", tr)
	}
	st = tr.NewState

	// Down 2/2: crosses threshold, EverAppUp=true - fires exactly once.
	tr = EvaluateApp(st, AppVerdictDown, threshold, now)
	if !tr.FireDown || tr.FireRecovery {
		t.Fatalf("down 2: expected exactly one FireDown, got %+v", tr)
	}
	if !tr.NewState.InIncident {
		t.Fatal("down 2: expected in_incident=true")
	}
	st = tr.NewState

	// Down 3: already in incident - de-duped.
	tr = EvaluateApp(st, AppVerdictDown, threshold, now)
	if tr.FireDown || tr.FireRecovery {
		t.Fatalf("down 3: expected de-dupe (no alert), got %+v", tr)
	}
	st = tr.NewState

	// Recovery: conclusive up while in incident - fires exactly one recovery.
	tr = EvaluateApp(st, AppVerdictUp, threshold, now)
	if !tr.FireRecovery || tr.FireDown {
		t.Fatalf("recovery: expected exactly one FireRecovery, got %+v", tr)
	}
	if tr.NewState.InIncident || tr.NewState.ConsecutiveDown != 0 {
		t.Fatalf("recovery: unexpected recovered state %+v", tr.NewState)
	}
	if !tr.NewState.EverAppUp {
		t.Fatal("recovery: EverAppUp must remain true (sticky)")
	}
	st = tr.NewState

	// Second recovery verdict: not in incident - no repeat alert.
	tr = EvaluateApp(st, AppVerdictUp, threshold, now)
	if tr.FireDown || tr.FireRecovery {
		t.Fatalf("steady-up: expected no alert, got %+v", tr)
	}
}

// TestEvaluateAppUnknownNeverAdvancesOrResets is rule 1: an unknown verdict
// must leave ConsecutiveDown, InIncident, LastStatus, and EverAppUp
// COMPLETELY untouched - not merely "no alert fires", but no state mutation
// at all (NewState == prev, field for field).
func TestEvaluateAppUnknownNeverAdvancesOrResets(t *testing.T) {
	const threshold = 3
	now := time.Now()

	// Case 1: unknown must not ADVANCE a down streak.
	st := AppAlertState{LastStatus: StatusDown, ConsecutiveDown: 2, EverAppUp: true}
	tr := EvaluateApp(st, AppVerdictUnknown, threshold, now)
	if tr.FireDown || tr.FireRecovery {
		t.Fatalf("unknown must never fire an alert, got %+v", tr)
	}
	if tr.NewState != st {
		t.Fatalf("unknown must leave state completely untouched: got %+v, want %+v", tr.NewState, st)
	}

	// Case 2: unknown must not RESET an accumulating down streak back to 0 -
	// simulating a site that flaps between conclusive-false and
	// inconclusive (e.g. an intermittently-blocking WAF). If unknown reset
	// the counter, this site could NEVER cross the threshold no matter how
	// many genuine conclusive-false observations it racked up.
	st = AppAlertState{LastStatus: StatusDown, ConsecutiveDown: 1, EverAppUp: true}
	tr = EvaluateApp(st, AppVerdictUnknown, threshold, now)
	if tr.NewState.ConsecutiveDown != 1 {
		t.Fatalf("unknown must not reset consecutive_down: got %d, want 1 (unchanged)", tr.NewState.ConsecutiveDown)
	}
	// Confirm the streak CAN still complete across an unknown in the middle.
	tr = EvaluateApp(tr.NewState, AppVerdictDown, threshold, now) // 2/3
	if tr.FireDown {
		t.Fatalf("2/%d: unexpected early fire, got %+v", threshold, tr)
	}
	tr = EvaluateApp(tr.NewState, AppVerdictUnknown, threshold, now) // still 2/3, untouched
	if tr.NewState.ConsecutiveDown != 2 {
		t.Fatalf("mid-streak unknown must not reset: got %d, want 2", tr.NewState.ConsecutiveDown)
	}
	tr = EvaluateApp(tr.NewState, AppVerdictDown, threshold, now) // 3/3 - fires
	if !tr.FireDown {
		t.Fatalf("3/%d after an interleaved unknown: expected FireDown, got %+v", threshold, tr)
	}

	// Case 3: unknown must not clear an open incident (no false recovery).
	st = AppAlertState{LastStatus: StatusDown, ConsecutiveDown: 5, InIncident: true, EverAppUp: true}
	tr = EvaluateApp(st, AppVerdictUnknown, threshold, now)
	if tr.FireRecovery {
		t.Fatal("unknown must never fire a recovery")
	}
	if !tr.NewState.InIncident {
		t.Fatal("unknown must not clear in_incident")
	}
}

// TestEvaluateAppThresholdClampedToOne mirrors TestEvaluateThresholdOne.
func TestEvaluateAppThresholdClampedToOne(t *testing.T) {
	st := AppAlertState{EverAppUp: true}
	tr := EvaluateApp(st, AppVerdictDown, 0, time.Now())
	if !tr.FireDown {
		t.Fatalf("threshold<1 clamped to 1: expected immediate FireDown, got %+v", tr)
	}
}

// ---------------------------------------------------------------------------
// EvaluateAppBreaker - the tenant-wide circuit breaker's own transition-only
// de-dupe, mirroring Evaluate/EvaluateApp's shape.
// ---------------------------------------------------------------------------

func TestEvaluateAppBreakerTripsOnceAndRecoversOnce(t *testing.T) {
	now := time.Now()
	tenant := uuid.New()
	st := AppBreakerState{TenantID: tenant}

	// Ratio below threshold: never trips.
	tr := EvaluateAppBreaker(st, false, 0, now)
	if tr.FireTrip || tr.FireRecovery || tr.FireUpdate {
		t.Fatalf("wantTrip=false on a never-tripped breaker: expected no alert, got %+v", tr)
	}
	st = tr.NewState

	// Ratio crosses threshold: trips exactly once.
	tr = EvaluateAppBreaker(st, true, 3, now)
	if !tr.FireTrip || tr.FireRecovery || tr.FireUpdate {
		t.Fatalf("expected exactly one FireTrip, got %+v", tr)
	}
	if !tr.NewState.Tripped || tr.NewState.TrippedAt == nil {
		t.Fatalf("expected Tripped=true with TrippedAt set, got %+v", tr.NewState)
	}
	if tr.NewState.LastDownCount != 3 {
		t.Fatalf("expected LastDownCount=3 after trip, got %d", tr.NewState.LastDownCount)
	}
	st = tr.NewState

	// Still above threshold on the next tick, SAME down count: de-duped, no
	// repeat trip AND no update (down did not worsen past the checkpoint).
	tr = EvaluateAppBreaker(st, true, 3, now)
	if tr.FireTrip || tr.FireRecovery || tr.FireUpdate {
		t.Fatalf("already-tripped breaker still above threshold at the SAME down count: expected no alert (de-dupe), got %+v", tr)
	}
	st = tr.NewState

	// Ratio drops back below threshold: recovers exactly once.
	tr = EvaluateAppBreaker(st, false, 1, now)
	if !tr.FireRecovery || tr.FireTrip || tr.FireUpdate {
		t.Fatalf("expected exactly one FireRecovery, got %+v", tr)
	}
	if tr.NewState.Tripped {
		t.Fatal("expected Tripped=false after recovery")
	}
	if tr.NewState.LastDownCount != 1 {
		t.Fatalf("expected LastDownCount=1 after recovery (the count AT recovery time), got %d", tr.NewState.LastDownCount)
	}
	st = tr.NewState

	// Steady state below threshold: no repeat recovery.
	tr = EvaluateAppBreaker(st, false, 0, now)
	if tr.FireTrip || tr.FireRecovery || tr.FireUpdate {
		t.Fatalf("steady state below threshold: expected no alert, got %+v", tr)
	}
}

// TestEvaluateAppBreakerFireUpdateRequiresMaterialWorseningAndMinInterval is
// GH #291 Phase 3 Fix 3: while tripped, an updated aggregate fires ONLY when
// BOTH the down count has increased past the last-notified checkpoint AND at
// least appAlertBreakerUpdateMinInterval has elapsed since the last
// notification - either condition alone must not be enough, so a ratio that
// merely oscillates near the boundary cannot itself become a flood.
func TestEvaluateAppBreakerFireUpdateRequiresMaterialWorseningAndMinInterval(t *testing.T) {
	tenant := uuid.New()
	t0 := time.Now()

	// Trip at down=2.
	tr := EvaluateAppBreaker(AppBreakerState{TenantID: tenant}, true, 2, t0)
	if !tr.FireTrip {
		t.Fatalf("expected FireTrip, got %+v", tr)
	}
	tripped := tr.NewState // Tripped=true, LastDownCount=2, LastAlertAt=t0

	// Case A: materially worse (down=5 > checkpoint 2) but the min interval
	// has NOT elapsed since the trip: suppressed, no update, checkpoint
	// unmoved.
	tSoon := t0.Add(appAlertBreakerUpdateMinInterval - time.Minute)
	tr = EvaluateAppBreaker(tripped, true, 5, tSoon)
	if tr.FireUpdate || tr.FireTrip || tr.FireRecovery {
		t.Fatalf("worse but before the min interval: expected no alert, got %+v", tr)
	}
	if tr.NewState.LastDownCount != 2 {
		t.Fatalf("a suppressed update must not move the checkpoint: got %d, want 2", tr.NewState.LastDownCount)
	}

	// Case B: the min interval HAS elapsed, but down count is UNCHANGED
	// (equal to the checkpoint, 2): no update.
	tPastInterval := t0.Add(appAlertBreakerUpdateMinInterval + time.Minute)
	tr = EvaluateAppBreaker(tripped, true, 2, tPastInterval)
	if tr.FireUpdate {
		t.Fatalf("interval elapsed but down count unchanged: expected no update, got %+v", tr)
	}

	// Case C: interval elapsed, but down count DECREASED (still tripped, so
	// not a full recovery either): no update.
	tr = EvaluateAppBreaker(tripped, true, 1, tPastInterval)
	if tr.FireUpdate {
		t.Fatalf("interval elapsed but down count decreased: expected no update, got %+v", tr)
	}

	// Case D: BOTH conditions satisfied - material increase (down=6 >
	// checkpoint 2) AND the min interval has elapsed since the trip - fires
	// exactly one update, and re-checkpoints LastDownCount/LastAlertAt to
	// this moment.
	tr = EvaluateAppBreaker(tripped, true, 6, tPastInterval)
	if !tr.FireUpdate || tr.FireTrip || tr.FireRecovery {
		t.Fatalf("material worsening past the min interval: expected exactly one FireUpdate, got %+v", tr)
	}
	if tr.NewState.LastDownCount != 6 {
		t.Fatalf("expected LastDownCount re-checkpointed to 6, got %d", tr.NewState.LastDownCount)
	}
	if tr.NewState.LastAlertAt == nil || !tr.NewState.LastAlertAt.Equal(tPastInterval) {
		t.Fatalf("expected LastAlertAt re-checkpointed to %v, got %v", tPastInterval, tr.NewState.LastAlertAt)
	}
	updated := tr.NewState

	// Case E (flood guard): immediately after D, a further big jump (down=
	// 100) but the min interval has NOT elapsed since the UPDATE (only 1s
	// passed): suppressed - the min interval guard applies to every update,
	// not just the first one after a trip.
	tr = EvaluateAppBreaker(updated, true, 100, tPastInterval.Add(time.Second))
	if tr.FireUpdate {
		t.Fatalf("second update inside the min interval: expected no alert (flood guard), got %+v", tr)
	}
}

// ---------------------------------------------------------------------------
// ValidateAppProbePath
// ---------------------------------------------------------------------------

func TestValidateAppProbePath(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "empty clears override", in: "", want: ""},
		{name: "whitespace-only clears override", in: "   ", want: ""},
		{name: "valid path", in: "/healthz", want: "/healthz"},
		{name: "valid nested path with query-like suffix", in: "/wp-content/plugins/foo/health.php", want: "/wp-content/plugins/foo/health.php"},
		{name: "trimmed", in: "  /healthz  ", want: "/healthz"},
		{name: "missing leading slash", in: "healthz", wantErr: true},
		{name: "protocol-relative", in: "//evil.example.com/x", wantErr: true},
		{name: "absolute URL with scheme", in: "https://evil.example.com/x", wantErr: true},
		{name: "scheme embedded mid-path", in: "/redirect?to=https://evil.example.com", wantErr: true},
		{name: "traversal", in: "/../../etc/passwd", wantErr: true},
		{name: "traversal mid-path", in: "/a/../b", wantErr: true},
		{name: "embedded whitespace", in: "/health check", wantErr: true},
		{name: "embedded newline (header injection attempt)", in: "/health\r\nX-Injected: 1", wantErr: true},
		{name: "too long", in: "/" + string(make([]byte, appProbePathMaxLen)), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateAppProbePath(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ValidateAppProbePath(%q) = %q, nil; want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateAppProbePath(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ValidateAppProbePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestClassifyAppVerdict mirrors AppProbeResult.Up's own tri-state contract.
func TestClassifyAppVerdict(t *testing.T) {
	tr, f := true, false
	cases := []struct {
		name string
		in   *bool
		want AppVerdict
	}{
		{"nil is unknown", nil, AppVerdictUnknown},
		{"true is up", &tr, AppVerdictUp},
		{"false is down", &f, AppVerdictDown},
	}
	for _, tc := range cases {
		if got := ClassifyAppVerdict(tc.in); got != tc.want {
			t.Errorf("%s: ClassifyAppVerdict(%v) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}
