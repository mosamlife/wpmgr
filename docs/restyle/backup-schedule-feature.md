# Backup Schedule — Complete Feature (time + date queue)

Status: APPROVED for implementation (2026-05-30). Supersedes the half-baked
cadence-only schedule. API + web change only — **NO agent code change, NO agent
zip** (timezone is already shipped by the agent via diagnostics).

## User-approved decisions
1. **Timezone = the site's own WordPress timezone** (no operator override). The
   agent already reports it in the diagnostics *identity* category
   (`timezone` = IANA `timezone_string`, `gmt_offset` = float;
   `class-diagnostics-command.php:386-387`). The CP captures it at diagnostics
   ingest onto the `sites` row; the scheduler + UI consume it. Zero agent change.
2. **Frequency = hourly / every-N-hours / daily / weekly / monthly**, each with a
   time-of-day (and weekday / day-of-month where relevant).
3. **Retention = age + count, strictest wins** — `retention_days` (default 30)
   AND `keep_last` (default 7); prune to satisfy whichever is stricter;
   `monthly_archive_keep` stays for long-term archives. Per-schedule values must
   actually drive the GC (today they are dead config).
4. **Queue = materialized** `backup_schedule_runs` (mirrors `restore_runs`):
   pre-insert the single next `scheduled` row; compute the further 2–3 upcoming
   occurrences in the response; PAST = terminal rows in this table (captures
   skipped/failed fires that never produced a snapshot).
5. **Firing stays CP-side** (River periodic evaluator → signed backup command to
   the agent). The agent is a stateless push-target; no change.
6. **Missed-run = single catch-up** (fire once, advance once — no backfill storm).
   Add deterministic per-site jitter (0–15 min from site_id) at advance time.

## Why it's half-baked today (ground truth, file:line)
- `nextRun()` (`internal/backup/model.go:178`) only `AddDate(+1d/+7d/+1mo)` from
  *now* — no clock time, weekday, day-of-month, or timezone.
- Every PUT recomputes `next_run_at` (`service.go` PutSchedule + repo
  `UpsertSchedule` ON CONFLICT) → editing retention pushes the next run a full
  cadence away.
- `RunRetentionGC` (`gc.go:52,65`) uses server-wide `s.retentionDays` /
  `s.monthlyArchiveKeep`, ignoring the per-row columns the API stores/returns.
- No row links a scheduled snapshot to its schedule → no upcoming/past queue.
- Editor (`apps/web/src/features/backups/backup-schedule-editor.tsx`): no
  time/day/timezone controls; status line renders the literal "Next run in the
  future."; `<Checkbox {...register("enabled")} />` won't bind (Radix checkbox is
  not a native input — needs a Controller).
- Firing itself WORKS: River periodic (`cmd/wpmgr/main.go:832`, 5m, RunOnStart)
  → `ScheduleWorker.Work` (`worker.go:557`) → `DueSchedules` → `ListDueSchedules`
  → `EnqueueScheduledBackup` (`service.go:813`) → snapshot + `EnqueueBackup` →
  `BackupWorker` pushes the signed `POST /wpmgr/v1/command/backup`. Manual and
  scheduled are indistinguishable to the agent.

---

## Data model

### Migration: `apps/api/migrations/20260531030000_m17_backup_schedule.sql`
Next monotonic after `20260531020000_m16_restore_runs.sql`. Copy the
header/idempotency style of m16. Runs in ONE transaction (no `CONCURRENTLY`).

**A. Extend `backup_schedules`** (idempotent `ADD COLUMN IF NOT EXISTS`):
```
run_hour       smallint NOT NULL DEFAULT 2   CHECK (run_hour   BETWEEN 0 AND 23)
run_minute     smallint NOT NULL DEFAULT 0   CHECK (run_minute BETWEEN 0 AND 59)
day_of_week    smallint NULL                 CHECK (day_of_week  BETWEEN 0 AND 6)   -- 0=Sun..6=Sat (weekly)
day_of_month   smallint NULL                 CHECK (day_of_month BETWEEN 1 AND 28)  -- capped 28 (monthly)
frequency_hours smallint NULL                CHECK (frequency_hours BETWEEN 1 AND 24) -- used by every_n_hours
keep_last      integer  NOT NULL DEFAULT 7   CHECK (keep_last >= 0)
```
Keep the existing column NAME `cadence` (avoid sqlc/ogen rename churn) but widen
its meaning. Add CHECK `cadence IN ('hourly','every_n_hours','daily','weekly','monthly')`
and `kind IN ('files','db','full')` (guard each by constraint-name IF NOT EXISTS).
`next_run_at`/`last_run_at` columns unchanged. Reuse `backup_schedules_due_idx`.

