-- m117 - GH #414 Phase 1 of 5: the schema for "pause monitoring". SCHEMA ONLY.
--
-- THIS MIGRATION DELIBERATELY CHANGES NO BEHAVIOUR. It adds four columns, one
-- constraint and one index to sites. No worker reads them, no route writes
-- them, and every existing row lands with monitoring_paused_at = NULL, which is
-- "not paused". The risky half of this feature is teaching a dozen schedulers a
-- new predicate; that lands in phases 2-3 on top of a foundation already
-- reviewed. Nothing pauses when this applies. That is the point.
--
-- GH #414 was reported as "pause monitor is programmed to do what? Nothing
-- happens." It was correct: apps/web called a handler whose entire body was a
-- toast saying the feature lands in a sprint that never came. There was no
-- column, no worker predicate and no route. This is the column.
--
-- WHAT PAUSE WILL EVENTUALLY STOP (phases 2-3, NOT this migration)
--
--   uptime probes and their alerts, update inventory refresh, scheduled
--   security scans, vulnerability rescans and their alerts, screenshots.
--
-- WHAT PAUSE MUST NEVER STOP. Written here, in the schema, because the column
-- outlives every phase document and a later phase WILL be tempted:
--
--   * BACKUPS. Data protection is not monitoring. Someone pausing "monitoring"
--     before a migration may well assume everything stops; if backups stopped
--     silently that is the one failure nobody recovers from.
--   * The CONNECTION SWEEP (site.SweepArgs "site_connection_sweep",
--     site.HealthCheckArgs "site_health_check"). Stopping it would freeze a
--     paused site at connection_state 'connected' forever after its agent died.
--     Pause means "do not tell me", never "lie to me".
--   * RUM beacon ingestion, which is agent-pushed and has no server-side switch.
--   * Retention and cleanup jobs.
--   * Anything a person clicks. Pause governs the SCHEDULE, never the operator.
--
-- WHY A NEW COLUMN AND NOT A NEW connection_state
--
-- sites.connection_state is a state machine (ADR-041, internal/site/connection.go)
-- over pending_enrollment / connected / degraded / disconnected / revoked /
-- archived. Every one of those describes whether the AGENT IS REACHABLE. Pause
-- describes whether WE CHOOSE TO ACT. The two are orthogonal: a connected site
-- can be paused, and a paused site can lose its agent. Folding pause into that
-- enum makes both facts unrepresentable at once, and would also silently change
-- the meaning of every existing connection_state predicate in the codebase.
--
-- WHY monitoring_paused_at IS A TIMESTAMP AND NOT A BOOLEAN
--
-- One column carries both the flag and the since-when. NULL is active. A
-- boolean plus a separate paused_at can disagree; a single nullable timestamp
-- cannot. Every predicate a later phase writes is "monitoring_paused_at IS NULL".
--
-- ---------------------------------------------------------------------------
-- DECISION 1: the foreign key on monitoring_paused_by is ON DELETE SET NULL
-- ---------------------------------------------------------------------------
--
-- sites rows outlive users. The user who paused a site can be deleted (account
-- deletion, the superadmin orphan cleanup, an erasure request), and the site
-- and its pause must both survive that.
--
-- The three candidates, and why the other two are wrong here:
--
--   ON DELETE CASCADE   would DELETE THE SITE when the user who paused it is
--                       deleted. Catastrophic and absurd; not a real option,
--                       stated only because it is the schema default people
--                       reach for by muscle memory.
--   NO ACTION/RESTRICT  would make the pause a hard blocker on deleting a user,
--                       so an account deletion fails with 23503 until every
--                       site that user ever paused is resumed. That turns an
--                       attribution field into a lifecycle dependency.
--   SET NULL            keeps the pause and loses only the attribution. The
--                       badge falls back to "paused" with no name, which is
--                       exactly the right degradation: the operational fact
--                       (this site is paused) is load-bearing, the display fact
--                       (who did it) is not.
--
-- SET NULL is also this schema's settled convention for an actor column, and it
-- is unanimous. Every user-attribution FK already in the tree uses it:
--   update_runs.created_by            (m3)
--   invitations.revoked_by            (m20)
--   site_connection_events.actor_user_id (m21)
--   client_members.invited_by         (m66)
-- The only ON DELETE CASCADE references to users (id) are OWNERSHIP rows whose
-- entire reason to exist is the user: client_members.user_id (m66) and
-- user_identities.user_id (m110). This column is attribution, not ownership.
--
-- The FK targets users (id) alone and NOT a composite (id, tenant_id), because
-- users are tenant-agnostic in this schema: a user reaches a tenant through
-- memberships, and there is no users (id, tenant_id) key to reference. The
-- database therefore CANNOT enforce that the pauser is a member of the site's
-- tenant. The service layer must set this column from the AUTHENTICATED ACTOR
-- and never from request input. That is a note for the handler, not a gap this
-- migration can close.
--
-- ---------------------------------------------------------------------------
-- DECISION 2: no index on "monitoring_paused_at IS NULL"; one small partial
--             index on the auto-resume sweep instead
-- ---------------------------------------------------------------------------
--
-- The obvious index is the wrong one. Every scheduler will eventually filter
-- "monitoring_paused_at IS NULL", so a partial index on that predicate looks
-- indicated. It is not, for two independent reasons, both read off the existing
-- queries rather than guessed:
--
--   1. It would index almost the whole table. Paused is the rare state; NULL is
--      the overwhelming majority of rows. A partial index whose predicate
--      matches ~99% of rows is a full-size index wearing a WHERE clause. It
--      costs a write on essentially every sites UPDATE, and sites is the
--      hottest-written table in this schema: the heartbeat path writes
--      last_seen_at and missed_heartbeats per site per interval, and the
--      sweeper, the prober and the metadata push all UPDATE it.
--
--   2. No scheduler query could use it anyway. The fleet-wide enumerations are
--      unqualified full scans today, in db/query/sites.sql:
--        ListEnrolledSitesAllTenants   SELECT ... WHERE enrolled_at IS NOT NULL
--        ListEnrolledSitesForProbe     SELECT ... WHERE enrolled_at IS NOT NULL
--        ListConnectedSiteIDsForScreenshot
--                                      WHERE connection_state = 'connected'
--                                        AND enrolled_at IS NOT NULL
--      There is no index on enrolled_at at all, and these are deliberately
--      uncapped whole-fleet reads. Adding "AND monitoring_paused_at IS NULL" to
--      a query the planner already resolves with a sequential scan changes the
--      plan not at all: a filter that removes a small minority of rows from a
--      scan that must happen anyway is free at read time and does not justify
--      an index. Inferred from the query shapes and from the fact that this
--      codebase is content to enumerate the entire fleet every cycle, which is
--      itself the strongest available statement about fleet size; I did not
--      measure a production table, and phase 2 should re-check with EXPLAIN
--      against real data if the fleet has grown an order of magnitude.
--
-- The index that DOES earn its cost is the inverse, and it is tiny. The only
-- genuinely NEW scheduled query this design introduces is the auto-resume
-- sweep: "which sites are due to un-pause?", cross-tenant, on a tick. Its
-- predicate matches only rows that have both a live pause and a scheduled
-- resume, which is a rare subset of a rare subset. So the partial index below
-- is near-empty by construction, imposes write cost only on the handful of rows
-- that actually set an auto-resume, and turns that sweep from a whole-fleet
-- scan into an index range read. It is created now rather than in phase 2 so
-- the sweeper never ships against an unindexed predicate, and because
-- migrate.go runs each migration in one transaction and therefore cannot use
-- CREATE INDEX CONCURRENTLY; taking that brief lock now, on an empty predicate,
-- is cheaper than taking it later on a live one.
--
-- ---------------------------------------------------------------------------
-- DECISION 3: the resume-implies-pause CHECK is enforced in the SCHEMA
-- ---------------------------------------------------------------------------
--
--   CHECK (monitoring_resume_at IS NULL OR monitoring_paused_at IS NOT NULL)
--
-- A resume time with no pause is incoherent, and it is incoherent in a way that
-- a later phase reads as an instruction. The auto-resume sweeper's predicate is
-- "monitoring_resume_at <= now()"; a dangling resume time on a site that is not
-- paused is a row that sweeper will pick up and act on, clearing a pause that
-- does not exist and writing an audit entry for an event that never happened.
-- The failure mode is not a cosmetic inconsistency, it is a phantom state
-- transition, so the database refuses it.
--
-- Schema rather than service, for the reason m115 exists: m113 left a check
-- constraint open, the value it should have closed got written, and m115 had to
-- converge the databases afterwards. A service-layer check holds only for the
-- callers that remember it, and this feature will grow a resume route, an
-- auto-resume worker, a bulk action and (phase 5) an unpause-on-delete path -
-- four writers, of which three do not exist yet. The constraint is trivially
-- satisfiable: the resume path clears both columns in one UPDATE.
--
-- WHAT THE CONSTRAINT DELIBERATELY DOES NOT SAY, and why:
--
--   * It does not require monitoring_paused_by to be non-NULL while paused.
--     It CANNOT: the FK above is ON DELETE SET NULL, so deleting a user nulls
--     this column on a site that is still legitimately paused. A constraint
--     tying the two together would be violated by the foreign key's own
--     referential action, turning every user deletion into a 23514 on an
--     unrelated table. The two decisions are coupled and only this combination
--     is consistent.
--   * It does not require monitoring_paused_reason to be empty while active.
--     Stale reason text on an un-paused site is inert - nothing reads it unless
--     monitoring_paused_at is set - and forcing it into the constraint would
--     make a resume that clears the pause but not the text fail loudly for a
--     cosmetic reason. The resume path should clear it; the database does not
--     insist.
--   * It does not require monitoring_resume_at > monitoring_paused_at. A resume
--     instant in the past is not incoherent, it means "due immediately", which
--     is exactly how a "<= now()" sweeper reads it, and it is the natural
--     result of an operator editing an existing pause to end it on the next
--     tick. Rejecting it would refuse a sane request to satisfy a tidiness rule.
--
-- ---------------------------------------------------------------------------
-- RLS: nothing to add, and this was verified, not assumed
-- ---------------------------------------------------------------------------
--
-- Row-level security is ROW level. A policy admits or refuses a row, so every
-- column on an admitted row is covered by whatever policies the table already
-- carries; a new column on an existing table inherits them with no new policy
-- and no new grant. sites already carries, and this was read rather than
-- presumed:
--
--   ENABLE + FORCE ROW LEVEL SECURITY          (initial, 20260527115454, l.32-33)
--   sites_tenant_isolation  PERMISSIVE          (initial, l.34)
--   sites_enroll / sites_agent  PERMISSIVE      (m2)
--   sites_shared_read  PERMISSIVE SELECT        (m22)
--   sites_client_read  PERMISSIVE SELECT        (m66)
--   sites_site_scope  RESTRICTIVE FOR ALL       (m19, l.330)
--
-- FORCE is on, so the policies apply to the table owner too, which is the role
-- the application connects as. The RESTRICTIVE sites_site_scope policy is the
-- one m112's lesson is about, and sites has carried it since m19: a collaborator
-- scoped to one site cannot read another site's row, and therefore cannot read
-- another site's pause state or who set it. No policy is added here and none
-- needs to be. (Note for a reader who greps db/schema.sql: that file mentions
-- sites_site_scope only in two comments and does not contain the policy itself.
-- The migrations are authoritative for RLS; schema.sql is sqlc's input and lags.)
--
-- There is also nothing to GRANT. The grants on sites are table-level, and a
-- table-level GRANT covers columns added later; there are no column-level
-- grants anywhere on this table.
--
-- ---------------------------------------------------------------------------
-- AUDIT TRAIL: no schema change needed
-- ---------------------------------------------------------------------------
--
-- audit_log.action is plain text with no CHECK constraint and no enum, so the
-- pause/resume events need no migration at all: the handler writes its action
-- string, target_type 'site', target_id the site id, and puts the reason and
-- any resume instant in the existing metadata jsonb. Adding a constraint to
-- audit_log.action to accommodate this would be a strictly larger, riskier
-- change than the feature needs, and would put every future action kind behind
-- a migration.
--
-- ---------------------------------------------------------------------------
-- IDEMPOTENCE
-- ---------------------------------------------------------------------------
--
-- Fully idempotent, matching m107/m108/m103/m101, which is the convention for a
-- column addition here. ADD COLUMN IF NOT EXISTS for the columns; the constraint
-- and the index are wrapped in existence checks because Postgres 16 has no
-- IF NOT EXISTS for ADD CONSTRAINT and the m116 pg_indexes guard is the house
-- pattern for the index. Nothing is dropped, no existing row is written, no
-- backfill is required: the new columns' defaults ARE the correct value for
-- every existing site (NULL = active, '' = no reason), so there is no m110/m111
-- shaped backfill to get wrong. internal/db/migrate.go applies this on boot
-- inside main(), in one transaction, so a failure here is a control-plane
-- outage; every statement above is a no-op on second application.
--
-- CONVERGE PATH: none is required. No prior version of this migration has ever
-- been applied to any database - this is a new ordinal (20260819000000, after
-- m116's 20260818000000, which was the newest at the time of writing), it
-- corrects nothing, and it edits no applied file.

-- ---------------------------------------------------------------------------
-- The columns
-- ---------------------------------------------------------------------------

ALTER TABLE "public"."sites"
    -- NULL means monitoring is ACTIVE. Non-NULL means paused, and the value is
    -- the instant it was paused: the flag and the since-when in one column, so
    -- the two can never disagree. Every phase-2 scheduler predicate is
    -- "monitoring_paused_at IS NULL".
    ADD COLUMN IF NOT EXISTS "monitoring_paused_at"     timestamptz NULL,
    -- Who paused it, for the badge. Nullable by design and NOT only because of
    -- ON DELETE SET NULL: a pause set by an automated path has no user to name.
    ADD COLUMN IF NOT EXISTS "monitoring_paused_by"     uuid        NULL,
    -- Optional free text, shown on hover. NOT NULL DEFAULT '' rather than
    -- nullable text, matching every other optional text column on this table
    -- (wp_version, host_provider, age_recipient, ...), so readers never have to
    -- distinguish NULL from empty.
    ADD COLUMN IF NOT EXISTS "monitoring_paused_reason" text        NOT NULL DEFAULT '',
    -- Optional auto-resume instant. NULL means "paused until someone resumes
    -- it", which is the default and the common case.
    ADD COLUMN IF NOT EXISTS "monitoring_resume_at"     timestamptz NULL;

-- ---------------------------------------------------------------------------
-- The attribution foreign key. ON DELETE SET NULL - see DECISION 1.
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.sites'::regclass
          AND conname  = 'sites_monitoring_paused_by_fkey'
    ) THEN
        ALTER TABLE "public"."sites"
            ADD CONSTRAINT "sites_monitoring_paused_by_fkey"
            FOREIGN KEY ("monitoring_paused_by") REFERENCES "public"."users" ("id")
            ON UPDATE NO ACTION ON DELETE SET NULL;
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- The resume-implies-pause CHECK - see DECISION 3.
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.sites'::regclass
          AND conname  = 'sites_monitoring_resume_requires_pause_check'
    ) THEN
        ALTER TABLE "public"."sites"
            ADD CONSTRAINT "sites_monitoring_resume_requires_pause_check"
            CHECK ("monitoring_resume_at" IS NULL OR "monitoring_paused_at" IS NOT NULL);
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- The auto-resume sweep index - see DECISION 2. Deliberately NOT an index on
-- "monitoring_paused_at IS NULL".
--
-- THE "AND monitoring_paused_at IS NOT NULL" BELOW IS REDUNDANT AND MUST STAY.
--
-- sites_monitoring_resume_requires_pause_check already guarantees it: a row
-- with a resume instant always has a pause instant, so that clause selects no
-- row the first clause did not already select. It is not here to filter rows.
-- It is here to be REPEATED BY THE QUERY.
--
-- The planner proves a partial index usable from the query's own WHERE clauses
-- alone. It does not consult check constraints to discharge an index predicate,
-- so it cannot derive "paused_at IS NOT NULL" from the constraint, and a query
-- that omits that clause cannot use this index however logically implied it is.
-- Both halves of the predicate must therefore appear in both places, and the
-- consumer (claimDueAutoResumesSQL, internal/site/monitoring_resume_worker.go)
-- repeats both deliberately.
--
-- Measured on 200k sites (95 MB table, 300 due), EXPLAIN ANALYZE as wpmgr_app:
--
--   query repeats both clauses:  Index Scan on this index          13.8 ms
--   query filters resume_at only: Seq Scan, 199,700 rows removed  395.0 ms
--
-- A later phase adding a second sweep will naturally write
-- "WHERE monitoring_resume_at <= now()". That is correct, and it scans the
-- whole fleet. Anyone dropping the redundant clause from EITHER side gets a
-- slower plan and no error.
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND tablename = 'sites'
          AND indexname = 'sites_monitoring_resume_due_idx'
    ) THEN
        CREATE INDEX "sites_monitoring_resume_due_idx"
            ON "public"."sites" ("monitoring_resume_at")
            WHERE "monitoring_resume_at" IS NOT NULL
              AND "monitoring_paused_at" IS NOT NULL;
    END IF;
END;
$$;
