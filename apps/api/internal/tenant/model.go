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

// AssistantState is the operator-console view of m130's two assistant columns.
//
// THE TWO NILS MEAN DIFFERENT THINGS AND ARE NOT SYMMETRIC (m130 DECISION 2):
// EnabledAt nil means the surface was NEVER ENABLED (off); PausedAt nil means
// NOT PAUSED (running). They must never be folded into one predicate.
type AssistantState struct {
	// EnabledAt is the configuration fact. NOTHING READS IT ON THE REQUEST
	// PATH — m130 DECISION 5 deliberately holds it out of the `authorized`
	// verdict until a follow-up migration adds the predicate together with the
	// backfill DECISION 6 specifies. It is surfaced here for the console only,
	// so an operator can see the recorded intent; it gates nothing.
	EnabledAt *time.Time
	// PausedAt is the kill switch, and it IS in the verdict
	// (db/query/mcp_connections.sql: `AND tn.assistant_paused_at IS NULL`
	// inside `authorized`). Non-nil refuses every assistant request for this
	// organisation on its NEXT request.
	PausedAt     *time.Time
	PausedReason *string
}

// Paused reports whether the kill switch is engaged. It exists so a caller
// cannot accidentally test EnabledAt with the same predicate — see
// AssistantState's doc comment.
func (s AssistantState) Paused() bool { return s.PausedAt != nil }

// PauseInput is the validated input for engaging the kill switch.
//
// Reason is OPTIONAL at the wire but constrained when present: the DB check
// constraint tenants_assistant_paused_reason_check refuses a blank or
// whitespace-only reason and caps it at 500 bytes. A caller that means "no
// reason given" sends no reason at all, which stores NULL — honestly absent —
// rather than a zero-length string that reads as an answer.
type PauseInput struct {
	Reason string `validate:"omitempty,max=500"`
}
