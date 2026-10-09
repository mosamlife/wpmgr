package abilityrequest

// The dispatch worker for approved ability requests (engine v4 §2.5-§2.7,
// amendment W1). Shape follows internal/assistantrequest/worker.go:
//
//   scan (agent read) -> per-row job -> grant checks in Go -> site checks
//   under a single-site principal -> ONE reservation (lifecycle try-lock,
//   per-site lock, lifecycle FOR SHARE, in-flight check, compare-and-set
//   with the approved entry hash in its WHERE) -> send write with expected{}
//   and no transaction open -> record the outcome (compare-and-set).
//
// A write is NEVER resent. A lost reply moves the row to outcome_unknown, and
// only ledger mode resolves it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/humantext"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/internal/wpversion"
)

// Timing and limits.
const (
	ScanInterval  = 15 * time.Second
	SweepInterval = time.Minute
	// BackoffBase and BackoffCap bound how soon a request that could not
	// start is tried again.
	BackoffBase = 30 * time.Second
	BackoffCap  = 5 * time.Minute
	// writeSendBudget is the worker->agent write budget (v4 §2.6).
	writeSendBudget = 60 * time.Second
	// StaleDispatchAfter: a dispatched row with no reply recorded after this
	// is moved to outcome_unknown (the worker died between send and record).
	StaleDispatchAfter = 3 * time.Minute
	// ledgerPollInterval and ledgerResolveWindow are v4 §2.7.
	ledgerPollInterval  = time.Minute
	ledgerResolveWindow = 15 * time.Minute
	// tokenSettled is past the command token's TTL plus skew: after it, a
	// write that never reached the agent can no longer arrive.
	tokenSettled = 2 * time.Minute
	// undoRetention is the ledger's retention (v4 §5).
	undoRetention = 14 * 24 * time.Hour

	scanRowLimit        = 200
	maxSiteReportedText = 512

	siteDispatchLockKey = "assistant_ability_site_dispatch"
	// lifecycleLockKey must equal org.LifecycleLockKey; a test pins it.
	lifecycleLockKey = "org_lifecycle"
)

// Outcomes (m156's closed set).
const (
	OutcomeCreated        = "created"
	OutcomeApplied        = "applied"
	OutcomeRefused        = "refused"
	OutcomeVerifyMismatch = "verify_mismatch"
	OutcomeFailed         = "failed"
	OutcomeUnknown        = "outcome_unknown"
	OutcomeNotSent        = "not_sent"
)

// Reasons an approved request closed without being sent.
const (
	ReasonGrantInactive          = "grant_inactive"
	ReasonAssistantPaused        = "assistant_paused"
	ReasonOrganisationDeleted    = "organisation_deleted"
	ReasonCapabilityNotHeld      = "capability_not_held"
	ReasonSiteAbsent             = "site_absent"
	ReasonForbiddenByContext     = "forbidden_by_context"
	ReasonAgentOutdated          = "agent_outdated"
	ReasonDispatchDeadlinePassed = "dispatch_deadline_passed"
	ReasonTransportPreSend       = "transport_pre_send"
	ReasonEntryChanged           = "entry_changed"
	ReasonEntryDisabled          = "entry_disabled"
	// m161: the REST route a rest-write request was approved against.
	ReasonRouteChanged  = "route_changed"
	ReasonRouteDisabled = "route_disabled"
)

// Transient reasons; the row stays approved.
const (
	AttemptSiteUnreachable    = "site_unreachable"
	AttemptSiteBusy           = "site_busy"
	AttemptOrgBusy            = "org_busy"
	AttemptContextUnavailable = "context_unavailable"
	AttemptWriteToolsDisabled = "write_tools_disabled"
)

// AbilityAgent is the one agent call the worker and undo make.
type AbilityAgent interface {
	AbilityRun(ctx context.Context, siteID uuid.UUID, siteURL string, call agentcmd.AbilityRunCall) (agentcmd.AbilityRunResponse, error)
}

// EntryEncoder is abilities.SendableEntry, handed in by main.go.
type EntryEncoder func(sqlc.AbilityCatalogue) ([]byte, string, error)

// ContextRules reads the operator's AI rules. *mcp.Service satisfies it.
type ContextRules interface {
	ForbiddenByContext(ctx context.Context, tenantID, siteID uuid.UUID, tool string) (matchedEntry string, forbidden bool, err error)
}

// RouteEncoder is abilities.SendableRoute, handed in by main.go.
type RouteEncoder func(sqlc.RestRouteCatalogue) ([]byte, string, error)

// SetRouteEncoder wires the route bytes for wpmgr/rest-write. Without it a
// request naming a route waits with write_tools_disabled and the sweeper
// closes it at its deadline.
func (s *Service) SetRouteEncoder(route RouteEncoder) { s.route = route }

// SetSender wires the send path. Without it every approved row waits with
// write_tools_disabled and the sweeper closes it at its deadline.
func (s *Service) SetSender(agent AbilityAgent, entry EntryEncoder, rules ContextRules) {
	s.agent, s.entry, s.rules = agent, entry, rules
}

func (s *Service) sendOn() bool {
	return s.enabled && s.agent != nil && s.entry != nil && s.rules != nil
}

// ---------------------------------------------------------------------------
// River jobs
// ---------------------------------------------------------------------------

const tenantQueueShards = 4

// QueueForTenant is the dispatch queue a tenant's jobs run on.
func QueueForTenant(tenantID uuid.UUID) string {
	return fmt.Sprintf("ability_request_t%d", int(tenantID[0])%tenantQueueShards)
}

