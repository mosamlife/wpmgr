// Package site implements the site domain: WordPress sites managed under a
// tenant. Every query is tenant-scoped both explicitly (tenant_id in the WHERE
// clause) and by Postgres RLS (the app.tenant_id policy), giving
// defense-in-depth against cross-tenant access.
package site

import (
	"time"

	"github.com/google/uuid"
)

// Site is a managed WordPress site.
type Site struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	URL        string
	Name       string
	Status     string
	WPVersion  string
	PHPVersion string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// CreateInput is the validated input for creating a site under a tenant.
type CreateInput struct {
	TenantID   uuid.UUID `validate:"required"`
	URL        string    `validate:"required,url,max=2048"`
	Name       string    `validate:"required,max=200"`
	Status     string    `validate:"omitempty,oneof=pending active error disabled"`
	WPVersion  string    `validate:"max=32"`
	PHPVersion string    `validate:"max=32"`
}

// ListInput is tenant-scoped pagination input.
type ListInput struct {
	TenantID uuid.UUID
	Limit    int32
	Offset   int32
}
