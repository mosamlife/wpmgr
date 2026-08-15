package uptime

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Repo is the Postgres persistence for M5 uptime: the cross-tenant probe-job
// reads (under app.agent), the per-site health/alert-state writes, and the
// tenant-scoped alert-config CRUD. Every tenant-scoped method runs under RLS.
type Repo interface {
	// Probe-job path (app.agent GUC, cross-tenant).
	//
	// ListEnrolledForProbe is the UNFILTERED whole-fleet enumeration. It is
	// what the CRON-KICKER uses and it must stay unfiltered - see
	// ListEnrolledForMonitoringProbe below, and cron_kick.go's Kick, for why
	// filtering this one would stop BACKUPS on a paused site.
	ListEnrolledForProbe(ctx context.Context) ([]EnrolledSite, error)

	// ListEnrolledForMonitoringProbe is ListEnrolledForProbe restricted to
	// sites whose MONITORING is active (sites.monitoring_paused_at IS NULL,
	// m117 / GH #414). It is the enumeration the uptime PROBE SWEEP uses, and
	// it exists as a separate method rather than as a predicate added to
	// ListEnrolledForProbe because the two callers want different fleets:
	//
	//   - ProbeWorker.Sweep      wants MONITORED sites. Pause means "stop
	//                            watching this site and stop telling me about
	//                            it", so a paused site must not be probed.
	//   - CronKicker.Kick        wants EVERY enrolled site, paused included.
	//                            The kick is a GET to the site's own
	//                            wp-cron.php; it is what boots PHP on a fully
	//                            page-cached site so the site's WP-Cron queue
	//                            drains, and that queue is what runs the
	//                            agent's heartbeats and its BACKUP work. Give
	//                            this method to the kicker and pausing
	//                            "monitoring" silently stops backups on
	//                            page-cached sites - the one failure people do
	//                            not recover from. GH #414 phase 2 scopes
	//                            pause to uptime probing and uptime alerting
	//                            and to nothing else; this split is where that
	//                            scope is actually enforced.
	//
	// Written as raw SQL rather than a sqlc query for the same reason the
	// fleet reads below are: db/query/**.sql is database-engineer's tree, and
	// this predicate needs no schema change (m117 already landed the column).
	// If it later earns a place in db/query/sites.sql, that is a
	// database-engineer change plus a regeneration, not a hand-sync.
	ListEnrolledForMonitoringProbe(ctx context.Context) ([]EnrolledSite, error)

	// IsMonitoringPaused reports whether sites.monitoring_paused_at is set for
	// one site, read FRESH at dispatch time (app.agent GUC, cross-tenant).
	//
	// This is the second half of the belt-and-braces. The query filter above
	// only decides which sites a sweep ADMITS; phase 1 deliberately does not
	// drain jobs that are already queued, so a probe admitted a second before
	// the pause landed is still in flight and will still reach the alert
	// dispatch. A stale snapshot cannot see that pause - only a fresh read
	// can - so the fire path re-reads here. A missing site reads as NOT
	// paused: fail towards alerting, never towards silence.
	//
	// The cost is one small indexed-by-primary-key read per FIRED alert, not
	// per probed site: fire() and fireApp() are only reached on an actual
	// transition, which is rare by construction.
	IsMonitoringPaused(ctx context.Context, siteID uuid.UUID) (bool, error)

	SetSiteHealth(ctx context.Context, siteID uuid.UUID, status string) (bool, error)
	GetAlertState(ctx context.Context, siteID uuid.UUID) (AlertState, bool, error)
	UpsertAlertState(ctx context.Context, st AlertState) error

	// TransitionAlertState atomically reads (with a row lock via FOR UPDATE),
	// evaluates, and persists the next alert state for one site in a SINGLE
	// transaction. This is the probe worker's entry point — it MUST be used
	// instead of a separate GetAlertState + UpsertAlertState pair, which is a
	// classic lost-update race: when a sweep runs longer than the probe
	// interval (exactly what happens during a real, fleet-wide outage — down
	// probes each take up to the probe timeout, so a sweep with many down
	// sites can overlap the NEXT periodic sweep), two overlapping sweeps for
	// the same site would each read the same stale ConsecutiveDown and never
	// let it accumulate past 1, so the down threshold is never crossed and no
	// alert ever fires — silently, with no error to log.
	//
	// It ALSO opens/closes the site's site_incidents row (M94, GH #148) in the
	// SAME transaction as the alert-state write: a FireDown transition opens
	// an incident, a FireRecovery transition closes it, and — defensively — a
	// probe that observes the site already in_incident with neither flag set
	// "adopts" (idempotently opens, a no-op if already open) an incident row
	// in case one was somehow missed. httpStatus/reason carry the triggering
	// probe's HTTP status and error text onto the incident row.
	//
	// m108 (GH #291 Phase 3): ALSO evaluates and persists the app-health
	// alert transition (site_app_alert_state) inside this SAME transaction,
	// so the two state machines stay consistent and race-free - never a
	// second round-trip. appAttempted is false on ticks where the app probe
	// did not run for this site at all (see appProbeDue); when true, appUp
	// is the tri-state verdict (nil=unknown) and appReason is the triggering
	// AppProbeReason*. The reachability logic above this doc comment is
	// COMPLETELY UNCHANGED by this addition - see EvaluateApp for the
	// app-health rules.
	TransitionAlertState(ctx context.Context, siteID, tenantID uuid.UUID, up bool, threshold int, now time.Time, httpStatus int, reason string,
		appAttempted bool, appUp *bool, appReason string, appThreshold int) (Transition, AppTransition, error)

	// GetTenantAppAlertRatio returns the fleet circuit breaker's eligible/down
	// counts for one tenant (app.agent GUC), evaluated once per sweep tick
	// (not per site) - see db/query/alerts.sql GetTenantAppAlertRatio.
	GetTenantAppAlertRatio(ctx context.Context, tenantID uuid.UUID) (eligible, down int, err error)

	// TransitionAppAlertBreaker atomically reads (locked), evaluates, and
	// persists the tenant-level circuit-breaker transition - the tenant-wide
	// sibling of TransitionAlertState's own SELECT FOR UPDATE + upsert shape.
	// down is the CURRENT down count (GetTenantAppAlertRatio) - threaded
	// through to EvaluateAppBreaker for its Fix 3 FireUpdate decision.
	TransitionAppAlertBreaker(ctx context.Context, tenantID uuid.UUID, wantTrip bool, down int, now time.Time) (AppBreakerTransition, error)

	// ListTrippedAppAlertBreakerTenants returns every tenant whose fleet
	// circuit breaker is CURRENTLY tripped (app.agent GUC) - GH #291 Phase 3
	// Fix 4. Called ONCE per sweep tick (never per-tenant) so a tripped
	// tenant with no pending transition this tick still gets its breaker
	// re-evaluated and can converge - see ProbeWorker.resolveAppAlerts.
	ListTrippedAppAlertBreakerTenants(ctx context.Context) ([]uuid.UUID, error)

	// ListTenantAppDownSites returns display names for the sites CURRENTLY
	// counted in GetTenantAppAlertRatio's "down" numerator for one tenant
	// (app.agent GUC), bounded by limit - GH #291 Phase 3 Fix 3. Used ONLY
	// for the circuit breaker's "updated" (still tripped, materially worse)
	// aggregate notification, which can fire several ticks after the
	// sites it names actually went down - see the query's own doc comment
	// for why this must read live state rather than this tick's fires.
	ListTenantAppDownSites(ctx context.Context, tenantID uuid.UUID, limit int) ([]string, error)

	// Evaluator path (app.agent GUC, cross-tenant).
	ListAlertConfigsAllTenants(ctx context.Context) ([]AlertConfig, error)

	// Tenant-scoped config CRUD (RLS).
	GetAlertConfig(ctx context.Context, tenantID uuid.UUID) (AlertConfig, bool, error)
	UpsertAlertConfig(ctx context.Context, cfg AlertConfig) (AlertConfig, error)

	// GetAppAlertRolloutDefault reads the m108 deployment-fresh decision (no
	// RLS/tenant dimension - see app_alert_rollout's schema.sql doc comment).
	// Used by GetAlertConfig's synthesized zero-value default so it never
	// disagrees with the persisted column's own DEFAULT.
	GetAppAlertRolloutDefault(ctx context.Context) (bool, error)

	// GetAppHealthSettings / UpdateAppHealthSettings (InTenantTx, RLS) serve
	// GET/PUT /sites/{siteId}/app-health-settings (GH #291 Phase 3 section
	// 3). found=false when siteID does not exist or belongs to another
	// tenant (RLS + the explicit tenant_id predicate both enforce this).
	GetAppHealthSettings(ctx context.Context, tenantID, siteID uuid.UUID) (AppHealthSettings, bool, error)
	UpdateAppHealthSettings(ctx context.Context, tenantID, siteID uuid.UUID, appProbePath string, appAlertsDisabled bool) (AppHealthSettings, bool, error)

	// Fleet uptime queries (tenant-scoped, InTenantTx). Implemented via raw SQL
	// because sqlc generates non-nullable types for nullable columns.

	// GetFleetSiteInfo returns the Postgres-resident fields for the requested
	// sites: name, url, connection_state, health_status, in_incident,
	// disconnected_reason. Probe / uptime metrics are NOT included: the
	// service layer merges those from the metrics.Store so both ClickHouse and
	// Postgres deployments work correctly.
	GetFleetSiteInfo(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID) ([]FleetSiteInfo, error)

	// GetFleetIncidents returns open incidents and incidents that started at or
	// after `since`, read from the persisted site_incidents table (M94).
	GetFleetIncidents(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID, since time.Time, limit int) ([]FleetIncidentItem, error)

	// GetIncidentByID returns one tenant-scoped incident (joined with its
	// site's name/url) for the GET /api/v1/fleet/incidents/:incidentId detail
	// endpoint. found=false (no error) when the id does not exist or belongs
	// to another tenant (RLS + the explicit tenant_id predicate both enforce
	// this — a foreign incident id reads as not-found, never a different
	// tenant's data).
	GetIncidentByID(ctx context.Context, tenantID, incidentID uuid.UUID) (IncidentSummary, bool, error)

	// CountRecentIncidents returns how many incidents (open or closed) have
	// started for siteID in the last 30 days — the "flapping" count surfaced
	// on the incident-detail endpoint.
	CountRecentIncidents(ctx context.Context, tenantID, siteID uuid.UUID) (int64, error)
}

