package abilities

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// ---------------------------------------------------------------------------
// The superadmin REST route catalogue (m161). admin_upsert_rest_route is the
// only write path: it refuses unless the actor is a superadmin, serialises
// writers of one route on an advisory lock, refuses an edit that keeps the
// old route_sha256, and audits in the same statement. New routes arrive by
// migration; this surface edits existing ones.
// ---------------------------------------------------------------------------

// RouteCatalogueStore is the route admin's data access. *AdminRepo
// implements it.
type RouteCatalogueStore interface {
	ListRoutes(ctx context.Context, actor uuid.UUID) ([]sqlc.RestRouteCatalogue, error)
	UpdateRoute(ctx context.Context, actor uuid.UUID, routeID string, in RouteInput) (sqlc.RestRouteCatalogue, error)
}

var _ RouteCatalogueStore = (*AdminRepo)(nil)

// RouteInput is one route edit. Every field is optional: omitted fields keep
// their stored values (merged in the write's transaction, under the route's
// lock). route_id and route_sha256 are never body fields.
type RouteInput struct {
	Method             *string          `json:"method"`
	Namespace          *string          `json:"namespace"`
	Template           *string          `json:"template"`
	CorePattern        *string          `json:"core_pattern"`
	PathParams         *json.RawMessage `json:"path_params"`
	QueryKeys          *json.RawMessage `json:"query_keys"`
	PinnedQuery        *json.RawMessage `json:"pinned_query"`
	BodyKeys           *json.RawMessage `json:"body_keys"`
	Class              *string          `json:"class"`
	OutputFields       *json.RawMessage `json:"output_fields"`
	Snapshot           *string          `json:"snapshot"`
	Target             *json.RawMessage `json:"target"`
	ArgRender          *json.RawMessage `json:"arg_render"`
	OperatorPermission *string          `json:"operator_permission"`
	EffectCopy         *string          `json:"effect_copy"`
	Enabled            *bool            `json:"enabled"`
	MinWPVersion       *string          `json:"min_wp_version"`
	Title              *string          `json:"title"`
	Description        *string          `json:"description"`
}

// merge overlays the supplied fields on base.
func (in RouteInput) merge(base sqlc.RestRouteCatalogue) sqlc.RestRouteCatalogue {
	s := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	p := func(dst **string, v *string) {
		if v != nil {
			if *v == "" {
				*dst = nil
			} else {
				x := *v
				*dst = &x
			}
		}
	}
	r := func(dst *[]byte, v *json.RawMessage) {
		if v != nil {
			*dst = nullableRaw(*v)
		}
	}
	s(&base.Method, in.Method)
	s(&base.Namespace, in.Namespace)
	s(&base.Template, in.Template)
	s(&base.CorePattern, in.CorePattern)
	r(&base.PathParams, in.PathParams)
	r(&base.QueryKeys, in.QueryKeys)
	r(&base.PinnedQuery, in.PinnedQuery)
	r(&base.BodyKeys, in.BodyKeys)
	s(&base.Class, in.Class)
	r(&base.OutputFields, in.OutputFields)
	s(&base.Snapshot, in.Snapshot)
	r(&base.Target, in.Target)
	r(&base.ArgRender, in.ArgRender)
	p(&base.OperatorPermission, in.OperatorPermission)
	s(&base.EffectCopy, in.EffectCopy)
	if in.Enabled != nil {
		base.Enabled = *in.Enabled
	}
	p(&base.MinWpVersion, in.MinWPVersion)
	s(&base.Title, in.Title)
	s(&base.Description, in.Description)
	return base
}

// validateRouteShape checks what the database cannot spell: every jsonb
// member holds integers only, written as jsonb prints them (so the bytes
// hashed from the request are the bytes reproduced from the stored row), and
// output_fields is in the same strict grammar as an entry's.
func validateRouteShape(r sqlc.RestRouteCatalogue) error {
	for field, raw := range map[string][]byte{
		"path_params": r.PathParams, "query_keys": r.QueryKeys, "pinned_query": r.PinnedQuery,
		"body_keys": r.BodyKeys, "output_fields": r.OutputFields, "target": r.Target, "arg_render": r.ArgRender,
	} {
		if len(raw) == 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		for {
			tok, err := dec.Token()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return domain.Validation("invalid_route", field+" is not valid JSON")
			}
			if n, ok := tok.(json.Number); ok && !canonicalIntPattern.MatchString(n.String()) {
				return domain.Validation("invalid_route",
					field+" may hold integers only, written plainly (for example 1000, not 1e3 or 1.0)")
			}
		}
	}
	if len(r.OutputFields) == 0 || string(r.OutputFields) == "null" {
		return domain.Validation("invalid_output_fields", "output_fields is required")
	}
	if jsonbTextLen(r.OutputFields) > outputFieldsMaxBytes {
		return domain.Validation("invalid_output_fields", "output_fields may be at most 16 KiB")
	}
	if _, err := agentcmd.ParseOutputShape(r.OutputFields); err != nil {
		return domain.Validation("invalid_output_fields",
			`output_fields must be {"fields":{...}}, {"items":...}, "string", "int" or "bool", at most 8 deep`)
	}
	return nil
}

