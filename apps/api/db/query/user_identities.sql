-- Social sign-in identities (m110). One row per external identity a user can
-- sign in with. See db/schema.sql for why this is a table and not two more
-- columns on users, and why email here is a record rather than a key.

-- name: GetUserByIdentity :one
-- The ONLY lookup that authenticates. (provider, subject) is the provider's
-- immutable id for the human; email is never used to authenticate, only to
-- decide whether linking a NEW identity is permitted.
--
-- ISSUER IS DELIBERATELY NOT PART OF THIS KEY. It used to be, and that made
-- every generic-OIDC identity on the install depend on a string an operator can
-- edit: moving a corporate IdP to a new hostname, or adding a trailing slash to
-- WPMGR_OIDC_ISSUER, invalidated every row at once and locked out every SSO
-- user simultaneously. An install has exactly one configured OIDC issuer at a
-- time, so issuer never discriminated between two rows here; it only ever added
-- a way for the lookup to miss. It is kept on the row as a record of where the
-- person last came from, refreshed by TouchIdentityLogin.
SELECT u.*
FROM users u
JOIN user_identities i ON i.user_id = u.id
WHERE i.provider = $1 AND i.subject = $2;

-- name: GetIdentity :one
SELECT * FROM user_identities
WHERE provider = $1 AND subject = $2;

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
--
-- issuer is refreshed rather than matched on, so an operator who repoints
-- WPMGR_OIDC_ISSUER gets rows that heal themselves to the new value on each
-- person's next sign-in instead of rows that no longer match anything.
UPDATE user_identities
SET last_login_at = now(), issuer = $3, email = $4, email_verified = $5
WHERE provider = $1 AND subject = $2;

-- name: AdoptLegacyIdentity :exec
-- Writes the user_identities row that m110's one-shot backfill never wrote.
--
-- A migration backfill runs exactly once, and schema_migrations guarantees it is
-- never revisited. Anything the previous release wrote to users.oidc_subject
-- afterwards, during a rollback window, therefore has legacy columns and no
-- identity row forever. This is the runtime half of that repair: the sign-in
-- path notices the gap and closes it, which works whatever order the deploy and
-- the rollback happened in.
--
-- ON CONFLICT DO NOTHING covers both unique indexes on purpose. Two concurrent
-- sign-ins racing to heal the same row is a no-op, and a user who already holds
-- an 'oidc' identity under a DIFFERENT subject keeps it: in both cases the
-- legacy columns already told us who is signing in, so failing here would turn
-- a successful repair into a failed login.
INSERT INTO user_identities (user_id, provider, subject, issuer, email, email_verified, last_login_at)
VALUES ($1, $2, $3, $4, $5, $6, now())
ON CONFLICT DO NOTHING;

-- name: DeleteIdentity :exec
DELETE FROM user_identities WHERE user_id = $1 AND provider = $2;

-- name: CountIdentitiesForUser :one
-- Guards unlink: removing the last identity from a user with no password would
-- lock them out of their own account permanently.
SELECT count(*) FROM user_identities WHERE user_id = $1;
