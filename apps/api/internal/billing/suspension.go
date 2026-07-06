package billing

// suspension.go — M16 Phase C1: the superadmin hard-lockout enforcement
// middleware. tenants.suspended_at (m92) is a SEPARATE flag from plan_status
// — set only by a superadmin's explicit suspend/restore action
// (internal/admin) — so this gate is independent of the ordinary
// plan-entitlement machinery in entitlements.go/checkout.go. Data (sites,
// agents, backups) is never touched by a suspension; this is purely a
// request-time gate.

import (
	"log/slog"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// suspendedGateExemptPrefix is the one exception to the suspension gate: the
// tenant-facing billing routes themselves (GET summary / POST checkout /
// POST portal, mounted at /api/v1/billing/...) must stay reachable so a
// suspended tenant can pay or otherwise resolve its way back in ("never
// blocks read of billing state"). Auth routes need no exception here because
// they are mounted on a completely separate, non-tenant-gated route group
// (see server.go) that this middleware is never attached to.
const suspendedGateExemptPrefix = "/api/v1/billing"

// SuspensionGate is a Gin middleware, mounted on the tenant-scoped /api/v1
// group (after authz.RequireAuth + authz.RequireTenant — see server.go),
// that returns 402 {code:"account_suspended"} for every request against a
// tenant whose tenants.suspended_at is set. A no-op when hosted billing is
// disabled (s.enabled == false): self-host never sees this check, mirroring
// every other billing.Service method's hosted-only no-op convention.
// cmd/wpmgr/main.go additionally only wires this into server.Deps when
// WPMGR_HOSTED is on, so an unhosted boot never even constructs the closure
// below — this in-body guard is belt-and-braces.
func (s *Service) SuspensionGate() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.enabled {
			c.Next()
			return
		}
		if strings.HasPrefix(c.Request.URL.Path, suspendedGateExemptPrefix) {
			c.Next()
			return
		}
		p, ok := domain.PrincipalFromContext(c.Request.Context())
		if !ok || p.TenantID == uuid.Nil {
			// authz.RequireAuth/RequireTenant (mounted earlier on this same
			// group) already guard this; fail open to whatever the real gate
			// decides rather than risk a false lockout here.
			c.Next()
			return
		}
		row, err := sqlc.New(s.pool.Pool).GetTenantSuspension(c.Request.Context(), p.TenantID)
		if err != nil {
			// A lookup failure must never itself hard-lock a fleet out — log and
			// let the request proceed; the miss shows up as a support ticket, not
			// a self-inflicted outage.
			s.logger.Warn("billing: suspension check failed, allowing request through", slog.Any("error", err))
			c.Next()
			return
		}
		if row.SuspendedAt.Valid {
			details := map[string]any{}
			if row.SuspendedReason != nil && *row.SuspendedReason != "" {
				details["reason"] = *row.SuspendedReason
			}
			httpx.Error(c, domain.PaymentRequired("account_suspended",
				"this workspace has been suspended; resolve billing or contact support to restore access").
				WithDetails(details))
			c.Abort()
			return
		}
		c.Next()
	}
}
