package update

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// auditActor decides who an audit entry says acted. It is tested directly
// rather than through a handler because audit.Recorder is a concrete,
// pool-backed type: reaching the same decision through the HTTP layer would
// need a database and would prove less, since the whole defect lived in these
// six lines.
//
// The bug it replaced: ActorType was hardcoded to ActorUser and the id was read
// from p.UserID. For an API-key principal UserID is uuid.Nil and APIKeyID
// carries the identity, so every key-initiated action was recorded as a HUMAN
// acting, identified by a zero UUID — an audit entry that asserts a person did
// it and names nobody. Worse than a missing entry: a missing record prompts a
// question, a wrong one answers it incorrectly.

// TestAuditActorIdentifiesAnAPIKeyPrincipal is the direction that was broken.
func TestAuditActorIdentifiesAnAPIKeyPrincipal(t *testing.T) {
	keyID := uuid.New()
	p := domain.Principal{
		Type:     domain.PrincipalAPIKey,
		APIKeyID: keyID,
		// Explicitly zero, which is the real shape of an API-key principal and
		// exactly what the old code read.
		UserID:   uuid.Nil,
		TenantID: uuid.New(),
	}
	ctx := domain.WithPrincipal(context.Background(), p)

	actorType, actorID := auditActor(ctx)

	if actorType != audit.ActorAPIKey {
		t.Errorf("actor type = %q, want %q; the audit log would claim a human performed a key-initiated action",
			actorType, audit.ActorAPIKey)
	}
	if actorID != keyID.String() {
		t.Errorf("actor id = %q, want the API key id %q", actorID, keyID)
	}
	if actorID == uuid.Nil.String() {
		t.Error("actor id is the zero UUID: the entry identifies nobody, which is the whole defect")
	}
}

// TestAuditActorIdentifiesAUserPrincipal is the control. A fix that swung the
// other way — recording every actor as an API key — would be just as wrong and
// would break audit.go's name join in the opposite direction.
func TestAuditActorIdentifiesAUserPrincipal(t *testing.T) {
	userID := uuid.New()
	p := domain.Principal{
		Type:     domain.PrincipalUser,
		UserID:   userID,
		TenantID: uuid.New(),
	}
	ctx := domain.WithPrincipal(context.Background(), p)

	actorType, actorID := auditActor(ctx)

	if actorType != audit.ActorUser {
		t.Errorf("actor type = %q, want %q", actorType, audit.ActorUser)
	}
	if actorID != userID.String() {
		t.Errorf("actor id = %q, want the user id %q", actorID, userID)
	}
}

// TestAuditActorFallsBackToSystem covers the no-principal case. Naming a user
// or a key there would be the same lie in a quieter form, and ActorSystem is
// what audit.go's join expects when there is no name to resolve.
func TestAuditActorFallsBackToSystem(t *testing.T) {
	actorType, actorID := auditActor(context.Background())

	if actorType != audit.ActorSystem {
		t.Errorf("actor type = %q, want %q for a request with no principal", actorType, audit.ActorSystem)
	}
	if actorID != "" {
		t.Errorf("actor id = %q, want empty; there is nobody to name", actorID)
	}
}
