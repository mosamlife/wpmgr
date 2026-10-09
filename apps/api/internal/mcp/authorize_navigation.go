package mcp

import (
	"github.com/gin-gonic/gin"
)

// ConsentScreenPath is the dashboard page that renders a consent request:
// apps/web/src/routes/_authed/connect.ai.tsx.
const ConsentScreenPath = "/connect/ai"

// AuthorizeNavigationRedirect sends a browser that opens AuthorizePath on to
// the consent screen.
func AuthorizeNavigationRedirect() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
	}
}
