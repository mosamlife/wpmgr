// Package httpx holds the central HTTP error->response mapping shared by all
// Gin handlers, so error semantics (status code + Error body) are consistent.
package httpx

import (
	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Error writes the given error as the OpenAPI Error schema with the status code
// derived from the domain Kind (domain.HTTPStatus). Non-domain errors become a
// generic 500 without leaking internal detail to the client.
func Error(c *gin.Context, err error) {
	status := domain.HTTPStatus(err)
	body := gen.Error{Code: "internal_error", Message: "internal server error"}
	if de, ok := domain.AsDomain(err); ok {
		body.Code = de.Code
		// Expose the message for every domain Kind except the catch-all internal
		// error, whose message may leak internals. 4xx and the explicit 501
		// (feature disabled) carry caller-actionable, safe messages.
		if de.Kind != domain.KindInternal {
			body.Message = de.Message
		}
	}
	c.AbortWithStatusJSON(status, body)
}
