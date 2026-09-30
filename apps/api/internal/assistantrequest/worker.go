package assistantrequest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/internal/perf"
)

// ---------------------------------------------------------------------------
// River jobs
// ---------------------------------------------------------------------------

// tenantQueueShards is the number of per-tenant dispatch queues. A tenant
// always maps to the same one, so one organisation's burst fills only its
// own shard.
const tenantQueueShards = 8

// dispatchMaxWorkers bounds concurrent dispatches per shard.
const dispatchMaxWorkers = 4

// QueueForTenant is the dispatch queue a tenant's jobs run on.
func QueueForTenant(tenantID uuid.UUID) string {
	return fmt.Sprintf("assistant_request_t%d", int(tenantID[0])%tenantQueueShards)
}

// Queues is the River queue configuration the dispatch jobs need.
func Queues() map[string]river.QueueConfig {
	out := make(map[string]river.QueueConfig, tenantQueueShards)
	for i := 0; i < tenantQueueShards; i++ {
		out[fmt.Sprintf("assistant_request_t%d", i)] = river.QueueConfig{MaxWorkers: dispatchMaxWorkers}
	}
	return out
}

// ScanArgs is the periodic scan for approved requests that are due.
type ScanArgs struct{}

// Kind implements river.JobArgs.
func (ScanArgs) Kind() string { return "assistant_request_scan" }

// DispatchArgs is one approved request to try. It carries ids only; the
// worker re-reads everything it acts on.
type DispatchArgs struct {
	TenantID  uuid.UUID `json:"tenant_id"`
	RequestID uuid.UUID `json:"request_id"`
	SiteID    uuid.UUID `json:"site_id"`
	GrantID   uuid.UUID `json:"grant_id"`
}

// Kind implements river.JobArgs.
func (DispatchArgs) Kind() string { return "assistant_request_dispatch" }

// InsertOpts pins the job to its tenant's shard and keeps at most one live
// job per request, so a scan every few seconds never stacks duplicates.
func (a DispatchArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       QueueForTenant(a.TenantID),
		MaxAttempts: 3,
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRunning,
				rivertype.JobStateRetryable,
				rivertype.JobStateScheduled,
			},
		},
	}
}

// SweepArgs is the periodic sweeper: waiting requests past their window
// become expired, and approved requests past their dispatch deadline close.
type SweepArgs struct{}

// Kind implements river.JobArgs.
func (SweepArgs) Kind() string { return "assistant_request_sweep" }

// ReconcileArgs is the periodic reconciler: a sent request with no outcome
// after StaleDispatchAfter is recorded as outcome unknown.
type ReconcileArgs struct{}

// Kind implements river.JobArgs.
func (ReconcileArgs) Kind() string { return "assistant_request_reconcile" }

// Enqueuer inserts dispatch jobs. The River client satisfies it through
// RiverEnqueuer.
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
		return fmt.Errorf("enqueue assistant request dispatch: %w", err)
	}
	return nil
}

// ScanWorker runs the scan. It runs whether or not write tools are on, so a
// switched-off server still records why each approved row is waiting.
type ScanWorker struct {
	river.WorkerDefaults[ScanArgs]
	svc      *Service
	enqueuer Enqueuer
}

// NewScanWorker builds the scan worker. Wire the enqueuer after River starts.
func NewScanWorker(svc *Service) *ScanWorker { return &ScanWorker{svc: svc} }

// SetEnqueuer wires the River enqueuer.
func (w *ScanWorker) SetEnqueuer(e Enqueuer) { w.enqueuer = e }

