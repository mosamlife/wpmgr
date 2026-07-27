// GH #291 Phase 3: application-health ALERTING.
//
// Phase 2 (app_probe.go) collects and displays a three-valued app_up signal
// and deliberately fires nothing. This file adds a SEPARATE, conservative
// state machine that mirrors alerts.go's Evaluate exactly in shape (locked
// vocabulary: SELECT FOR UPDATE + a pure Evaluate-style function, transition-
// only alerting) without touching a single line of it - the reachability
// alert machine is FROZEN by this phase's own constraints.
package uptime

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// AppVerdict is the tri-state classification EvaluateApp consumes, derived
// from an AppProbeResult.Up pointer (nil=unknown, non-nil=conclusive).
// Kept as its own type (rather than passing the raw *bool around) so the
// three states are named at every call site instead of re-deriving nil
// checks in more than one place.
type AppVerdict int

const (
	// AppVerdictUnknown: the app probe did not reach a conclusive verdict
	// this tick (or did not run at all this tick). NEVER advances or resets
	// EvaluateApp's counters - see EvaluateApp's doc comment for why.
	AppVerdictUnknown AppVerdict = iota
	// AppVerdictUp: the app probe conclusively found the application healthy.
	AppVerdictUp
	// AppVerdictDown: the app probe conclusively found the application
	// broken (the ONLY two conclusive-false conditions app_probe.go ever
	// classifies: HTTP 500, or a 200 carrying the WordPress fatal-error
	// signature).
	AppVerdictDown
)

// ClassifyAppVerdict maps an AppProbeResult.Up pointer onto the three-valued
// AppVerdict EvaluateApp consumes.
func ClassifyAppVerdict(up *bool) AppVerdict {
	if up == nil {
		return AppVerdictUnknown
	}
	if *up {
		return AppVerdictUp
	}
	return AppVerdictDown
}

// AppAlertState is a site's durable app-health alert transition memory -
// the app-health sibling of AlertState (alerts.go), persisted in
// site_app_alert_state (m108) inside the SAME transaction as AlertState
// (see Repo.TransitionAlertState).
type AppAlertState struct {
	SiteID          uuid.UUID
	TenantID        uuid.UUID
	LastStatus      string // StatusUp | StatusDown | StatusUnknown (shared with AlertState)
	ConsecutiveDown int32
	InIncident      bool
	// EverAppUp is STICKY: set true the first time a CONCLUSIVE app_up=true
	// verdict is observed for this site, and never reset false again - see
	// EvaluateApp rule 2. Gates whether this site may EVER fire an app alert.
	EverAppUp   bool
	LastAlertAt *time.Time
}

// AppTransition is the decision EvaluateApp makes for one app-probe verdict
// applied to a site's prior AppAlertState - the app-health sibling of
// Transition (alerts.go).
type AppTransition struct {
	// NewState is the state to persist after applying the verdict. On an
	// AppVerdictUnknown verdict this is EXACTLY prev (no field changes at
	// all - not even a status/updated_at bump), so a caller that skips
	// persisting when nothing changed pays no write cost on the common case
	// (unknown verdicts, and ticks where the app probe did not run at all).
	NewState AppAlertState
	// FireDown is true when this verdict crossed the down threshold for the
	// first time (transition into an app-health incident) AND EverAppUp was
	// already true - fire ONE app-down alert.
	FireDown bool
	// FireRecovery is true when this verdict recovered an open app-health
	// incident - fire ONE app-recovery alert.
	FireRecovery bool
}

