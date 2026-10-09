-- m161: rest_route_catalogue (global allowlist of reviewed WordPress REST
-- routes). See the migration header for the grant model. The table is global
-- and its one policy is FOR SELECT USING (true), so every read here works in
-- any transaction.

-- name: ListEnabledRestRoutes :many
-- The routes the engine may offer: enabled. A disabled route is its kill
-- switch.
SELECT * FROM rest_route_catalogue
WHERE enabled
ORDER BY route_id;

-- name: ListRestRoutes :many
-- Every route, enabled or not, for the admin screen and the boot stamp.
SELECT * FROM rest_route_catalogue
ORDER BY route_id;

-- name: GetRestRoute :one
-- One route by id. pgx.ErrNoRows when it does not exist.
SELECT * FROM rest_route_catalogue
WHERE route_id = sqlc.arg(route_id)::text;

-- name: AdminUpsertRestRoute :one
-- The superadmin write path. Call it only behind requireSuperadmin.
-- create true inserts (23505 if the route exists); false updates (P0002 if it
-- does not). Refuses 42501 unless actor_user_id names a superadmin, 22023 on
-- a NULL create flag or route_id, 23514 on any row CHECK, and writes a
-- rest_route_catalogue_audit row in the same statement.
SELECT * FROM admin_upsert_rest_route(
    sqlc.arg(actor_user_id)::uuid,
    sqlc.arg(create_route)::boolean,
    sqlc.arg(route_id)::text,
    sqlc.arg(method)::text,
    sqlc.arg(namespace)::text,
    sqlc.arg(template)::text,
    sqlc.arg(core_pattern)::text,
    sqlc.arg(path_params)::jsonb,
    sqlc.arg(query_keys)::jsonb,
    sqlc.arg(pinned_query)::jsonb,
    sqlc.arg(body_keys)::jsonb,
    sqlc.arg(class)::text,
    sqlc.arg(output_fields)::jsonb,
    sqlc.arg(snapshot)::text,
    sqlc.narg(target)::jsonb,
    sqlc.arg(arg_render)::jsonb,
    sqlc.narg(operator_permission)::text,
    sqlc.arg(effect_copy)::text,
    sqlc.arg(enabled)::boolean,
    sqlc.narg(min_wp_version)::text,
    sqlc.arg(title)::text,
    sqlc.arg(description)::text,
    sqlc.narg(route_sha256)::text
);

-- name: StampWpmgrRestRouteHash :one
-- m161, m157's stamp for routes. Stores the Go-computed route hash on a route
-- whose hash is still NULL, and audits it with a NULL actor. Needs no
-- superadmin. Refusals: 22023 not 64 lowercase hex, P0002 no route, 55000
-- already stamped (a concurrent stamper lost the race; re-read).
SELECT * FROM stamp_wpmgr_rest_route_hash(
    sqlc.arg(route_id)::text,
    sqlc.arg(route_sha256)::text
);

-- name: ListRestRouteAudit :many
-- Newest first, for the admin screen.
SELECT * FROM rest_route_catalogue_audit
WHERE route_id = sqlc.arg(route_id)::text
ORDER BY id DESC
LIMIT sqlc.arg(row_limit)::int;
