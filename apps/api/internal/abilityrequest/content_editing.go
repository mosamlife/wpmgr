package abilityrequest

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// ContentEditingEnabler sends content_editing_enable. *agentcmd.Client
// satisfies it.
type ContentEditingEnabler interface {
	ContentEditingEnable(ctx context.Context, siteID uuid.UUID, siteURL string) (agentcmd.ContentEditingEnableResponse, error)
}

// SetEnabler wires the enable command.
func (s *Service) SetEnabler(e ContentEditingEnabler) { s.enabler = e }

// ContentEditingState is a site's content-editing state on the wire.
type ContentEditingState struct {
	SiteID          uuid.UUID  `json:"site_id"`
	Enabled         bool       `json:"enabled"`
	EnabledAt       *time.Time `json:"enabled_at"`
	PrincipalUserID *int64     `json:"principal_user_id"`
	EnabledBy       *uuid.UUID `json:"enabled_by"`
}

func stateOf(id uuid.UUID, row sqlc.GetSiteContentEditingRow) ContentEditingState {
	st := ContentEditingState{SiteID: id, Enabled: row.ContentEditingEnabledAt.Valid, PrincipalUserID: row.ContentEditingPrincipalUserID}
	if row.ContentEditingEnabledAt.Valid {
		t := row.ContentEditingEnabledAt.Time
		st.EnabledAt = &t
	}
	if row.ContentEditingEnabledBy.Valid {
		u := uuid.UUID(row.ContentEditingEnabledBy.Bytes)
		st.EnabledBy = &u
	}
	return st
}

// ContentEditing reads one site's state.
func (s *Service) ContentEditing(ctx context.Context, p domain.Principal, siteID uuid.UUID) (ContentEditingState, error) {
	var row sqlc.GetSiteContentEditingRow
	err := s.runAsCaller(ctx, p, func(q *sqlc.Queries, _ pgx.Tx) error {
		var err error
		row, err = q.GetSiteContentEditing(ctx, sqlc.GetSiteContentEditingParams{SiteID: siteID, TenantID: p.TenantID})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ContentEditingState{}, domain.NotFound("site_not_found", "site not found")
	}
	if err != nil {
		return ContentEditingState{}, domain.Internal("content_editing_read_failed", "failed to read content editing").WithCause(err)
	}
	return stateOf(siteID, row), nil
}

// EnableContentEditing asks the site's agent to create (or confirm) its
// content service user, then records it with the acting person.
func (s *Service) EnableContentEditing(ctx context.Context, p domain.Principal, siteID uuid.UUID) (ContentEditingState, error) {
	if err := requireSession(p); err != nil {
		return ContentEditingState{}, err
	}
	if s.enabler == nil {
		return ContentEditingState{}, domain.Unavailable("content_editing_unavailable", "Content editing cannot be enabled on this server.")
	}
	var site sqlc.GetSiteRow
	err := s.runAsCaller(ctx, p, func(q *sqlc.Queries, _ pgx.Tx) error {
		var err error
		site, err = q.GetSite(ctx, sqlc.GetSiteParams{ID: siteID, TenantID: p.TenantID})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ContentEditingState{}, domain.NotFound("site_not_found", "site not found")
	}
	if err != nil {
		return ContentEditingState{}, domain.Internal("content_editing_read_failed", "failed to read the site").WithCause(err)
	}
	if !agentMeetsFloor(site.AgentVersion, agentcmd.MinAgentVersionForPageCreate) {
		return ContentEditingState{}, domain.Conflict("content_editing_agent_outdated",
			"This site's WPMgr agent must be updated to "+agentcmd.MinAgentVersionForPageCreate+" or later before content editing can be enabled.")
	}
	resp, err := s.enabler.ContentEditingEnable(ctx, siteID, site.Url)
	var refusal *agentcmd.ContentEditingEnableRefusal
	if errors.As(err, &refusal) {
		return ContentEditingState{}, domain.Conflict("content_editing_refused",
			"The site refused to enable content editing ("+refusal.Code+"). Nothing changed in WPMgr.")
	}
	if err != nil || resp.UserID < 1 {
		return ContentEditingState{}, domain.Unavailable("content_editing_unreachable",
			"The site could not be reached, so content editing was not enabled. Try again.")
	}
	var out sqlc.MarkSiteContentEditingEnabledRow
	err = s.runAsCaller(ctx, p, func(q *sqlc.Queries, tx pgx.Tx) error {
		var err error
		out, err = q.MarkSiteContentEditingEnabled(ctx, sqlc.MarkSiteContentEditingEnabledParams{
			PrincipalUserID: resp.UserID, EnabledBy: p.UserID, SiteID: siteID, TenantID: p.TenantID,
		})
		if err != nil {
			return err
		}
		if s.audit == nil {
			return errors.New("audit recorder not wired")
		}
		_, err = s.audit.RecordInTx(ctx, tx, audit.Event{
			TenantID: p.TenantID, ActorType: audit.ActorUser, ActorID: p.UserID.String(),
			Action: audit.ActionSiteContentEditingEnabled, TargetType: "site", TargetID: siteID.String(),
			Metadata: map[string]any{"site_id": siteID.String(), "principal_user_id": resp.UserID, "outcome": resp.Outcome},
		})
		return err
	})
	if err != nil {
		return ContentEditingState{}, domain.Internal("content_editing_record_failed",
			"The site enabled content editing but WPMgr could not record it. Try again.").WithCause(err)
	}
	return stateOf(siteID, sqlc.GetSiteContentEditingRow(out)), nil
}

func (h *Handler) getContentEditing(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	siteID, ok := parseID(c, "siteId")
	if !ok {
		return
	}
	st, err := h.svc.ContentEditing(c.Request.Context(), p, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, st)
}

func (h *Handler) enableContentEditing(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	siteID, ok := parseID(c, "siteId")
	if !ok {
		return
	}
	st, err := h.svc.EnableContentEditing(c.Request.Context(), p, siteID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	c.JSON(http.StatusOK, st)
}
