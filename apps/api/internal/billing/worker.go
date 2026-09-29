package billing

// worker.go — the billing River jobs. Registered only when hosted billing is
// enabled (see cmd/wpmgr/main.go); a no-op boot (WPMGR_HOSTED unset) never
// registers these workers or the periodic reconcile.
//
// Every job here runs on BillingQueue with MaxWorkers 1 per process. Ordering
// between processes comes from the per-tenant billing lock (LockTenantBilling),
// not from the queue.

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// BillingQueue is the River queue every billing job runs on.
const BillingQueue = "billing"

// ReconcileQueue is kept as an alias of BillingQueue for the boot wiring.
const ReconcileQueue = BillingQueue

const (
	applyMaxAttempts   = 12
	refreshMaxAttempts = 8
	auditMaxAttempts   = 12

	// jobTimeout bounds one billing job. One provider call is bounded at
	// about 20 s including the SDK's own retries.
	jobTimeout = 30 * time.Second

	// requestRefreshWindow is the request-path uniqueness period for
	// billing_refresh (see RequestRefreshInsertOpts).
	requestRefreshWindow = time.Minute
)

// ---------------------------------------------------------------------------
// billing_apply
// ---------------------------------------------------------------------------

// BillingApplyArgs names one recorded webhook event to apply.
type BillingApplyArgs struct {
	EventID         uuid.UUID `json:"event_id"`
	Provider        string    `json:"provider"`
	ProviderEventID string    `json:"provider_event_id"`
}

// Kind implements river.JobArgs. Must stay stable.
func (BillingApplyArgs) Kind() string { return "billing_apply" }

// InsertOpts: unique by args with River's default states, and no period. A
// completed job therefore still counts as a duplicate, and a discarded or
// cancelled one does not, so intake can re-enqueue an event whose earlier job
// gave up.
func (BillingApplyArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       BillingQueue,
		MaxAttempts: applyMaxAttempts,
		UniqueOpts:  river.UniqueOpts{ByArgs: true},
	}
}

// BillingApplyWorker applies one recorded webhook event.
type BillingApplyWorker struct {
	river.WorkerDefaults[BillingApplyArgs]
	svc *Service
}

// NewBillingApplyWorker builds a BillingApplyWorker.
func NewBillingApplyWorker(svc *Service) *BillingApplyWorker { return &BillingApplyWorker{svc: svc} }

// Timeout implements river.Worker.
func (w *BillingApplyWorker) Timeout(*river.Job[BillingApplyArgs]) time.Duration { return jobTimeout }

// Work implements river.Worker.
func (w *BillingApplyWorker) Work(ctx context.Context, job *river.Job[BillingApplyArgs]) error {
	return w.svc.applyEvent(ctx, job.Args)
}

// ---------------------------------------------------------------------------
// billing_refresh
// ---------------------------------------------------------------------------

// BillingRefreshArgs asks the apply function to re-derive one tenant's state
// from its provider. SubscriptionID is optional; empty means the stored one.
type BillingRefreshArgs struct {
	TenantID       uuid.UUID `json:"tenant_id"`
	SubscriptionID string    `json:"subscription_id,omitempty"`
	Source         string    `json:"source"`
}

// Kind implements river.JobArgs. Must stay stable.
func (BillingRefreshArgs) Kind() string { return "billing_refresh" }

// InsertOpts are the worker-side options (reconcile, operator actions): no
// uniqueness, so every sweep gets a fresh job.
func (BillingRefreshArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: BillingQueue, MaxAttempts: refreshMaxAttempts}
}

// RequestRefreshInsertOpts are the options for a billing_refresh enqueued from
// a request path (checkout, confirm). Unique by args within one clock minute,
// with River's DEFAULT ByState: the default includes Completed, so a job that
// already finished within the same minute still counts as a duplicate, and
// repeated clicks add at most one job per args per minute.
func RequestRefreshInsertOpts() *river.InsertOpts {
	return &river.InsertOpts{
		Queue:       BillingQueue,
		MaxAttempts: refreshMaxAttempts,
		UniqueOpts: river.UniqueOpts{
			ByArgs:   true,
			ByPeriod: requestRefreshWindow,
		},
	}
}

// BillingRefreshWorker re-derives one tenant's billing state.
type BillingRefreshWorker struct {
	river.WorkerDefaults[BillingRefreshArgs]
	svc *Service
}

// NewBillingRefreshWorker builds a BillingRefreshWorker.
func NewBillingRefreshWorker(svc *Service) *BillingRefreshWorker {
	return &BillingRefreshWorker{svc: svc}
}

// Timeout implements river.Worker.
func (w *BillingRefreshWorker) Timeout(*river.Job[BillingRefreshArgs]) time.Duration {
	return jobTimeout
}

// Work implements river.Worker.
func (w *BillingRefreshWorker) Work(ctx context.Context, job *river.Job[BillingRefreshArgs]) error {
	return w.svc.applyRefresh(ctx, job.ID, job.Args)
}