// EvaluateApp is the pure app-health transition/de-dupe logic - the
// app-health sibling of Evaluate (alerts.go), deliberately kept as a
// SEPARATE function (never a generalized/shared one) so the FROZEN
// reachability machine is never touched by this feature. Three rules the
// design requires, all enforced here:
//
//  1. Only a CONCLUSIVE app_up=false (verdict == AppVerdictDown) counts
//     toward the threshold. AppVerdictUnknown NEVER advances ConsecutiveDown
//     and NEVER resets it either. Resetting on unknown would let a site that
//     flaps between a genuine conclusive-false and an inconclusive result
//     (e.g. a WAF that intermittently blocks the REST probe) never
//     accumulate enough CONSECUTIVE conclusive-false observations to ever
//     cross the threshold - permanently masking a real incident, the
//     opposite failure mode from the one this feature exists to catch. Not
//     touching the counter on unknown is the safer choice: the worst case is
//     a merely-stale count (an incident that takes a little longer to
//     confirm because unknowns don't help it along), never a counter that
//     can be reset indefinitely by an attacker- or infra-controlled signal
//     the probe does not trust in the first place (see app_probe.go's own
//     "prefer UNKNOWN whenever conclusive evidence is unavailable" rule -
//     the same conservatism applies one layer up, here, to what unknown is
//     allowed to do to alerting state).
//  2. ever_app_up gates FireDown: a site that has never been conclusively
//     observed healthy may NEVER fire an app alert - almost always a site
//     whose REST route is permanently blocked/disabled, not a site that
//     broke. ever_app_up is set (and ONLY ever set - never cleared) on the
//     first AppVerdictUp. Recovery (FireRecovery) needs no separate gate:
//     InIncident can only be true after a prior FireDown, which already
//     required ever_app_up, and ever_app_up is sticky, so by construction
//     ever_app_up is already true whenever a recovery is possible.
//  3. Transition-only, exactly like Evaluate: one alert when the incident
//     opens, one when it recovers, never a repeating flood.
func EvaluateApp(prev AppAlertState, verdict AppVerdict, threshold int, now time.Time) AppTransition {
	if threshold < 1 {
		threshold = 1
	}
	t := AppTransition{}

	switch verdict {
	case AppVerdictUnknown:
		// Rule 1: no observation strong enough to move the counter either
		// way. No state change, no transition - NewState is the untouched
		// prior state (see the NewState doc comment above).
		t.NewState = prev
		return t

	case AppVerdictUp:
		next := prev
		next.LastStatus = StatusUp
		next.ConsecutiveDown = 0
		next.EverAppUp = true
		if prev.InIncident {
			next.InIncident = false
			t.FireRecovery = true
			ts := now
			next.LastAlertAt = &ts
		}
		t.NewState = next
		return t

	default: // AppVerdictDown
		next := prev
		next.LastStatus = StatusDown
		next.ConsecutiveDown = prev.ConsecutiveDown + 1
		if !prev.InIncident && prev.EverAppUp && int(next.ConsecutiveDown) >= threshold {
			next.InIncident = true
			t.FireDown = true
			ts := now
			next.LastAlertAt = &ts
		}
		t.NewState = next
		return t
	}
}

// ---------------------------------------------------------------------------
// Fleet circuit breaker (GH #291 Phase 3, section 2)
// ---------------------------------------------------------------------------

// AppBreakerState is a tenant's durable circuit-breaker transition memory,
// persisted in tenant_app_alert_breaker (m108) - the tenant-wide sibling of
// AppAlertState, evaluated once per sweep tick (not per site).
type AppBreakerState struct {
	TenantID    uuid.UUID
	Tripped     bool
	TrippedAt   *time.Time
	LastAlertAt *time.Time
	// LastDownCount is the down count AT THE TIME OF THE LAST notification
	// (trip, update, or recovery) - never bumped on a silent steady-state
	// tick. Fix 3's wantBreakerUpdate compares the CURRENT down count
	// against this to detect "materially worse since we last said
	// anything" without a second table.
	LastDownCount int
}

// AppBreakerTransition is the decision EvaluateAppBreaker makes for one
// ratio evaluation applied to a tenant's prior AppBreakerState.
type AppBreakerTransition struct {
	NewState AppBreakerState
	// FireTrip is true when the ratio crossed the trip threshold for the
	// first time (transition into "suppress and collapse") - fire ONE
	// aggregate down notification.
	FireTrip bool
	// FireRecovery is true when the ratio dropped back below threshold
	// while tripped - fire ONE aggregate recovery notification.
	FireRecovery bool
	// FireUpdate (GH #291 Phase 3 Fix 3) is true when the breaker REMAINS
	// tripped this tick but the down count has materially worsened since
	// the last notification and at least appAlertBreakerUpdateMinInterval
	// has elapsed - fire ONE "still suppressed, now worse" aggregate
	// notification, so an operator is never left believing the situation is
	// still whatever it was at the original trip time. Mutually exclusive
	// with FireTrip/FireRecovery (see EvaluateAppBreaker's switch).
	FireUpdate bool
}

// appAlertBreakerUpdateMinInterval is the minimum time between two "still
// tripped, situation update" aggregate notifications for the SAME tenant -
// the rate-limit guard that keeps Fix 3's material-change resend from
// itself becoming a flood if the down count oscillates up and down near the
// trip boundary tick after tick.
const appAlertBreakerUpdateMinInterval = 30 * time.Minute

// wantBreakerUpdate reports whether an ALREADY-tripped breaker should send
// an updated aggregate notification this tick (Fix 3): the down count
// materially worsened since the last notification (ANY increase - the
// breaker's own minAppAlertBreakerDownCount floor already keeps the
// population this applies to small, so integer-count granularity needs no
// further "materiality" threshold on top) AND at least
// appAlertBreakerUpdateMinInterval has elapsed since the last one.
func wantBreakerUpdate(prev AppBreakerState, down int, now time.Time) bool {
	if down <= prev.LastDownCount {
		return false
	}
	if prev.LastAlertAt != nil && now.Sub(*prev.LastAlertAt) < appAlertBreakerUpdateMinInterval {
		return false
	}
	return true
}

