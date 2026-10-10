package abilityrequest

import (
	"context"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// RenderAbilityRequests renders rows exactly as the request queues return
// them, for the AI activity feed: the same object, the same digest rule, the
// same agent versions for the recovery undo, the same newest-edit rule for a
// page edit's undo and the same setter names.
func (h *Handler) RenderAbilityRequests(ctx context.Context, p domain.Principal, rows []sqlc.AssistantAbilityRequest) ([]any, error) {
	withDigest := p.Type == domain.PrincipalUser
	versions := h.svc.AgentVersions(ctx, p, rows)
	newest := h.svc.NewestEdits(ctx, p, rows)
	names := h.svc.readSetterNames(ctx, p, rows)
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, toDTO(r, withDigest, versions[r.SiteID], newest[r.ID], names))
	}
	return out, nil
}