**B. Extend `sites`** (idempotent): add
```
wp_timezone   text    NOT NULL DEFAULT ''   -- IANA name from diagnostics identity.timezone
wp_gmt_offset real    NOT NULL DEFAULT 0    -- fallback offset hours from identity.gmt_offset (e.g. 5.5)
```
(If `sites` has a different real name, discover it — the initial migration is
`20260527115454_initial.sql` / `20260527172114_m2_site_registry.sql`.)

**C. New table `backup_schedule_runs`** (mirror `restore_runs` column shape):
```
id            uuid PK DEFAULT gen_random_uuid()
tenant_id     uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE
site_id       uuid NOT NULL REFERENCES sites(id)   ON DELETE CASCADE
schedule_id   uuid NOT NULL REFERENCES backup_schedules(id) ON DELETE CASCADE
snapshot_id   uuid NULL     REFERENCES backup_snapshots(id) ON DELETE SET NULL
scheduled_for timestamptz NOT NULL
status        text NOT NULL DEFAULT 'scheduled'
                CHECK (status IN ('scheduled','queued','running','completed','failed','skipped','canceled'))
kind          text NOT NULL DEFAULT 'full'
error         text NULL
triggered_by  text NULL          -- 'schedule' for automatic fires
created_at    timestamptz NOT NULL DEFAULT now()
started_at    timestamptz NULL
finished_at   timestamptz NULL
updated_at    timestamptz NOT NULL DEFAULT now()
```
Indexes: `(tenant_id, site_id, scheduled_for DESC)`; `(status, scheduled_for)`;
`(schedule_id)`; **UNIQUE `(schedule_id, scheduled_for)`** (idempotent pre-insert).
RLS quartet copied VERBATIM from `restore_runs` in m16 EXCEPT the agent policy is
**`FOR ALL`** (the scheduler INSERTs/UPDATEs cross-tenant under `app.agent='on'`,
unlike restore_runs where the agent only reads):
- ENABLE + FORCE ROW LEVEL SECURITY
- `backup_schedule_runs_tenant_isolation` USING `tenant_id = current_setting('app.tenant_id')::uuid`
- `backup_schedule_runs_agent` FOR ALL USING `current_setting('app.agent', true) = 'on'`

### sqlc
Add to `apps/api/internal/db/queries/backups.sql`: thread the new
`backup_schedules` columns through the Get/Upsert/AdvanceBackupSchedule queries
and `ListDueBackupSchedules`. **Drop `next_run_at` from the Upsert ON CONFLICT DO
UPDATE set** (edit-reset fix — service decides when to recompute). Add
`sites.wp_timezone/wp_gmt_offset` to whatever site-info query the backup
scheduler reads (`GetBackupSiteInfo`). New file
`apps/api/internal/db/queries/schedule_runs.sql`: `UpsertScheduleRun` (ON CONFLICT
(schedule_id, scheduled_for)), `SetScheduleRunSnapshot`, `SetScheduleRunStatus`
(by id and by snapshot_id), `GetScheduleRun`, `ListScheduleRunsBySite`,
`ListUpcomingScheduleRuns`, `ListPastScheduleRuns`. Run `sqlc generate`.

---

## Go (package `internal/backup`)

### model.go
- Extend `Schedule`: `RunHour, RunMinute int32`; `DayOfWeek, DayOfMonth *int32`;
  `FrequencyHours *int32`; `Timezone string` (resolved site zone, for DTO display);
  `KeepLast int32`. (`Cadence string` stays; add the 2 new cadence consts
  `CadenceHourly = "hourly"`, `CadenceEveryNHours = "every_n_hours"`.)
- **Replace `nextRun`** with:
  `nextOccurrence(now time.Time, cadence string, hour, minute int, dow, dom, freqHours *int, loc *time.Location) time.Time`
  - resolve `loc` (see timezone resolution below); compute in `loc`, return `.UTC()`.
  - `hourly`: next `:minute` strictly after now (advance an hour if past).
  - `every_n_hours`: anchor the daily sequence at `hour:minute` in `loc`
    (00:00-relative steps of `freqHours`), return the next slot strictly after now.
  - `daily`: today `hour:minute` in loc; if ≤ now, +1 day.
  - `weekly`: next date whose weekday == `dow` at `hour:minute` (advance 7d if past).
  - `monthly`: `dom` (cap 28) at `hour:minute`; roll to next month if past.
  - Fold deterministic per-site jitter (0–15 min) IN at advance time so it persists
    in `next_run_at` (compute from `site_id` hash; pass site_id or a jitter int).
  - Truncate to the minute so the pre-inserted `scheduled_for` matches the value
    the next due-tick recomputes (idempotency of UNIQUE(schedule_id, scheduled_for)).