// EvaluateAppBreaker is the pure transition/de-dupe logic for the fleet
// circuit breaker - mirrors Evaluate/EvaluateApp's shape exactly
// (transition-only: one alert when it trips, one when it recovers, never a
// repeating flood while the ratio stays above/below threshold on successive
// ticks) - PLUS the FireUpdate case (Fix 3), which is deliberately NOT a
// repeating flood either: it requires both a material worsening AND the
// minimum interval, so a ratio that merely oscillates near the boundary
// cannot cause more than one update per appAlertBreakerUpdateMinInterval.
// wantTrip is computed by the caller (ProbeWorker.resolveAppAlerts) from
// GetTenantAppAlertRatio - eligible > 0 && down/eligible > the configured
// ratio (a strict ">" - "MORE than a configurable ratio", per the design).
// down is the CURRENT down count from that same call, threaded through
// ONLY for the FireUpdate decision - it plays no role in wantTrip itself,
// which the caller has already decided.
func EvaluateAppBreaker(prev AppBreakerState, wantTrip bool, down int, now time.Time) AppBreakerTransition {
	t := AppBreakerTransition{}
	next := prev
	switch {
	case wantTrip && !prev.Tripped:
		next.Tripped = true
		ts := now
		next.TrippedAt = &ts
		next.LastAlertAt = &ts
		next.LastDownCount = down
		t.FireTrip = true
	case !wantTrip && prev.Tripped:
		next.Tripped = false
		ts := now
		next.LastAlertAt = &ts
		next.LastDownCount = down
		t.FireRecovery = true
	case wantTrip && prev.Tripped:
		if wantBreakerUpdate(prev, down, now) {
			ts := now
			next.LastAlertAt = &ts
			next.LastDownCount = down
			t.FireUpdate = true
		}
	}
	t.NewState = next
	return t
}

// ---------------------------------------------------------------------------
// Per-site app-health settings (GH #291 Phase 3, section 3): the B3 override
// path (sites.app_probe_path, added inert in m107) and the per-site alerting
// opt-out (sites.app_alerts_disabled, m108).
// ---------------------------------------------------------------------------

// AppHealthSettings is the operator-facing per-site app-health settings
// resource (GET/PUT /sites/{siteId}/app-health-settings).
type AppHealthSettings struct {
	SiteID            uuid.UUID
	TenantID          uuid.UUID
	AppProbePath      string
	AppAlertsDisabled bool
}

// appProbePathMaxLen bounds the stored override path length - generous for
// any real WordPress permalink/REST path, small enough to keep the column
// and every log line it appears in bounded.
const appProbePathMaxLen = 512

// ValidateAppProbePath validates an operator-supplied B3 override path
// (sites.app_probe_path). It must be a bare site-relative path - no scheme,
// no host, no traversal - because AppProber.buildURL (app_probe.go)
// concatenates it directly onto the site's own trusted origin; this
// validation is defense-in-depth data hygiene, not the only thing standing
// between an operator and an SSRF (buildURL never re-parses this value as an
// independent URL, so it cannot itself redirect the probe to a different
// host). An empty string is valid and clears the override back to
// auto-detect (B1 /wp-json/, falling back to B2 ?rest_route=/).
func ValidateAppProbePath(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", nil
	}
	if len(p) > appProbePathMaxLen {
		return "", domain.Validation("app_probe_path_too_long",
			fmt.Sprintf("app_probe_path must be at most %d characters", appProbePathMaxLen))
	}
	if strings.ContainsAny(p, " \t\r\n") {
		return "", domain.Validation("app_probe_path_invalid", "app_probe_path must not contain whitespace")
	}
	if !strings.HasPrefix(p, "/") {
		return "", domain.Validation("app_probe_path_invalid", "app_probe_path must start with / (a site-relative path)")
	}
	if strings.HasPrefix(p, "//") {
		return "", domain.Validation("app_probe_path_invalid", "app_probe_path must not be protocol-relative (no leading //)")
	}
	if strings.Contains(p, "://") {
		return "", domain.Validation("app_probe_path_invalid", "app_probe_path must not contain a scheme")
	}
	if strings.Contains(p, "..") {
		return "", domain.Validation("app_probe_path_invalid", "app_probe_path must not contain path traversal (..)")
	}
	u, err := url.Parse(p)
	if err != nil || u.Host != "" || u.Scheme != "" || u.User != nil {
		return "", domain.Validation("app_probe_path_invalid", "app_probe_path must be a bare site-relative path")
	}
	return p, nil
}
