-- m120 - GH #510: give an API key an EXPLICIT capability set, a key kind and a
-- site allowlist, so a non-human principal can be granted least privilege.
--
-- THE DEFECT
-- ----------
-- api_keys.role is a single rank in a totally ordered hierarchy
-- (internal/authz/role.go: client 1 < viewer 2 < operator 3 < admin 4 < owner 5)
-- and authz.Allows() is `r.AtLeast(minRoleFor[p])`. Granting an assistant the
-- admin-tier "site.files.read" therefore also grants every other admin-and-below
-- permission in the same map, including member:manage, apikey:manage,
-- audit:read and site:autologin. There is no way to say "read files, manage
-- nothing". api_keys also carries no site column at all, so a key is tenant-wide
-- by construction.
--
-- WHAT THIS MIGRATION DOES *NOT* DO — READ THIS BEFORE TRUSTING A COLUMN
-- ---------------------------------------------------------------------
-- allowed_site_ids stores an allowlist. It does NOT enforce one. There is no
-- RLS policy in this migration that reads it, and adding these columns widens
-- nothing: RLS in PostgreSQL is row-level, so api_keys_tenant_isolation and
-- api_keys_prefix_lookup (20260527130000_auth_multitenancy.sql) already govern
-- every column on the row, the new ones included.
--
-- Per the #510 design, database-level site scoping for API-key principals is
-- explicitly v2. In v1 the boundary is APPLICATION-ENFORCED, at one audited
-- chokepoint in Go. Until that chokepoint exists and is proven, these columns
-- are inert metadata and a reviewer must not read their presence as a boundary.
--
-- What v2 would add, for the record: api_keys has no site_id, so it is not a
-- site-keyed table and the RESTRICTIVE <table>_site_scope policy the site-keyed
-- siblings carry (see 20260531050000_m19_orgs_sharing.sql and
-- 20260814000000_m112_email_site_scope_rls.sql) does not apply to api_keys
-- itself. v2 is the mirror image: the auth layer sets app.site_scope='on' plus
-- the resolved allowlist GUC for a site-scoped API-key principal exactly as it
-- already does for a site-scoped human collaborator, and every site-keyed table
-- then refuses out-of-allowlist rows through the RESTRICTIVE policy it ALREADY
-- has. That is a Go change in the dispatch layer plus proofs, not new DDL here.
--
-- COEXISTENCE WITH role: role IS RETAINED, NOT DERIVED, NOT REPLACED
-- ------------------------------------------------------------------
-- Every column below is additive with a default that reproduces present-day
-- behaviour exactly, so a key that exists before this migration runs neither
-- gains nor loses a single permission:
--
--   kind             -> 'integration'   (what every existing key is)
--   auth_model       -> 'role'          (resolve permissions from role, as now)
--   capabilities     -> NULL            (no set; role is authoritative)
--   site_scope       -> 'org'           (tenant-wide, as now)
--   allowed_site_ids -> '{}'            (empty; meaningless while site_scope='org')
--
-- role remains the authority for auth_model='role'. For auth_model='capability'
-- the capability set is the authority and role must NOT be consulted for any
-- permission decision. role stays on the row regardless, for display, audit
-- attribution and the non-permission role-rank checks — it is never dropped.
--
-- WHY auth_model EXISTS WHEN `capabilities IS NOT NULL` SAYS THE SAME THING
-- ------------------------------------------------------------------------
-- It is deliberately redundant, and constraint (5) below keeps the redundancy
-- from ever diverging. It exists because the NULL-vs-'{}' distinction does not
-- survive the round trip into Go: sqlc/pgx scan both SQL NULL and the empty
-- array into a zero-length []string, and `len(caps) == 0` is the idiom any
-- reviewer writes. Collapsing those two states is precisely the fail-open this
-- ticket is about — an empty capability set read as "fall back to role" hands a
-- deliberately-restricted key its full role authority. A NOT NULL text
-- discriminator cannot be collapsed by a driver, by JSON, or by len().
--
-- The same reasoning gives site_scope its own column rather than inferring
-- restriction from `allowed_site_ids <> '{}'`: "restricted to zero sites" is a
-- legitimate, fail-CLOSED state and must be expressible. site_scope's two values
-- are the exact string constants domain.ScopeOrg and domain.ScopeSite already
-- use, so the Go mapping is an assignment and not a translation table.
--
-- CONSTRAINT POLICY: SHAPE IS THE DATABASE'S, VOCABULARY IS GO'S
-- --------------------------------------------------------------
-- There is deliberately NO CHECK enumerating the permission strings.
-- `grep -hoE 'Permission = "[^"]+"' internal/authz/role.go | sort -u | grep -c .`
-- returns 30 today and has only ever grown; the vocabulary is owned by Go and
-- extended by ordinary feature work. A CHECK listing them inverts that: adding
-- a permission in Go would require a migration before any key could hold it,
-- and until that migration applied, INSERTs would fail 23514 in production at
-- the exact moment a feature rolls out. Migrations here apply inside main() at
-- boot, so a lagging database fails a NEW binary's writes closed. This repeats
-- the reasoning that already rejected a status CHECK on the same grounds.
--
-- The kind and auth_model and site_scope CHECKs are NOT the same call, and the
-- asymmetry is the point: those are closed structural discriminators that
-- change how authorization is computed, not an open vocabulary. A third kind
-- cannot be honoured without a new Go branch shipping anyway, so the migration
-- rides the same release; and an unconstrained free-text kind lets a typo
-- ('agnet') fall through a Go switch's default branch, which is the fail-OPEN
-- direction. Constrain closed discriminators; never constrain an open
-- vocabulary the application owns.
--
-- The capabilities CHECK constrains shape only, using exclusively IMMUTABLE
-- primitives. Verified on this server, not assumed:
--   select proname, provolatile from pg_proc
--    where proname in ('array_ndims','cardinality','array_position','array_to_string');
--   -> array_ndims i | cardinality i | array_position i | array_to_string s
-- array_to_string is STABLE, so the tempting whole-array regex format check
-- does not belong in a CHECK; per-element format and vocabulary validation is
-- Go's, at the same place that already validates role.
--
-- IDEMPOTENCE: every statement is guarded, so this file applies twice with no
-- error. That is the intended contract, matching the surrounding migrations.

