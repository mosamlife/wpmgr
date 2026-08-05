package admin

// gate.go: the authorisation gates for the /api/v1/admin route group.
//
// Two gates live here and they are deliberately kept apart:
//
//	requireSuperadmin
//	    The gate on the /admin group as a whole. Behaviour unchanged: the
//	    principal must be a logged-in user whose users.is_superadmin is true.
//
//	requireSuperadminOrSoleTenantOwner
//	    GH #322. Mounted on exactly ONE route, POST /admin/agent-mirror/check,
//	    and on nothing else. It does not decide anything itself: it asks
//	    admingate.CanRunAgentMirrorCheck and turns a false into this package's
//	    single refusal. See that function for what it admits, why, and for the
//	    property that must not be lost.
//
// WHY THE DECISION MOVED OUT (GH #322 follow-up). The same question is now
// asked by GET /api/v1/fleet/agents, which reports it as the capability field
// agent_mirror.can_check_now so the Sites page can render a Check now button
// exactly when this gate would admit the request. A gate and a capability flag
// that compute the same permission separately can drift, and both drift
// directions are operator-visible bugs (a button that always 403s, or a
// permission nobody is ever offered). The reads and the rule therefore live in
// internal/admingate and BOTH callers call them. Nothing was reimplemented and
// no SQL was copied.
//
// Both gates read through admingate.Store rather than reaching for *db.Pool
// directly, so both stay drivable from a unit test without a database.

import (
	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/admingate"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// adminGateStore is the narrow read surface the admin gates need. An alias, not
// a second interface: the fleet capability flag must be answered from the very
// same reads, so there is one type and one implementation of it.
type adminGateStore = admingate.Store

// newPoolGateStore builds the production store (Postgres reads, see
// admingate.PoolStore for the SQL and for why the membership half must run
// inside a user transaction).
func newPoolGateStore(pool *db.Pool) adminGateStore { return admingate.NewPoolStore(pool) }

// denyAdminGate is the ONE refusal both gates use. The code and the message are
// byte-identical on every path, including the widened one, so a caller cannot
// learn from a refusal whether this install has one organisation or many.
func denyAdminGate(c *gin.Context) {
	httpx.Error(c, domain.Forbidden("superadmin_required", "superadmin access required"))
	c.Abort()
}

// requireSuperadmin is a Gin middleware that returns 403 unless the
// authenticated principal has is_superadmin=true. It gates the whole /admin
// group.
func requireSuperadmin(store adminGateStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		p, ok := domain.PrincipalFromContext(c.Request.Context())
		if !ok || p.Type != domain.PrincipalUser {
			denyAdminGate(c)
			return
		}
		isSA, err := store.IsSuperadmin(c.Request.Context(), p.UserID)
		if err != nil || !isSA {
			denyAdminGate(c)
			return
		}
		c.Next()
	}
}

// requireSuperadminOrSoleTenantOwner gates POST /admin/agent-mirror/check, and
// nothing else on this install. The whole of the admission rule, including the
// principal-type check, the fail-closed behaviour and the reason the widened
// arm exists at all, is admingate.CanRunAgentMirrorCheck's; this middleware
// only maps false to the standard refusal.
func requireSuperadminOrSoleTenantOwner(store adminGateStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !admingate.CanRunAgentMirrorCheck(c.Request.Context(), store) {
			denyAdminGate(c)
			return
		}
		c.Next()
	}
}
