-- name: CreateTenant :one
INSERT INTO tenants (name, slug)
VALUES ($1, $2)
RETURNING *;

-- name: GetTenant :one
SELECT * FROM tenants
WHERE id = $1;

-- name: ListTenants :many
SELECT * FROM tenants
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;
