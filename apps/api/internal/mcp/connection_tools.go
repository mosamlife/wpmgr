package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// ---------------------------------------------------------------------------
// CONNECTION TOOLS -- what this connection can actually call (wizard step 10).
//
// THE SCREEN IT SERVES EXISTS TO PREVENT ONE FAILURE: a "you're connected"
// page that lists tools the model cannot call. A list drawn from a constant, or
// from the whole registry, is that failure -- the operator reads five names,
// asks for one of them, and the connection answers with a refusal on a screen
// that had just told them it would work.
//
// SO THE ANSWER IS registry.VisibleTools AND NOTHING ELSE. That function is
// already the tools/list answer this exact grant receives over MCP: the same
// membership (the org ceiling drops a tool) and the same descriptions (a tool
// the grant does not hold is annotated as permanently unavailable). This
// endpoint resolves the same AuthorizedRequest fields Authenticate resolves and
// hands them to the same function, so the dashboard and the model are looking
// at one list rather than at two lists that agree today.
//
// WHAT IT DELIBERATELY DOES NOT DO: it does not filter, sort, re-label,
// summarise or count. Every one of those is a second opinion about what a
// connection can do, and a second opinion is a thing that can be wrong on its
// own.
//
// THE SITE AXIS IS NOT CONSULTED, exactly as it is not consulted by
// VisibleTools: a site-keyed tool with an empty resolved scope is still listed
// and still refuses at invocation with mcp_scope_empty. Step 3's screen is
// where the site axis is shown; this is the tool axis.
// ---------------------------------------------------------------------------

// ConnectionTools resolves the tools ONE grant can see.
//
// requireOrgScopedPrincipal FIRST, before any read, for the reason
// ConnectionStatus gives: a grant is an ORGANISATION-wide credential with no
// :siteId for RequireSiteAccess to key on, so a site-constrained principal is
// refused outright rather than filtered. What a connection may do is not a fact
// a single-site collaborator is entitled to.
//
// AN ABSENT, FOREIGN OR RLS-REFUSED GRANT ALL ANSWER 404, indistinguishably. A
// 403 on another organisation's grant id would confirm that the id exists.
func (s *Service) ConnectionTools(ctx context.Context, p domain.Principal, grantID uuid.UUID) ([]ToolDescriptor, error) {
	if err := requireOrgScopedPrincipal(p); err != nil {
		return nil, err
	}
	if grantID == uuid.Nil {
		return nil, domain.NotFound("connection_not_found", "connection not found")
	}

	grant, err := s.store.GetGrant(ctx, p, grantID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.NotFound("connection_not_found", "connection not found")
		}
		return nil, fmt.Errorf("read mcp grant for tool list: %w", err)
	}

	// THE CEILING AND THE STORED SET, RESOLVED THE WAY Authenticate RESOLVES
	// THEM, because VisibleTools reads both and tells them apart: a tool
	// outside the ceiling is OMITTED (the organisation switched it off and a
	// grant holder may not enumerate that), a tool inside the ceiling that this
	// grant does not hold is LISTED and annotated. Passing the stored set as
	// both would collapse the two boundaries into one and quietly hide every
	// tool the operator unticked -- which is the D1 ruling backwards.
	ceiling, err := OrgDefaultCapabilities(grantScopes())
	if err != nil {
		// A ceiling that cannot be resolved is a REFUSAL, never an empty list.
		// An empty list here reads as "this connection can call nothing",
		// which is a fact about the grant, and this is a failure to answer.
		return nil, fmt.Errorf("resolve organisation capabilities: %w", err)
	}

	stored := capabilitiesFromColumn(grant.Capabilities)
	if len(stored) == 0 {
		// The same refusal Authenticate gives, by the same name and for the
		// same reason: the column's shape CHECK admits '{}', no write path in
		// this package mints one, and a connection holding nothing is a
		// configuration state rather than an empty answer. Rendering it as an
		// empty tool list would put "this connection has no tools" on the
		// screen for a credential that is in fact refused on every request.
		return nil, domain.Forbidden(ErrCodeCapabilityUnmapped,
			"this connection holds no capability, so it can reach no tool")
	}

	caps, err := ceiling.NarrowTo(stored)
	if err != nil {
		// A stored set the ceiling cannot admit is not silently reduced to the
		// intersection here any more than it is at authentication time. The
		// operator is shown the refusal, not a shorter list.
		return nil, fmt.Errorf("resolve grant capabilities: %w", err)
	}

	// The SAME function the transport's tools/list arm calls, on an
	// AuthorizedRequest carrying the same two capability axes. Sites are
	// deliberately left as the zero value: VisibleTools does not read them, and
	// resolving a site scope here would be a read this answer does not depend
	// on.
	return VisibleTools(AuthorizedRequest{
		TenantID:     p.TenantID,
		GrantID:      grant.ID,
		Capabilities: caps,
		OrgCeiling:   ceiling,
	}), nil
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// connectionTools answers GET /api/v1/mcp/connections/:connectionId/tools.
//
// PermAPIKeyRead, THE SAME PERMISSION AS THE LIST AND THE STATUS POLL, and it
// is mounted at the route in RegisterConnections. A caller who may list the
// organisation's connections may read what one of them can call; a caller who
// may not must not learn it a tool at a time. It is an org-level permission, so
// a site-constrained principal is refused by the middleware and again by the
// service.
func (h *Handler) connectionTools(c *gin.Context) {
	principal, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok {
		// Unreachable behind RequireAuth; refusing anyway, because a handler
		// that trusts its mount point is one refactor away from anonymous.
		httpx.Error(c, domain.Unauthorized("unauthenticated", "authentication required"))
		return
	}

	grantID, err := uuid.Parse(c.Param(connectionIDParam))
	if err != nil {
		// 404 and not 400, for the reason connectionStatus gives: a malformed
		// id and another organisation's id must answer identically or the
		// difference is an oracle for which ids exist.
		httpx.Error(c, domain.NotFound("connection_not_found", "connection not found"))
		return
	}

	tools, err := h.svc.ConnectionTools(c.Request.Context(), principal, grantID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, toConnectionToolListDTO(tools))
}