// Queues is the River queue configuration the dispatch jobs need.
func Queues() map[string]river.QueueConfig {
	out := make(map[string]river.QueueConfig, tenantQueueShards)
	for i := 0; i < tenantQueueShards; i++ {
		out[fmt.Sprintf("ability_request_t%d", i)] = river.QueueConfig{MaxWorkers: 2}
	}
	return out
}

// ScanArgs is the periodic scan for approved requests that are due.
type ScanArgs struct{}

// Kind implements river.JobArgs.
func (ScanArgs) Kind() string { return "ability_request_scan" }

// DispatchArgs is one approved request to try. Ids only.
type DispatchArgs struct {
	TenantID  uuid.UUID `json:"tenant_id"`
	RequestID uuid.UUID `json:"request_id"`
	SiteID    uuid.UUID `json:"site_id"`
	GrantID   uuid.UUID `json:"grant_id"`
}

// Kind implements river.JobArgs.
func (DispatchArgs) Kind() string { return "ability_request_dispatch" }

// InsertOpts pins the job to its tenant's shard, at most one live job per
// request. MaxAttempts 1: a job that errors after the send must not run the
// send again; the next scan decides afresh from the row.
func (a DispatchArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       QueueForTenant(a.TenantID),
		MaxAttempts: 1,
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning,
				rivertype.JobStateRetryable, rivertype.JobStateScheduled,
			},
		},
	}
}

// SweepArgs: lapsed waiting rows expire; approved rows past deadline close.
type SweepArgs struct{}

// Kind implements river.JobArgs.
func (SweepArgs) Kind() string { return "ability_request_sweep" }

// ReconcileArgs: stale dispatched rows to outcome_unknown, and the ledger
// resolution of outcome_unknown rows.
type ReconcileArgs struct{}

// Kind implements river.JobArgs.
func (ReconcileArgs) Kind() string { return "ability_request_reconcile" }

// Enqueuer inserts dispatch jobs.
type Enqueuer interface {
	EnqueueDispatch(ctx context.Context, args DispatchArgs) error
}

// RiverEnqueuer inserts dispatch jobs on a started River client.
type RiverEnqueuer struct{ client *river.Client[pgx.Tx] }

// NewRiverEnqueuer wraps a started River client.
func NewRiverEnqueuer(client *river.Client[pgx.Tx]) *RiverEnqueuer {
	return &RiverEnqueuer{client: client}
}

// EnqueueDispatch inserts one dispatch job.
func (e *RiverEnqueuer) EnqueueDispatch(ctx context.Context, args DispatchArgs) error {
	if _, err := e.client.Insert(ctx, args, nil); err != nil {
		return fmt.Errorf("enqueue ability request dispatch: %w", err)
	}
	return nil
}

// ScanWorker runs the scan.
type ScanWorker struct {
	river.WorkerDefaults[ScanArgs]
	svc      *Service
	enqueuer Enqueuer
}

// NewScanWorker builds the scan worker; wire the enqueuer after River starts.
func NewScanWorker(svc *Service) *ScanWorker { return &ScanWorker{svc: svc} }

// SetEnqueuer wires the River enqueuer.
func (w *ScanWorker) SetEnqueuer(e Enqueuer) { w.enqueuer = e }

// Work runs one scan.
func (w *ScanWorker) Work(ctx context.Context, _ *river.Job[ScanArgs]) error {
	if w.enqueuer == nil {
		w.svc.logger.WarnContext(ctx, "ability request scan: enqueuer not wired; skipping")
		return nil
	}
	return w.svc.scan(ctx, w.enqueuer)
}

// DispatchWorker tries one approved request.
type DispatchWorker struct {
	river.WorkerDefaults[DispatchArgs]
	svc *Service
}

// NewDispatchWorker builds the dispatch worker.
func NewDispatchWorker(svc *Service) *DispatchWorker { return &DispatchWorker{svc: svc} }

// Timeout bounds one dispatch; the send is bounded well below it.
func (w *DispatchWorker) Timeout(*river.Job[DispatchArgs]) time.Duration { return 2 * time.Minute }

// Work tries one approved request.
func (w *DispatchWorker) Work(ctx context.Context, job *river.Job[DispatchArgs]) error {
	return w.svc.dispatch(ctx, job.Args)
}

// SweepWorker runs the sweeper.
type SweepWorker struct {
	river.WorkerDefaults[SweepArgs]
	svc *Service
}

// NewSweepWorker builds the sweeper.
func NewSweepWorker(svc *Service) *SweepWorker { return &SweepWorker{svc: svc} }

// Work runs one sweep; both passes run even if the first fails.
func (w *SweepWorker) Work(ctx context.Context, _ *river.Job[SweepArgs]) error {
	return errors.Join(w.svc.sweepExpired(ctx), w.svc.sweepPastDeadline(ctx))
}

// ReconcileWorker runs the reconciler.
type ReconcileWorker struct {
	river.WorkerDefaults[ReconcileArgs]
	svc *Service
}

// NewReconcileWorker builds the reconciler.
func NewReconcileWorker(svc *Service) *ReconcileWorker { return &ReconcileWorker{svc: svc} }

// Work runs one reconcile pass: stale sends first, then the ledger.
func (w *ReconcileWorker) Work(ctx context.Context, _ *river.Job[ReconcileArgs]) error {
	return errors.Join(w.svc.reconcileStale(ctx), w.svc.resolveUnknown(ctx))
}

// ---------------------------------------------------------------------------
// Transactions
// ---------------------------------------------------------------------------

func (s *Service) runAgentScan(ctx context.Context, fn func(q *sqlc.Queries) error) error {
	return s.pool.InAgentTx(ctx, func(tx pgx.Tx) error { return fn(sqlc.New(tx)) })
}