type pgRepo struct {
	pool *db.Pool
}

// NewRepo builds a Repo backed by the pgx pool with RLS enforcement.
func NewRepo(pool *db.Pool) Repo { return &pgRepo{pool: pool} }

func (r *pgRepo) ListEnrolledForProbe(ctx context.Context) ([]EnrolledSite, error) {
	var out []EnrolledSite
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListEnrolledSitesForProbe(ctx)
		if err != nil {
			return domain.Internal("uptime_list_enrolled_failed", "failed to list enrolled sites").WithCause(err)
		}
		out = make([]EnrolledSite, 0, len(rows))
		for _, row := range rows {
			s := EnrolledSite{ID: row.ID, TenantID: row.TenantID, URL: row.Url, HealthStatus: row.HealthStatus}
			if row.LastSeenAt.Valid {
				t := row.LastSeenAt.Time
				s.LastSeenAt = &t
			}
			if row.AppProbePath != nil {
				s.AppProbePath = *row.AppProbePath
			}
			s.AppAlertsDisabled = row.AppAlertsDisabled
			s.ConnectionState = row.ConnectionState
			out = append(out, s)
		}
		return nil
	})
	return out, err
}

// listEnrolledForMonitoringProbeSQL mirrors db/query/sites.sql's
// ListEnrolledSitesForProbe column-for-column and adds ONE predicate:
// monitoring_paused_at IS NULL (m117, GH #414).
//
// PLAN NOTE, checked with EXPLAIN rather than assumed. m117's migration
// deliberately shipped NO index on monitoring_paused_at, on the reasoning that
// the predicate matches nearly every row and that this enumeration is an
// uncapped sequential scan already. That reasoning still holds: the added
// predicate is a filter applied during a scan that has to happen anyway (there
// is no index on enrolled_at either), so the plan is the same Seq Scan with one
// more cheap IS NULL filter. Do not add an index for this query.
const listEnrolledForMonitoringProbeSQL = `
SELECT id, tenant_id, url, health_status, last_seen_at, app_probe_path, app_alerts_disabled, connection_state
  FROM sites
 WHERE enrolled_at IS NOT NULL
   AND monitoring_paused_at IS NULL`

