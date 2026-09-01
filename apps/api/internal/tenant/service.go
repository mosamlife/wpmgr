package tenant

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Service holds tenant business logic.
type Service struct {
	repo      Repo
	validator *domain.Validator
	clock     domain.Clock
}

// NewService builds a tenant Service.
func NewService(repo Repo, v *domain.Validator, clock domain.Clock) *Service {
	return &Service{repo: repo, validator: v, clock: clock}
}

// Create validates and persists a new tenant.
func (s *Service) Create(ctx context.Context, in CreateInput) (Tenant, error) {
	if err := s.validator.Struct(in); err != nil {
		return Tenant{}, err
	}
	return s.repo.Create(ctx, in)
}

// GetForPrincipal returns a tenant by ID, scoped to the principal's access: a
// user principal must have a membership in the tenant; an API-key principal may
// only read its own (single) tenant. Any other case yields domain.NotFound so a
// caller cannot probe for tenants they do not belong to.
func (s *Service) GetForPrincipal(ctx context.Context, p domain.Principal, id uuid.UUID) (Tenant, error) {
	switch p.Type {
	case domain.PrincipalAPIKey:
		if id != p.TenantID || p.TenantID == uuid.Nil {
			return Tenant{}, domain.NotFound("tenant_not_found", "tenant not found")
		}
		// The key is bound to exactly this tenant, so reading it is in-scope.
		return s.repo.GetByID(ctx, id)
	case domain.PrincipalUser:
		return s.repo.GetForUser(ctx, id, p.UserID)
	default:
		return Tenant{}, domain.NotFound("tenant_not_found", "tenant not found")
	}
}

// ListForPrincipal returns the page of tenants the principal may see: a user's
// own memberships, or the single tenant an API key is bound to.
func (s *Service) ListForPrincipal(ctx context.Context, p domain.Principal, in ListInput) ([]Tenant, error) {
	in.Limit, in.Offset = normalizePage(in.Limit, in.Offset)
	switch p.Type {
	case domain.PrincipalAPIKey:
		if p.TenantID == uuid.Nil {
			return []Tenant{}, nil
		}
		t, err := s.repo.GetByID(ctx, p.TenantID)
		if err != nil {
			if de, ok := domain.AsDomain(err); ok && de.Kind == domain.KindNotFound {
				return []Tenant{}, nil
			}
			return nil, err
		}
		return []Tenant{t}, nil
	case domain.PrincipalUser:
		return s.repo.ListForUser(ctx, p.UserID, in)
	default:
		return []Tenant{}, nil
	}
}

// --- m130 assistant kill switch ----------------------------------------------

// Audit actions. Plain strings (audit.Event.Action has no enum), declared
// locally in the package that emits them, as govcontext does. PAUSE AND RESUME
// ARE TWO ACTIONS, not one action with a boolean, so an auditor reading the
// chain sees two distinct incident events and can never mistake the second for
// a repeat of the first.
const (
	actionAssistantPaused  = "tenant.assistant.paused"
	actionAssistantResumed = "tenant.assistant.resumed"
)

// AuditRecorder is the narrow slice of *audit.Recorder these controls need.
// Declared as an interface (the internal/mcp shape, service.go:244) so a test
// can prove the fail-closed rollback by returning an error from it —
// *audit.Recorder itself cannot be made to fail on demand.
type AuditRecorder interface {
	RecordInTx(ctx context.Context, tx pgx.Tx, e audit.Event) (audit.Entry, error)
}

// assertOwnTenant is THE tenancy boundary for every assistant control below,
// and it is load-bearing in a way it would not be on almost any other table.
//
// `tenants` HAS NO ROW LEVEL SECURITY (m130 DECISION 1). On a normal table an
// InTenantTx dispatch means the database itself refuses another organisation's
// row even if the handler passed the wrong id. Here nothing does: the only
// thing standing between `PATCH /tenants/{someoneElse}/assistant/pause` and a
// cross-tenant kill switch is this comparison. RequirePermission(tenant:manage)
// does NOT close it — that middleware asks whether the principal is an owner
// somewhere and whether it is site-constrained, never whether the path id is
// the principal's own tenant.
//
// It returns NotFound rather than Forbidden on purpose: a Forbidden would
// confirm that the other organisation exists.
func assertOwnTenant(p domain.Principal, id uuid.UUID) error {
	if id == uuid.Nil || p.TenantID == uuid.Nil || id != p.TenantID {
		return domain.NotFound("tenant_not_found", "tenant not found")
	}
	return nil
}

