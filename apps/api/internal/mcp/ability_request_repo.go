package mcp

// The ability request rail's Repo methods (m156). Every one runs either
// under runConnectionTx (the connection-scoped principal, site scope
// asserted from inside the transaction) or on the revoke transaction the
// caller holds; site ids come only from principal.AllowedSiteIDs.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// RunAbilityRequestTx is the ability creation transaction.
func (r *Repo) RunAbilityRequestTx(ctx context.Context, principal domain.Principal, fn func(tx pgx.Tx, q abilityRequestQueries) error) error {
	return r.runConnectionTx(ctx, principal, "create ability request", func(tx pgx.Tx) error {
		return fn(tx, sqlc.New(tx))
	})
}

// ReadAbilityRequestStatus reads one of this connection's ability requests on
// a site still in its scope.
func (r *Repo) ReadAbilityRequestStatus(ctx context.Context, principal domain.Principal, grantID, requestID uuid.UUID) (AbilityStatusRow, bool, error) {
	var out AbilityStatusRow
	var found bool
	err := r.runConnectionTx(ctx, principal, "read ability request status", func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetAbilityRequestStatusForGrant(ctx, sqlc.GetAbilityRequestStatusForGrantParams{
			TenantID: principal.TenantID, ID: requestID, ProposedByGrantID: grantID, SiteIds: principal.AllowedSiteIDs,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out, found = row, true
		return nil
	})
	return out, found, err
}

// ListOpenAbilityRequestStatus lists this connection's open ability requests.
func (r *Repo) ListOpenAbilityRequestStatus(ctx context.Context, principal domain.Principal, grantID uuid.UUID, limit int32) ([]AbilityStatusRow, error) {
	var out []AbilityStatusRow
	err := r.runConnectionTx(ctx, principal, "list ability request status", func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListOpenAbilityRequestStatusForGrant(ctx, sqlc.ListOpenAbilityRequestStatusForGrantParams{
			TenantID: principal.TenantID, ProposedByGrantID: grantID, SiteIds: principal.AllowedSiteIDs, RowLimit: limit,
		})
		if err != nil {
			return err
		}
		out = make([]AbilityStatusRow, 0, len(rows))
		for _, row := range rows {
			out = append(out, AbilityStatusRow(row))
		}
		return nil
	})
	return out, err
}

// CloseAbilityRequestsForGrantTx withdraws the connection's waiting ability
// requests and closes its approved, unreserved ones as not sent
// (grant_inactive). The caller already holds the connection's grant lock
// (CloseAssistantRequestsForGrantTx took it in this transaction); it is
// taken again here, which is a no-op for the holder, so this method is safe
// on its own too.
func (r *Repo) CloseAbilityRequestsForGrantTx(ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID) (withdrawn, notSent []uuid.UUID, err error) {
	q := sqlc.New(tx)
	if err := q.TakeAssistantRequestXactLock(ctx, sqlc.TakeAssistantRequestXactLockParams{
		LockKey: grantRequestLockKey, LockID: grantID.String(),
	}); err != nil {
		return nil, nil, fmt.Errorf("take the connection's request lock: %w", err)
	}
	withdrawn, err = q.WithdrawPendingAbilityRequestsForGrant(ctx, sqlc.WithdrawPendingAbilityRequestsForGrantParams{
		TenantID: tenantID, ProposedByGrantID: grantID,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("withdraw waiting ability requests: %w", err)
	}
	notSent, err = q.CloseApprovedAbilityRequestsForGrant(ctx, sqlc.CloseApprovedAbilityRequestsForGrantParams{
		TenantID: tenantID, ProposedByGrantID: grantID,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("close approved ability requests: %w", err)
	}
	return withdrawn, notSent, nil
}