// Work runs one scan.
func (w *ScanWorker) Work(ctx context.Context, _ *river.Job[ScanArgs]) error {
	if w.enqueuer == nil {
		w.svc.logger.WarnContext(ctx, "assistant request scan: enqueuer not wired; skipping")
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

// Timeout bounds one dispatch: the send is bounded well below it.
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

// Work runs one sweep. Both passes run even if the first fails.
func (w *SweepWorker) Work(ctx context.Context, _ *river.Job[SweepArgs]) error {
	err1 := w.svc.sweepExpired(ctx)
	err2 := w.svc.sweepPastDeadline(ctx)
	return errors.Join(err1, err2)
}

// ReconcileWorker runs the reconciler.
type ReconcileWorker struct {
	river.WorkerDefaults[ReconcileArgs]
	svc *Service
}

// NewReconcileWorker builds the reconciler.
func NewReconcileWorker(svc *Service) *ReconcileWorker { return &ReconcileWorker{svc: svc} }

// Work runs one reconcile pass.
func (w *ReconcileWorker) Work(ctx context.Context, _ *river.Job[ReconcileArgs]) error {
	return w.svc.reconcile(ctx)
}

// ---------------------------------------------------------------------------
// Scan
// ---------------------------------------------------------------------------

func (s *Service) scan(ctx context.Context, enq Enqueuer) error {
	var due []sqlc.ScanDueApprovedAssistantCachePurgeRequestsRow
	err := s.repo.runAgentScan(ctx, func(tx pgx.Tx) error {
		var err error
		due, err = sqlc.New(tx).ScanDueApprovedAssistantCachePurgeRequests(ctx,
			sqlc.ScanDueApprovedAssistantCachePurgeRequestsParams{
				BackoffCapSeconds:  int32(BackoffCap / time.Second),
				BackoffBaseSeconds: int32(BackoffBase / time.Second),
				RowLimit:           scanRowLimit,
			})
		return err
	})
	if err != nil {
		return fmt.Errorf("scan approved assistant requests: %w", err)
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

// ---------------------------------------------------------------------------
// Dispatch
// ---------------------------------------------------------------------------

// transientError is a reason the request cannot start yet. The reservation
// transaction returns it to roll back; the attempt is then recorded in a
// short transaction of its own.
type transientError struct{ code string }

func (e *transientError) Error() string { return "assistant request not started: " + e.code }

// terminalCloseInTx closes an approved request as not sent, on the caller's
// single-site transaction, and writes its audit row only if this call closed
// it. A 0-row close means another path closed or reserved it first.
func (s *Service) terminalCloseInTx(ctx context.Context, tx pgx.Tx, tenantID, requestID uuid.UUID, reason string, lastAttemptCode *string) error {
	n, err := sqlc.New(tx).CloseApprovedAssistantCachePurgeRequestNotSent(ctx,
		sqlc.CloseApprovedAssistantCachePurgeRequestNotSentParams{NotSentReason: reason, TenantID: tenantID, ID: requestID})
	if err != nil {
		return fmt.Errorf("close assistant request as not sent: %w", err)
	}
	if n != 1 {
		return nil
	}
	return s.recordNotSent(ctx, tx, tenantID, requestID, reason, lastAttemptCode, "")
}

func (s *Service) recordNotSent(ctx context.Context, tx pgx.Tx, tenantID, requestID uuid.UUID, reason string, lastAttemptCode *string, closedBy string) error {
	if s.audit == nil {
		return errors.New("audit recorder not wired")
	}
	md := map[string]any{"request_id": requestID.String(), "reason": reason}
	if lastAttemptCode != nil {
		md["last_attempt_code"] = *lastAttemptCode
	}
	if closedBy != "" {
		md["closed_by"] = closedBy
	}
	_, err := s.audit.RecordInTx(ctx, tx, audit.Event{
		TenantID:   tenantID,
		ActorType:  audit.ActorSystem,
		Action:     audit.ActionAssistantRequestNotSent,
		TargetType: audit.TargetTypeAssistantCachePurgeRequest,
		TargetID:   requestID.String(),
		Metadata:   md,
	})
	return err
}

// closeWithoutSiteReasons is the closed set closeWithoutSite accepts: the
// closes decided before a site principal can exist.
var closeWithoutSiteReasons = map[string]struct{}{
	ReasonGrantInactive:     {},
	ReasonAssistantPaused:   {},
	ReasonCapabilityNotHeld: {},
	ReasonSiteAbsent:        {},
}

// closeWithoutSite closes an approved request as not sent when no site
// principal can be built for it: the connection is inactive, the assistant is
// paused, the capability is gone, or the site has left the connection's
// scope. It runs in a tenant transaction and touches only the request row and
// the audit log, never site data. A reason outside its closed set is a
// programming error: it returns an error and writes nothing.
func (s *Service) closeWithoutSite(ctx context.Context, tenantID, requestID uuid.UUID, reason string) error {
	if _, ok := closeWithoutSiteReasons[reason]; !ok {
		return fmt.Errorf("closeWithoutSite: reason %q is not one it may record", reason)
	}
	if tenantID == uuid.Nil || requestID == uuid.Nil {
		return errors.New("closeWithoutSite: nil id")
	}
	return s.repo.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		n, err := sqlc.New(tx).CloseApprovedAssistantCachePurgeRequestNotSent(ctx,
			sqlc.CloseApprovedAssistantCachePurgeRequestNotSentParams{NotSentReason: reason, TenantID: tenantID, ID: requestID})
		if err != nil {
			return fmt.Errorf("close assistant request without site: %w", err)
		}
		if n != 1 {
			return nil
		}
		return s.recordNotSent(ctx, tx, tenantID, requestID, reason, nil, "")
	})
}

// recordAttempt records a transient reason on the row, which stays approved.
func (s *Service) recordAttempt(ctx context.Context, p domain.Principal, tenantID, siteID, requestID uuid.UUID, code string) error {
	return s.repo.runSiteTx(ctx, p, siteID, func(tx pgx.Tx) error {
		_, err := sqlc.New(tx).RecordAssistantCachePurgeDispatchAttempt(ctx,
			sqlc.RecordAssistantCachePurgeDispatchAttemptParams{LastAttemptCode: code, TenantID: tenantID, ID: requestID})
		return err
	})
}

// dispatchPlan is what the checks hand the reservation and the send.
type dispatchPlan struct {
	row     sqlc.AssistantCachePurgeRequest
	siteURL string
}

// dispatch tries one approved request: grant checks in Go, the site checks,
// the one reservation, the send, and the outcome.
func (s *Service) dispatch(ctx context.Context, a DispatchArgs) error {
	tenantID, requestID, siteID := a.TenantID, a.RequestID, a.SiteID
	if tenantID == uuid.Nil || requestID == uuid.Nil || siteID == uuid.Nil || a.GrantID == uuid.Nil {
		return errors.New("assistant request dispatch: nil id in job")
	}

	// (1) The grant and the site's membership in its scope, decided in Go
	// before any site principal exists. Every close here is closeWithoutSite.
	v, err := s.grants.ReCheckGrantAuthorization(ctx, tenantID, a.GrantID)
	if err != nil {
		return fmt.Errorf("re-check grant: %w", err)
	}
	if !v.Found || !v.Authorized {
		reason := ReasonGrantInactive
		if v.TenantAssistantPaused {
			reason = ReasonAssistantPaused
		}
		return s.closeWithoutSite(ctx, tenantID, requestID, reason)
	}
	auth, err := s.authz.AuthorizeGrant(ctx, tenantID, v)
	if errors.Is(err, mcp.ErrGrantNotAuthorized) {
		return s.closeWithoutSite(ctx, tenantID, requestID, ReasonGrantInactive)
	}
	if err != nil {
		return fmt.Errorf("derive grant: %w", err)
	}
	if !mcp.ToolPermitted(mcp.ToolSiteCachePurgeRequest, auth) {
		return s.closeWithoutSite(ctx, tenantID, requestID, ReasonCapabilityNotHeld)
	}
	// auth, and so p, is built once per run and never re-derived after the
	// send, so an in-flight clear always records its outcome.
	p, err := mcp.SingleSitePrincipal(auth, siteID)
	if errors.Is(err, mcp.ErrSiteNotInScope) {
		return s.closeWithoutSite(ctx, tenantID, requestID, ReasonSiteAbsent)
	}
	if err != nil {
		return fmt.Errorf("site principal: %w", err)
	}

	// (2) The site checks.
	plan, done, err := s.checkSite(ctx, p, a)
	if err != nil || done {
		return err
	}

	// (3) The one reservation point.
	cdn, reserved, err := s.reserve(ctx, p, a, plan)
	var te *transientError
	if errors.As(err, &te) {
		return s.recordAttempt(ctx, p, tenantID, siteID, requestID, te.code)
	}
	if err != nil {
		s.logger.ErrorContext(ctx, "assistant request: reservation failed",
			slog.String("request_id", requestID.String()), slog.Any("error", err))
		return err
	}
	if !reserved {
		return nil
	}

	// (4) The send, with no transaction open.
	scope := plan.row.Scope
	pageURL := ""
	if plan.row.Url != nil {
		pageURL = *plan.row.Url
	}
	var res perf.AssistantPurgeResult
	var sendErr error
	if s.sender == nil {
		sendErr = perf.ErrAssistantPurgeNotWired
	} else {
		res, sendErr = s.sender.SendAssistantPurge(ctx, siteID, plan.siteURL, cdn, perf.AssistantPurge{Scope: scope, URL: pageURL})
	}

	// (5) The outcome, compare-and-set on "sent with no outcome".
	oc := classify(res, sendErr)
	return s.recordOutcome(ctx, p, a, plan.row, oc)
}

// checkSite runs the site checks under the single-site principal. done is
// true when the request was closed, or when there is nothing left to do.
func (s *Service) checkSite(ctx context.Context, p domain.Principal, a DispatchArgs) (dispatchPlan, bool, error) {
	var plan dispatchPlan
	var site sqlc.Site
	done := false
	err := s.repo.runSiteTx(ctx, p, a.SiteID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetApprovedAssistantCachePurgeRequestForDispatch(ctx,
			sqlc.GetApprovedAssistantCachePurgeRequestForDispatchParams{
				DeadlineSeconds: int32(DispatchDeadline / time.Second),
				TenantID:        a.TenantID,
				ID:              a.RequestID,
			})
		if errNoRow(err) {
			done = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("read approved request: %w", err)
		}
		plan.row = row.AssistantCachePurgeRequest
		if plan.row.SiteID != a.SiteID || plan.row.ProposedByGrantID != a.GrantID {
			return errors.New("assistant request dispatch: job ids do not match the row")
		}
		var found bool
		site, found, err = readScopedSite(ctx, tx, a.TenantID, a.SiteID)
		if err != nil {
			return err
		}
		switch {
		case !found:
			done = true
			return s.terminalCloseInTx(ctx, tx, a.TenantID, a.RequestID, ReasonSiteAbsent, plan.row.LastAttemptCode)
		case row.PastDeadline:
			done = true
			return s.terminalCloseInTx(ctx, tx, a.TenantID, a.RequestID, ReasonDispatchDeadlinePassed, plan.row.LastAttemptCode)
		case !agentMeetsFloor(site.AgentVersion):
			done = true
			return s.terminalCloseInTx(ctx, tx, a.TenantID, a.RequestID, ReasonAgentOutdated, plan.row.LastAttemptCode)
		}
		plan.siteURL = site.Url
		return nil
	})
	if err != nil || done {
		return plan, done, err
	}

	// The operator's AI rules, read only after the scoped site read. The job
	// context carries no principal, so this is a tenant read.
	_, forbidden, ctxErr := s.authz.ForbiddenByContext(ctx, a.TenantID, a.SiteID, mcp.ToolSiteCachePurgeRequest)

	var transient string
	err = s.repo.runSiteTx(ctx, p, a.SiteID, func(tx pgx.Tx) error {
		if ctxErr == nil && forbidden {
			done = true
			return s.terminalCloseInTx(ctx, tx, a.TenantID, a.RequestID, ReasonForbiddenByContext, plan.row.LastAttemptCode)
		}
		switch {
		case !s.writeToolsOn():
			transient = AttemptWriteToolsDisabled
		case !connectedEnough(site.ConnectionState):
			transient = AttemptSiteUnreachable
		case ctxErr != nil:
			transient = AttemptContextUnavailable
		default:
			code, err := previewLimits(ctx, tx, a.TenantID, a.SiteID, plan.row.Scope)
			if err != nil {
				return err
			}
			transient = code
		}
		if transient == "" {
			return nil
		}
		done = true
		_, err := sqlc.New(tx).RecordAssistantCachePurgeDispatchAttempt(ctx,
			sqlc.RecordAssistantCachePurgeDispatchAttemptParams{LastAttemptCode: transient, TenantID: a.TenantID, ID: a.RequestID})
		return err
	})
	return plan, done, err
}

// previewLimits reads the cooldown and the hourly cap. It returns the
// transient code that blocks, or "".
//
// The cooldown applies to a whole-site clear: it keeps a site from being
// held uncached by back-to-back whole-site clears. A page clear is not held
// by it.
func previewLimits(ctx context.Context, tx pgx.Tx, tenantID, siteID uuid.UUID, scope string) (string, error) {
	q := sqlc.New(tx)
	if scope == string(perf.PurgeKindAll) {
		_, err := q.GetLatestWholeSitePurgeOnSiteSince(ctx, sqlc.GetLatestWholeSitePurgeOnSiteSinceParams{
			WindowSeconds: int32(WholeSiteCooldown / time.Second), TenantID: tenantID, SiteID: siteID,
		})
		if err == nil {
			return AttemptSiteCooldown, nil
		}
		if !errNoRow(err) {
			return "", fmt.Errorf("read whole-site cooldown: %w", err)
		}
	}
	n, err := q.CountAssistantCachePurgesOnSiteSince(ctx, sqlc.CountAssistantCachePurgesOnSiteSinceParams{
		WindowSeconds: int32(time.Hour / time.Second), TenantID: tenantID, SiteID: siteID,
	})
	if err != nil {
		return "", fmt.Errorf("read site hourly cap: %w", err)
	}
	if n >= SiteHourlyCap {
		return AttemptSiteHourlyCap, nil
	}
	return "", nil
}

// reserve is the ONE place an AI clear is recorded in cache_purge_audit and
// the request moves to dispatched. Lock order: the organisation lifecycle
// try-lock, the per-site dispatch lock, tenants FOR SHARE, the request row,
// then the audit chain. A transient reason returns *transientError and rolls
// everything back. reserved is false when another path closed or reserved the
// row first, or when a terminal close was written here instead.
func (s *Service) reserve(ctx context.Context, p domain.Principal, a DispatchArgs, plan dispatchPlan) (perf.CDNCiphertext, bool, error) {
	var cdn perf.CDNCiphertext
	reserved := false
	err := s.repo.runSiteTx(ctx, p, a.SiteID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		acquired, err := q.TryAssistantRequestXactLock(ctx, sqlc.TryAssistantRequestXactLockParams{
			LockKey: lifecycleLockKey, LockID: a.TenantID.String(),
		})
		if err != nil {
			// A statement error is never org_busy.
			s.logger.ErrorContext(ctx, "assistant request: lock_statement_failed",
				slog.String("request_id", a.RequestID.String()), slog.Any("error", err))
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
			return s.terminalCloseInTx(ctx, tx, a.TenantID, a.RequestID, ReasonOrganisationDeleted, plan.row.LastAttemptCode)
		case life.AssistantPaused:
			return s.terminalCloseInTx(ctx, tx, a.TenantID, a.RequestID, ReasonAssistantPaused, plan.row.LastAttemptCode)
		}
		busy, err := q.AssistantCachePurgeInFlightOnSite(ctx, sqlc.AssistantCachePurgeInFlightOnSiteParams{
			TenantID: a.TenantID, SiteID: a.SiteID,
		})
		if err != nil {
			return fmt.Errorf("read in-flight clear: %w", err)
		}
		if busy {
			return &transientError{code: AttemptSiteBusy}
		}
		code, err := previewLimits(ctx, tx, a.TenantID, a.SiteID, plan.row.Scope)
		if err != nil {
			return err
		}
		if code != "" {
			return &transientError{code: code}
		}
		if s.store == nil {
			return &transientError{code: AttemptWriteToolsDisabled}
		}
		cdn, err = s.store.GetCDNCredentialsCiphertextTx(ctx, tx, a.TenantID, a.SiteID)
		if err != nil {
			return err
		}
		if !plan.row.DecidedByUserID.Valid {
			return errors.New("approved request names no approver")
		}
		approver := uuid.UUID(plan.row.DecidedByUserID.Bytes)
		targets := []string{}
		if plan.row.Url != nil {
			targets = []string{*plan.row.Url}
		}
		entry, err := q.InsertAssistantCachePurgeAudit(ctx, sqlc.InsertAssistantCachePurgeAuditParams{
			TenantID:         a.TenantID,
			SiteID:           a.SiteID,
			Kind:             plan.row.Scope,
			ApproverUserID:   approver,
			InitiatorGrantID: a.GrantID,
			TargetUrls:       targets,
			UrlsCount:        int32(len(targets)),
		})
		if err != nil {
			return fmt.Errorf("record purge attempt: %w", err)
		}
		n, err := q.ReserveAssistantCachePurgeRequest(ctx, sqlc.ReserveAssistantCachePurgeRequestParams{
			CachePurgeAuditID: entry.ID,
			DeadlineSeconds:   int32(DispatchDeadline / time.Second),
			TenantID:          a.TenantID,
			ID:                a.RequestID,
		})
		if err != nil {
			return fmt.Errorf("reserve request: %w", err)
		}
		if n != 1 {
			// Another replica, the revoke cascade, a close or the sweeper won.
			// Roll back the attempt record with it.
			return errLostReservation
		}
		md := map[string]any{
			"request_id":           a.RequestID.String(),
			"site_id":              a.SiteID.String(),
			"scope":                plan.row.Scope,
			"cache_purge_audit_id": entry.ID.String(),
		}
		if !entry.InitiatorUserID.Valid {
			md["approver_account_deleted"] = true
		}
		if s.audit == nil {
			return errors.New("audit recorder not wired")
		}
		if _, err := s.audit.RecordInTx(ctx, tx, audit.Event{
			TenantID:   a.TenantID,
			ActorType:  audit.ActorSystem,
			Action:     audit.ActionAssistantRequestDispatched,
			TargetType: audit.TargetTypeAssistantCachePurgeRequest,
			TargetID:   a.RequestID.String(),
			Metadata:   md,
		}); err != nil {
			return err
		}
		reserved = true
		return nil
	})
	if errors.Is(err, errLostReservation) {
		return perf.CDNCiphertext{}, false, nil
	}
	if err != nil {
		return perf.CDNCiphertext{}, false, err
	}
	return cdn, reserved, nil
}