// GetAssistantState returns the console view of the organisation's assistant
// enablement and kill-switch state.
func (s *Service) GetAssistantState(ctx context.Context, p domain.Principal, id uuid.UUID) (AssistantState, error) {
	if err := assertOwnTenant(p, id); err != nil {
		return AssistantState{}, err
	}
	return s.repo.GetAssistantState(ctx, id, p.UserID)
}

// PauseAssistant engages the kill switch for one organisation.
//
// EFFECT IS IMMEDIATE AND NEEDS NO SWEEP, NO CACHE INVALIDATION AND NO TOKEN
// REFRESH. The assistant request path recomputes its whole verdict per request
// in ReCheckMCPRequestAuthorizationInTenantTx, whose `authorized` column
// carries `AND tn.assistant_paused_at IS NULL`
// (db/query/mcp_connections.sql). So the next request on an existing,
// otherwise perfectly valid connection token is refused the moment this
// transaction commits. Nothing here may add a cache in front of that read.
//
// IT IS NOT A TOGGLE. There is no "flip" method on this service by design:
// pausing and resuming are separate calls on separate routes with separate
// permissions checks and separate audit actions, so the click that stopped the
// surface cannot restart it (the brief's requirement, and m130 DECISION 2's).
func (s *Service) PauseAssistant(ctx context.Context, p domain.Principal, id uuid.UUID, in PauseInput, rec AuditRecorder) (AssistantState, error) {
	if err := assertOwnTenant(p, id); err != nil {
		return AssistantState{}, err
	}
	if err := s.validator.Struct(in); err != nil {
		return AssistantState{}, err
	}

	// Normalise to the DB constraint's vocabulary BEFORE the write.
	// tenants_assistant_paused_reason_check refuses a reason that is present
	// but blank after btrim, so a whitespace-only reason would 500 as a
	// constraint violation. "No reason given" is NULL, which is honestly
	// absent; a zero-length string reads as an answer.
	var reason *string
	if trimmed := strings.TrimSpace(in.Reason); trimmed != "" {
		reason = &trimmed
	}

	st, err := s.repo.PauseAssistant(ctx, id, p.UserID, reason, func(tx pgx.Tx) error {
		meta := map[string]any{"reason_given": reason != nil}
		if reason != nil {
			meta["reason"] = *reason
		}
		_, aerr := rec.RecordInTx(ctx, tx, audit.Event{
			TenantID:   id,
			ActorType:  audit.ActorUser,
			ActorID:    p.ActorID(),
			Action:     actionAssistantPaused,
			TargetType: "tenant",
			TargetID:   id.String(),
			Metadata:   meta,
		})
		return aerr
	})
	if err != nil {
		return AssistantState{}, err
	}
	// st was read INSIDE the write transaction, so it cannot report a failure
	// for a pause that committed. See Repo.PauseAssistant.
	return st, nil
}

// ResumeAssistant releases the kill switch. It is a SEPARATE, DELIBERATE
// action, never the inverse half of a toggle.
//
// IT DOES NOT TOUCH assistant_enabled_at, and that is the whole point of two
// columns (m130 DECISION 2): an organisation that was deliberately off before
// the incident is still off after it. Releasing the switch never enables a
// surface nobody chose to enable.
func (s *Service) ResumeAssistant(ctx context.Context, p domain.Principal, id uuid.UUID, rec AuditRecorder) (AssistantState, error) {
	if err := assertOwnTenant(p, id); err != nil {
		return AssistantState{}, err
	}

	st, err := s.repo.ResumeAssistant(ctx, id, p.UserID, func(tx pgx.Tx) error {
		_, aerr := rec.RecordInTx(ctx, tx, audit.Event{
			TenantID:   id,
			ActorType:  audit.ActorUser,
			ActorID:    p.ActorID(),
			Action:     actionAssistantResumed,
			TargetType: "tenant",
			TargetID:   id.String(),
		})
		return aerr
	})
	if err != nil {
		return AssistantState{}, err
	}
	// Read inside the write transaction, which also resolved the rows-affected
	// ambiguity the second read used to exist for. See Repo.ResumeAssistant.
	return st, nil
}

func normalizePage(limit, offset int32) (int32, int32) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