// runSiteTx runs fn under a single-site principal; the first statement
// proves the transaction admits exactly siteID.
func (s *Service) runSiteTx(ctx context.Context, p domain.Principal, siteID uuid.UUID, fn func(tx pgx.Tx, q *sqlc.Queries) error) error {
	return s.pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
		if err := mcp.AssertSingleSiteTx(ctx, tx, siteID); err != nil {
			return err
		}
		return fn(tx, sqlc.New(tx))
	})
}

func readScopedSite(ctx context.Context, q *sqlc.Queries, tenantID, siteID uuid.UUID) (sqlc.Site, bool, error) {
	rows, err := q.ListSitesForMCPScope(ctx, sqlc.ListSitesForMCPScopeParams{
		TenantID: tenantID, SiteIds: []uuid.UUID{siteID}, RowLimit: 2,
	})
	if err != nil {
		return sqlc.Site{}, false, fmt.Errorf("read site: %w", err)
	}
	for _, st := range rows {
		if st.ID == siteID && st.TenantID == tenantID {
			return st, true, nil
		}
	}
	return sqlc.Site{}, false, nil
}

func (s *Service) record(ctx context.Context, tx pgx.Tx, tenantID, requestID uuid.UUID, action string, md map[string]any) error {
	if s.audit == nil {
		return errors.New("audit recorder not wired")
	}
	md["request_id"] = requestID.String()
	_, err := s.audit.RecordInTx(ctx, tx, audit.Event{
		TenantID: tenantID, ActorType: audit.ActorSystem, Action: action,
		TargetType: audit.TargetTypeAssistantAbilityRequest, TargetID: requestID.String(), Metadata: md,
	})
	return err
}

// closeNotSent closes an approved row as not sent and audits only if this
// call closed it.
func (s *Service) closeNotSent(ctx context.Context, tx pgx.Tx, q *sqlc.Queries, tenantID, requestID uuid.UUID, reason string) error {
	n, err := q.CloseApprovedAbilityRequestNotSent(ctx, sqlc.CloseApprovedAbilityRequestNotSentParams{
		NotSentReason: reason, TenantID: tenantID, ID: requestID,
	})
	if err != nil {
		return fmt.Errorf("close ability request as not sent: %w", err)
	}
	if n != 1 {
		return nil
	}
	return s.record(ctx, tx, tenantID, requestID, audit.ActionAbilityRequestNotSent, map[string]any{"reason": reason})
}

// closeWithoutSite is for reasons decided before a site principal exists.
func (s *Service) closeWithoutSite(ctx context.Context, tenantID, requestID uuid.UUID, reason string) error {
	switch reason {
	case ReasonGrantInactive, ReasonAssistantPaused, ReasonCapabilityNotHeld, ReasonSiteAbsent:
	default:
		return fmt.Errorf("closeWithoutSite: reason %q is not one it may record", reason)
	}
	return s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		return s.closeNotSent(ctx, tx, sqlc.New(tx), tenantID, requestID, reason)
	})
}

// ---------------------------------------------------------------------------
// Scan and dispatch
// ---------------------------------------------------------------------------

