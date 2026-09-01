package mcp

import (
	"context"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/govcontext"
)

// contextInstructionByteBudget is the budget for GOVERNED OPERATOR CONTEXT in
// a tool result header. It is a THIRD budget alongside instructionByteBudget
// (the tool's own compiled-in safety text) and recordByteBudget (the records),
// and it is separate from both for the reason given at instructionByteBudget:
// a budget shared between two kinds of text is a budget in which the larger
// kind evicts the smaller. Sharing with the tool instructions would let an
// organisation's context push out the never_collected warning; sharing with
// the records would let a large fleet push out the operator's instructions.
//
// It is DEFINED AS govcontext.MaxDeliverableInstructionBytes rather than as a
// literal, and that is the entire point of this line. The operator's editor
// refuses a write above that constant; if this number were written out
// separately the two could drift, and the failure mode of that drift is
// exactly the one this change exists to fix — an operator authoring text the
// model never receives. One definition, two uses, no synchronisation to
// remember.
const contextInstructionByteBudget = govcontext.MaxDeliverableInstructionBytes

// ContextResolver is the seam this package reads governed operator context
// through: ADR-064's single resolution function, and nothing else.
//
// The signature is Resolve's exactly, so *govcontext.Resolver satisfies it
// with no adapter and no second assembly of the layers. That is deliberate and
// load-bearing: ADR-064 Decision 8 requires the effective-context preview and
// the model-facing path to call ONE function, and an interface shaped like
// that function is the narrowest way to depend on it. A wider port (a service,
// a "GetOrgInstructions" convenience) would be a place for a second assembly
// to grow.
type ContextResolver interface {
	Resolve(ctx context.Context, tenantID, siteID uuid.UUID, session *govcontext.SessionInput) (govcontext.ResolvedContext, error)
}

// operatorContext resolves the governed instruction text for a FLEET-WIDE tool
// call and returns the exact bytes to place in the result header.
//
// ORGANISATION SCOPE, NOT A SITE. Both shipped tools answer questions about
// the fleet, so there is no site whose overrides could apply; uuid.Nil is
// Resolve's organisation-scope resolution (see resolver.go), which reads the
// org layer and does not touch a site row at all. Site context applies when
// the assistant acts on a named site, and no such tool exists yet.
//
// EVERY FAILURE PATH HERE REFUSES. There is no branch that returns "" with a
// nil error on a load failure, a nil resolver, or an over-budget context, and
// there must never be one: falling back to the compiled-in tool instructions
// would answer the operator's question with the operator's own governance
// silently absent, which is indistinguishable to them from a working system.
// The one legitimate empty string is an organisation that has authored
// nothing, which Resolve reports as a successful resolution of an empty
// context — a fact, not a failure.
func (s *Service) operatorContext(ctx context.Context, auth AuthorizedRequest) (string, error) {
	if s.context == nil {
		// A Service reaching a live tool call with no resolver is a wiring
		// bug, and it refuses for the same reason requireRecorder does: the
		// surface promises the operator's context governs it, and serving
		// without one delivers the opposite of that promise while looking
		// identical to a working call.
		return "", domain.Internal(ErrCodeContextUnavailable,
			"this connection cannot be served because its organisation's context cannot be resolved")
	}
	rc, err := s.context.Resolve(ctx, auth.TenantID, uuid.Nil, nil)
	if err != nil {
		return "", err
	}
	return rc.ModelInstructions()
}

// ErrCodeContextUnavailable is the reason code for a tool call refused because
// the organisation's governed context could not be resolved. It matches
// govcontext's own "context_unavailable" (ADR-064 Decision 13's single reason
// code for this path) so an operator sees one code for one cause, whichever
// surface reports it.
const ErrCodeContextUnavailable = "context_unavailable"