func (r *pgRepo) ListEnrolledForMonitoringProbe(ctx context.Context) ([]EnrolledSite, error) {
	var out []EnrolledSite
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, listEnrolledForMonitoringProbeSQL)
		if err != nil {
			return domain.Internal("uptime_list_monitored_failed", "failed to list monitored sites").WithCause(err)
		}
		defer rows.Close()
		out = out[:0]
		for rows.Next() {
			var (
				s            EnrolledSite
				lastSeen     pgtype.Timestamptz
				appProbePath *string
			)
			if err := rows.Scan(&s.ID, &s.TenantID, &s.URL, &s.HealthStatus, &lastSeen,
				&appProbePath, &s.AppAlertsDisabled, &s.ConnectionState); err != nil {
				return domain.Internal("uptime_list_monitored_failed", "failed to list monitored sites").WithCause(err)
			}
			if lastSeen.Valid {
				t := lastSeen.Time
				s.LastSeenAt = &t
			}
			if appProbePath != nil {
				s.AppProbePath = *appProbePath
			}
			out = append(out, s)
		}
		if err := rows.Err(); err != nil {
			return domain.Internal("uptime_list_monitored_failed", "failed to list monitored sites").WithCause(err)
		}
		return nil
	})
	return out, err
}

func (r *pgRepo) IsMonitoringPaused(ctx context.Context, siteID uuid.UUID) (bool, error) {
	var paused bool
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`SELECT monitoring_paused_at IS NOT NULL FROM sites WHERE id = $1`, siteID).Scan(&paused)
		if errors.Is(err, pgx.ErrNoRows) {
			// No row: the site was deleted between the probe and the fire.
			// Report NOT paused - the caller's next step is a normal alert
			// dispatch that will find no config or no site and drop it
			// harmlessly, which is a better failure than inventing a pause.
			paused = false
			return nil
		}
		if err != nil {
			return domain.Internal("uptime_monitoring_pause_read_failed", "failed to read monitoring pause state").WithCause(err)
		}
		return nil
	})
	return paused, err
}

func (r *pgRepo) SetSiteHealth(ctx context.Context, siteID uuid.UUID, status string) (bool, error) {
	var changed bool
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		n, err := sqlc.New(tx).SetSiteHealthStatus(ctx, sqlc.SetSiteHealthStatusParams{ID: siteID, HealthStatus: status})
		if err != nil {
			return domain.Internal("uptime_set_health_failed", "failed to set site health").WithCause(err)
		}
		changed = n > 0
		return nil
	})
	return changed, err
}

func (r *pgRepo) GetAlertState(ctx context.Context, siteID uuid.UUID) (AlertState, bool, error) {
	var st AlertState
	var found bool
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetSiteAlertState(ctx, siteID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return domain.Internal("uptime_get_state_failed", "failed to read site alert state").WithCause(err)
		}
		st = alertStateFromRow(row)
		found = true
		return nil
	})
	return st, found, err
}

func (r *pgRepo) UpsertAlertState(ctx context.Context, st AlertState) error {
	return r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		_, err := sqlc.New(tx).UpsertSiteAlertState(ctx, sqlc.UpsertSiteAlertStateParams{
			SiteID:          st.SiteID,
			TenantID:        st.TenantID,
			LastStatus:      st.LastStatus,
			ConsecutiveDown: st.ConsecutiveDown,
			InIncident:      st.InIncident,
			LastAlertAt:     toTimestamptz(st.LastAlertAt),
		})
		if err != nil {
			return domain.Internal("uptime_upsert_state_failed", "failed to upsert site alert state").WithCause(err)
		}
		return nil
	})
}

