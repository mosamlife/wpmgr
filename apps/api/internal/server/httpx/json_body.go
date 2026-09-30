package httpx

import (
	"mime"
	"net/http"

	"github.com/gin-gonic/gin"
)

// CodeUnsupportedMediaType is the error code RequireJSONBody answers with.
const CodeUnsupportedMediaType = "unsupported_media_type"

// RequireJSONBody refuses, with 415, any request whose Content-Type is not
// application/json. Mount it on a cookie-authenticated route that changes
// state.
//
// WHY A HEADER CHECK IS A CSRF GUARD HERE. The session cookie is SameSite=Lax,
// and Gin's ShouldBindJSON decodes the body whatever the Content-Type says. A
// cross-site page can send a form-encoded or text/plain POST without a CORS
// preflight, and a JSON body inside a text/plain request would otherwise be
// bound and acted on. A cross-site request that sets application/json is not
// a CORS-simple request, so a browser sends it only after a preflight the
// server's CORS policy admits. Requiring the media type therefore moves the
// question from "can a page send this" to "does the CORS policy admit that
// origin".
//
// The media type is parsed with mime.ParseMediaType, so parameters such as
// "; charset=utf-8" are accepted and case is ignored, while a missing header,
// a malformed one, text/plain, multipart/form-data and
// application/x-www-form-urlencoded are all refused. Nothing downstream runs
// on a refusal, so no row changes.
func RequireJSONBody() gin.HandlerFunc {
	return func(c *gin.Context) {
		mt, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
		if err != nil || mt != "application/json" {
			c.AbortWithStatusJSON(http.StatusUnsupportedMediaType, errorEnvelope{
				Code:    CodeUnsupportedMediaType,
				Message: "this endpoint accepts application/json only",
			})
			return
		}
		c.Next()
	}
}