- `validateTimezone(name)` via `time.LoadLocation`; `validateSchedule` enforces
  cadence↔field consistency: `dow` required iff weekly, `dom` iff monthly,
  `freqHours` iff every_n_hours.
- **Timezone resolution helper** `resolveLocation(wpTimezone string, gmtOffset float64) *time.Location`:
  IANA `wpTimezone` non-empty → `LoadLocation` (DST-aware); else
  `time.FixedZone("wpoffset", int(gmtOffset*3600))` (handles +5:30, no DST — correct
  for WP manual offsets); else UTC. Unit-test DST spring-forward/fall-back + month-end + +5:30.

### service.go
- `PutScheduleInput` gains the new fields. `PutSchedule`: validate tz availability
  (resolve from the site's wp_timezone — NOT operator input) + day/freq consistency.
  **Only (re)compute `NextRunAt` when the row is new OR any timing field
  (cadence/run_hour/run_minute/day_of_week/day_of_month/frequency_hours) changed**;
  otherwise preserve existing `next_run_at` (load current row first). When timing
  changed, recompute AND replace the pending `scheduled` run row.
- `EnqueueScheduledBackup` (the scheduler hot path): in ONE `InTenantTx`:
  (a) `UpsertScheduleRun(schedule_id, scheduled_for=sched.NextRunAt)` → status `queued`;
  (b) create the pending snapshot, `SetScheduleRunSnapshot`;
  (c) `EnqueueBackup`;
  (d) advance `next_run_at = nextOccurrence(...)` (with jitter) AND pre-insert the
      next `scheduled` run row.
  Un-enrollable / no-recipient site → mark the run `skipped` (with error) and still
  advance (visible in history, no busy-loop).
- **Reconciliation**: where a snapshot reaches a terminal status (the snapshot
  finalize path the progress/worker pipeline already uses), call
  `SetScheduleRunStatus(by snapshot_id)` → running/completed/failed with
  started_at/finished_at. Find ALL terminal entry points (success, failure,
  watchdog-stall) so history never sticks on 'running'.

### gc.go
- `RunRetentionGC`: per site that has a schedule, prune scheduled snapshots to
  satisfy whichever is stricter of `retention_days` (age) and `keep_last` (count);
  keep `monthly_archive_keep` long-term archives separate. Replace the global
  `s.retentionDays`/`s.monthlyArchiveKeep` for scheduled sites.

### New files (clone restore_runs)
- `schedule_run_model.go` — `ScheduleRun` struct + status consts + terminal set
  `{completed,failed,skipped,canceled}` (mirror `restore_run_model.go`).
- `schedule_run_repo.go` — `ScheduleRunStore` interface + pgx repo; every
  tenant-scoped method in `pool.InTenantTx`; the scheduler's cross-tenant writes use
  the agent context like `ListDueSchedules`; `domain.NotFound` on `pgx.ErrNoRows`;
  nil→empty slice (mirror `restore_run_repo.go`).
- `schedule_run_handler.go` — clone `restore_run_handler.go`: `UserDirectory` reuse,
  `scheduleRunDTO` with nullable `*string triggered_by_email/_name`,
  `Register(r *gin.RouterGroup)` mounting:
  - `GET /sites/:siteId/schedule-runs?status=upcoming|past` (default: both, split)
  - `GET /schedule-runs/:runId`
  both `authz.RequirePermission(authz.PermSiteRead)`; rfc3339 `.UTC().Format`.

### Wiring
- `internal/server/server.go`: add `ScheduleRunH *backup.ScheduleRunHandler` to deps
  (next to `RestoreRunH`); `deps.ScheduleRunH.Register(v1)` in the feature-gated v1
  group (next to RestoreRunH).
- `cmd/wpmgr/main.go`: construct `scheduleRunH = backup.NewScheduleRunHandler(backupSvc)`
  next to `restoreRunH`; `scheduleRunH.SetUserDirectory(authSvc)`.

### Diagnostics timezone capture (`internal/diagnostics`)
- At ingest of the *identity* category, extract `timezone` (string) + `gmt_offset`
  (float) and `UPDATE sites SET wp_timezone=$1, wp_gmt_offset=$2 WHERE id=site`
  (runs under `app.tenant_id`, tenant-scoped — allowed). Tolerate missing/empty.
- Extend `GetBackupSiteInfo` (the backup site-info lookup) to return
  `WpTimezone string`, `WpGmtOffset float64` so the scheduler resolves the zone.

---

## OpenAPI + ogen (`packages/openapi/openapi.yaml`)
- Extend `BackupSchedule` and `BackupScheduleUpdate`: `run_hour` (int32 0-23),
  `run_minute` (int32 0-59), `day_of_week` (nullable int32 0-6), `day_of_month`
  (nullable int32 1-28), `frequency_hours` (nullable int32 1-24), `keep_last`
  (int32), and widen `cadence` enum to
  `hourly|every_n_hours|daily|weekly|monthly`.
- `BackupSchedule` RESPONSE also returns: `timezone` (string, resolved site zone,
  read-only), `gmt_offset` (number), and `next_runs` (array of rfc3339 strings —
  the next ~3 occurrences for the upcoming preview).
- Regenerate the full ogen tree (`apps/api/internal/api/gen/*`). Thread fields in
  `handler.go` `putSchedule` + `toAPISchedule` (incl computed `next_runs`,
  `timezone`, `gmt_offset`). Keep GET/PUT `/api/v1/sites/:siteId/backup-schedule`
  as the single config surface (ogen — do NOT fork to hand-rolled).
- Regenerate `packages/openapi-client` (`types.gen.ts`) for the web layer.

---

## Frontend (`apps/web`)
### use-backups.ts
`useBackupSchedule`/`usePutBackupSchedule` carry the new config fields + `timezone`,
`gmt_offset`, `next_runs`.

### backup-schedule-editor.tsx (rebuild)
- Cadence select: Hourly / Every N hours / Daily / Weekly / Monthly.
- Time-of-day control (`run_hour`:`run_minute`) — shown for daily/weekly/monthly
  (and as the anchor for every_n_hours).
- `frequency_hours` select shown via `useWatch` when cadence = every_n_hours.
- Day-of-week control when weekly; day-of-month (1–28) when monthly.
- `keep_last` number input beside `retention_days`.
- **Read-only** site-timezone line: "Times are in the site's timezone:
  {timezone} (UTC{±offset})" — NO operator picker.
- **Fix status**: render `next_run_at` as an absolute, site-timezone-aware time via
  `Intl.DateTimeFormat` (with `<time dateTime>` + ISO title) PLUS a forward-relative
  helper ("in 6h"). Remove reliance on `relativeTime()`'s "in the future" branch (or
  fix `utils.ts:relativeTime` to handle future instants). Surface `last_run_at`.
