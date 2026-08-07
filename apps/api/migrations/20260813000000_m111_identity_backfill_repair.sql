-- m111 - re-runs m110's identity backfill, because a backfill only ever runs
-- once and the population it had to cover kept changing after it ran.
--
-- WHAT THIS DOES NOT DO. It does not touch the identity key. m110 made
-- (provider, subject, issuer) unique and that is correct: a subject is unique
-- only within the issuer that minted it, so a key without issuer would let two
-- IdPs that happen to mint the same opaque string resolve to one account, and a
-- sign-in would silently land on somebody else's. The lockout that a
-- config-driven key causes is real, but it is answered in internal/auth as an
-- explicit, audited migration (WPMGR_OIDC_PREVIOUS_ISSUER), not by widening the
-- key until it stops discriminating.
--
-- WHY THE BACKFILL RUNS AGAIN. m110's INSERT could only ever see the rows that
-- existed at the instant it ran; schema_migrations then records it and it is
-- never revisited. Anything the PREVIOUS release wrote to users.oidc_issuer /
-- users.oidc_subject afterwards, during a rollback window, has legacy columns
-- and no user_identities row.
--
-- That gap is not cosmetic. The old SSO path never wrote email_verified_at, so
-- such an account looks never-verified to this release, and the account-takeover
-- defence correctly declines to link a social identity onto a never-verified
-- account. A correct rule meeting the wrong population: people who have signed
-- in through SSO for months are refused at the door. Re-running the backfill
-- turns them back into ordinary identity hits, in bulk, at deploy time.
--
-- The runtime half lives in internal/auth (AdoptLegacyIdentity), which repairs
-- anything written AFTER this migration runs. Both halves are needed: a
-- migration cannot cover a rollback that happens after it, and a sign-in path
-- cannot repair someone who never signs in again.
--
-- SAFE TO RUN ANY NUMBER OF TIMES. ON CONFLICT DO NOTHING covers every unique
-- index on the table, so a row written since m110 always wins over anything
-- derived from the legacy columns, and a re-apply moves nothing.
--
-- The predicate is m110's, deliberately unchanged: users with oidc_subject set
-- but oidc_issuer NULL are still skipped, because the pre-m110 sign-in matched
-- on (oidc_issuer, oidc_subject) and a NULL issuer never matched anything. They
-- are not people who lost a working sign-in; inventing an issuer-less identity
-- row for them would create a binding that never existed.
INSERT INTO user_identities (user_id, provider, subject, issuer, email, email_verified, created_at)
SELECT
    u.id,
    'oidc',
    u.oidc_subject,
    u.oidc_issuer,
    u.email,
    -- The old code required the provider to assert a verified email before it
    -- would link to an existing account, so true records what was enforced at
    -- link time rather than an assumption made now.
    true,
    u.created_at
FROM users u
WHERE u.oidc_subject IS NOT NULL
  AND u.oidc_issuer IS NOT NULL
ON CONFLICT DO NOTHING;
