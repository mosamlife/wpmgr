package domain

import (
	"context"

	"github.com/google/uuid"
)

type ctxKey int

const (
	tenantIDKey ctxKey = iota
	requestIDKey
)

// WithTenantID returns a copy of ctx carrying the tenant ID.
func WithTenantID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, tenantIDKey, id)
}

// TenantIDFromContext returns the tenant ID and whether it was present.
func TenantIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(tenantIDKey).(uuid.UUID)
	return id, ok
}

// WithRequestID returns a copy of ctx carrying the request ID.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFromContext returns the request ID and whether it was present.
func RequestIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestIDKey).(string)
	return id, ok
}
