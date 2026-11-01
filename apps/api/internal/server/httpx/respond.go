// Package httpx holds the central HTTP error->response mapping shared by all
// Gin handlers, so error semantics (status code + Error body) are consistent.
package httpx

import (
	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// errorEnvelope is the canonical JSON error body. It mirrors the OpenAPI Error
// schema (#/components/schemas/Error) using standard encoding/json so that
// Gin's AbortWithStatusJSON works without the ogen codec. The details field is
// omitted when nil/empty so existing callers that expect only {code, message}
// are unaffected.
type errorEnvelope struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// Error writes the given error as the OpenAPI Error schema with the status code
// derived from the domain Kind (domain.HTTPStatus). Non-domain errors become a
// generic 500 without leaking internal detail to the client.
//
// When the domain.Error carries a Details map it is forwarded as a top-level
// "details" object in the JSON envelope (e.g. the site_url_exists 409 that
// carries site_id + connection_state so the web can branch without parsing the
// message string).
func Error(c *gin.Context, err error) {
	status := domain.HTTPStatus(err)
	body := errorEnvelope{Code: "internal_error", Message: "internal server error"}
	if de, ok := domain.AsDomain(err); ok {
		body.Code = de.Code
		// Expose the message for every domain Kind except the catch-all internal
		// error, whose message may leak internals. 4xx, the explicit 501
		// (feature disabled) and the 502 (another system failed; fixed copy,
		// see domain.BadGateway) carry caller-actionable, safe messages.
		if de.Kind != domain.KindInternal {
			body.Message = de.Message
		}
		if len(de.Details) > 0 {
			body.Details = de.Details
		}
	}
	c.AbortWithStatusJSON(status, body)
}