func (s *Service) scan(ctx context.Context, enq Enqueuer) error {
	var due []sqlc.ScanDueApprovedAbilityRequestsRow
	if err := s.runAgentScan(ctx, func(q *sqlc.Queries) error {
		var err error
		due, err = q.ScanDueApprovedAbilityRequests(ctx, sqlc.ScanDueApprovedAbilityRequestsParams{
			BackoffCapSeconds: int32(BackoffCap / time.Second), BackoffBaseSeconds: int32(BackoffBase / time.Second),
			RowLimit: scanRowLimit,
		})
		return err
	}); err != nil {
		return fmt.Errorf("scan approved ability requests: %w", err)
	}
	var errs []error
	for _, r := range due {
		if err := enq.EnqueueDispatch(ctx, DispatchArgs{
			TenantID: r.TenantID, RequestID: r.ID, SiteID: r.SiteID, GrantID: r.ProposedByGrantID,
		}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type transientError struct{ code string }

func (e *transientError) Error() string { return "ability request not started: " + e.code }

var errLostReservation = errors.New("ability request: reservation lost to another path")

type dispatchPlan struct {
	row      sqlc.AssistantAbilityRequest
	siteURL  string
	entry    []byte
	sum      string
	route    []byte
	routeSum string
}

// agentFloorFor is the first agent release that runs a request: its
// ability's floor, and for wpmgr/page-create the floor of the stored input
// (a layout outline needs a newer agent than a text-only one). A site whose
// plugin went below it after approval closes not_sent/agent_outdated and is
// never sent a write it cannot build.
func agentFloorFor(r sqlc.AssistantAbilityRequest) string {
	switch r.AbilityName {
	case mcp.AbilityRestWrite:
		return agentcmd.MinAgentVersionForRestCall
	case mcp.AbilityPageCreate:
		return mcp.PageCreateAgentFloor([]byte(r.InputJson))
	}
	return agentcmd.MinAgentVersionForPageCreate
}

func agentMeetsFloor(v, floor string) bool {
	return v != "" && wpversion.Compare(v, floor) >= 0
}

// notSentBeforeReserve is the reason an approved request closes not_sent
// before it is reserved, or "" when it may go on: the site left scope, the
// deadline passed, the entry or route changed after approval (W1), or the
// site's plugin is below the floor for this request's stored input.
func notSentBeforeReserve(row sqlc.GetApprovedAbilityRequestForDispatchRow, site sqlc.Site, found bool) string {
	switch {
	case !found:
		return ReasonSiteAbsent
	case row.PastDeadline:
		return ReasonDispatchDeadlinePassed
	case !row.EntryEnabled: // W1
		return ReasonEntryDisabled
	case !row.EntryHashCurrent: // W1
		return ReasonEntryChanged
	case !row.RouteEnabled: // W1, m161
		return ReasonRouteDisabled
	case !row.RouteHashCurrent: // W1, m161
		return ReasonRouteChanged
	case !agentMeetsFloor(site.AgentVersion, agentFloorFor(row.AssistantAbilityRequest)):
		return ReasonAgentOutdated
	}
	return ""
}

// routeSendable is W1 for a route (m161). It returns the bytes to send for a
// request approved against approvedSum, or reason route_changed: the bytes
// sent are the current row's, and only when they hash to the APPROVED hash
// (the stored hash alone is not enough: a row edited outside the admin write
// keeps its old stored hash) and the input still names that route.
func routeSendable(enc RouteEncoder, r sqlc.RestRouteCatalogue, approvedSum, inputJSON string) ([]byte, string, string) {
	if !r.Enabled || r.Class != "write" {
		return nil, "", ReasonRouteDisabled
	}
	b, sum, err := enc(r)
	if err != nil || sum != approvedSum || r.RouteSha256 == nil || *r.RouteSha256 != approvedSum {
		return nil, "", ReasonRouteChanged
	}
	var in struct {
		RouteID string `json:"route_id"`
	}
	if json.Unmarshal([]byte(inputJSON), &in) != nil || in.RouteID != r.RouteID {
		return nil, "", ReasonRouteChanged
	}
	return b, sum, ""
}

func connectedEnough(state string) bool { return state == "connected" || state == "degraded" }

func (s *Service) dispatch(ctx context.Context, a DispatchArgs) error {
	if a.TenantID == uuid.Nil || a.RequestID == uuid.Nil || a.SiteID == uuid.Nil || a.GrantID == uuid.Nil {
		return errors.New("ability request dispatch: nil id in job")
	}
	// (1) The grant, decided in Go before any site principal exists.
	v, err := s.grants.ReCheckGrantAuthorization(ctx, a.TenantID, a.GrantID)
	if err != nil {
		return fmt.Errorf("re-check grant: %w", err)
	}
	if !v.Found || !v.Authorized {
		reason := ReasonGrantInactive
		if v.TenantAssistantPaused {
			reason = ReasonAssistantPaused
		}
		return s.closeWithoutSite(ctx, a.TenantID, a.RequestID, reason)
	}
	auth, err := s.authz.AuthorizeGrant(ctx, a.TenantID, v)
	if errors.Is(err, mcp.ErrGrantNotAuthorized) {
		return s.closeWithoutSite(ctx, a.TenantID, a.RequestID, ReasonGrantInactive)
	}
	if err != nil {
		return fmt.Errorf("derive grant: %w", err)
	}
	if !auth.Capabilities.Allows(mcp.CapAbilityRequest) {
		return s.closeWithoutSite(ctx, a.TenantID, a.RequestID, ReasonCapabilityNotHeld)
	}
	p, err := mcp.SingleSitePrincipal(auth, a.SiteID)
	if errors.Is(err, mcp.ErrSiteNotInScope) {
		return s.closeWithoutSite(ctx, a.TenantID, a.RequestID, ReasonSiteAbsent)
	}
	if err != nil {
		return fmt.Errorf("site principal: %w", err)
	}
	// (2) The site and entry checks.
	plan, done, err := s.checkSite(ctx, p, a)
	if err != nil || done {
		return err
	}
	// (3) The one reservation.
	reserved, err := s.reserve(ctx, p, a, plan)
	var te *transientError
	if errors.As(err, &te) {
		return s.runSiteTx(ctx, p, a.SiteID, func(_ pgx.Tx, q *sqlc.Queries) error {
			_, err := q.RecordAbilityRequestDispatchAttempt(ctx, sqlc.RecordAbilityRequestDispatchAttemptParams{
				LastAttemptCode: te.code, TenantID: a.TenantID, ID: a.RequestID,
			})
			return err
		})
	}
	if err != nil || !reserved {
		return err
	}
	// (4) The send, no transaction open. The request id IS the ledger key,
	// so the agent's idempotency check covers any duplicate delivery.
	sendCtx, cancel := context.WithTimeout(ctx, writeSendBudget)
	resp, sendErr := s.agent.AbilityRun(sendCtx, a.SiteID, plan.siteURL, agentcmd.AbilityRunCall{
		Mode: agentcmd.AbilityRunModeWrite, RequestID: a.RequestID,
		Entry: plan.entry, EntrySHA256: plan.sum, Input: []byte(plan.row.InputJson),
		Route: plan.route, RouteSHA256: plan.routeSum,
		Expected: writeExpected(plan),
	})
	cancel()
	// (5) The outcome.
	return s.recordOutcome(ctx, p, a, classifyWrite(resp, sendErr, s.now()))
}

// writeExpected is write's expected{}: the preview digest for page-create,
// the base fingerprint for a route write.
func writeExpected(plan dispatchPlan) *agentcmd.AbilityRunExpected {
	if plan.row.RouteID != nil {
		return &agentcmd.AbilityRunExpected{PrecheckDigest: plan.row.PrecheckDigest, BaseFingerprint: plan.row.BaseFingerprint}
	}
	return &agentcmd.AbilityRunExpected{
		PrecheckDigest: plan.row.PrecheckDigest, PreviewDigest: derefOr(plan.row.PreviewDigest, ""),
	}
}

func derefOr(p *string, d string) string {
	if p == nil {
		return d
	}
	return *p
}

func (s *Service) checkSite(ctx context.Context, p domain.Principal, a DispatchArgs) (dispatchPlan, bool, error) {
	var plan dispatchPlan
	var site sqlc.Site
	done := false
	err := s.runSiteTx(ctx, p, a.SiteID, func(tx pgx.Tx, q *sqlc.Queries) error {
		row, err := q.GetApprovedAbilityRequestForDispatch(ctx, sqlc.GetApprovedAbilityRequestForDispatchParams{
			TenantID: a.TenantID, ID: a.RequestID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			done = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("read approved ability request: %w", err)
		}
		plan.row = row.AssistantAbilityRequest
		if plan.row.SiteID != a.SiteID || plan.row.ProposedByGrantID != a.GrantID {
			return errors.New("ability request dispatch: job ids do not match the row")
		}
		var found bool
		site, found, err = readScopedSite(ctx, q, a.TenantID, a.SiteID)
		if err != nil {
			return err
		}
		if reason := notSentBeforeReserve(row, site, found); reason != "" {
			done = true
			return s.closeNotSent(ctx, tx, q, a.TenantID, a.RequestID, reason)
		}
		if s.entry == nil {
			return nil
		}
		// The bytes sent are the current catalogue row's, which the query
		// above just proved hashes to the approved entry_sha256.
		e, err := q.GetAbilityCatalogueEntry(ctx, plan.row.EntryID)
		if err != nil {
			return fmt.Errorf("read catalogue entry: %w", err)
		}
		plan.entry, plan.sum, err = s.entry(e)
		if err != nil || plan.sum != plan.row.EntrySha256 {
			done = true
			return s.closeNotSent(ctx, tx, q, a.TenantID, a.RequestID, ReasonEntryChanged)
		}
		if (plan.row.AbilityName == mcp.AbilityRestWrite) != (plan.row.RouteID != nil) {
			// The table CHECK pairs them; a row that breaks it is never sent.
			done = true
			return s.closeNotSent(ctx, tx, q, a.TenantID, a.RequestID, ReasonRouteChanged)
		}
		if plan.row.RouteID != nil {
			if s.route == nil {
				return nil // sendOn() is false: recorded as write_tools_disabled below
			}
			rr, err := q.GetRestRoute(ctx, *plan.row.RouteID)
			if errors.Is(err, pgx.ErrNoRows) {
				done = true
				return s.closeNotSent(ctx, tx, q, a.TenantID, a.RequestID, ReasonRouteDisabled)
			}
			if err != nil {
				return fmt.Errorf("read rest route: %w", err)
			}
			var why string
			plan.route, plan.routeSum, why = routeSendable(s.route, rr, derefOr(plan.row.RouteSha256, ""), plan.row.InputJson)
			if why != "" {
				done = true
				return s.closeNotSent(ctx, tx, q, a.TenantID, a.RequestID, why)
			}
		}
		plan.siteURL = site.Url
		return nil
	})
	if err != nil || done {
		return plan, done, err
	}
	transient := ""
	var forbidden bool
	var ctxErr error
	if s.rules != nil {
		_, forbidden, ctxErr = s.rules.ForbiddenByContext(ctx, a.TenantID, a.SiteID, mcp.ToolSiteAbilityRun)
	}
	switch {
	case !s.sendOn(), plan.row.RouteID != nil && s.route == nil:
		transient = AttemptWriteToolsDisabled
	case ctxErr != nil:
		transient = AttemptContextUnavailable
	case !connectedEnough(site.ConnectionState):
		transient = AttemptSiteUnreachable
	}
	if !forbidden && transient == "" {
		return plan, false, nil
	}
	err = s.runSiteTx(ctx, p, a.SiteID, func(tx pgx.Tx, q *sqlc.Queries) error {
		if forbidden && ctxErr == nil {
			return s.closeNotSent(ctx, tx, q, a.TenantID, a.RequestID, ReasonForbiddenByContext)
		}
		_, err := q.RecordAbilityRequestDispatchAttempt(ctx, sqlc.RecordAbilityRequestDispatchAttemptParams{
			LastAttemptCode: transient, TenantID: a.TenantID, ID: a.RequestID,
		})
		return err
	})
	return plan, true, err
}

// reserve: lifecycle try-lock, per-site lock, lifecycle FOR SHARE, the
// in-flight check, then the compare-and-set whose WHERE carries the
// deadline and the approved entry hash (W1), and its audit row.
func (s *Service) reserve(ctx context.Context, p domain.Principal, a DispatchArgs, plan dispatchPlan) (bool, error) {
	reserved := false
	err := s.runSiteTx(ctx, p, a.SiteID, func(tx pgx.Tx, q *sqlc.Queries) error {
		acquired, err := q.TryAssistantRequestXactLock(ctx, sqlc.TryAssistantRequestXactLockParams{
			LockKey: lifecycleLockKey, LockID: a.TenantID.String(),
		})
		if err != nil {
			return fmt.Errorf("lifecycle try-lock: %w", err)
		}
		if !acquired {
			return &transientError{code: AttemptOrgBusy}
		}
		if err := q.TakeAssistantRequestXactLock(ctx, sqlc.TakeAssistantRequestXactLockParams{
			LockKey: siteDispatchLockKey, LockID: a.SiteID.String(),
		}); err != nil {
			return fmt.Errorf("site dispatch lock: %w", err)
		}
		life, err := q.GetTenantAssistantLifecycleForShare(ctx, a.TenantID)
		if err != nil {
			return fmt.Errorf("read organisation lifecycle: %w", err)
		}
		switch {
		case life.Deleted:
			return s.closeNotSent(ctx, tx, q, a.TenantID, a.RequestID, ReasonOrganisationDeleted)
		case life.AssistantPaused:
			return s.closeNotSent(ctx, tx, q, a.TenantID, a.RequestID, ReasonAssistantPaused)
		}
		busy, err := q.AbilityRequestInFlightOnSite(ctx, sqlc.AbilityRequestInFlightOnSiteParams{
			TenantID: a.TenantID, SiteID: a.SiteID,
		})
		if err != nil {
			return fmt.Errorf("read in-flight ability write: %w", err)
		}
		if busy {
			return &transientError{code: AttemptSiteBusy}
		}
		n, err := q.ReserveAbilityRequestForDispatch(ctx, sqlc.ReserveAbilityRequestForDispatchParams{
			TenantID: a.TenantID, ID: a.RequestID,
		})
		if err != nil {
			return fmt.Errorf("reserve ability request: %w", err)
		}
		if n != 1 {
			return errLostReservation
		}
		if err := s.record(ctx, tx, a.TenantID, a.RequestID, audit.ActionAbilityRequestDispatched, map[string]any{
			"site_id": a.SiteID.String(), "ability": plan.row.AbilityName, "entry_sha256": plan.row.EntrySha256,
			"input_sha256": plan.row.InputSha256, "precheck_digest": plan.row.PrecheckDigest,
		}); err != nil {
			return err
		}
		reserved = true
		return nil
	})
	if errors.Is(err, errLostReservation) {
		return false, nil
	}
	return reserved, err
}

// ---------------------------------------------------------------------------
// Outcomes
// ---------------------------------------------------------------------------

// writeOutcome is what one write (or one ledger read) settled.
type writeOutcome struct {
	outcome       string // "" means still unknown: mark outcome_unknown
	code          *string
	notSentReason *string
	createdPostID *int64
	trashed       *bool
	siteText      *string
	undoUntil     pgtype.Timestamptz
	// restored and columnsStillChanged are a failed rest-write's own-undo
	// report (agentcmd.AbilityRunRefusal). restored is recorded on the row;
	// the columns, from the closed post column set, go to the audit row.
	restored            *bool
	columnsStillChanged []string
}

// withRestoreReport adds a failed write's own-undo report to a refusal
// outcome. Only a failed outcome carries one.
func (oc writeOutcome) withRestoreReport(restored *bool, columns []string) writeOutcome {
	if oc.outcome != OutcomeRefused && oc.outcome != OutcomeVerifyMismatch && oc.outcome != OutcomeFailed {
		return oc
	}
	oc.restored = restored
	if len(columns) > 0 {
		oc.columnsStillChanged = columns
	}
	return oc
}

func strp(s string) *string { return &s }

func createdOutcome(postID int64, now time.Time) writeOutcome {
	if postID < 1 {
		return writeOutcome{outcome: OutcomeFailed, code: strp("bad_answer")}
	}
	return writeOutcome{
		outcome: OutcomeCreated, createdPostID: &postID,
		undoUntil: pgtype.Timestamptz{Time: now.Add(undoRetention), Valid: true},
	}
}

// appliedOutcome is a route write that changed the target post: the post id
// is the row's target_post_id, and the person's undo opens.
func appliedOutcome(now time.Time) writeOutcome {
	return writeOutcome{
		outcome:   OutcomeApplied,
		undoUntil: pgtype.Timestamptz{Time: now.Add(undoRetention), Valid: true},
	}
}

func refusedOutcome(code, detail string, postID int64, trashed bool) writeOutcome {
	oc := writeOutcome{outcome: OutcomeRefused, code: strp(code)}
	switch code {
	case "verify_mismatch":
		oc.outcome = OutcomeVerifyMismatch
		oc.trashed = &trashed
		if postID > 0 {
			oc.createdPostID = &postID
		}
	case "snapshot_failed":
		// After the insert, the site names the draft it created and whether
		// it trashed it: recorded so the draft is visible, and recoverable
		// when it was not trashed. Before the insert nothing was created.
		if postID > 0 {
			oc.createdPostID = &postID
			oc.trashed = &trashed
		}
	}
	if d := humantext.CapBytes(humantext.Clean(detail), maxSiteReportedText); d != "" {
		oc.siteText = &d
	}
	return oc
}

// ledgerResult is the stored write result inside a ledger or
// already_applied answer.
type ledgerResult struct {
	OK      bool   `json:"ok"`
	Outcome string `json:"outcome"`
	PostID  int64  `json:"post_id"`
	Code    string `json:"code"`
	Detail  string `json:"detail"`
	Trashed bool   `json:"trashed"`
	// A failed rest-write's own-undo report, as in a direct refusal.
	Restored            *bool           `json:"restored"`
	Changed             *bool           `json:"changed"`
	ColumnsStillChanged json.RawMessage `json:"columns_still_changed"`
}

func outcomeFromStored(raw json.RawMessage, now time.Time) (writeOutcome, bool) {
	var r ledgerResult
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &r) != nil {
		return writeOutcome{}, false
	}
	if r.OK && r.Outcome == "created" {
		return createdOutcome(r.PostID, now), true
	}
	if r.OK && r.Outcome == "updated" {
		return appliedOutcome(now), true
	}
	if !r.OK && r.Code != "" {
		code := r.Code
		if _, known := agentcmd.AbilityRunRefusalCodes[code]; !known {
			code = "unknown"
		}
		return refusedOutcome(code, r.Detail, r.PostID, r.Trashed).withRestoreReport(
			agentcmd.RestoreReport(r.Restored, r.Changed), agentcmd.DecodePostColumns(r.ColumnsStillChanged)), true
	}
	return writeOutcome{}, false
}

// classifyWrite maps the send's result. Never a resend: anything that is
// not a definite answer is outcome unknown.
func classifyWrite(resp agentcmd.AbilityRunResponse, err error, now time.Time) writeOutcome {
	if err == nil {
		switch resp.Outcome {
		case "created":
			return createdOutcome(resp.PostID, now)
		case "updated":
			return appliedOutcome(now)
		case "already_applied":
			if oc, ok := outcomeFromStored(resp.Result, now); ok {
				return oc
			}
		}
		return writeOutcome{}
	}
	var refusal *agentcmd.AbilityRunRefusal
	if errors.As(err, &refusal) {
		if refusal.Code == "request_in_flight" {
			return writeOutcome{} // ours is running on the site; the ledger answers
		}
		return refusedOutcome(refusal.Code, refusal.Detail, refusal.PostID, refusal.Trashed).withRestoreReport(
			refusal.Restored, refusal.ColumnsStillChanged)
	}
	if errors.Is(err, agentcmd.ErrCommandNotSent) {
		return writeOutcome{outcome: OutcomeNotSent, notSentReason: strp(ReasonTransportPreSend)}
	}
	return writeOutcome{}
}

func (s *Service) recordOutcome(ctx context.Context, p domain.Principal, a DispatchArgs, oc writeOutcome) error {
	err := s.runSiteTx(ctx, p, a.SiteID, func(tx pgx.Tx, q *sqlc.Queries) error {
		return s.writeOutcomeTx(ctx, tx, q, a.TenantID, a.RequestID, oc)
	})
	if err != nil {
		// The reconciler moves the row to outcome_unknown; never resend.
		s.logger.ErrorContext(ctx, "ability request: outcome not recorded",
			slog.String("request_id", a.RequestID.String()), slog.Any("error", err))
	}
	return nil
}

func (s *Service) writeOutcomeTx(ctx context.Context, tx pgx.Tx, q *sqlc.Queries, tenantID, requestID uuid.UUID, oc writeOutcome) error {
	if oc.outcome == "" {
		n, err := q.MarkAbilityRequestOutcomeUnknown(ctx, sqlc.MarkAbilityRequestOutcomeUnknownParams{TenantID: tenantID, ID: requestID})
		if err != nil || n != 1 {
			return err
		}
		return s.record(ctx, tx, tenantID, requestID, audit.ActionAbilityRequestFailed, map[string]any{"class": OutcomeUnknown})
	}
	n, err := q.RecordAbilityRequestOutcome(ctx, sqlc.RecordAbilityRequestOutcomeParams{
		Outcome: oc.outcome, OutcomeCode: oc.code, NotSentReason: oc.notSentReason,
		CreatedPostID: oc.createdPostID, Restored: oc.restored, Trashed: oc.trashed, SiteReportedText: oc.siteText,
		UndoAvailableUntil: oc.undoUntil, TenantID: tenantID, ID: requestID,
	})
	if err != nil {
		return fmt.Errorf("record ability outcome: %w", err)
	}
	if n != 1 {
		s.logger.WarnContext(ctx, "ability request: late_outcome",
			slog.String("request_id", requestID.String()), slog.String("outcome", oc.outcome))
		return nil
	}
	md := map[string]any{"outcome": oc.outcome}
	if oc.code != nil {
		md["code"] = *oc.code
	}
	if oc.createdPostID != nil {
		md["created_post_id"] = *oc.createdPostID
	}
	if oc.trashed != nil {
		md["trashed"] = *oc.trashed
	}
	if oc.restored != nil {
		md["restored"] = *oc.restored
	}
	if len(oc.columnsStillChanged) > 0 {
		md["columns_still_changed"] = oc.columnsStillChanged
	}
	action := audit.ActionAbilityRequestFailed
	switch oc.outcome {
	case OutcomeCreated, OutcomeApplied:
		action = audit.ActionAssistantRequestCompleted
	case OutcomeNotSent:
		action = audit.ActionAbilityRequestNotSent
	}
	return s.record(ctx, tx, tenantID, requestID, action, md)
}

// ---------------------------------------------------------------------------
// Sweeper and reconciler
// ---------------------------------------------------------------------------

func (s *Service) sweepExpired(ctx context.Context) error {
	var rows []sqlc.ScanLapsedPendingAbilityRequestsRow
	if err := s.runAgentScan(ctx, func(q *sqlc.Queries) error {
		var err error
		rows, err = q.ScanLapsedPendingAbilityRequests(ctx, scanRowLimit)
		return err
	}); err != nil {
		return fmt.Errorf("scan lapsed ability requests: %w", err)
	}
	var errs []error
	for _, r := range rows {
		tenantID, id := r.TenantID, r.ID
		errs = append(errs, s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
			n, err := sqlc.New(tx).ExpireAbilityRequest(ctx, sqlc.ExpireAbilityRequestParams{TenantID: tenantID, ID: id})
			if err != nil || n != 1 {
				return err
			}
			return s.record(ctx, tx, tenantID, id, audit.ActionAbilityRequestExpired, map[string]any{"expired_by": "sweeper"})
		}))
	}
	return errors.Join(errs...)
}

func (s *Service) sweepPastDeadline(ctx context.Context) error {
	var rows []sqlc.ScanApprovedAbilityRequestsPastDeadlineRow
	if err := s.runAgentScan(ctx, func(q *sqlc.Queries) error {
		var err error
		rows, err = q.ScanApprovedAbilityRequestsPastDeadline(ctx, scanRowLimit)
		return err
	}); err != nil {
		return fmt.Errorf("scan ability requests past deadline: %w", err)
	}
	var errs []error
	for _, r := range rows {
		tenantID, id := r.TenantID, r.ID
		errs = append(errs, s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
			n, err := sqlc.New(tx).CloseAbilityRequestPastDeadline(ctx, sqlc.CloseAbilityRequestPastDeadlineParams{TenantID: tenantID, ID: id})
			if err != nil || n != 1 {
				return err
			}
			return s.record(ctx, tx, tenantID, id, audit.ActionAbilityRequestNotSent,
				map[string]any{"reason": ReasonDispatchDeadlinePassed, "closed_by": "sweeper"})
		}))
	}
	return errors.Join(errs...)
}

