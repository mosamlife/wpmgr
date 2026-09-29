package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

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