// TransitionAlertState is the race-safe replacement for a separate
// GetAlertState + UpsertAlertState pair: it locks the site's row (or, for a
// brand-new site, relies on the ON CONFLICT upsert's implicit row lock),
// evaluates the transition, and writes the result — all inside one
// transaction — so an overlapping sweep for the same site blocks on the SELECT
// FOR UPDATE until this one commits, then observes the fresh state instead of
// a stale one.
func (r *pgRepo) TransitionAlertState(ctx context.Context, siteID, tenantID uuid.UUID, up bool, threshold int, now time.Time, httpStatus int, reason string,
	appAttempted bool, appUp *bool, appReason string, appThreshold int) (Transition, AppTransition, error) {
	var tr Transition
	var appTr AppTransition
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		var prev AlertState
		row, err := q.GetSiteAlertStateForUpdate(ctx, siteID)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return domain.Internal("uptime_get_state_failed", "failed to read site alert state").WithCause(err)
			}
			// No row yet: zero-value prior state, no lock to hold (nothing exists
			// to lock). The INSERT below takes the DB's own conflict-resolution
			// lock on the newly-inserted row, so a concurrent first-insert for the
			// same site still serializes correctly via ON CONFLICT DO UPDATE.
		} else {
			prev = alertStateFromRow(row)
		}
		prev.SiteID = siteID
		prev.TenantID = tenantID
		if prev.LastStatus == "" {
			prev.LastStatus = StatusUnknown
		}

		tr = Evaluate(prev, up, threshold, now)

		if _, err := q.UpsertSiteAlertState(ctx, sqlc.UpsertSiteAlertStateParams{
			SiteID:          tr.NewState.SiteID,
			TenantID:        tr.NewState.TenantID,
			LastStatus:      tr.NewState.LastStatus,
			ConsecutiveDown: tr.NewState.ConsecutiveDown,
			InIncident:      tr.NewState.InIncident,
			LastAlertAt:     toTimestamptz(tr.NewState.LastAlertAt),
		}); err != nil {
			return domain.Internal("uptime_upsert_state_failed", "failed to upsert site alert state").WithCause(err)
		}

		// M94 (GH #148): open/close the persisted site_incidents row in the
		// SAME transaction as the alert-state write above, keyed off the
		// Transition Evaluate already computed — never re-derive it here.
		switch {
		case tr.FireDown:
			if err := q.OpenIncident(ctx, sqlc.OpenIncidentParams{
				TenantID:       tenantID,
				SiteID:         siteID,
				StartedAt:      now,
				LastHttpStatus: int32(httpStatus),
				Reason:         reason,
			}); err != nil {
				return domain.Internal("uptime_open_incident_failed", "failed to open incident").WithCause(err)
			}
		case tr.FireRecovery:
			if err := q.CloseIncident(ctx, sqlc.CloseIncidentParams{
				SiteID:         siteID,
				LastHttpStatus: int32(httpStatus),
			}); err != nil {
				return domain.Internal("uptime_close_incident_failed", "failed to close incident").WithCause(err)
			}
		case tr.NewState.InIncident:
			// Adopt path: the state is already in_incident but neither flag
			// fired on THIS probe (a steady-state down heartbeat, or a state
			// this code never itself opened an incident row for — e.g. an
			// alert-state row that predates m94 and fell outside the
			// migration's day-1 seed window). started_at falls back to the
			// state's LastAlertAt (the original down-alert timestamp) so an
			// adopted incident's duration is accurate, not reset to now().
			// OpenIncident's ON CONFLICT DO NOTHING makes this a safe no-op
			// on every steady-state down probe once a row is already open.
			startedAt := now
			if tr.NewState.LastAlertAt != nil {
				startedAt = *tr.NewState.LastAlertAt
			}
			if err := q.OpenIncident(ctx, sqlc.OpenIncidentParams{
				TenantID:       tenantID,
				SiteID:         siteID,
				StartedAt:      startedAt,
				LastHttpStatus: int32(httpStatus),
				Reason:         reason,
			}); err != nil {
				return domain.Internal("uptime_adopt_incident_failed", "failed to adopt open incident").WithCause(err)
			}
		}

		// m108 (GH #291 Phase 3): the app-health transition, inside this SAME
		// transaction (never a second round-trip). Skipped entirely when the
		// app probe did not attempt a verdict this tick (appAttempted=false -
		// the common case, since the app probe runs on a slower cadence than
		// the reachability probe): no state change, no write, mirroring
		// EvaluateApp's own "no observation, no change" rule for an unknown
		// verdict. This is the ONLY new code path added to this method; the
		// reachability logic above is completely untouched.
		if appAttempted {
			var appPrev AppAlertState
			appRow, err := q.GetSiteAppAlertStateForUpdate(ctx, siteID)
			if err != nil {
				if !errors.Is(err, pgx.ErrNoRows) {
					return domain.Internal("uptime_get_app_state_failed", "failed to read site app alert state").WithCause(err)
				}
			} else {
				appPrev = appAlertStateFromRow(appRow)
			}
			appPrev.SiteID = siteID
			appPrev.TenantID = tenantID
			if appPrev.LastStatus == "" {
				appPrev.LastStatus = StatusUnknown
			}

			appTr = EvaluateApp(appPrev, ClassifyAppVerdict(appUp), appThreshold, now)

			if _, err := q.UpsertSiteAppAlertState(ctx, sqlc.UpsertSiteAppAlertStateParams{
				SiteID:          appTr.NewState.SiteID,
				TenantID:        appTr.NewState.TenantID,
				LastStatus:      appTr.NewState.LastStatus,
				ConsecutiveDown: appTr.NewState.ConsecutiveDown,
				InIncident:      appTr.NewState.InIncident,
				EverAppUp:       appTr.NewState.EverAppUp,
				LastAlertAt:     toTimestamptz(appTr.NewState.LastAlertAt),
			}); err != nil {
				return domain.Internal("uptime_upsert_app_state_failed", "failed to upsert site app alert state").WithCause(err)
			}
		}
		return nil
	})
	return tr, appTr, err
}

// GetTenantAppAlertRatio returns the fleet circuit breaker's eligible/down
// counts for one tenant (app.agent GUC).
func (r *pgRepo) GetTenantAppAlertRatio(ctx context.Context, tenantID uuid.UUID) (int, int, error) {
	var eligible, down int
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetTenantAppAlertRatio(ctx, tenantID)
		if err != nil {
			return domain.Internal("uptime_app_alert_ratio_failed", "failed to query tenant app alert ratio").WithCause(err)
		}
		eligible = int(row.Eligible)
		down = int(row.Down)
		return nil
	})
	return eligible, down, err
}

