package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// The creation rail's and the status tool's database surface.
// ---------------------------------------------------------------------------

// requestQueries is every statement the creation transaction runs, in the
// shape sqlc generated them. *sqlc.Queries satisfies it; a unit test drives
// the rail against an in-memory one.
type requestQueries interface {
	TakeAssistantRequestXactLock(ctx context.Context, arg sqlc.TakeAssistantRequestXactLockParams) error
	ExpireLapsedPendingAssistantCachePurgeRequest(ctx context.Context, arg sqlc.ExpireLapsedPendingAssistantCachePurgeRequestParams) ([]uuid.UUID, error)
	CountLivePendingAssistantCachePurgeRequestsForGrant(ctx context.Context, arg sqlc.CountLivePendingAssistantCachePurgeRequestsForGrantParams) (int64, error)
	CountAssistantCachePurgeRequestsForGrantSince(ctx context.Context, arg sqlc.CountAssistantCachePurgeRequestsForGrantSinceParams) (int64, error)
	CountAssistantCachePurgesOnSiteSince(ctx context.Context, arg sqlc.CountAssistantCachePurgesOnSiteSinceParams) (int64, error)
	InsertAssistantCachePurgeRequest(ctx context.Context, arg sqlc.InsertAssistantCachePurgeRequestParams) (sqlc.AssistantCachePurgeRequest, error)
	GetPendingAssistantCachePurgeRequestForGrantSite(ctx context.Context, arg sqlc.GetPendingAssistantCachePurgeRequestForGrantSiteParams) (sqlc.AssistantCachePurgeRequest, error)
}

var _ requestQueries = (*sqlc.Queries)(nil)

// siteAddressRow is one in-scope site's id and stored address.
type siteAddressRow struct {
	ID  uuid.UUID
	URL string
}

// requestStatusRow is the status tool's narrow projection, field for field
// the sqlc row. It holds no digest, nonce, grant label, setup client, decider
// or site-reported text, so nothing built from it can carry them to the model.
type requestStatusRow struct {
	ID                   uuid.UUID
	SiteID               uuid.UUID
	SiteLabel            string
	Scope                string
	Url                  *string
	State                string
	CreatedAt            time.Time
	ExpiresAt            time.Time
	DecidedAt            pgtype.Timestamptz
	WithdrawnAt          pgtype.Timestamptz
	ClaimedAt            pgtype.Timestamptz
	LastAttemptAt        pgtype.Timestamptz
	LastAttemptCode      *string
	Outcome              *string
	NotSentReason        *string
	OutcomeAt            pgtype.Timestamptz
	HostingCachesCleared []string
	HostingCachesSkipped []string
	OriginOnlyConfirmed  *bool
	WpmgrCdn             *string
}

// requestStore is what the creation rail and the status tool need beyond
// Store. *Repo implements it. A Service whose store does not has NO request
// rail, and the write-tools switch cannot be turned on for it
// (SetWriteToolsEnabled).
//
// Every method takes the connection-scoped principal and runs under
// RunTenantTx with it, asserting from inside the transaction that the
// site-scope settings are on and name exactly that principal's sites.
type requestStore interface {
	ListSiteAddressesInScope(ctx context.Context, principal domain.Principal) ([]siteAddressRow, error)
	RunRequestTx(ctx context.Context, principal domain.Principal, fn func(tx pgx.Tx, q requestQueries) error) error
	ReadRequestStatus(ctx context.Context, principal domain.Principal, grantID, requestID uuid.UUID) (requestStatusRow, bool, error)
	ListOpenRequestStatus(ctx context.Context, principal domain.Principal, grantID uuid.UUID, limit int32) ([]requestStatusRow, error)
}

var _ requestStore = (*Repo)(nil)

// runConnectionTx opens RunTenantTx with a site-constrained principal and
// asserts, inside the transaction, that it admits exactly that principal's
// sites. An org-scoped or empty principal is refused by name: under it the
// site-scope policies are inert and the counts would be the tenant's.
func (r *Repo) runConnectionTx(ctx context.Context, principal domain.Principal, what string, fn func(tx pgx.Tx) error) error {
	if !principal.IsSiteConstrained() || len(principal.AllowedSiteIDs) == 0 {
		return fmt.Errorf("%s: principal is not site-constrained; this path runs only under the "+
			"connection's resolved scope (tenant %s)", what, principal.TenantID)
	}
	return r.pool.RunTenantTx(ctx, principal, func(tx pgx.Tx) error {
		if err := assertSiteScopedTx(ctx, tx, principal.AllowedSiteIDs); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		return fn(tx)
	})
}

