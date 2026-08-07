-- m110 - social sign-in: one row per linked external identity, replacing the
-- single (oidc_issuer, oidc_subject) pair on users.
--
-- WHY A TABLE AND NOT TWO MORE COLUMNS. users carries exactly one identity
-- today, so linking a second provider means overwriting the first. With Google
-- and GitHub both offered that is not a theoretical limit, it is the ordinary
-- case: somebody signs up with Google, comes back a month later, clicks GitHub
-- because it is the button they remember, and the same verified email resolves
-- to the same account. With one slot the GitHub identity would replace the
-- Google one, and the next Google sign-in would no longer recognise them. A
-- child table makes "this human has two ways in" representable, which is what
-- is actually true.
--
-- (provider, subject) IS THE IDENTITY, NOT THE EMAIL. subject is the
-- provider's own immutable id for the person. Email is stored for display and
-- for the linking decision at sign-in time, and it is deliberately NOT unique
-- here: emails change, get reassigned inside a Workspace, and are reused across
-- providers. Matching on email is how account takeovers happen; matching on
-- (provider, subject) is how they do not. The email column is a record of what
-- the provider asserted, never a key.
--
-- ONE IDENTITY PER PROVIDER PER USER. user_identities_user_provider_key stops
-- one account accumulating several Google identities, which would otherwise be
-- reachable by linking a second Google account and would make "unlink Google"
-- ambiguous.
--
-- NO RLS, matching users itself. A user spans tenants, so neither the user nor
-- the ways they authenticate belong to one tenant, and an RLS policy here would
-- have to invent a tenant for a row that genuinely has none. Access is
-- controlled by the fact that nothing outside the auth package reads it.
--
-- email_verified RECORDS WHAT THE PROVIDER SAID, at the time it said it. It is
-- not a claim about our own verification of the address; users.email_verified_at
-- is that. The linking rules need both, separately, which is the whole reason
-- this column exists rather than being folded into the user row.

CREATE TABLE IF NOT EXISTS user_identities (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- 'google' | 'github' | 'oidc' (the generic single-issuer SSO that predates
    -- this table). Free text rather than an enum so adding a provider is a code
    -- change and not a migration.
    provider       text        NOT NULL,
    subject        text        NOT NULL,
    -- Only meaningful for 'oidc', where a deployment picks its own issuer and
    -- two deployments can legitimately hand out the same subject.
    issuer         text        NOT NULL DEFAULT '',
    email          text        NOT NULL DEFAULT '',
    email_verified boolean     NOT NULL DEFAULT false,
    created_at     timestamptz NOT NULL DEFAULT now(),
    last_login_at  timestamptz
);

-- The identity key. An external identity belongs to exactly one local user.
CREATE UNIQUE INDEX IF NOT EXISTS user_identities_provider_subject_key
    ON user_identities (provider, subject, issuer);

-- One Google, one GitHub, one generic-OIDC identity per user.
CREATE UNIQUE INDEX IF NOT EXISTS user_identities_user_provider_key
    ON user_identities (user_id, provider);

CREATE INDEX IF NOT EXISTS user_identities_user_id_idx
    ON user_identities (user_id);

-- Backfill every identity that already exists on users. The old columns are
-- deliberately NOT dropped in this migration: a deploy is not atomic, and the
-- previous release still reads them, so removing them here would sign out every
-- SSO user for the length of the rollout and break a rollback outright. They
-- become dead weight once this release is everywhere, and dropping them is a
-- later migration's job.
INSERT INTO user_identities (user_id, provider, subject, issuer, email, email_verified, created_at)
SELECT
    u.id,
    'oidc',
    u.oidc_subject,
    COALESCE(u.oidc_issuer, ''),
    u.email,
    -- These identities were linked under the old rule, which already required
    -- the provider to assert a verified email before linking to an existing
    -- account. Recording true here preserves that meaning; it is not an
    -- assumption, it is what the old code enforced at link time.
    true,
    u.created_at
FROM users u
WHERE u.oidc_subject IS NOT NULL
  AND u.oidc_issuer IS NOT NULL
ON CONFLICT DO NOTHING;