// mapRouteErr maps admin_upsert_rest_route's refusals to domain errors.
func mapRouteErr(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "42501":
		return domain.Forbidden("superadmin_required", "superadmin access required")
	case "22023":
		if pgErr.Message == "rest_route_catalogue_hash_not_moved" {
			return domain.Conflict("route_hash_not_moved",
				"this edit changes what the route does but would keep its old hash; nothing was saved")
		}
		return domain.Validation("invalid_route", "the route write is missing a required value")
	case "23505":
		return domain.Conflict("route_conflict", "a route with this id already exists")
	case "P0002":
		return domain.NotFound("route_not_found", "rest route not found")
	case "23514", "23502", "22P02":
		return domain.Validation("invalid_route", "the route failed a database check").
			WithDetails(map[string]any{"constraint": pgErr.ConstraintName})
	}
	return err
}

// ListRoutes reads every route. The table is global and readable in any
// transaction; InUserTx keeps the actor on the connection.
func (r *AdminRepo) ListRoutes(ctx context.Context, actor uuid.UUID) ([]sqlc.RestRouteCatalogue, error) {
	var out []sqlc.RestRouteCatalogue
	err := r.pool.InUserTx(ctx, actor, func(tx pgx.Tx) error {
		var err error
		out, err = sqlc.New(tx).ListRestRoutes(ctx)
		return err
	})
	return out, err
}