// EnqueueRequestRefresh enqueues a billing_refresh from a request path with
// RequestRefreshInsertOpts. It opens no transaction and calls no provider.
// inserted is false when an identical request already enqueued one within the
// same clock minute.
func (s *Service) EnqueueRequestRefresh(ctx context.Context, tenantID uuid.UUID, subscriptionID, source string) (inserted bool, err error) {
	if s.river == nil {
		return false, errQueueNotWired
	}
	res, err := s.river.Insert(ctx, BillingRefreshArgs{
		TenantID: tenantID, SubscriptionID: subscriptionID, Source: source,
	}, RequestRefreshInsertOpts())
	if err != nil {
		return false, err
	}
	return !res.UniqueSkippedAsDuplicate, nil
}

// ---------------------------------------------------------------------------
// billing_audit
// ---------------------------------------------------------------------------

// BillingAuditArgs is one audit entry, enqueued in the same transaction as
// the state change it records. AuditKey makes the append idempotent.
type BillingAuditArgs struct {
	TenantID uuid.UUID      `json:"tenant_id"`
	ActorID  string         `json:"actor_id"`
	Action   string         `json:"action"`
	AuditKey string         `json:"audit_key"`
	Metadata map[string]any `json:"metadata"`
}

// Kind implements river.JobArgs. Must stay stable.
func (BillingAuditArgs) Kind() string { return "billing_audit" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (BillingAuditArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: BillingQueue, MaxAttempts: auditMaxAttempts}
}

// BillingAuditWorker appends one billing audit entry.
type BillingAuditWorker struct {
	river.WorkerDefaults[BillingAuditArgs]
	svc *Service
}

// NewBillingAuditWorker builds a BillingAuditWorker.
func NewBillingAuditWorker(svc *Service) *BillingAuditWorker { return &BillingAuditWorker{svc: svc} }

// Timeout implements river.Worker.
func (w *BillingAuditWorker) Timeout(*river.Job[BillingAuditArgs]) time.Duration { return jobTimeout }

// Work implements river.Worker.
func (w *BillingAuditWorker) Work(ctx context.Context, job *river.Job[BillingAuditArgs]) error {
	return w.svc.recordAuditJob(ctx, job.Args)
}

// recordAuditJob appends the entry once. It completes without writing when
// the tenant is gone or the entry with this key already exists.
func (s *Service) recordAuditJob(ctx context.Context, args BillingAuditArgs) error {
	if s.auditRec == nil {
		return nil
	}
	exists, err := s.tenantExists(ctx, args.TenantID)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	return s.pool.InTenantTx(ctx, args.TenantID, func(tx pgx.Tx) error {
		if err := audit.LockChain(ctx, tx, args.TenantID); err != nil {
			return err
		}
		if args.AuditKey != "" {
			done, err := sqlc.New(tx).AuditEntryExistsByKey(ctx, sqlc.AuditEntryExistsByKeyParams{
				TenantID: args.TenantID, AuditKey: args.AuditKey,
			})
			if err != nil {
				return err
			}
			if done {
				return nil
			}
		}
		meta := make(map[string]any, len(args.Metadata)+1)
		for k, v := range args.Metadata {
			meta[k] = v
		}
		if args.AuditKey != "" {
			meta["audit_key"] = args.AuditKey
		}
		_, err := s.auditRec.RecordInTx(ctx, tx, audit.Event{
			TenantID:   args.TenantID,
			ActorType:  audit.ActorSystem,
			ActorID:    args.ActorID,
			Action:     args.Action,
			TargetType: "tenant",
			TargetID:   args.TenantID.String(),
			Metadata:   meta,
		})
		return err
	})
}

// ---------------------------------------------------------------------------
// billing_reconcile (periodic)
// ---------------------------------------------------------------------------

// ReconcileArgs is the (empty) periodic job payload.
type ReconcileArgs struct{}

// Kind implements river.JobArgs.
func (ReconcileArgs) Kind() string { return "billing_reconcile" }

// InsertOpts routes the reconcile job to BillingQueue.
func (ReconcileArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: BillingQueue}
}

// ReconcileWorker drives the daily sweep (see reconcile.go). It only lists
// and enqueues.
type ReconcileWorker struct {
	river.WorkerDefaults[ReconcileArgs]
	svc    *Service
	logger *slog.Logger
}

// NewReconcileWorker builds a ReconcileWorker.
func NewReconcileWorker(svc *Service, logger *slog.Logger) *ReconcileWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &ReconcileWorker{svc: svc, logger: logger}
}

// Timeout implements river.Worker.
func (w *ReconcileWorker) Timeout(*river.Job[ReconcileArgs]) time.Duration {
	return 5 * time.Minute
}

// Work runs one reconcile sweep.
func (w *ReconcileWorker) Work(ctx context.Context, _ *river.Job[ReconcileArgs]) error {
	result, err := w.svc.Reconcile(ctx)
	if err != nil {
		return err
	}
	w.logger.Info("billing reconcile: sweep enqueued",
		slog.Int("checked", result.Checked), slog.Int("enqueued", result.Enqueued))
	return nil
}
