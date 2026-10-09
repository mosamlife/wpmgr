package admingate

// route_gate.go: the Gin middleware for every route governed by
// InstanceEmailAuthority, so the routes that share the decision also share the
// one piece of code that applies it.
//
// Two route families carry it:
//
//	internal/settings  /api/v1/settings/smtp (GET, PUT and POST /test), the
//	                   instance SMTP relay.
//	internal/admin     /api/v1/admin/vuln-feed/* (GET status, PUT and DELETE
//	                   key, POST sync), the vulnerability-feed key. Owner
//	                   ruling, 2026-10-09: whoever may manage the instance
//	                   email settings may manage the feed key.
//
// Each family keeps its own refusal body, because each family already has one
// and a refusal must read the same on every route of that family, whatever fact
// was missing. The decision, the order its arms are consulted in and its
// fail-closed handling are InstanceEmailAuthority's alone; this file only maps
// a refusal to the family's 403 and hands the admitting decision to the handler
// behind it, which routes its audit record by the arm that admitted.

import (
	"github.com/gin-gonic/gin"
)

// admittingAuthorityKey is the gin context key under which
// RequireInstanceEmailAuthority leaves the admitting decision.
const admittingAuthorityKey = "admingate.instance_email_authority"

// RequireInstanceEmailAuthority refuses the request unless
// InstanceEmailAuthority admits the principal carried on the request context.
// refuse writes the family's refusal; the chain is aborted after it whether or
// not refuse aborts it itself, so a refusal can never fall through to the
// handler. A nil store refuses every request.
//
// On admission the decision is left on the gin context for AdmittingAuthority.
// The Me response's can_manage_instance_email reads the same decision, so the
// dashboard offers these settings exactly when these routes would admit.
func RequireInstanceEmailAuthority(store InstanceEmailStore, refuse func(*gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) {
		a := InstanceEmailAuthority(c.Request.Context(), store)
		if !a.Admitted() {
			refuse(c)
			c.Abort()
			return
		}
		c.Set(admittingAuthorityKey, a)
		c.Next()
	}
}

// AdmittingAuthority returns the decision RequireInstanceEmailAuthority left on
// c. ok is false when there is none, which means the handler was reached
// without the gate in front of it; a handler that records by the admitting arm
// refuses rather than write an unattributed change.
func AdmittingAuthority(c *gin.Context) (Authority, bool) {
	v, ok := c.Get(admittingAuthorityKey)
	if !ok {
		return Authority{}, false
	}
	a, ok := v.(Authority)
	return a, ok && a.Admitted()
}