// ListSiteAddressesInScope reads every in-scope site's address, with no row
// limit, for the creation rail's tie check.
func (r *Repo) ListSiteAddressesInScope(ctx context.Context, principal domain.Principal) ([]siteAddressRow, error) {
	var out []siteAddressRow
	err := r.runConnectionTx(ctx, principal, "list site addresses in scope", func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListSiteAddressesInScope(ctx, sqlc.ListSiteAddressesInScopeParams{
			TenantID: principal.TenantID,
			SiteIds:  principal.AllowedSiteIDs,
		})
		if err != nil {
			return fmt.Errorf("list site addresses in scope: %w", err)
		}
		out = make([]siteAddressRow, 0, len(rows))
		for _, row := range rows {
			out = append(out, siteAddressRow{ID: row.ID, URL: row.Url})
		}
		return nil
	})
	return out, err
}

// RunRequestTx is the creation transaction: one connection-scoped
// transaction holding every statement the rail runs.
func (r *Repo) RunRequestTx(ctx context.Context, principal domain.Principal, fn func(tx pgx.Tx, q requestQueries) error) error {
	return r.runConnectionTx(ctx, principal, "create assistant request", func(tx pgx.Tx) error {
		return fn(tx, sqlc.New(tx))
	})
}

// ReadRequestStatus reads one of this connection's requests on a site still
// in its scope. found is false for anything else.
func (r *Repo) ReadRequestStatus(ctx context.Context, principal domain.Principal, grantID, requestID uuid.UUID) (requestStatusRow, bool, error) {
	var out requestStatusRow
	var found bool
	err := r.runConnectionTx(ctx, principal, "read assistant request status", func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetAssistantCachePurgeRequestStatusForGrant(ctx, sqlc.GetAssistantCachePurgeRequestStatusForGrantParams{
			TenantID:          principal.TenantID,
			ID:                requestID,
			ProposedByGrantID: grantID,
			SiteIds:           principal.AllowedSiteIDs,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read assistant request status: %w", err)
		}
		found = true
		out = requestStatusRow(row)
		return nil
	})
	return out, found, err
}

// ListOpenRequestStatus lists this connection's requests with no final
// result yet, newest first, on sites still in its scope.
func (r *Repo) ListOpenRequestStatus(ctx context.Context, principal domain.Principal, grantID uuid.UUID, limit int32) ([]requestStatusRow, error) {
	var out []requestStatusRow
	err := r.runConnectionTx(ctx, principal, "list open assistant requests", func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListOpenAssistantCachePurgeRequestStatusForGrant(ctx, sqlc.ListOpenAssistantCachePurgeRequestStatusForGrantParams{
			TenantID:          principal.TenantID,
			ProposedByGrantID: grantID,
			SiteIds:           principal.AllowedSiteIDs,
			RowLimit:          limit,
		})
		if err != nil {
			return fmt.Errorf("list open assistant requests: %w", err)
		}
		out = make([]requestStatusRow, 0, len(rows))
		for _, row := range rows {
			out = append(out, requestStatusRow(row))
		}
		return nil
	})
	return out, err
}

// CloseAssistantRequestsForGrantTx runs on the caller's transaction, which is
// the revoke transaction opened with the org-scoped revoker: the table's
// _site_scope policy is inert there and every row of the grant is visible.
// Waiting rows first, then approved rows, so the lock order (request rows,
// then the audit chain on the caller's first RecordInTx) matches every other
// writer of this table.
func (r *Repo) CloseAssistantRequestsForGrantTx(ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID) (withdrawn, notSent []uuid.UUID, err error) {
	if tx == nil {
		return nil, nil, errors.New("close assistant requests for grant: no transaction")
	}
	q := sqlc.New(tx)
	withdrawn, err = q.WithdrawPendingAssistantCachePurgeRequestsForGrant(ctx,
		sqlc.WithdrawPendingAssistantCachePurgeRequestsForGrantParams{
			TenantID: tenantID, ProposedByGrantID: grantID,
		})
	if err != nil {
		return nil, nil, fmt.Errorf("withdraw waiting assistant requests: %w", err)
	}
	notSent, err = q.CloseApprovedAssistantCachePurgeRequestsForGrant(ctx,
		sqlc.CloseApprovedAssistantCachePurgeRequestsForGrantParams{
			TenantID: tenantID, ProposedByGrantID: grantID,
		})
	if err != nil {
		return nil, nil, fmt.Errorf("close approved assistant requests: %w", err)
	}
	return withdrawn, notSent, nil
}