// TransitionAppAlertBreaker atomically reads (locked), evaluates, and
// persists the tenant-level circuit-breaker transition - the tenant-wide
// sibling of TransitionAlertState's own SELECT FOR UPDATE + upsert shape, so
// two overlapping sweeps for the same tenant cannot race on this row either.
func (r *pgRepo) TransitionAppAlertBreaker(ctx context.Context, tenantID uuid.UUID, wantTrip bool, down int, now time.Time) (AppBreakerTransition, error) {
	var tr AppBreakerTransition
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		var prev AppBreakerState
		row, err := q.GetTenantAppAlertBreakerForUpdate(ctx, tenantID)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return domain.Internal("uptime_get_breaker_failed", "failed to read tenant app alert breaker state").WithCause(err)
			}
		} else {
			prev = appBreakerStateFromRow(row)
		}
		prev.TenantID = tenantID

		tr = EvaluateAppBreaker(prev, wantTrip, down, now)

		if _, err := q.UpsertTenantAppAlertBreaker(ctx, sqlc.UpsertTenantAppAlertBreakerParams{
			TenantID:      tenantID,
			Tripped:       tr.NewState.Tripped,
			TrippedAt:     toTimestamptz(tr.NewState.TrippedAt),
			LastAlertAt:   toTimestamptz(tr.NewState.LastAlertAt),
			LastDownCount: int32(tr.NewState.LastDownCount),
		}); err != nil {
			return domain.Internal("uptime_upsert_breaker_failed", "failed to upsert tenant app alert breaker state").WithCause(err)
		}
		return nil
	})
	return tr, err
}

// ListTrippedAppAlertBreakerTenants returns every tenant whose fleet circuit
// breaker is CURRENTLY tripped (app.agent GUC) - GH #291 Phase 3 Fix 4. See
// the Repo interface doc comment for why this is safe to call once per sweep
// tick regardless of tenant count.
func (r *pgRepo) ListTrippedAppAlertBreakerTenants(ctx context.Context) ([]uuid.UUID, error) {
	var out []uuid.UUID
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListTrippedAppAlertBreakerTenants(ctx)
		if err != nil {
			return domain.Internal("uptime_list_tripped_breakers_failed", "failed to list tripped app alert breaker tenants").WithCause(err)
		}
		out = rows
		return nil
	})
	return out, err
}

// ListTenantAppDownSites returns display names for the sites CURRENTLY
// counted as "down" for tenantID (app.agent GUC), bounded by limit - GH #291
// Phase 3 Fix 3. See the Repo interface doc comment for why this reads live
// state rather than a single sweep tick's transitions.
func (r *pgRepo) ListTenantAppDownSites(ctx context.Context, tenantID uuid.UUID, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 25
	}
	var out []string
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListTenantAppDownSites(ctx, sqlc.ListTenantAppDownSitesParams{
			TenantID: tenantID,
			RowLimit: int32(limit),
		})
		if err != nil {
			return domain.Internal("uptime_list_app_down_sites_failed", "failed to list tenant app down sites").WithCause(err)
		}
		out = rows
		return nil
	})
	if out == nil {
		out = []string{}
	}
	return out, err
}

// GetAppAlertRolloutDefault reads the m108 deployment-fresh decision. No RLS
// on app_alert_rollout (see its schema.sql doc comment), so any tx wrapper
// works; InAgentTx is used simply because every other cross-cutting uptime
// read in this file already opens one.
func (r *pgRepo) GetAppAlertRolloutDefault(ctx context.Context) (bool, error) {
	var fresh bool
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		v, err := sqlc.New(tx).GetAppAlertRolloutDefault(ctx)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Defensive only: m108 always seeds exactly one row. Default
				// to false (never opt a deployment into alerting it never
				// asked for) if this is somehow missing.
				return nil
			}
			return domain.Internal("uptime_app_alert_rollout_failed", "failed to read app alert rollout default").WithCause(err)
		}
		fresh = v
		return nil
	})
	return fresh, err
}

// GetAppHealthSettings is the tenant-scoped read behind
// GET /sites/{siteId}/app-health-settings.
func (r *pgRepo) GetAppHealthSettings(ctx context.Context, tenantID, siteID uuid.UUID) (AppHealthSettings, bool, error) {
	var out AppHealthSettings
	var found bool
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetSiteAppHealthSettings(ctx, sqlc.GetSiteAppHealthSettingsParams{ID: siteID, TenantID: tenantID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return domain.Internal("uptime_get_app_health_settings_failed", "failed to read site app-health settings").WithCause(err)
		}
		out = AppHealthSettings{SiteID: siteID, TenantID: tenantID, AppAlertsDisabled: row.AppAlertsDisabled}
		if row.AppProbePath != nil {
			out.AppProbePath = *row.AppProbePath
		}
		found = true
		return nil
	})
	return out, found, err
}

