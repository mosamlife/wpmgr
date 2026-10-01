package mcp

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// abilityRequestRevoker closes a revoked connection's ability requests in
// the revoke transaction. *Repo implements it.
type abilityRequestRevoker interface {
	CloseAbilityRequestsForGrantTx(ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID) (withdrawn, notSent []uuid.UUID, err error)
}

var _ abilityRequestRevoker = (*Repo)(nil)

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

// closeAbilityRequestsForGrant runs the cascade when the store supports it.
// The production store (*Repo) always does; a unit-test store without
// ability requests has none to close.
func closeAbilityRequestsForGrant(ctx context.Context, store any, tx pgx.Tx, tenantID, grantID uuid.UUID) ([]uuid.UUID, []uuid.UUID, error) {
	rv, ok := store.(abilityRequestRevoker)
	if !ok {
		return nil, nil, nil
	}
	return rv.CloseAbilityRequestsForGrantTx(ctx, tx, tenantID, grantID)
}

// auditInTx is the one audit method the cascade needs.
type auditInTx interface {
	RecordInTx(ctx context.Context, tx pgx.Tx, e audit.Event) (audit.Entry, error)
}

// recordAbilityRevokeCascade writes one row per closed ability request,
// with the revoker as the actor.
func recordAbilityRevokeCascade(ctx context.Context, rec auditInTx, tx pgx.Tx, tenantID, grantID uuid.UUID, actorType, actorID string, withdrawn, notSent []uuid.UUID) error {
	for _, id := range withdrawn {
		if _, err := rec.RecordInTx(ctx, tx, audit.Event{
			TenantID: tenantID, ActorType: actorType, ActorID: actorID,
			Action: audit.ActionAssistantRequestWithdrawn, TargetType: audit.TargetTypeAssistantAbilityRequest,
			TargetID: id.String(),
			Metadata: map[string]any{"reason": "connection_revoked", "proposed_by_grant_id": grantID.String()},
		}); err != nil {
			return err
		}
	}
	for _, id := range notSent {
		if _, err := rec.RecordInTx(ctx, tx, audit.Event{
			TenantID: tenantID, ActorType: actorType, ActorID: actorID,
			Action: audit.ActionAssistantRequestNotSent, TargetType: audit.TargetTypeAssistantAbilityRequest,
			TargetID: id.String(),
			Metadata: map[string]any{
				"reason": "grant_inactive", "closed_by": "connection_revoked", "proposed_by_grant_id": grantID.String(),
			},
		}); err != nil {
			return err
		}
	}
	return nil
}
