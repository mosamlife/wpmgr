-- m115: close the site_object_reclaim.kind set on databases that already have
-- the table.
--
-- WHY THE CONSTRAINT EXISTS AT ALL
--
-- kind was free text. The worker knows exactly one value, 'backup_manifest',
-- and cannot derive a storage prefix for anything else, so a row carrying any
-- other value reclaims nothing. That would be a curiosity if rows only ever
-- arrived from DELETE /sites/{id}, which writes a code constant. They do not:
-- the m113 header and the CHANGELOG both tell an operator to hand a
-- known-deleted site to the sweep with a hand-written INSERT, because that is
-- the only way to clear the objects orphaned BEFORE m113 existed, and the
-- account that reported GH #402 has 90 of them. A typo in that statement
-- produced a task that could never run, against objects that have no other
-- record anywhere. The remedy for the bug delivered the bug.
--
-- Refusing the row is the fix that lands where the mistake is made. The
-- operator gets an error naming this constraint, on the statement they are
-- looking at, and retypes it. The alternatives were considered and rejected:
-- an enum makes adding the next kind an ALTER TYPE with its own transaction
-- rules and changes the generated Go type for no extra safety, and relying only
-- on the worker means the bad row is accepted, sits there, and needs somebody
-- to go and read a table nobody reads.
--
-- WHY THIS IS A NEW FILE AND NOT AN EDIT TO m113 OR m114
--
-- internal/db/migrate.go skips any version already present in
-- schema_migrations, so a database that has run m113 will never read m113 again
-- however that file is edited, and the same is now true of m114. m114 says this
-- in its own header and exists because the first attempt at that fix ignored
-- it. Folding this constraint into either file would repeat exactly the mistake
-- m114 was written to document: a corrective statement placed where the thing
-- it corrects cannot reach it. So it gets a version no database has applied.
--
-- m113 as it now stands carries the constraint inline, so a FRESH database
-- arrives here with it already present and this migration does nothing.
--
-- WHY NOT VALID
--
-- ADD CONSTRAINT normally validates every existing row and fails if one does
-- not pass. migrate.go applies these on boot and a failure takes the control
-- plane down, so on a database already holding a bad row that would be a
-- boot-blocking migration in the name of preventing bad rows. NOT VALID skips
-- the scan of what is already there and enforces on every INSERT and UPDATE
-- from this point on, which is the half that matters: the door is shut. It also
-- takes a weaker lock and does not read the table.
--
-- Deliberately NOT followed by VALIDATE CONSTRAINT, for the same reason. A row
-- that predates this file is exactly the row nobody can afford to lose: it
-- names objects that are otherwise unnamed. It stays, and internal/backup's
-- reclaim worker treats an unknown kind as a retryable failure rather than a
-- cancel, so it stays visible in the every-tick stuck report with the bad value
-- in the log line, which is what an operator needs in order to correct it.
--
-- Which databases need this: any that ran the GH #402 branch before this
-- round, so developer machines and preview environments. No released version
-- carries m113 at all. It must therefore be safe on a database that needs
-- nothing, which is every production one, and it is: guarded on the table
-- existing and on the constraint being absent, so it is re-runnable forever and
-- does nothing at all twice.

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.tables
        WHERE table_schema = 'public' AND table_name = 'site_object_reclaim'
    ) AND NOT EXISTS (
        SELECT 1 FROM pg_constraint c
        JOIN pg_class t ON t.oid = c.conrelid
        JOIN pg_namespace n ON n.oid = t.relnamespace
        WHERE n.nspname = 'public'
          AND t.relname = 'site_object_reclaim'
          AND c.conname = 'site_object_reclaim_kind_check'
    ) THEN
        ALTER TABLE "public"."site_object_reclaim"
            ADD CONSTRAINT "site_object_reclaim_kind_check"
            CHECK ("kind" IN ('backup_manifest')) NOT VALID;
    END IF;
END;
$$;