// UpdateAppHealthSettings is the tenant-scoped write behind
// PUT /sites/{siteId}/app-health-settings. appProbePath must already be
// validated (uptime.ValidateAppProbePath) by the service layer; an empty
// string clears the override back to auto-detect (NULL).
func (r *pgRepo) UpdateAppHealthSettings(ctx context.Context, tenantID, siteID uuid.UUID, appProbePath string, appAlertsDisabled bool) (AppHealthSettings, bool, error) {
	var out AppHealthSettings
	var found bool
	var pathParam *string
	if appProbePath != "" {
		pathParam = &appProbePath
	}
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).UpdateSiteAppHealthSettings(ctx, sqlc.UpdateSiteAppHealthSettingsParams{
			AppProbePath:      pathParam,
			AppAlertsDisabled: appAlertsDisabled,
			ID:                siteID,
			TenantID:          tenantID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return domain.Internal("uptime_update_app_health_settings_failed", "failed to save site app-health settings").WithCause(err)
		}
		out = AppHealthSettings{SiteID: siteID, TenantID: tenantID, AppAlertsDisabled: row.AppAlertsDisabled}
		if row.AppProbePath != nil {
			out.AppProbePath = *row.AppProbePath
		}
		found = true
		return nil
	})
	return out, found, err
}

func (r *pgRepo) ListAlertConfigsAllTenants(ctx context.Context) ([]AlertConfig, error) {
	var out []AlertConfig
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListAlertConfigsAllTenants(ctx)
		if err != nil {
			return domain.Internal("uptime_list_configs_failed", "failed to list alert configs").WithCause(err)
		}
		out = make([]AlertConfig, 0, len(rows))
		for _, row := range rows {
			out = append(out, alertConfigFromRow(row))
		}
		return nil
	})
	return out, err
}

func (r *pgRepo) GetAlertConfig(ctx context.Context, tenantID uuid.UUID) (AlertConfig, bool, error) {
	var cfg AlertConfig
	var found bool
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetAlertConfig(ctx, tenantID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return domain.Internal("uptime_get_config_failed", "failed to read alert config").WithCause(err)
		}
		cfg = alertConfigFromRow(row)
		found = true
		return nil
	})
	return cfg, found, err
}

func (r *pgRepo) UpsertAlertConfig(ctx context.Context, cfg AlertConfig) (AlertConfig, error) {
	recipients := cfg.EmailRecipients
	if recipients == nil {
		recipients = []string{}
	}
	var out AlertConfig
	err := r.pool.InTenantTx(ctx, cfg.TenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).UpsertAlertConfig(ctx, sqlc.UpsertAlertConfigParams{
			TenantID:            cfg.TenantID,
			EmailRecipients:     recipients,
			WebhookUrl:          cfg.WebhookURL,
			WebhookSecret:       cfg.WebhookSecret,
			Enabled:             cfg.Enabled,
			NotifySecurity:      cfg.NotifySecurity,
			NotifyVulns:         cfg.NotifyVulns,
			VulnMinSeverity:     cfg.VulnMinSeverity,
			VulnIncludeInDigest: cfg.VulnIncludeInDigest,
			AppAlertsEnabled:    cfg.AppAlertsEnabled,
		})
		if err != nil {
			return domain.Internal("uptime_upsert_config_failed", "failed to save alert config").WithCause(err)
		}
		out = alertConfigFromRow(row)
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Fleet uptime repo methods (raw SQL — Postgres-resident fields only)
// ---------------------------------------------------------------------------

// GetFleetSiteInfo returns the Postgres-resident fields for each requested
// site: name, url, connection_state, health_status, in_incident,
// disconnected_reason. Probe / uptime metrics are intentionally excluded:
// the service merges those from the metrics.Store so the endpoint works on
// both ClickHouse and Postgres deployments (previously these were read
// directly from site_uptime_probes, which is empty on ClickHouse installs).
func (r *pgRepo) GetFleetSiteInfo(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID) ([]FleetSiteInfo, error) {
	const q = `
SELECT
    s.id,
    s.name,
    s.url,
    s.connection_state,
    s.health_status,
    COALESCE(ast.in_incident, false) AS in_incident,
    s.disconnected_reason
FROM sites s
LEFT JOIN site_alert_state ast ON ast.site_id = s.id
WHERE s.tenant_id = $1
  AND s.id = ANY($2::uuid[])
ORDER BY s.name ASC
`
	var out []FleetSiteInfo
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, tenantID, siteIDs)
		if err != nil {
			return domain.Internal("fleet_site_info_failed", "failed to query fleet site info").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			var info FleetSiteInfo
			var disconnectedReason *string
			if err := rows.Scan(
				&info.SiteID, &info.Name, &info.URL,
				&info.ConnectionState, &info.HealthStatus, &info.InIncident,
				&disconnectedReason,
			); err != nil {
				return domain.Internal("fleet_site_info_scan_failed", "failed to scan fleet site info row").WithCause(err)
			}
			if disconnectedReason != nil {
				info.DisconnectedReason = *disconnectedReason
			}
			out = append(out, info)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []FleetSiteInfo{}
	}
	return out, nil
}