-- ---------------------------------------------------------------------------
-- 1. Columns. All additive, all with backward-compatible defaults.
-- ---------------------------------------------------------------------------
ALTER TABLE "public"."api_keys"
  ADD COLUMN IF NOT EXISTS "kind" text NOT NULL DEFAULT 'integration';

ALTER TABLE "public"."api_keys"
  ADD COLUMN IF NOT EXISTS "auth_model" text NOT NULL DEFAULT 'role';

-- No DEFAULT, and nullable on purpose. A DEFAULT of '{}' would give every
-- pre-existing key a zero-length capability set, which under a
-- capabilities-are-authoritative reading strips all authority from every key in
-- the fleet at boot. NULL means "this key has no capability set at all".
ALTER TABLE "public"."api_keys"
  ADD COLUMN IF NOT EXISTS "capabilities" text[];

ALTER TABLE "public"."api_keys"
  ADD COLUMN IF NOT EXISTS "site_scope" text NOT NULL DEFAULT 'org';

-- NOT NULL with an empty-array default: the presence of a restriction is
-- site_scope's job, so this column never needs a NULL to mean "unrestricted"
-- and can therefore never be read ambiguously.
ALTER TABLE "public"."api_keys"
  ADD COLUMN IF NOT EXISTS "allowed_site_ids" uuid[] NOT NULL DEFAULT '{}'::uuid[];

-- ---------------------------------------------------------------------------
-- 2. Constraints. Each is guarded so a re-run is a no-op rather than a 42710.
-- ---------------------------------------------------------------------------

-- (1) Closed discriminator: what sort of principal this key represents.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
     WHERE conrelid = 'public.api_keys'::regclass
       AND conname  = 'api_keys_kind_check'
  ) THEN
    ALTER TABLE "public"."api_keys"
      ADD CONSTRAINT "api_keys_kind_check"
      CHECK ("kind" IN ('integration', 'agent'));
  END IF;
END
$$;

-- (2) Closed discriminator: how permissions are computed for this key.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
     WHERE conrelid = 'public.api_keys'::regclass
       AND conname  = 'api_keys_auth_model_check'
  ) THEN
    ALTER TABLE "public"."api_keys"
      ADD CONSTRAINT "api_keys_auth_model_check"
      CHECK ("auth_model" IN ('role', 'capability'));
  END IF;
END
$$;

-- (3) Closed discriminator, reusing domain.ScopeOrg / domain.ScopeSite verbatim.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
     WHERE conrelid = 'public.api_keys'::regclass
       AND conname  = 'api_keys_site_scope_check'
  ) THEN
    ALTER TABLE "public"."api_keys"
      ADD CONSTRAINT "api_keys_site_scope_check"
      CHECK ("site_scope" IN ('org', 'site'));
  END IF;
