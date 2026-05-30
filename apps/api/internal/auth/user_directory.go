package auth

import (
	"context"

	"github.com/google/uuid"
)

// ResolveActor implements the backup.UserDirectory interface on *Service.
// It parses id as a UUID, looks up the user, and returns their email + name.
// Returns ok=false when id is not a valid UUID or the user is not found.
func (s *Service) ResolveActor(ctx context.Context, id string) (email string, name string, ok bool) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return "", "", false
	}
	u, err := s.repo.GetUserByID(ctx, uid)
	if err != nil {
		return "", "", false
	}
	return u.Email, u.Name, true
}
