# Restore Runs + Logs — feature design (per-site, full persistence)

Decision: restore becomes a first-class persisted entity with a phase log, like
Update runs. Per-site placement. NO agent change (the agent already POSTs every
restore phase + message to /agent/v1/backups/{snapshotId}/progress; we persist
them). Migration: next number after m15 → m16.

## Problem today
Restore progress is overlaid on snapshot.progress (ephemeral, overwritten by the
next backup/restore). No restore history, no persisted logs, no restore entity.

## CP data model (m16)
- restore_runs(
    id uuid PK default gen_random_uuid(), tenant_id uuid, site_id uuid,
    snapshot_id uuid, mode text, components text[] NOT NULL DEFAULT '{}',
    selection jsonb NOT NULL DEFAULT '{}'  -- paths/db_tables/etc snapshot of the request,
    status text NOT NULL DEFAULT 'queued'  -- queued|running|completed|failed|rolled_back,
    current_phase text, error text, triggered_by text,
    created_at timestamptz default now(), started_at, finished_at,
    updated_at timestamptz default now())
  RLS: tenant-isolation + agent. idx(tenant_id, site_id, created_at DESC),
  idx(snapshot_id, status) for the active-run lookup.
- restore_run_events(
    id bigserial PK, tenant_id uuid,
    restore_run_id uuid → restore_runs(id) ON DELETE CASCADE,
    phase text, status text, message text, detail jsonb,
    occurred_at timestamptz default now())
  RLS. idx(restore_run_id, id).

## CP behavior wiring
- CreateRestore (internal/backup/service.go): after validation + enqueue, INSERT
  a restore_runs row (status=queued) capturing snapshot_id, mode, components,
  selection, and triggered_by (PrincipalFromContext login/id). Thread the
  restore_run_id into the River restore job args. Return the restore_run id in
  the create response so the FE can navigate to the detail.
- RestoreWorker (worker.go): on start → status=running, started_at; success →
  completed; failure → failed; always finished_at. (current_phase is driven by
  the agent via RecordProgress below.)
- RecordProgress (the /agent/v1/backups/{snapshotId}/progress handler path): it
  already writes snapshot.progress + publishes the SSE BackupEvent. ADD: look up
  the ACTIVE restore_run for snapshot_id (status IN (queued,running), most
  recent). If found AND the phase is a restore phase:
    * INSERT restore_run_events(phase, status, message=phase_detail.message,
      detail=phase_detail).
    * UPDATE restore_runs.current_phase=phase, updated_at=now(); if phase is a
      terminal restore phase (completed/failed/rolled_back) set status +
      finished_at.
  This makes every restore phase POST a durable log line, for free.

## CP endpoints (hand-rolled gin, mirror backup/handler.go + security/scan; NO ogen)
- GET /api/v1/sites/{siteId}/restores                 PermSiteRead  -> { items: RestoreRun[] }
- GET /api/v1/restores/{restoreId}                    PermSiteRead* -> RestoreRun  (by-id, standalone detail; RLS + site-permission on the run's site)
- GET /api/v1/restores/{restoreId}/events             PermSiteRead* -> { items: RestoreRunEvent[] } (ordered by id; supports ?after= for incremental)
RestoreRun = { id, site_id, snapshot_id, mode, components, status, current_phase,
  error, triggered_by, created_at, started_at, finished_at }
RestoreRunEvent = { id, phase, status, message, detail, occurred_at }
(*) by-id endpoints resolve the run's site_id, then enforce PermSiteRead on it.

## Live progress
Reuse the EXISTING snapshot SSE (/backups/{snapshotId}/events) for the live
stepper/card on the restore detail page (one snapshot has one active restore at a
time). The persisted restore_run_events power the durable scrollable log (poll
every few seconds while running; static once terminal). No new SSE channel.

## Web (per-site under Backups)
- features/backups/use-restores.ts: useRestoreRuns(siteId), useRestoreRun(runId),
  useRestoreEvents(runId, {refetchInterval while running}). Hand-rolled client.get
  (these are gin endpoints, not in @wpmgr/api) — mirror features/security/use-scan.ts.
- $siteId.backups.tsx: add a "Restore history" section under the snapshots — a
  list of recent restores (status chip, snapshot link, current/last phase,
  relativeTime started, triggered_by) each linking to the detail.
- NEW standalone route routes/_authed/restores/$restoreId.tsx (mirror the
  /backups/$snapshotId standalone detail): PageHeader (mono run id, backTo the
  site Backups tab), live SnapshotProgressCard while the source snapshot has an
  active restore phase (reuse isRestoreActive), the selection/outcome summary
  (DefinitionList), and a scrollable PHASE LOG built from restore_run_events
  (phase label + message + timestamp, mono, newest-last, auto-scroll while
  running, PageError on fetch fail).
- Link to the restore detail from: the backups-tab restore-history row, AND the
  "Restore requested"/in-flight banner on the snapshot detail page.

## Effort: CP M (migration + RecordProgress wiring is the crux), web M.
## Risks: active-run lookup must pick the single most-recent running restore for
## the snapshot; terminal-phase finalization must be idempotent; log volume is
## bounded (a restore has ~13 phases). triggered_by from the principal.
