-- m139 - add mcp_grants.oauth_scopes WITH every existing row already filled,
-- ahead of m136.
--
-- WHAT IT DOES. When mcp_grants has no oauth_scopes column yet, this file adds
-- it NOT NULL and fills every existing row with ARRAY['mcp:read'] in the same
-- statement, then drops the default so the column ends with none. m136 then
-- finds the column present, NOT NULL and filled, and its own statements become
-- no-ops apart from the three CHECK constraints it adds, which every filled
-- row satisfies.
--
-- WHY. m136 adds the column nullable, fills it with an UPDATE, and then arms
-- NOT NULL. On a table that already holds rows the UPDATE is not guaranteed to
-- reach them under the role that applies migrations in production, and the
-- NOT NULL then fails with 23502 and stops the boot. Filling the rows as part
-- of adding the column reaches every row regardless of the applying role.
--
-- THE VALUE IS m136's, UNCHANGED. ARRAY['mcp:read']::text[] is exactly what
-- m136's backfill writes for an existing grant (m136 DECISION 6): the complete
-- scope set every grant already holds, and the only member of the vocabulary.
-- Nothing is widened and nothing is narrowed.
--
-- NO DEFAULT SURVIVES THIS FILE. m136 DECISION 3 forbids a database default on
-- this column, so the default exists only for the length of the ADD COLUMN that
-- uses it to fill existing rows. Rows that existed keep the filled value after
-- the default is dropped; every future INSERT that omits the column still fails
-- with 23502, exactly as m136 intends.
--
-- END STATE IS m136's. text[] NOT NULL, no default, and (added by m136, not
-- here) the shape, not-empty and vocabulary constraints. db/schema.sql already
-- describes that end state and is not changed.
--
-- ORDINAL. 20260906120000 sorts after m135 (20260906000000) and before m136
-- (20260907000000), which is the point: it must apply first. The label m139 is
-- the next free number (m138 is taken by an open branch at 20260909000000); the
-- label is a name, the ordinal is the order.
--
-- CONVERGE PATH.
--   * A database that has NOT applied m136 (every production database; m136 is
--     in no release): this file adds and fills the column, then m136 applies as
--     described above.
--   * A database that HAS applied m136 (CI and dev databases built from main):
--     the runner applies any unapplied version, including one that sorts before
--     versions already applied, so this file runs there too. The column already
--     exists, the probe below finds it, and the file changes nothing. It never
--     touches a column it did not add, so it cannot drop a default or rewrite a
--     value on a database that is already correct.
--
-- RLS. No table, no policy. mcp_grants keeps m124's ENABLE and FORCE ROW LEVEL
-- SECURITY and its five policies; a column is covered by them without a
-- statement.

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_attribute
        WHERE attrelid = 'public.mcp_grants'::regclass
          AND attname  = 'oauth_scopes'
          AND NOT attisdropped
    ) THEN
        ALTER TABLE "public"."mcp_grants"
            ADD COLUMN "oauth_scopes" text[] NOT NULL
            DEFAULT ARRAY['mcp:read']::text[];

        ALTER TABLE "public"."mcp_grants"
            ALTER COLUMN "oauth_scopes" DROP DEFAULT;
    END IF;
END;
$$;
