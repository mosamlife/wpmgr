package tenant

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Repo is the tenant persistence interface.
type Repo interface {
	Create(ctx context.Context, in CreateInput) (Tenant, error)
	// GetForUser returns a tenant by id only when userID has a membership in it,
	// otherwise domain.NotFound. Reads are scoped via the memberships_self_read
	// policy (InUserTx), never the unscoped tenants table.
	GetForUser(ctx context.Context, id, userID uuid.UUID) (Tenant, error)
	// ListForUser returns only the tenants userID is a member of.
	ListForUser(ctx context.Context, userID uuid.UUID, in ListInput) ([]Tenant, error)
	// GetByID loads a tenant by id without membership scoping. It is used only
	// for an API-key principal, which is already bound to exactly one tenant by
	// the auth middleware (so it can only ever be called for that one tenant).
	GetByID(ctx context.Context, id uuid.UUID) (Tenant, error)

	// GetAssistantState reads m130's three assistant columns for one tenant.
	// This is the CONSOLE read, never the request path: the request path reads
	// the pause through the `authorized` verdict in
	// ReCheckMCPRequestAuthorizationInTenantTx and must not gain a second read
	// of these columns (db/query/tenants.sql).
	GetAssistantState(ctx context.Context, tenantID, userID uuid.UUID) (AssistantState, error)

	// PauseAssistant engages the kill switch and appends the audit entry in the
	// SAME transaction via onCommit. If onCommit returns an error the pause
	// rolls back with it: a kill switch nobody can prove was thrown is not a
	// control, and an audit chain missing the incident action is worse than a
	// failed request.
	//
	// IT RETURNS THE RESULTING STATE READ INSIDE THAT SAME TRANSACTION. An
	// earlier shape re-read the row in a SECOND transaction after this one
	// committed, which meant a transient read failure reported an error for a
	// pause that HAD committed. The operator retry is harmless; the belief is
	// not. Mid-incident, "the kill switch failed" over an already-stopped
	// surface is what makes someone reach for something more drastic, and it is
	// this project's signature defect pointed the other way: reporting failure
	// over its own success.
	PauseAssistant(ctx context.Context, tenantID, userID uuid.UUID, reason *string, onCommit func(tx pgx.Tx) error) (AssistantState, error)

	// ResumeAssistant releases the kill switch. Separate method, separate
	// query, separate audit action — never a toggle (m130 DECISION 2).
	//
	// It returns the state read inside the write transaction, for the reason
	// on PauseAssistant. That read also RESOLVES the rows-affected ambiguity
	// that used to need a second query: 0 rows means the switch was already
	// released OR the tenant does not exist, and those are told apart by
	// whether the state row comes back at all — in the same transaction,
	// against the same snapshot.
	ResumeAssistant(ctx context.Context, tenantID, userID uuid.UUID, onCommit func(tx pgx.Tx) error) (AssistantState, error)
}

// pgRepo is a Postgres-backed Repo over the pgx pool. Tenant rows themselves are
// not RLS-scoped, so reads MUST be membership-scoped in the application layer:
// list/get join the caller's memberships under the memberships_self_read policy
// (app.user_id GUC, set by InUserTx) so a caller can only ever see tenants they
// belong to.
type pgRepo struct {
	pool *db.Pool
	q    *sqlc.Queries
}

// NewRepo builds a Repo over the pgx pool. The pool is required for the
// per-user (InUserTx) scoping used by the read paths.
func NewRepo(pool *db.Pool) Repo {
	return &pgRepo{pool: pool, q: sqlc.New(pool.Pool)}
}

func (r *pgRepo) Create(ctx context.Context, in CreateInput) (Tenant, error) {
	// Tenant slugs are globally unique. Auto-uniquify on collision (the slug is
	// an internal identifier) so duplicate/concurrent creates never hard-fail —
	// defense-in-depth beyond callers that pre-derive a unique slug. The first
	// attempt uses the requested slug verbatim; retries append a random suffix.
	base := in.Slug
	if len(base) > 50 {
		base = base[:50]
	}
	for attempt := 0; attempt < 5; attempt++ {
		slug := in.Slug
		if attempt > 0 {
			suf := make([]byte, 3)
			_, _ = rand.Read(suf)
			slug = base + "-" + hex.EncodeToString(suf)
		}
		row, err := r.q.CreateTenant(ctx, sqlc.CreateTenantParams{Name: in.Name, Slug: slug})
		if err == nil {
			return toModel(row), nil
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			continue // slug taken — retry with a fresh random suffix
		}
		return Tenant{}, domain.Internal("tenant_create_failed", "failed to create tenant").WithCause(err)
	}
	return Tenant{}, domain.Conflict("tenant_slug_exists", "could not allocate a unique tenant slug")
}

func (r *pgRepo) GetForUser(ctx context.Context, id, userID uuid.UUID) (Tenant, error) {
	var out Tenant
	err := r.pool.InUserTx(ctx, userID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetTenantForUser(ctx, sqlc.GetTenantForUserParams{ID: id, UserID: userID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Non-member or unknown tenant: do not disclose existence.
				return domain.NotFound("tenant_not_found", "tenant not found")
			}
			return domain.Internal("tenant_get_failed", "failed to load tenant").WithCause(err)
		}
		out = Tenant{ID: row.ID, Name: row.Name, Slug: row.Slug, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
		return nil
	})
	return out, err
}