var errLostReservation = errors.New("assistant request: reservation lost to another path")

// recordOutcome writes the outcome on the same single-site principal. A 0-row
// compare-and-set means the reconciler already closed it: nothing is written
// and the real class is logged.
func (s *Service) recordOutcome(ctx context.Context, p domain.Principal, a DispatchArgs, row sqlc.AssistantCachePurgeRequest, oc outcome) error {
	wrote := false
	err := s.repo.runSiteTx(ctx, p, a.SiteID, func(tx pgx.Tx) error {
		n, err := sqlc.New(tx).RecordAssistantCachePurgeOutcome(ctx, sqlc.RecordAssistantCachePurgeOutcomeParams{
			Outcome:              oc.Outcome,
			NotSentReason:        oc.NotSentReason,
			HostingCachesCleared: oc.HostingCleared,
			HostingCachesSkipped: oc.HostingSkipped,
			OriginOnlyConfirmed:  oc.OriginOnlyConfirmed,
			WpmgrCdn:             oc.WpmgrCDN,
			SiteReportedText:     oc.SiteReportedText,
			TenantID:             a.TenantID,
			ID:                   a.RequestID,
		})
		if err != nil {
			return fmt.Errorf("record outcome: %w", err)
		}
		if n != 1 {
			s.logger.WarnContext(ctx, "assistant request: late_outcome",
				slog.String("request_id", a.RequestID.String()), slog.String("outcome", oc.Outcome))
			return nil
		}
		if s.audit == nil {
			return errors.New("audit recorder not wired")
		}
		ev := outcomeAuditEvent(a, row, oc)
		if _, err := s.audit.RecordInTx(ctx, tx, ev); err != nil {
			return err
		}
		if oc.Outcome == OutcomePurged {
			if s.store == nil {
				return errors.New("site cache store not wired")
			}
			if err := s.store.MarkCachePurgedTx(ctx, tx, a.TenantID, a.SiteID, row.Scope); err != nil {
				return fmt.Errorf("stamp purge gauge: %w", err)
			}
		}
		wrote = true
		return nil
	})
	if err != nil {
		// The reconciler closes the row as outcome unknown.
		s.logger.ErrorContext(ctx, "assistant request: audit_gap",
			slog.String("request_id", a.RequestID.String()), slog.String("outcome", oc.Outcome), slog.Any("error", err))
		return nil
	}
	if wrote && oc.Outcome == OutcomePurged && s.sender != nil {
		s.sender.PublishAssistantPurge(ctx, a.TenantID, a.SiteID, row.Scope)
	}
	return nil
}