// reconcileStale moves a sent row with no reply recorded to outcome_unknown.
// It never resends.
func (s *Service) reconcileStale(ctx context.Context) error {
	var rows []sqlc.ScanStaleDispatchedAbilityRequestsRow
	if err := s.runAgentScan(ctx, func(q *sqlc.Queries) error {
		var err error
		rows, err = q.ScanStaleDispatchedAbilityRequests(ctx, sqlc.ScanStaleDispatchedAbilityRequestsParams{
			StaleAfterSeconds: int32(StaleDispatchAfter / time.Second), RowLimit: scanRowLimit,
		})
		return err
	}); err != nil {
		return fmt.Errorf("scan stale dispatched ability requests: %w", err)
	}
	var errs []error
	for _, r := range rows {
		tenantID, id := r.TenantID, r.ID
		errs = append(errs, s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
			return s.writeOutcomeTx(ctx, tx, sqlc.New(tx), tenantID, id, writeOutcome{})
		}))
	}
	return errors.Join(errs...)
}

// ledgerVerdict decides a resolving row from one ledger answer. done=false
// means still running or not yet decidable: record the check and wait.
func ledgerVerdict(resp agentcmd.AbilityRunResponse, unknownFor time.Duration, now time.Time) (writeOutcome, bool) {
	if resp.Inflight {
		return writeOutcome{}, false
	}
	if !resp.Found {
		// The ledger row is written before any effect, and no write can
		// arrive once its token has lapsed: nothing was created.
		if unknownFor >= tokenSettled {
			return writeOutcome{outcome: OutcomeFailed, code: strp("not_received")}, true
		}
		return writeOutcome{}, false
	}
	if oc, ok := outcomeFromStored(resp.Result, now); ok {
		return oc, true
	}
	// A ledger row with no result and no inflight claim: interrupted mid-way.
	oc := writeOutcome{outcome: OutcomeFailed, code: strp("interrupted")}
	if resp.CreatedPostID != nil && *resp.CreatedPostID > 0 {
		id := *resp.CreatedPostID
		oc.createdPostID = &id
	}
	return oc, true
}