// ---------------------------------------------------------------------------
// DTO
// ---------------------------------------------------------------------------

// connectionToolDTO is one tool as this connection sees it.
//
// NAME AND DESCRIPTION, VERBATIM FROM THE REGISTRY, and no derived field
// beside them. There is deliberately no `available` boolean: whether a tool can
// be called is already in the description -- VisibleTools appends the permanent
// refusal notice naming the missing capability -- and computing a second answer
// to the same question here would be a second thing that can disagree with the
// refusal the model actually receives.
//
// The input schema is not carried. It is the model's contract for calling the
// tool, not the operator's for reading about one, and step 10 renders neither.
type connectionToolDTO struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// connectionToolListDTO wraps the list in an OBJECT rather than returning a
// bare array, for the reason connectionListDTO gives: a bare `[]` and an error
// body are both valid JSON, so a client that forgets to check the status can
// decode a failure into an empty list and render "this connection has no
// tools". The house error envelope cannot decode into this struct.
type connectionToolListDTO struct {
	// Always non-nil on a 200. An empty list is a truthful answer for a
	// connection whose organisation ceiling admits nothing, and it marshals as
	// [] rather than null so it is not a third state.
	Tools []connectionToolDTO `json:"tools"`
}

func toConnectionToolListDTO(tools []ToolDescriptor) connectionToolListDTO {
	out := connectionToolListDTO{Tools: make([]connectionToolDTO, 0, len(tools))}
	for _, t := range tools {
		out.Tools = append(out.Tools, connectionToolDTO{
			Name:        t.Name,
			Description: t.Description,
		})
	}
	return out
}