func outcomeAuditEvent(a DispatchArgs, row sqlc.AssistantCachePurgeRequest, oc outcome) audit.Event {
	md := map[string]any{
		"request_id":         a.RequestID.String(),
		"requested_by_grant": a.GrantID.String(),
		"scope":              row.Scope,
		"outcome":            oc.Outcome,
	}
	if row.Url != nil {
		md["url"] = *row.Url
	}
	switch oc.Outcome {
	case OutcomePurged:
		md["hosting_caches_cleared"] = nonNil(oc.HostingCleared)
		md["hosting_caches_skipped"] = nonNil(oc.HostingSkipped)
		md["origin_only_confirmed"] = oc.OriginOnlyConfirmed != nil && *oc.OriginOnlyConfirmed
		md["unknown_integrations"] = oc.UnknownIntegrations
		if oc.WpmgrCDN != nil {
			md["wpmgr_cdn"] = *oc.WpmgrCDN
		}
		actorType, actorID := audit.ActorSystem, ""
		if row.DecidedByUserID.Valid {
			actorType, actorID = audit.ActorUser, uuid.UUID(row.DecidedByUserID.Bytes).String()
		}
		return audit.Event{
			TenantID: a.TenantID, ActorType: actorType, ActorID: actorID,
			Action: audit.ActionCachePurged, TargetType: "site", TargetID: a.SiteID.String(),
			Metadata: md,
		}
	case OutcomeNotSent:
		if oc.NotSentReason != nil {
			md["reason"] = *oc.NotSentReason
		}
		return audit.Event{
			TenantID: a.TenantID, ActorType: audit.ActorSystem,
			Action: audit.ActionAssistantRequestNotSent, TargetType: audit.TargetTypeAssistantCachePurgeRequest,
			TargetID: a.RequestID.String(), Metadata: md,
		}
	default:
		md["class"] = oc.Outcome
		if oc.SiteReportedText != nil {
			md["site_reported_text"] = *oc.SiteReportedText
		}
		return audit.Event{
			TenantID: a.TenantID, ActorType: audit.ActorSystem,
			Action: audit.ActionAssistantRequestFailed, TargetType: audit.TargetTypeAssistantCachePurgeRequest,
			TargetID: a.RequestID.String(), Metadata: md,
		}
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ---------------------------------------------------------------------------
// Sweeper and reconciler. Each is a plain agent scan, then a per-row
// compare-and-set in a tenant transaction that touches only the request row
// and the audit log.
// ---------------------------------------------------------------------------

// sweepExpired moves waiting requests past their window to expired. It never
// names a decider.
func (s *Service) sweepExpired(ctx context.Context) error {
	var rows []sqlc.ScanLapsedPendingAssistantCachePurgeRequestsRow
	if err := s.repo.runAgentScan(ctx, func(tx pgx.Tx) error {
		var err error
		rows, err = sqlc.New(tx).ScanLapsedPendingAssistantCachePurgeRequests(ctx, scanRowLimit)
		return err
	}); err != nil {
		return fmt.Errorf("scan lapsed assistant requests: %w", err)
	}
	var errs []error
	for _, r := range rows {
		tenantID, id := r.TenantID, r.ID
		err := s.repo.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
			n, err := sqlc.New(tx).ExpireAssistantCachePurgeRequest(ctx,
				sqlc.ExpireAssistantCachePurgeRequestParams{TenantID: tenantID, ID: id})
			if err != nil || n != 1 {
				return err
			}
			if s.audit == nil {
				return errors.New("audit recorder not wired")
			}
			_, err = s.audit.RecordInTx(ctx, tx, audit.Event{
				TenantID: tenantID, ActorType: audit.ActorSystem,
				Action: audit.ActionAssistantRequestExpired, TargetType: audit.TargetTypeAssistantCachePurgeRequest,
				TargetID: id.String(), Metadata: map[string]any{"request_id": id.String()},
			})
			return err
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// sweepPastDeadline closes every approved request that has not started
// within DispatchDeadline of approval, whatever kept it from starting. It is
// the backstop that guarantees every approved request closes.
func (s *Service) sweepPastDeadline(ctx context.Context) error {
	deadline := int32(DispatchDeadline / time.Second)
	var rows []sqlc.ScanApprovedAssistantCachePurgeRequestsPastDeadlineRow
	if err := s.repo.runAgentScan(ctx, func(tx pgx.Tx) error {
		var err error
		rows, err = sqlc.New(tx).ScanApprovedAssistantCachePurgeRequestsPastDeadline(ctx,
			sqlc.ScanApprovedAssistantCachePurgeRequestsPastDeadlineParams{DeadlineSeconds: deadline, RowLimit: scanRowLimit})
		return err
	}); err != nil {
		return fmt.Errorf("scan approved assistant requests past deadline: %w", err)
	}
	var errs []error
	for _, r := range rows {
		tenantID, id := r.TenantID, r.ID
		err := s.repo.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
			q := sqlc.New(tx)
			n, err := q.CloseAssistantCachePurgeRequestPastDeadline(ctx,
				sqlc.CloseAssistantCachePurgeRequestPastDeadlineParams{DeadlineSeconds: deadline, TenantID: tenantID, ID: id})
			if err != nil || n != 1 {
				return err
			}
			return s.recordNotSent(ctx, tx, tenantID, id, ReasonDispatchDeadlinePassed, nil, "sweeper")
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// reconcile records outcome unknown on every sent request that has waited
// longer than StaleDispatchAfter. The compare-and-set winner is the only
// writer: a late outcome after this finds 0 rows and writes nothing.
func (s *Service) reconcile(ctx context.Context) error {
	stale := int32(StaleDispatchAfter / time.Second)
	var rows []sqlc.ScanStaleDispatchedAssistantCachePurgeRequestsRow
	if err := s.repo.runAgentScan(ctx, func(tx pgx.Tx) error {
		var err error
		rows, err = sqlc.New(tx).ScanStaleDispatchedAssistantCachePurgeRequests(ctx,
			sqlc.ScanStaleDispatchedAssistantCachePurgeRequestsParams{StaleAfterSeconds: stale, RowLimit: scanRowLimit})
		return err
	}); err != nil {
		return fmt.Errorf("scan stale dispatched assistant requests: %w", err)
	}
	var errs []error
	for _, r := range rows {
		tenantID, id := r.TenantID, r.ID
		err := s.repo.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
			n, err := sqlc.New(tx).CloseStaleDispatchedAssistantCachePurgeRequest(ctx,
				sqlc.CloseStaleDispatchedAssistantCachePurgeRequestParams{StaleAfterSeconds: stale, TenantID: tenantID, ID: id})
			if err != nil || n != 1 {
				return err
			}
			if s.audit == nil {
				return errors.New("audit recorder not wired")
			}
			_, err = s.audit.RecordInTx(ctx, tx, audit.Event{
				TenantID: tenantID, ActorType: audit.ActorSystem,
				Action: audit.ActionAssistantRequestFailed, TargetType: audit.TargetTypeAssistantCachePurgeRequest,
				TargetID: id.String(),
				Metadata: map[string]any{"request_id": id.String(), "class": OutcomeUnknown, "closed_by": "reconciler"},
			})
			return err
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
