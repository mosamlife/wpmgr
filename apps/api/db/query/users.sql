-- name: CreateUser :one
INSERT INTO users (email, password_hash, oidc_subject, oidc_issuer, name)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: GetUserByOIDC :one
SELECT * FROM users WHERE oidc_issuer = $1 AND oidc_subject = $2;

-- name: CountUsers :one
SELECT count(*) FROM users;

-- name: SetUserPasswordHash :exec
UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1;

-- name: LinkUserOIDC :one
UPDATE users
SET oidc_issuer = $2, oidc_subject = $3, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: TouchUserLogin :exec
UPDATE users SET last_login_at = now() WHERE id = $1;
