package abilities

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// CatalogueStore is the admin handler's data access.
type CatalogueStore interface {
	List(ctx context.Context, actor uuid.UUID) ([]sqlc.AbilityCatalogue, error)
	Upsert(ctx context.Context, actor uuid.UUID, entryID *uuid.UUID, in CatalogueInput) (sqlc.AbilityCatalogue, error)
}

// AdminHandler serves the superadmin catalogue routes.
type AdminHandler struct {
	store CatalogueStore
	rec   *audit.Recorder
}

// NewAdminHandler builds the handler.
func NewAdminHandler(store CatalogueStore) *AdminHandler { return &AdminHandler{store: store} }

// SetAuditRecorder wires the audit recorder.
func (h *AdminHandler) SetAuditRecorder(r *audit.Recorder) { h.rec = r }

// RegisterAdmin mounts the routes on a group that is ALREADY behind the admin
// package's requireSuperadmin. Nothing here re-checks the gate.
func (h *AdminHandler) RegisterAdmin(g *gin.RouterGroup) {
	g.GET("/abilities/catalogue", h.list)
	g.POST("/abilities/catalogue", h.create)
	g.PUT("/abilities/catalogue/:entryId", h.update)
	if h.routeStore() != nil {
		g.GET("/abilities/rest-routes", h.listRoutes)
		g.PUT("/abilities/rest-routes/:routeId", h.updateRoute)
	}
}

// catalogueEntryDTO is one entry on the wire.
type catalogueEntryDTO struct {
	EntryID            string          `json:"entry_id"`
	Name               string          `json:"name"`
	Source             string          `json:"source"`
	Class              string          `json:"class"`
	Status             string          `json:"status"`
	Enabled            bool            `json:"enabled"`
	ApprovalMode       string          `json:"approval_mode"`
	PermissionMode     string          `json:"permission_mode"`
	IntegrationID      *string         `json:"integration_id"`
	OwnerDir           *string         `json:"owner_dir"`
	VersionMin         *string         `json:"version_min"`
	VersionMaxTested   *string         `json:"version_max_tested"`
	MinWPVersion       *string         `json:"min_wp_version"`
	MinAgentVersion    *string         `json:"min_agent_version"`
	SchemaStructSHA256 *string         `json:"schema_struct_sha256"`
	DynamicEnumPaths   []string        `json:"dynamic_enum_paths"`
	Title              string          `json:"title"`
	Description        string          `json:"description"`
	Usage              *string         `json:"usage"`
	OperatorPermission *string         `json:"operator_permission"`
	Target             json.RawMessage `json:"target"`
	Snapshot           string          `json:"snapshot"`
	Preview            *string         `json:"preview"`
	ArgRender          json.RawMessage `json:"arg_render"`
	EffectCopy         string          `json:"effect_copy"`
	Limits             json.RawMessage `json:"limits"`
	NestedAllow        []string        `json:"nested_allow"`
	GlobalOptionKeys   []string        `json:"global_option_keys"`
	IntegrationBlock   json.RawMessage `json:"integration_block"`
	Admission          json.RawMessage `json:"admission"`
	EntrySHA256        *string         `json:"entry_sha256"`
	OutputFields       json.RawMessage `json:"output_fields"`
	UpdatedAt          string          `json:"updated_at"`
}

func toCatalogueDTO(r sqlc.AbilityCatalogue) catalogueEntryDTO {
	return catalogueEntryDTO{
		EntryID: r.EntryID.String(), Name: r.Name, Source: r.Source, Class: r.Class, Status: r.Status,
		Enabled: r.Enabled, ApprovalMode: r.ApprovalMode, PermissionMode: r.PermissionMode,
		IntegrationID: r.IntegrationID, OwnerDir: r.OwnerDir, VersionMin: r.VersionMin,
		VersionMaxTested: r.VersionMaxTested, MinWPVersion: r.MinWpVersion, MinAgentVersion: r.MinAgentVersion,
		SchemaStructSHA256: r.SchemaStructSha256, DynamicEnumPaths: nonNil(r.DynamicEnumPaths),
		Title: r.Title, Description: r.Description, Usage: r.Usage, OperatorPermission: r.OperatorPermission,
		Target: rawOrNull(r.Target), Snapshot: r.Snapshot, Preview: r.Preview, ArgRender: rawOrNull(r.ArgRender),
		EffectCopy: r.EffectCopy, Limits: rawOrNull(r.Limits), NestedAllow: nonNil(r.NestedAllow),
		GlobalOptionKeys: nonNil(r.GlobalOptionKeys), IntegrationBlock: rawOrNull(r.IntegrationBlock),
		Admission: rawOrNull(r.Admission), EntrySHA256: r.EntrySha256, OutputFields: rawOrNull(r.OutputFields),
		UpdatedAt: r.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// sessionActor is the acting superadmin: the authenticated session user, and
// only that. No body field can name an actor (the body decoder refuses
// unknown fields, and CatalogueInput has none).
func sessionActor(c *gin.Context) (uuid.UUID, bool) {
	p, ok := domain.PrincipalFromContext(c.Request.Context())
	if !ok || p.Type != domain.PrincipalUser || p.UserID == uuid.Nil {
		httpx.Error(c, domain.Forbidden("superadmin_required", "superadmin access required"))
		return uuid.Nil, false
	}
	return p.UserID, true
}

func (h *AdminHandler) list(c *gin.Context) {
	actor, ok := sessionActor(c)
	if !ok {
		return
	}
	rows, err := h.store.List(c.Request.Context(), actor)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	out := make([]catalogueEntryDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, toCatalogueDTO(r))
	}
	c.JSON(http.StatusOK, gin.H{"entries": out})
}

func decodeCatalogueInput(c *gin.Context) (CatalogueInput, bool) {
	var in CatalogueInput
	body, err := c.GetRawData()
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body could not be read"))
		return in, false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid for this route"))
		return in, false
	}
	return in, true
}

func (h *AdminHandler) create(c *gin.Context) {
	actor, ok := sessionActor(c)
	if !ok {
		return
	}
	in, ok := decodeCatalogueInput(c)
	if !ok {
		return
	}
	h.write(c, actor, nil, in, http.StatusCreated)
}

func (h *AdminHandler) update(c *gin.Context) {
	actor, ok := sessionActor(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("entryId"))
	if err != nil {
		httpx.Error(c, domain.NotFound("entry_not_found", "catalogue entry not found"))
		return
	}
	in, ok := decodeCatalogueInput(c)
	if !ok {
		return
	}
	h.write(c, actor, &id, in, http.StatusOK)
}

func (h *AdminHandler) write(c *gin.Context, actor uuid.UUID, id *uuid.UUID, in CatalogueInput, status int) {
	row, err := h.store.Upsert(c.Request.Context(), actor, id, in)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	// The database wrote ability_catalogue_audit in the same statement; this
	// is the operator-facing audit_log row.
	if h.rec != nil {
		sha := ""
		if row.EntrySha256 != nil {
			sha = *row.EntrySha256
		}
		_, _ = h.rec.Record(c.Request.Context(), audit.Event{
			ActorType:  audit.ActorUser,
			ActorID:    actor.String(),
			Action:     "admin.ability_catalogue.upsert",
			TargetType: "ability_catalogue_entry",
			TargetID:   row.EntryID.String(),
			Metadata:   map[string]any{"name": row.Name, "enabled": row.Enabled, "status": row.Status, "entry_sha256": sha},
		})
	}
	c.JSON(status, toCatalogueDTO(row))
}
