-- Social sign-in identities (m110). One row per external identity a user can
-- sign in with. See db/schema.sql for why this is a table and not two more
-- columns on users, and why email here is a record rather than a key.

-- name: GetUserByIdentity :one
-- The ONLY lookup that authenticates. (provider, subject, issuer) is the
-- provider's immutable id for the human; email is never used to authenticate,
-- only to decide whether linking a NEW identity is permitted.
SELECT u.*
FROM users u
JOIN user_identities i ON i.user_id = u.id
WHERE i.provider = $1 AND i.subject = $2 AND i.issuer = $3;

-- name: GetIdentity :one
SELECT * FROM user_identities
WHERE provider = $1 AND subject = $2 AND issuer = $3;

-- name: ListIdentitiesForUser :many
-- Powers the account settings list, so a user can see what is linked and unlink
-- it. Ordered for a stable UI.
SELECT * FROM user_identities WHERE user_id = $1 ORDER BY provider, created_at;

-- name: CreateIdentity :one
INSERT INTO user_identities (user_id, provider, subject, issuer, email, email_verified, last_login_at)
VALUES ($1, $2, $3, $4, $5, $6, now())
RETURNING *;

-- name: TouchIdentityLogin :exec
-- Records the provider's current email alongside the login stamp: a provider
-- may report a changed address, and keeping the last-seen value makes an
-- unexpected change visible instead of silently discarding it. This never
-- changes users.email, which stays the account's own address.
UPDATE user_identities
SET last_login_at = now(), email = $4, email_verified = $5
WHERE provider = $1 AND subject = $2 AND issuer = $3;

-- name: DeleteIdentity :exec
DELETE FROM user_identities WHERE user_id = $1 AND provider = $2;

-- name: CountIdentitiesForUser :one
-- Guards unlink: removing the last identity from a user with no password would
-- lock them out of their own account permanently.
SELECT count(*) FROM user_identities WHERE user_id = $1;