// deriveFleetStatus computes the FleetSiteStatus from the latest probe result,
// plus a short machine-readable reason (FleetReason*, empty when the status is
// self-explanatory) so the API/UI can say WHICH degraded it is instead of
// rendering a bare chip.
//
// GH #291: a page cache in front of a fatal-on-every-request site keeps
// answering probe A (the reachability probe, frozen, see the design doc) with
// a cached 200, so `up` alone cannot see past the cache. It does not need to:
// the control plane already has a stronger, independent signal from the agent
// side. ADR-039's connection sweeper sends a SIGNED, UNCACHEABLE POST straight
// to the agent and tracks the outcome in sites.connection_state, completely
// out of band from the cached probe. Two of its states now flip the derived
// fleet status to Degraded even though the cached probe reported up=true:
//
//   - "degraded": the heartbeat is stale (missed >= 300s) but not yet stale
//     enough to disconnect. This is the weaker of the two signals.
//   - "disconnected": the sweeper's active-verify already tried the signed
//     POST and got a failure (5xx/timeout/conn-error), OR the passive
//     heartbeat-timeout fallback fired. This is the strongest signal short of
//     `up` itself going false, and it is the literal GH #291 case: previously
//     this state fell through to a clean FleetStatusUp.
//
// GH #291 follow-up (false Degraded on a clean deactivation): "disconnected"
// is NOT reached only by the sweeper. A SIGNED agent last-will
// (ADR-040, connService.RecordLastWillTenant) drives the exact same
// connected/degraded -> disconnected transition when an operator deactivates
// or uninstalls the plugin, and that site is a perfectly healthy site that
// chose to stop heartbeating, not an outage. Both paths land on
// connection_state "disconnected" with no other distinguishing column on the
// read side except sites.disconnected_reason (threaded through as
// disconnectedReason here). The two are DISTINGUISHABLE: the sweeper always
// writes one of exactly two CP-authored strings (see
// sweeperDisconnectReasons), while a last-will disconnect is either the
// agent's own fixed reasons ("deactivated", "uninstalled") or the handler's
// "user_initiated" default, never one of the sweeper's strings. Because the
// agent controls its last-will reason text (bounded, not enum-validated),
// this function does NOT try to enumerate every last-will value; it does the
// reverse and requires a positive match against the small, CP-authored
// sweeper set before it will raise the alarming Degraded chip. Any value it
// cannot positively attribute to the sweeper (a known last-will reason, an
// unrecognized string, or an empty/legacy row that predates this column) is
// treated as healthy, per the conservative rule: never show an alarming
// Degraded when the data cannot prove the site is unhealthy.
//
// "revoked" and "archived" are deliberately EXCLUDED from this check. Both
// mean the OPERATOR chose to stop managing the site (see GH #282, which
// excludes the same two states from backup scheduling for the identical
// reason), not a connectivity problem. Flagging them Degraded would raise a
// false alarm on a site nobody is watching. FleetStatusUnknown is not a
// substitute: it already means "no probe recorded yet", and reusing it here
// would make an operator unable to tell "never probed" apart from
// "deliberately archived". None of the four existing FleetSiteStatus values
// expresses "deliberately unmanaged" without being misleading in one
// direction or the other, and adding a fifth would break the API enum and
// every FE consumer (out of scope for this phase, per the design doc's "no
// new enum value" rule). So revoked/archived sites keep exactly today's
// behavior: their derived status depends only on `up` and latency, same as
// before this fix. A distinct non-alarming "unmanaged" indicator is a
// candidate for a future phase, not squeezed in here.
//
// `up` itself, sites.health_status, uptime percentages and everything else
// probe A feeds are UNCHANGED by this function. Only the derived display
// status moves.
//
// appUp is the GH #291 Phase 2 signal (metrics.FleetUptimeRow.AppUp): the
// application-health probe's most recent verdict, tri-state (true/false/nil
// =unknown). A conclusive false - the app probe positively determined
// WordPress is not responding, independent of and possibly disagreeing with
// `up` - derives Degraded with FleetReasonAppDown. This is the phase's
// headline case: a page cache can keep `up` reporting true (visitors ARE
// being served a cached page) while the app probe proves the backend is
// dead. appUp==nil (never probed, or the most recent probe was
// inconclusive - cache-hit-defeated, 4xx, etc.) makes NO difference here;
// unknown is never dressed up as broken, so it simply falls through to
// whatever this site would have derived before Phase 2 existed.
func deriveFleetStatus(up *bool, totalMs *float64, connectionState, disconnectedReason string, appUp *bool) (FleetSiteStatus, string) {
	if up == nil {
		return FleetStatusUnknown, ""
	}
	if !*up {
		return FleetStatusDown, ""
	}
	switch connectionState {
	case "disconnected":
		if sweeperDisconnectReasons[disconnectedReason] {
			return FleetStatusDegraded, FleetReasonAgentUnreachable
		}
		// A clean last-will disconnect (or a reason we cannot positively
		// attribute to the sweeper): fall through to the same derivation a
		// healthy "connected" site gets, below.
	case "degraded":
		return FleetStatusDegraded, FleetReasonAgentDegraded
	}
	if appUp != nil && !*appUp {
		return FleetStatusDegraded, FleetReasonAppDown
	}
	if totalMs != nil && *totalMs > slowThresholdMs {
		return FleetStatusDegraded, FleetReasonSlowResponse
	}
	return FleetStatusUp, ""
}

// GetFleetIncidents returns open incidents (ended_at IS NULL) and incidents
// that started at or after `since`, read directly from the persisted
// site_incidents table (M94, GH #148) — real incident rows, not an estimate
// derived from site_alert_state's single mutable transition-memory row.
func (r *pgRepo) GetFleetIncidents(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID, since time.Time, limit int) ([]FleetIncidentItem, error) {
	const q = `
SELECT
    si.id,
    si.site_id,
    s.name,
    s.url,
    si.started_at,
    si.ended_at,
    (si.ended_at IS NULL) AS ongoing,
    p.total_ms
FROM site_incidents si
JOIN sites s ON s.id = si.site_id AND s.tenant_id = si.tenant_id
LEFT JOIN LATERAL (
    SELECT total_ms
    FROM site_uptime_probes
    WHERE site_id = s.id AND tenant_id = s.tenant_id
    ORDER BY probed_at DESC
    LIMIT 1
) p ON true
WHERE si.tenant_id = $1
  AND si.site_id = ANY($2::uuid[])
  AND (si.ended_at IS NULL OR si.started_at >= $3)
ORDER BY si.started_at DESC
LIMIT $4
`
	var out []FleetIncidentItem
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, tenantID, siteIDs, since, limit)
		if err != nil {
			return domain.Internal("fleet_incidents_failed", "failed to query fleet incidents").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				id        uuid.UUID
				siteID    uuid.UUID
				siteName  string
				siteURL   string
				startedAt time.Time
				endedAt   pgtype.Timestamptz
				ongoing   bool
				totalMs   *float64
			)
			if err := rows.Scan(&id, &siteID, &siteName, &siteURL, &startedAt, &endedAt, &ongoing, &totalMs); err != nil {
				return domain.Internal("fleet_incidents_scan_failed", "failed to scan incident row").WithCause(err)
			}
			started := startedAt
			item := FleetIncidentItem{
				ID:            id,
				SiteID:        siteID,
				Kind:          string(AlertDown),
				SiteName:      siteName,
				SiteURL:       siteURL,
				StartedAt:     &started,
				Ongoing:       ongoing,
				LatestTotalMs: totalMs,
			}
			if !ongoing && endedAt.Valid {
				t := endedAt.Time
				item.EndedAt = &t
				dur := int64(t.Sub(started).Seconds())
				if dur < 0 {
					dur = 0
				}
				item.DurationSeconds = &dur
			}
			out = append(out, item)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []FleetIncidentItem{}
	}
	return out, nil
}

