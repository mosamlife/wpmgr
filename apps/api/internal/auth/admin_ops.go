package auth

import (
	"context"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ResendVerificationByID re-issues a verification link for a user identified
// by UUID. Called from the admin domain; only acts on status=="pending".
func (s *Service) ResendVerificationByID(ctx context.Context, userID uuid.UUID) error {
	u, err := s.repo.GetUserByID(ctx, userID)
	if err != nil {
		return err
	}
	if u.Status != "pending" {
		return domain.Validation("not_pending", "user is not pending verification")
	}
	s.sendVerificationEmail(ctx, u.ID, u.Email, u.Name, s.priorDesiredPlan(ctx, u.ID))
	return nil
}
