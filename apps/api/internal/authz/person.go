package authz

import (
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// CodeSessionRequired is the error code for an action that only a person
// signed in to WPMgr may take.
const CodeSessionRequired = "session_required"

// AuthorizeLoosening admits only a person signed in to WPMgr: a session
// principal carrying both a user and an active tenant. Every other caller,
// API keys included, is refused with 403 session_required.
//
// Loosening an AI control needs a signed-in person. Tightening one does not
// call this, so it stays open to every caller the route already admits.
func AuthorizeLoosening(p domain.Principal) error {
	if p.Type != domain.PrincipalUser || p.UserID == uuid.Nil || p.TenantID == uuid.Nil {
		return domain.Forbidden(CodeSessionRequired, "Loosening an AI control needs a signed-in person.")
	}
	return nil
}