- Render a "Next 3 runs" preview from `next_runs[]`.
- Fix the enable toggle: use a `Controller` (or the proper Radix `onCheckedChange`),
  NOT `register()`, on the Switch/Checkbox.
- Build shared `components/ui` `Select` (and `Switch`) primitives to replace the raw
  `<select>` copies (here + `BackupNowControl`).

### use-schedule-runs.ts (clone use-restores.ts)
`client.get` against the hand-rolled routes; `scheduleKeys` query-key family;
TERMINAL set `{completed,failed,skipped,canceled}`; `refetchInterval=3000` while any
run is non-terminal else false; split upcoming (status `scheduled`,
`scheduled_for` > now) vs past (terminal, DESC).

### Queue view
A "Backup schedule runs" section in `backups-section.tsx` (sibling card to Restore
history): UPCOMING list (pre-inserted next run + `next_runs[]`, absolute + relative
times) and PAST history (StatusChip tone per status, `scheduled_for`, link to
`snapshot_id` when present, error on failure). Optional detail route
`apps/web/src/routes/_authed/schedule-runs/$runId.tsx` mirroring
`restores/$restoreId.tsx` (routeTree.gen.ts auto-regenerates). Reuse
PageHeader/DefinitionList/StatusChip/Card.

---

## Risks / invariants (must hold)
- Edit-reset fix must NOT break first-run (new row still computes next_run_at;
  timing-field change recomputes; non-timing edit preserves).
- `scheduled_for` computed deterministically (truncate to minute, fold jitter once)
  so pre-insert and the next due-tick agree → UNIQUE(schedule_id, scheduled_for)
  prevents duplicate/orphan rows across ticks + CP restarts.
- Agent RLS policy on `backup_schedule_runs` MUST be FOR ALL (scheduler writes
  cross-tenant), else materialization is silently blocked.
- nextOccurrence DST + month-end correctness (unit-tested).
- Reconciliation must cover every terminal snapshot path or history sticks on
  'running'.
- Single catch-up only (no backfill storm after CP downtime).

## Verify (post-build, before deploy)
`gofmt -l`, `go build ./...`, `go vet ./...`, `go test ./internal/backup/...`
(nextOccurrence table tests), `sqlc generate` clean, ogen regen diff reviewed,
`tsc --noEmit`. Smoke: PUT "daily 02:00" on an India (+5:30) site → next_run_at is
the correct UTC instant; editor shows it in site tz + next-3; editing retention does
NOT move next_run_at; scheduler fires; a `backup_schedule_runs` row goes
scheduled→queued→running→completed linked to its snapshot. Then version-bump
(API+web only) + Cloud Build deploy. NO agent zip.