// UpdateRoute edits one route. The stored row is read, merged and written in
// ONE transaction under the route's advisory lock (the SQL function takes the
// same one, re-entrant). route_sha256 is stamped from RouteBytes of the merged
// row, and the bytes reproduced from the row the database returned must hash
// the same, or the transaction rolls back.
func (r *AdminRepo) UpdateRoute(ctx context.Context, actor uuid.UUID, routeID string, in RouteInput) (sqlc.RestRouteCatalogue, error) {
	var out sqlc.RestRouteCatalogue
	err := r.pool.InUserTx(ctx, actor, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('rest_route_catalogue'), hashtext($1))`, routeID); err != nil {
			return err
		}
		cur, err := q.GetRestRoute(ctx, routeID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("route_not_found", "rest route not found")
		}
		if err != nil {
			return err
		}
		row := in.merge(cur)
		if err := validateRouteShape(row); err != nil {
			return err
		}
		_, sum, err := RouteBytes(row)
		if err != nil {
			return err
		}
		stored, err := q.AdminUpsertRestRoute(ctx, sqlc.AdminUpsertRestRouteParams{
			ActorUserID: actor, CreateRoute: false, RouteID: row.RouteID, Method: row.Method,
			Namespace: row.Namespace, Template: row.Template, CorePattern: row.CorePattern,
			PathParams: row.PathParams, QueryKeys: row.QueryKeys, PinnedQuery: row.PinnedQuery,
			BodyKeys: row.BodyKeys, Class: row.Class, OutputFields: row.OutputFields, Snapshot: row.Snapshot,
			Target: row.Target, ArgRender: row.ArgRender, OperatorPermission: row.OperatorPermission,
			EffectCopy: row.EffectCopy, Enabled: row.Enabled, MinWpVersion: row.MinWpVersion,
			Title: row.Title, Description: row.Description, RouteSha256: &sum,
		})
		if err != nil {
			return mapRouteErr(err)
		}
		if _, again, err := RouteBytes(stored); err != nil || again != sum ||
			stored.RouteSha256 == nil || *stored.RouteSha256 != sum {
			return domain.Validation("route_not_reproducible",
				"the stored route does not reproduce the bytes its hash was computed over")
		}
		out = stored
		return nil
	})
	if err != nil {
		if de, ok := domain.AsDomain(err); ok {
			return sqlc.RestRouteCatalogue{}, de
		}
		return sqlc.RestRouteCatalogue{}, fmt.Errorf("update rest route: %w", mapRouteErr(err))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// restRouteDTO is one route on the wire. hash_current says whether the
// stored route_sha256 is set and reproduces from the row now; a false value
// means the route is not offered and nothing is sent against it.
type restRouteDTO struct {
	RouteID            string          `json:"route_id"`
	Method             string          `json:"method"`
	Namespace          string          `json:"namespace"`
	Template           string          `json:"template"`
	CorePattern        string          `json:"core_pattern"`
	PathParams         json.RawMessage `json:"path_params"`
	QueryKeys          json.RawMessage `json:"query_keys"`
	PinnedQuery        json.RawMessage `json:"pinned_query"`
	BodyKeys           json.RawMessage `json:"body_keys"`
	Class              string          `json:"class"`
	OutputFields       json.RawMessage `json:"output_fields"`
	Snapshot           string          `json:"snapshot"`
	Target             json.RawMessage `json:"target"`
	ArgRender          json.RawMessage `json:"arg_render"`
	OperatorPermission *string         `json:"operator_permission"`
	EffectCopy         string          `json:"effect_copy"`
	Enabled            bool            `json:"enabled"`
	MinWPVersion       *string         `json:"min_wp_version"`
	Title              string          `json:"title"`
	Description        string          `json:"description"`
	RouteSHA256        *string         `json:"route_sha256"`
	HashCurrent        bool            `json:"hash_current"`
	UpdatedAt          string          `json:"updated_at"`
}

func toRouteDTO(r sqlc.RestRouteCatalogue) restRouteDTO {
	_, sum, err := RouteBytes(r)
	return restRouteDTO{
		RouteID: r.RouteID, Method: r.Method, Namespace: r.Namespace, Template: r.Template,
		CorePattern: r.CorePattern, PathParams: rawOrNull(r.PathParams), QueryKeys: rawOrNull(r.QueryKeys),
		PinnedQuery: rawOrNull(r.PinnedQuery), BodyKeys: rawOrNull(r.BodyKeys), Class: r.Class,
		OutputFields: rawOrNull(r.OutputFields), Snapshot: r.Snapshot, Target: rawOrNull(r.Target),
		ArgRender: rawOrNull(r.ArgRender), OperatorPermission: r.OperatorPermission, EffectCopy: r.EffectCopy,
		Enabled: r.Enabled, MinWPVersion: r.MinWpVersion, Title: r.Title, Description: r.Description,
		RouteSHA256: r.RouteSha256, HashCurrent: err == nil && r.RouteSha256 != nil && *r.RouteSha256 == sum,
		UpdatedAt: r.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

var routeIDParam = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

func (h *AdminHandler) routeStore() RouteCatalogueStore {
	rs, _ := h.store.(RouteCatalogueStore)
	return rs
}

func (h *AdminHandler) listRoutes(c *gin.Context) {
	actor, ok := sessionActor(c)
	if !ok {
		return
	}
	rows, err := h.routeStore().ListRoutes(c.Request.Context(), actor)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	out := make([]restRouteDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, toRouteDTO(r))
	}
	c.JSON(http.StatusOK, gin.H{"routes": out})
}

func (h *AdminHandler) updateRoute(c *gin.Context) {
	actor, ok := sessionActor(c)
	if !ok {
		return
	}
	id := c.Param("routeId")
	if !routeIDParam.MatchString(id) {
		httpx.Error(c, domain.NotFound("route_not_found", "rest route not found"))
		return
	}
	body, err := c.GetRawData()
	if err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body could not be read"))
		return
	}
	var in RouteInput
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		httpx.Error(c, domain.Validation("invalid_body", "request body is not valid for this route"))
		return
	}
	row, err := h.routeStore().UpdateRoute(c.Request.Context(), actor, id, in)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	if h.rec != nil {
		_, _ = h.rec.Record(c.Request.Context(), audit.Event{
			ActorType:  audit.ActorUser,
			ActorID:    actor.String(),
			Action:     "admin.rest_route_catalogue.update",
			TargetType: "rest_route",
			TargetID:   row.RouteID,
			Metadata:   map[string]any{"route_id": row.RouteID, "enabled": row.Enabled, "route_sha256": derefStr(row.RouteSha256)},
		})
	}
	c.JSON(http.StatusOK, toRouteDTO(row))
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
