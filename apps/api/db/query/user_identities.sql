-- Social sign-in identities (m110). One row per external identity a user can
-- sign in with. See db/schema.sql for why this is a table and not two more
-- columns on users, and why email here is a record rather than a key.

-- name: GetUserByIdentity :one
-- The ONLY lookup that authenticates. (provider, subject, issuer) is the
-- provider's immutable id for the human; email is never used to authenticate,
-- only to decide whether linking a NEW identity is permitted.
--
-- ISSUER STAYS IN THE KEY. A subject is only unique WITHIN the issuer that
-- minted it: two identity providers can hand out the same opaque string for two
-- different people, and dropping issuer here would turn that collision into a
-- silent sign-in as somebody else. Continuity across an issuer change is
-- handled one level up, by ListIdentitiesBySubject plus an issuer the operator
-- has explicitly declared, never by widening this key.
SELECT u.*
FROM users u
JOIN user_identities i ON i.user_id = u.id
WHERE i.provider = $1 AND i.subject = $2 AND i.issuer = $3;

-- name: GetIdentity :one
SELECT * FROM user_identities
WHERE provider = $1 AND subject = $2 AND issuer = $3;

-- name: ListIdentitiesBySubject :many
-- The candidates for an issuer migration, and NOT an authenticating lookup on
-- its own: what comes back here is handed to a pure policy that decides whether
-- any of it may be used (see matchStoredIdentity).
--
-- Reached only when the exact-issuer lookup above missed. The policy accepts a
-- row whose issuer differs from the current one in exactly two shapes: the
-- difference is cosmetic (a trailing slash, host case), or the stored issuer is
-- the one the operator DECLARED as this install's previous issuer. Everything
-- else, and any ambiguity at all, resolves to no match.
--
-- The cap is a sanity bound, not part of the policy: more rows than this for
-- one subject is not a state any install reaches, and the policy refuses on
-- ambiguity anyway.
SELECT * FROM user_identities
WHERE provider = $1 AND subject = $2
ORDER BY created_at, id
LIMIT 10;

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

-- name: MigrateIdentityIssuer :execrows
-- Moves ONE identity from the issuer it was stored under to the one that just
-- signed the token. The operator declared the old issuer, the sign-in path
-- decided this row is the unambiguous single candidate, and the caller audits
-- the move: this is the deliberate, recorded half of "an issuer change is a
-- migration", as opposed to a lookup that quietly ignores issuer.
--
-- Matching on the OLD issuer, not just (provider, subject), is what keeps it
-- one row and makes a concurrent second sign-in a no-op rather than a double
-- move; :execrows so the caller only writes an audit entry when a row actually
-- moved.
UPDATE user_identities
SET issuer = $4, last_login_at = now(), email = $5, email_verified = $6
WHERE provider = $1 AND subject = $2 AND issuer = $3;

-- name: AdoptLegacyIdentity :execrows
-- Writes the user_identities row that m110's one-shot backfill never wrote.
--
-- A migration backfill runs exactly once, and schema_migrations guarantees it is
-- never revisited. Anything the previous release wrote to users.oidc_subject
-- afterwards, during a rollback window, therefore has legacy columns and no
-- identity row forever. This is the runtime half of that repair: the sign-in
-- path notices the gap and closes it, which works whatever order the deploy and
-- the rollback happened in.
--
-- Only ever called AFTER the policy has allowed the sign-in, so a refused
-- account cannot acquire a permanent identity binding on its way out.
--
-- ON CONFLICT DO NOTHING covers both unique indexes on purpose. Two concurrent
-- sign-ins racing to heal the same row is a no-op, and a user who already holds
-- an 'oidc' identity under a DIFFERENT subject keeps it: in both cases the
-- legacy columns already told us who is signing in, so failing here would turn
-- a successful repair into a failed login.
--
-- :execrows so the caller can tell a real repair from a no-op. One repair, one
-- audit entry: a conflict that repeats on every sign-in must not write an audit
-- entry on every sign-in.
INSERT INTO user_identities (user_id, provider, subject, issuer, email, email_verified, last_login_at)
VALUES ($1, $2, $3, $4, $5, $6, now())
ON CONFLICT DO NOTHING;

-- name: DeleteIdentity :exec
DELETE FROM user_identities WHERE user_id = $1 AND provider = $2;

-- name: CountIdentitiesForUser :one
-- Guards unlink: removing the last identity from a user with no password would
-- lock them out of their own account permanently.
SELECT count(*) FROM user_identities WHERE user_id = $1;
