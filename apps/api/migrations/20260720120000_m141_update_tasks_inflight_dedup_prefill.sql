-- m141 - deduplicate in-flight update_tasks and create
-- update_tasks_inflight_target_idx, ahead of m88.
--
-- WHAT IT GUARANTEES. When update_tasks_inflight_target_idx does not exist,
-- this file marks every in-flight ('pending', 'running') task except the
-- newest per (tenant_id, site_id, target_type, target_slug) as 'skipped',
-- with m88's own UPDATE unchanged, then creates m88's unique index in the same
-- transaction, so no duplicate can be inserted between the two. m88 then finds
-- nothing to deduplicate and the index already present, and changes nothing.
--
-- WHY. update_tasks is FORCE ROW LEVEL SECURITY, and the production migrator
-- is a NOSUPERUSER NOBYPASSRLS table owner with no app.* setting in scope. For
-- that role m88's UPDATE reads no rows, so its CREATE UNIQUE INDEX fails on any
-- duplicate already present and the boot aborts. This file lifts FORCE for the
-- length of the deduplication, so the owner reads every row, and restores it
-- before it returns. row_security is off while FORCE is lifted: a statement a
-- policy would still filter raises an error instead of touching fewer rows.
--
-- ORDINAL. 20260720120000 sorts after m87 (20260720000000) and before m88
-- (20260721000000), so it applies first. The label m141 is a name; the ordinal
-- is the order.
--
-- LATE RUN. The runner applies every unapplied version, including one that
-- sorts before versions already applied, so this file also runs on every
-- database already past m88. There the index exists, the probe returns before
-- any statement takes a lock, and nothing changes. An existing unique index is
-- itself proof that no in-flight duplicate exists, so nothing is left to
-- converge.
--
-- END STATE IS m88's. update_tasks keeps ENABLE and FORCE ROW LEVEL SECURITY
-- and every policy; the check at the end refuses to commit otherwise.
-- db/schema.sql already describes this end state and is not changed.

DO $$
BEGIN
    IF to_regclass('public.update_tasks_inflight_target_idx') IS NOT NULL THEN
        RETURN;
    END IF;

    PERFORM set_config('lock_timeout', '5s', true);

    ALTER TABLE "public"."update_tasks" NO FORCE ROW LEVEL SECURITY;

    PERFORM set_config('row_security', 'off', true);

    UPDATE update_tasks u
    SET status = 'skipped', finished_at = now(), updated_at = now(),
        detail = 'skipped: superseded by a newer duplicate in-flight task for the same target (m88 dedup)'
    WHERE u.status IN ('pending', 'running')
      AND EXISTS (
          SELECT 1 FROM update_tasks v
          WHERE v.tenant_id = u.tenant_id
            AND v.site_id = u.site_id
            AND v.target_type = u.target_type
            AND v.target_slug = u.target_slug
            AND v.status IN ('pending', 'running')
            AND (v.created_at, v.id) > (u.created_at, u.id)
      );

    CREATE UNIQUE INDEX IF NOT EXISTS update_tasks_inflight_target_idx ON update_tasks
        (tenant_id, site_id, target_type, target_slug)
        WHERE status IN ('pending', 'running');

    PERFORM set_config('row_security', 'on', true);

    ALTER TABLE "public"."update_tasks" FORCE ROW LEVEL SECURITY;

    IF NOT EXISTS (
        SELECT 1 FROM pg_class
        WHERE oid = 'public.update_tasks'::regclass
          AND relrowsecurity
          AND relforcerowsecurity
    ) THEN
        RAISE EXCEPTION 'm141: update_tasks must end with ENABLE and FORCE ROW LEVEL SECURITY';
    END IF;
END;
$$;