// GetIncidentByID returns one tenant-scoped incident (joined with its site's
// name/url) for the incident-detail endpoint. found=false when the id does
// not exist, belongs to another tenant (RLS + the explicit tenant_id
// predicate both enforce this), or was hard-deleted along with its site.
func (r *pgRepo) GetIncidentByID(ctx context.Context, tenantID, incidentID uuid.UUID) (IncidentSummary, bool, error) {
	var out IncidentSummary
	var found bool
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetIncidentByID(ctx, sqlc.GetIncidentByIDParams{ID: incidentID, TenantID: tenantID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return domain.Internal("incident_get_failed", "failed to read incident").WithCause(err)
		}
		out = incidentSummaryFromRow(row)
		found = true
		return nil
	})
	return out, found, err
}

// CountRecentIncidents returns how many incidents (open or closed) have
// started for siteID in the last 30 days — the incident-detail endpoint's
// flapping count.
func (r *pgRepo) CountRecentIncidents(ctx context.Context, tenantID, siteID uuid.UUID) (int64, error) {
	var count int64
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		n, err := sqlc.New(tx).CountRecentIncidents(ctx, sqlc.CountRecentIncidentsParams{SiteID: siteID, TenantID: tenantID})
		if err != nil {
			return domain.Internal("incident_count_recent_failed", "failed to count recent incidents").WithCause(err)
		}
		count = n
		return nil
	})
	return count, err
}

// incidentSummaryFromRow maps the sqlc join row to the domain type.
func incidentSummaryFromRow(row sqlc.GetIncidentByIDRow) IncidentSummary {
	out := IncidentSummary{
		ID:             row.ID,
		SiteID:         row.SiteID,
		SiteName:       row.SiteName,
		SiteURL:        row.SiteUrl,
		StartedAt:      row.StartedAt,
		PeakStatus:     row.PeakStatus,
		LastHTTPStatus: int(row.LastHttpStatus),
		Reason:         row.Reason,
	}
	if row.EndedAt.Valid {
		t := row.EndedAt.Time
		out.EndedAt = &t
	}
	return out
}

func alertConfigFromRow(row sqlc.AlertConfig) AlertConfig {
	recipients := row.EmailRecipients
	if recipients == nil {
		recipients = []string{}
	}
	return AlertConfig{
		TenantID:            row.TenantID,
		EmailRecipients:     recipients,
		WebhookURL:          row.WebhookUrl,
		WebhookSecret:       row.WebhookSecret,
		Enabled:             row.Enabled,
		NotifySecurity:      row.NotifySecurity,
		NotifyVulns:         row.NotifyVulns,
		VulnMinSeverity:     row.VulnMinSeverity,
		VulnIncludeInDigest: row.VulnIncludeInDigest,
		AppAlertsEnabled:    row.AppAlertsEnabled,
		UpdatedAt:           row.UpdatedAt,
	}
}

func alertStateFromRow(row sqlc.SiteAlertState) AlertState {
	st := AlertState{
		SiteID:          row.SiteID,
		TenantID:        row.TenantID,
		LastStatus:      row.LastStatus,
		ConsecutiveDown: row.ConsecutiveDown,
		InIncident:      row.InIncident,
	}
	if row.LastAlertAt.Valid {
		t := row.LastAlertAt.Time
		st.LastAlertAt = &t
	}
	return st
}

// appAlertStateFromRow maps the sqlc row to the domain AppAlertState - the
// app-health sibling of alertStateFromRow.
func appAlertStateFromRow(row sqlc.SiteAppAlertState) AppAlertState {
	st := AppAlertState{
		SiteID:          row.SiteID,
		TenantID:        row.TenantID,
		LastStatus:      row.LastStatus,
		ConsecutiveDown: row.ConsecutiveDown,
		InIncident:      row.InIncident,
		EverAppUp:       row.EverAppUp,
	}
	if row.LastAlertAt.Valid {
		t := row.LastAlertAt.Time
		st.LastAlertAt = &t
	}
	return st
}

// appBreakerStateFromRow maps the sqlc row to the domain AppBreakerState.
func appBreakerStateFromRow(row sqlc.TenantAppAlertBreaker) AppBreakerState {
	st := AppBreakerState{
		TenantID:      row.TenantID,
		Tripped:       row.Tripped,
		LastDownCount: int(row.LastDownCount),
	}
	if row.TrippedAt.Valid {
		t := row.TrippedAt.Time
		st.TrippedAt = &t
	}
	if row.LastAlertAt.Valid {
		t := row.LastAlertAt.Time
		st.LastAlertAt = &t
	}
	return st
}

func toTimestamptz(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}