END
$$;

-- (4) Shape of the capability set. Vocabulary is NOT checked here (see header).
--     - 1-dimensional: PostgreSQL will happily store a 2-D text[] otherwise,
--       which unnest() would silently flatten into capabilities nobody granted.
--     - bounded cardinality: a capability set is a hand-picked grant, not a
--       bulk column; 64 is far above the 30 permissions that exist.
--     - no NULL element: array_position(arr, NULL) finds the first NULL, so
--       `IS NULL` here means "no NULL element present".
--     - no empty-string element: '' is not a permission and would otherwise sit
--       in the set looking like one.
--     An empty array IS permitted and means exactly "zero capabilities", the
--     fail-closed state; only cardinality > 64 and malformed elements are refused.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
     WHERE conrelid = 'public.api_keys'::regclass
       AND conname  = 'api_keys_capabilities_shape_check'
  ) THEN
    ALTER TABLE "public"."api_keys"
      ADD CONSTRAINT "api_keys_capabilities_shape_check"
      CHECK (
        "capabilities" IS NULL
        OR (
          coalesce(array_ndims("capabilities"), 1) = 1
          AND cardinality("capabilities") <= 64
          AND array_position("capabilities", NULL) IS NULL
          AND NOT ('' = ANY ("capabilities"))
        )
      );
  END IF;
END
$$;

-- (5) The biconditional that stops auth_model and capabilities diverging. This
--     is what makes auth_model safe to trust as the sole discriminator in Go:
--     auth_model='capability' guarantees a non-NULL set (possibly empty, which
--     is zero authority), and auth_model='role' guarantees there is no set to
--     misread.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
     WHERE conrelid = 'public.api_keys'::regclass
       AND conname  = 'api_keys_auth_model_capabilities_check'
  ) THEN
    ALTER TABLE "public"."api_keys"
      ADD CONSTRAINT "api_keys_auth_model_capabilities_check"
      CHECK (
        ("auth_model" = 'capability' AND "capabilities" IS NOT NULL)
        OR ("auth_model" = 'role' AND "capabilities" IS NULL)
      );
  END IF;
END
$$;

-- (6) An org-scoped key must not carry an allowlist. A row with site_scope='org'
--     and a populated allowed_site_ids is an ambiguous half-state: one reader
--     treats the list as restrictive, another ignores it, and the two disagree
--     about the boundary. Forbid it rather than document it.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
     WHERE conrelid = 'public.api_keys'::regclass
       AND conname  = 'api_keys_site_scope_allowlist_check'
  ) THEN
    ALTER TABLE "public"."api_keys"
      ADD CONSTRAINT "api_keys_site_scope_allowlist_check"
      CHECK (
        "site_scope" = 'site'
        OR cardinality("allowed_site_ids") = 0
      );
  END IF;
END
$$;

-- (7) The least-privilege guarantee, in the database. An 'agent' key is the
--     principal this ticket exists for; it must never be able to fall back to
--     whole-role authority. Creating one without a capability set is refused
--     here, not merely discouraged in a handler.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
     WHERE conrelid = 'public.api_keys'::regclass
       AND conname  = 'api_keys_agent_capability_check'
  ) THEN
    ALTER TABLE "public"."api_keys"
      ADD CONSTRAINT "api_keys_agent_capability_check"
      CHECK (
        "kind" <> 'agent'
        OR "auth_model" = 'capability'
      );
  END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- 3. Indexes: none added, on purpose.
-- ---------------------------------------------------------------------------
-- A GIN index on capabilities would serve a capability-predicate sweep ("revoke
-- every key holding site.files.read"). No such query exists in
-- db/query/api_keys.sql today; every current read path is by primary key, by the
-- unique prefix index, or a per-tenant list over a table bounded at tens of rows
-- per tenant. The index lands with the query that needs it, not before.
--
-- 4. RLS: no policy changes. api_keys already has ENABLE + FORCE ROW LEVEL
-- SECURITY, api_keys_tenant_isolation (USING + WITH CHECK on app.tenant_id) and
-- the SELECT-only api_keys_prefix_lookup gated on app.apikey_lookup='on'. Row
-- policies cover every column, so these columns are exactly as protected as
-- key_hash already is, and nothing a tenant can see has widened. No
-- api_keys_site_scope policy is added: see the header — api_keys has no site_id
-- and is not a site-keyed table, and the site boundary for these principals is
-- v2 and application-enforced today. No new GRANT is needed either; the
-- table-level grants to wpmgr_app from the original migration apply to columns
-- added later.