func (r *pgRepo) ListForUser(ctx context.Context, userID uuid.UUID, in ListInput) ([]Tenant, error) {
	var out []Tenant
	err := r.pool.InUserTx(ctx, userID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListTenantsForUser(ctx, sqlc.ListTenantsForUserParams{
			UserID: userID,
			Limit:  in.Limit,
			Offset: in.Offset,
		})
		if err != nil {
			return domain.Internal("tenant_list_failed", "failed to list tenants").WithCause(err)
		}
		out = make([]Tenant, 0, len(rows))
		for _, row := range rows {
			out = append(out, Tenant{ID: row.ID, Name: row.Name, Slug: row.Slug, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt})
		}
		return nil
	})
	return out, err
}

func (r *pgRepo) GetByID(ctx context.Context, id uuid.UUID) (Tenant, error) {
	row, err := r.q.GetTenant(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Tenant{}, domain.NotFound("tenant_not_found", "tenant not found")
		}
		return Tenant{}, domain.Internal("tenant_get_failed", "failed to load tenant").WithCause(err)
	}
	return toModel(row), nil
}

// --- m130 assistant kill switch ---------------------------------------------
//
// ALL THREE RUN INSIDE InTenantTxAsUser, NOT InTenantTx, AND NOT ON THE BARE
// POOL. `tenants` has no RLS, so the transaction is not what scopes the row —
// the explicit tenant_id in every query's WHERE is (m130 DECISION 1). What
// InTenantTxAsUser buys is app.user_id, which the audit hash chain requires,
// and one transaction the audit append can share with the write it records.
// The caller-side scope check that stops one organisation writing another's
// row lives in the SERVICE (assertOwnTenant), before any of this runs.

func (r *pgRepo) GetAssistantState(ctx context.Context, tenantID, userID uuid.UUID) (AssistantState, error) {
	var out AssistantState
	err := r.pool.InTenantTxAsUser(ctx, tenantID, userID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetTenantAssistantState(ctx, tenantID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("tenant_not_found", "tenant not found")
			}
			return domain.Internal("assistant_state_get_failed", "failed to load assistant state").WithCause(err)
		}
		out = toAssistantState(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) PauseAssistant(ctx context.Context, tenantID, userID uuid.UUID, reason *string, onCommit func(tx pgx.Tx) error) (AssistantState, error) {
	var out AssistantState
	err := r.pool.InTenantTxAsUser(ctx, tenantID, userID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		n, err := q.EngageTenantAssistantKillSwitch(ctx, sqlc.EngageTenantAssistantKillSwitchParams{
			TenantID: tenantID,
			Reason:   reason,
		})
		if err != nil {
			return domain.Internal("assistant_pause_failed", "failed to pause the assistant").WithCause(err)
		}
		// 0 rows means no such tenant, or it is soft-deleted. The query carries
		// `deleted_at IS NULL`, so this is the only way to tell — and it must
		// NOT be reported as a successful pause.
		if n == 0 {
			return domain.NotFound("tenant_not_found", "tenant not found")
		}
		// FAIL CLOSED. The audit append shares this transaction, so an audit
		// failure rolls the pause back rather than leaving an unattributable
		// kill switch. This is the ADR-064 Decision 7 shape, applied here
		// because the same argument holds: an incident action nobody can prove
		// was taken is not a record.
		if err := onCommit(tx); err != nil {
			return err
		}
		// Read the result back HERE, not after this transaction commits. See
		// the interface doc comment: a second-transaction read can fail over a
		// pause that already succeeded, and tell an operator mid-incident that
		// the switch did not fire when it did.
		row, err := q.GetTenantAssistantState(ctx, tenantID)
		if err != nil {
			return domain.Internal("assistant_pause_failed", "failed to pause the assistant").WithCause(err)
		}
		out = toAssistantState(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) ResumeAssistant(ctx context.Context, tenantID, userID uuid.UUID, onCommit func(tx pgx.Tx) error) (AssistantState, error) {
	var out AssistantState
	err := r.pool.InTenantTxAsUser(ctx, tenantID, userID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		n, err := q.ReleaseTenantAssistantKillSwitch(ctx, tenantID)
		if err != nil {
			return domain.Internal("assistant_resume_failed", "failed to resume the assistant").WithCause(err)
		}
		// No audit entry for a no-op resume: recording a release that released
		// nothing would put a false incident-end marker in the chain. n == 0 is
		// ambiguous here between "already released" and "no such tenant"
		// because the query carries both `deleted_at IS NULL` and
		// `assistant_paused_at IS NOT NULL` — the state read below tells them
		// apart, in this same transaction, against this same snapshot.
		if n > 0 {
			if err := onCommit(tx); err != nil {
				return err
			}
		}
		row, err := q.GetTenantAssistantState(ctx, tenantID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// No row: the tenant does not exist (or is soft-deleted). This
				// is the arm that used to need a second transaction.
				return domain.NotFound("tenant_not_found", "tenant not found")
			}
			return domain.Internal("assistant_resume_failed", "failed to resume the assistant").WithCause(err)
		}
		out = toAssistantState(row)
		return nil
	})
	return out, err
}

// toAssistantState maps the generated row to the domain model. One mapper, so
// the console read and the two write paths cannot drift in how they read the
// two non-symmetric NULLs (m130 DECISION 2).
func toAssistantState(row sqlc.GetTenantAssistantStateRow) AssistantState {
	out := AssistantState{PausedReason: row.AssistantPausedReason}
	if row.AssistantEnabledAt.Valid {
		t := row.AssistantEnabledAt.Time
		out.EnabledAt = &t
	}
	if row.AssistantPausedAt.Valid {
		t := row.AssistantPausedAt.Time
		out.PausedAt = &t
	}
	return out
}

func toModel(t sqlc.Tenant) Tenant {
	return Tenant{
		ID:        t.ID,
		Name:      t.Name,
		Slug:      t.Slug,
		CreatedAt: t.CreatedAt,
		UpdatedAt: t.UpdatedAt,
	}
}
