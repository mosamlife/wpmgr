-- m111 - the identity key stops depending on a value an operator can edit, and
-- the m110 backfill is re-run with the predicate it should have had.
--
-- WHY THE KEY CHANGES. m110 made (provider, subject, issuer) unique, which
-- reads as the careful choice and is the opposite. issuer is not a fact about
-- the person, it is a configuration string: WPMGR_OIDC_ISSUER, typed by whoever
-- runs the install. Editing it, to follow a corporate IdP to a new hostname or
-- to add a trailing slash, changed the key of every generic-OIDC identity at
-- once. Every SSO user on the install stopped being recognised on the same
-- deploy, and because user_identities_user_provider_key still held their old
-- row, the re-link that would have papered over it failed outright with a
-- unique violation. One config edit, whole-install lockout.
--
-- An install has exactly one configured OIDC issuer at a time, so issuer never
-- distinguished two rows here. It only ever supplied a way for the lookup to
-- miss. Google and GitHub carry a constant issuer and are unaffected either way.
-- The column stays: it is a useful record of where somebody last came from, and
-- TouchIdentityLogin now refreshes it on each sign-in.
--
-- WHY THE BACKFILL RUNS AGAIN. A migration backfill runs exactly once and
-- schema_migrations guarantees it is never revisited, so m110's INSERT could
-- only ever see the rows that existed at that instant. It missed two groups:
-- anyone whose oidc_issuer was NULL (m110's WHERE required it non-null even
-- though the SELECT already COALESCEd it), and anyone the PREVIOUS release
-- wrote to users.oidc_subject during a rollback window, after m110 had already
-- recorded itself as applied. Each miss became a hard refusal under the new
-- policy, because that release never wrote email_verified_at either, so the
-- account looks never-verified and the takeover defence correctly declines to
-- link onto it. Correct rule, wrong population.
--
-- Re-running here catches everything up to this deploy. The runtime adoption in
-- internal/auth (AdoptLegacyIdentity) catches anything after it, which is the
-- half a migration structurally cannot cover.

-- Duplicates first: the new index cannot be created while any (provider,
-- subject) pair appears twice, and a failed CREATE INDEX inside the boot-time
-- migration transaction is a crash loop, not a warning.
--
-- Reaching here needs two rows for one subject under different issuers, which a
-- single-issuer install does not produce; this is here so that an install which
-- somehow did cannot fail to boot. The survivor is the row actually in use
-- (most recently logged in), and losing a row unlinks a sign-in method without
-- touching the account, which is recoverable. A boot loop is not.
DELETE FROM user_identities i
USING (
    SELECT id,
           row_number() OVER (
               PARTITION BY provider, subject
               ORDER BY last_login_at DESC NULLS LAST, created_at DESC, id DESC
           ) AS rn
    FROM user_identities
) d
WHERE d.id = i.id AND d.rn > 1;

DROP INDEX IF EXISTS user_identities_provider_subject_key;

CREATE UNIQUE INDEX IF NOT EXISTS user_identities_provider_subject_uniq
    ON user_identities (provider, subject);

-- The re-backfill. Identical to m110's corrected form, and safe to run any
-- number of times: ON CONFLICT DO NOTHING leaves every existing row alone, so
-- rows written since m110 win over anything derived from the legacy columns.
INSERT INTO user_identities (user_id, provider, subject, issuer, email, email_verified, created_at)
SELECT
    u.id,
    'oidc',
    u.oidc_subject,
    COALESCE(u.oidc_issuer, ''),
    u.email,
    -- The old code required the provider to assert a verified email before it
    -- would link to an existing account, so true records what was enforced at
    -- link time rather than an assumption made now.
    true,
    u.created_at
FROM users u
WHERE u.oidc_subject IS NOT NULL
  -- A subject shared by two users under different issuers identifies neither of
  -- them once issuer leaves the key. Binding one at random would hand one person
  -- the other's account, so both are left to the sign-in path, which sees the
  -- ambiguity and refuses to guess.
  AND NOT EXISTS (
      SELECT 1 FROM users u2
      WHERE u2.oidc_subject = u.oidc_subject AND u2.id <> u.id
  )
ON CONFLICT DO NOTHING;
