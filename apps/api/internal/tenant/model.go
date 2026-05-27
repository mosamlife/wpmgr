// Package tenant implements the tenant domain: the registry of customer
// tenants the control plane serves. Tenants are not themselves tenant-scoped
// (they are the scoping key), so this table has no RLS.
package tenant

import (
	"time"

	"github.com/google/uuid"
)

// Tenant is a customer tenant.
type Tenant struct {
	ID        uuid.UUID
	Name      string
	Slug      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CreateInput is the validated input for creating a tenant.
type CreateInput struct {
	Name string `validate:"required,max=200"`
	Slug string `validate:"required,max=64,slug"`
}

// ListInput is pagination input for listing tenants.
type ListInput struct {
	Limit  int32
	Offset int32
}