// resolveUnknown asks each resolving row's site for its ledger row, once per
// poll interval, for up to the resolution window; then "could not tell".
func (s *Service) resolveUnknown(ctx context.Context) error {
	var rows []sqlc.ScanResolvingAbilityRequestsRow
	if err := s.runAgentScan(ctx, func(q *sqlc.Queries) error {
		var err error
		rows, err = q.ScanResolvingAbilityRequests(ctx, sqlc.ScanResolvingAbilityRequestsParams{
			PollIntervalSeconds: int32(ledgerPollInterval / time.Second), RowLimit: scanRowLimit,
		})
		return err
	}); err != nil {
		return fmt.Errorf("scan resolving ability requests: %w", err)
	}
	var errs []error
	for _, r := range rows {
		errs = append(errs, s.resolveOne(ctx, r))
	}
	return errors.Join(errs...)
}

func (s *Service) resolveOne(ctx context.Context, r sqlc.ScanResolvingAbilityRequestsRow) error {
	now := s.now()
	unknownFor := time.Duration(0)
	if r.UnknownSince.Valid {
		unknownFor = now.Sub(r.UnknownSince.Time)
	}
	var site sqlc.GetSiteRow
	var found bool
	if err := s.pool.InTenantTx(ctx, r.TenantID, func(tx pgx.Tx) error {
		var err error
		site, err = sqlc.New(tx).GetSite(ctx, sqlc.GetSiteParams{TenantID: r.TenantID, ID: r.SiteID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	}); err != nil {
		return fmt.Errorf("read site for ledger: %w", err)
	}
	var verdict writeOutcome
	decided := false
	if found && s.agent != nil {
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		resp, err := s.agent.AbilityRun(callCtx, r.SiteID, site.Url, agentcmd.AbilityRunCall{
			Mode: agentcmd.AbilityRunModeLedger, RequestID: r.ID,
		})
		cancel()
		if err == nil {
			verdict, decided = ledgerVerdict(resp, unknownFor, now)
		}
	}
	return s.pool.InTenantTx(ctx, r.TenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if decided {
			return s.writeOutcomeTx(ctx, tx, q, r.TenantID, r.ID, verdict)
		}
		if unknownFor >= ledgerResolveWindow {
			n, err := q.GiveUpAbilityRequestOutcome(ctx, sqlc.GiveUpAbilityRequestOutcomeParams{
				TenantID: r.TenantID, ID: r.ID, WindowSeconds: int32(ledgerResolveWindow / time.Second),
			})
			if err != nil || n != 1 {
				return err
			}
			return s.record(ctx, tx, r.TenantID, r.ID, audit.ActionAbilityRequestFailed,
				map[string]any{"class": OutcomeUnknown, "closed_by": "ledger_window"})
		}
		_, err := q.RecordAbilityRequestLedgerCheck(ctx, sqlc.RecordAbilityRequestLedgerCheckParams{TenantID: r.TenantID, ID: r.ID})
		return err
	})
}
