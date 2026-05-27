// Package middleware provides Gin middleware: request-id injection, structured
// slog request logging, panic recovery, and tenant-context extraction.
package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// HeaderRequestID is the inbound/outbound correlation header.
const HeaderRequestID = "X-Request-ID"

const ctxRequestID = "request_id"

// RequestID assigns a request ID (honoring an inbound X-Request-ID), stores it
// on the Gin context and request context, and echoes it on the response.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(HeaderRequestID)
		if id == "" {
			id = uuid.NewString()
		}
		c.Set(ctxRequestID, id)
		c.Request = c.Request.WithContext(domain.WithRequestID(c.Request.Context(), id))
		c.Header(HeaderRequestID, id)
		c.Next()
	}
}

// Logger logs one structured line per request including request_id, and
// tenant_id when present.
func Logger(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		attrs := []any{
			slog.String("method", c.Request.Method),
			slog.String("path", c.Request.URL.Path),
			slog.Int("status", c.Writer.Status()),
			slog.Duration("latency", time.Since(start)),
			slog.String("client_ip", c.ClientIP()),
		}
		if rid, ok := c.Get(ctxRequestID); ok {
			attrs = append(attrs, slog.Any("request_id", rid))
		}
		if tid, ok := domain.TenantIDFromContext(c.Request.Context()); ok {
			attrs = append(attrs, slog.String("tenant_id", tid.String()))
		}

		level := slog.LevelInfo
		switch {
		case c.Writer.Status() >= 500:
			level = slog.LevelError
		case c.Writer.Status() >= 400:
			level = slog.LevelWarn
		}
		log.LogAttrs(c.Request.Context(), level, "http request", toLogAttrs(attrs)...)
	}
}

func toLogAttrs(kv []any) []slog.Attr {
	out := make([]slog.Attr, 0, len(kv))
	for _, v := range kv {
		if a, ok := v.(slog.Attr); ok {
			out = append(out, a)
		}
	}
	return out
}

// Recovery converts any panic into a 500 JSON response, logging the stack. No
// panic escapes to the client.
func Recovery(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic recovered",
					slog.Any("panic", r),
					slog.String("stack", string(debug.Stack())),
				)
				if !c.Writer.Written() {
					c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
						"code":    "internal_error",
						"message": "internal server error",
					})
				} else {
					c.Abort()
				}
			}
		}()
		c.Next()
	}
}
