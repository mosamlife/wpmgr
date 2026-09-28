-- m140 - add mcp_oauth_clients.registered_scopes WITH every existing row
-- already filled, ahead of m137.
--
-- WHAT IT DOES. When mcp_oauth_clients has no registered_scopes column yet,
-- this file adds it NOT NULL and fills every existing row with
-- ARRAY['mcp:read'] in the same statement, then drops the default so the column
-- ends with none. m137 then finds the column present, NOT NULL and filled, and
-- its own statements become no-ops apart from the three CHECK constraints it
-- adds, which every filled row satisfies.
--
-- WHY. m137 adds the column nullable, fills it with an UPDATE, and then arms
-- NOT NULL. On a table that already holds rows the UPDATE is not guaranteed to
-- reach them under the role that applies migrations in production, and the
-- NOT NULL then fails with 23502 and stops the boot. Filling the rows as part
-- of adding the column reaches every row regardless of the applying role.
--
-- THE VALUE IS m137's, UNCHANGED. ARRAY['mcp:read']::text[] is exactly what
-- m137's backfill writes for an existing client (m137 DECISION 7): the only
-- scope any client has ever been able to obtain, and the only member of the
-- vocabulary. It is a constant, not derived from any other row, so the column
-- default can carry it. Nothing is widened and nothing is narrowed.
--
-- NO DEFAULT SURVIVES THIS FILE. m137 DECISION 3 forbids a database default on
-- this column, so the default exists only for the length of the ADD COLUMN that
-- uses it to fill existing rows. Rows that existed keep the filled value after
-- the default is dropped; every future INSERT that omits the column still fails
-- with 23502, exactly as m137 intends.
--
-- END STATE IS m137's. text[] NOT NULL, no default, and (added by m137, not
-- here) the shape, present and vocabulary constraints. db/schema.sql already
-- describes that end state and is not changed.
--
-- ORDINAL. 20260907120000 sorts after m136 (20260907000000) and before m137
-- (20260908000000), which is the point: it must apply first. The label m140
-- follows m139, its sibling for m136; the ordinal is the order.
--
-- CONVERGE PATH.
--   * A database that has NOT applied m137 (every production database; m137 is
--     in no release): this file adds and fills the column, then m137 applies as
--     described above.
--   * A database that HAS applied m137 (CI and dev databases built from main):
--     the runner applies any unapplied version, including one that sorts before
--     versions already applied, so this file runs there too. The column already
--     exists, the probe below finds it, and the file changes nothing. It never
--     touches a column it did not add, so it cannot drop a default or rewrite a
--     value on a database that is already correct.
--
-- RLS. No table, no policy. mcp_oauth_clients keeps m124's ENABLE and FORCE ROW
-- LEVEL SECURITY and its two command-split policies; a column is covered by them
-- without a statement.

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_attribute
        WHERE attrelid = 'public.mcp_oauth_clients'::regclass
          AND attname  = 'registered_scopes'
          AND NOT attisdropped
    ) THEN
        ALTER TABLE "public"."mcp_oauth_clients"
            ADD COLUMN "registered_scopes" text[] NOT NULL
            DEFAULT ARRAY['mcp:read']::text[];

        ALTER TABLE "public"."mcp_oauth_clients"
            ALTER COLUMN "registered_scopes" DROP DEFAULT;
    END IF;
END;
$$;
