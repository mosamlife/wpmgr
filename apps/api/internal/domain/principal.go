package domain

import (
	"context"

	"github.com/google/uuid"
)

// PrincipalType distinguishes a human (session) caller from a machine (API key).
type PrincipalType string

const (
	// PrincipalUser is a human authenticated via a session cookie.
	PrincipalUser PrincipalType = "user"
	// PrincipalAPIKey is a machine authenticated via a bearer API key.
	PrincipalAPIKey PrincipalType = "api_key"
)

// Principal is the authenticated caller for a request: who they are, which
// tenant is active, and their role in that tenant. It is derived by the auth
// middleware from EITHER a session OR a bearer API key, never from a raw header.
type Principal struct {
	Type     PrincipalType
	UserID   uuid.UUID // set when Type == PrincipalUser
	APIKeyID uuid.UUID // set when Type == PrincipalAPIKey
	TenantID uuid.UUID // the active tenant the request operates in
	Role     string    // the principal's role within TenantID
}

// ActorID returns the stable identifier of the principal for audit logging.
func (p Principal) ActorID() string {
	if p.Type == PrincipalAPIKey {
		return p.APIKeyID.String()
	}
	return p.UserID.String()
}

type principalCtxKey struct{}

// WithPrincipal returns a copy of ctx carrying the authenticated principal. The
// principal's tenant is also mirrored into the tenant-id context so existing
// tenant-scoped code (and logging) keeps working unchanged.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	ctx = context.WithValue(ctx, principalCtxKey{}, p)
	if p.TenantID != uuid.Nil {
		ctx = WithTenantID(ctx, p.TenantID)
	}
	return ctx
}

// PrincipalFromContext returns the authenticated principal and whether one was
// present.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(Principal)
	return p, ok
}
