package abilities

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// Route is one rest_route_catalogue row (m161) exactly as it is sent to the
// agent. Its JSON encoding (RouteBytes) is the text the agent hashes, so the
// field order here IS the wire order (the table's column order) and must not
// be reshuffled without re-stamping every stored route_sha256.
//
// It leaves out created_at, updated_at, updated_by_user_id (bookkeeping) and
// route_sha256 (the hash cannot cover itself).
type Route struct {
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
}

// ErrRouteChanged: a route's stored hash differs from the bytes its row
// reproduces, so the catalogue changed outside the admin write.
var ErrRouteChanged = errors.New("abilities: rest route changed since it was stamped")

// ErrRouteUnstamped: a route whose route_sha256 is still NULL. Nothing is
// sent against it until the boot stamp has run.
var ErrRouteUnstamped = errors.New("abilities: rest route has no stamped hash")

// RouteFromRow projects a route row onto the wire route.
func RouteFromRow(r sqlc.RestRouteCatalogue) Route {
	return Route{
		RouteID: r.RouteID, Method: r.Method, Namespace: r.Namespace, Template: r.Template,
		CorePattern: r.CorePattern, PathParams: rawOrNull(r.PathParams), QueryKeys: rawOrNull(r.QueryKeys),
		PinnedQuery: rawOrNull(r.PinnedQuery), BodyKeys: rawOrNull(r.BodyKeys), Class: r.Class,
		OutputFields: rawOrNull(r.OutputFields), Snapshot: r.Snapshot, Target: rawOrNull(r.Target),
		ArgRender: rawOrNull(r.ArgRender), OperatorPermission: r.OperatorPermission,
		EffectCopy: r.EffectCopy, Enabled: r.Enabled, MinWPVersion: r.MinWpVersion,
		Title: r.Title, Description: r.Description,
	}
}

// RouteBytes is the canonical route text: encoding/json over Route, whose
// field order is fixed and whose jsonb members are canonicalised (rawOrNull,
// keys sorted, numbers kept as written). The same row always yields the same
// bytes; route_sha256 is their sha256.
func RouteBytes(r sqlc.RestRouteCatalogue) ([]byte, string, error) {
	b, err := json.Marshal(RouteFromRow(r))
	if err != nil {
		return nil, "", fmt.Errorf("marshal rest route %s: %w", r.RouteID, err)
	}
	return b, agentcmd.SHA256Hex(b), nil
}

// SendableRoute returns the bytes and hash to send for a route. Unlike an
// entry, a route with no stored hash is refused (fail closed): every route is
// NULL until the boot stamp fills it.
func SendableRoute(r sqlc.RestRouteCatalogue) ([]byte, string, error) {
	if r.RouteSha256 == nil {
		return nil, "", fmt.Errorf("%w: %s", ErrRouteUnstamped, r.RouteID)
	}
	b, sum, err := RouteBytes(r)
	if err != nil {
		return nil, "", err
	}
	if *r.RouteSha256 != sum {
		return nil, "", fmt.Errorf("%w: %s", ErrRouteChanged, r.RouteID)
	}
	return b, sum, nil
}

// StampOwnRouteHashes runs at boot, as StampOwnEntryHashes does for entries.
// For each route with a NULL hash it computes RouteBytes and stores their
// hash through stamp_wpmgr_rest_route_hash (m161), which only moves NULL to a
// hash and audits it with no human actor. One failure does not stop the
// others; each is logged.
func StampOwnRouteHashes(ctx context.Context, pool *db.Pool, logger *slog.Logger) (stamped int, err error) {
	var rows []sqlc.RestRouteCatalogue
	if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		var err error
		rows, err = sqlc.New(tx).ListRestRoutes(ctx)
		return err
	}); err != nil {
		return 0, fmt.Errorf("read rest route catalogue: %w", err)
	}
	for _, r := range rows {
		if r.RouteSha256 != nil {
			continue
		}
		_, sum, err := RouteBytes(r)
		if err != nil {
			logger.ErrorContext(ctx, "rest route catalogue: stamp skipped", slog.String("route_id", r.RouteID), slog.Any("error", err))
			continue
		}
		err = pool.InAgentTx(ctx, func(tx pgx.Tx) error {
			_, err := sqlc.New(tx).StampWpmgrRestRouteHash(ctx, sqlc.StampWpmgrRestRouteHashParams{
				RouteID: r.RouteID, RouteSha256: sum,
			})
			return err
		})
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "55000" {
			var now sqlc.RestRouteCatalogue
			_ = pool.InAgentTx(ctx, func(tx pgx.Tx) error {
				var rerr error
				now, rerr = sqlc.New(tx).GetRestRoute(ctx, r.RouteID)
				return rerr
			})
			logger.InfoContext(ctx, "rest route catalogue: route already stamped", slog.String("route_id", r.RouteID),
				slog.Bool("matches", now.RouteSha256 != nil && *now.RouteSha256 == sum))
			continue
		}
		if err != nil {
			logger.WarnContext(ctx, "rest route catalogue: stamp refused", slog.String("route_id", r.RouteID), slog.Any("error", err))
			continue
		}
		stamped++
		logger.InfoContext(ctx, "rest route catalogue: route hash stamped", slog.String("route_id", r.RouteID), slog.String("route_sha256", sum))
	}
	return stamped, nil
}
