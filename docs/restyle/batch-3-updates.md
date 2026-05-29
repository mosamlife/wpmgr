# Batch 3 (Updates) — Impeccable Redesign Plan

Status: planned (research complete via Plan agent 2026-05-29). Implement after the
lint-debt cleanup lands. This is the binding plan for the Updates surfaces.

## Acceptance criteria (derived — no standalone brief doc exists)
Sources of truth: `DESIGN.md` (binding spec) + parity with shipped exemplars
(Health tab, Backup detail) + Activity-feed density. Tracked in commit history,
not a per-batch doc.

1. Three Updates surfaces read at Activity-feed density and use Batch-0 shared
   primitives, not bespoke markup:
   - per-site available-updates panel
   - `/updates` run list
   - `/updates/$runId` run detail
2. Zero off-token colors (`impeccable detect` clean). Updates is the largest
   remaining offender: hardcoded `bg-green-100/text-green-800`, `bg-red-100`,
   `bg-amber-100`, `bg-blue-100`, `bg-green-500`, `bg-amber-500`, and literal
   glyphs `→ ↺ ✓ ✗ ↗ —` plus a hand-rolled spinner. All → tokens + primitives +
   lucide icons.
3. Verb-first labels, `font-mono` on every version/run-id/slug, `tabular-nums` on
   every count, no em-dashes in UI strings, status = dot+label+time, borders over
   shadows, no nested cards.
4. SSE live progress preserved verbatim (no wire-contract changes).

## NEW shared primitive (one)
- `apps/web/src/components/shared/version-arrow.tsx` — `VersionArrow`: mono
  `from → to` delta using lucide `ArrowRight` (not literal `→`), `tabular-nums`,
  muted-from / foreground-to. Dedupes the inline `VersionArrow`
  (available-updates-card.tsx:418) and `VersionDiff` (update-tasks-table.tsx:75).
  Will be reused by Errors/Security (Batch 4) for patched-in versions.

## Reuse (do NOT recreate)
- `UpdateChip` (`components/status/update-chip.tsx`) — aggregate "N updates" pill.
- `StatusChip` / `StatusDot` (`components/status/`) — per-row/task status;
  replaces `TASK_BADGE`/`RUN_BADGE` color maps.
- `LiveIndicator` (`components/shared/live-indicator.tsx`) — replaces the bespoke
  `LiveIndicator` in `$runId.tsx:176-217` and the `bg-amber-500/bg-green-500` dots.
  Map `RunStreamState`→`LiveState`: live→live, connecting→connecting,
  polling→connecting (label "Polling"), closed→idle.
- `PageHeader`+`backTo`, `DefinitionList`/`KvRow`, `FreshnessBadge`, `PageError`,
  `toast` — all exist.

## Files to modify
- `features/updates/update-status.tsx` — move `summarizeTasks` to new
  `features/updates/summarize.ts` (also clears its react-refresh warning).
  Replace `TASK_BADGE`/`RUN_BADGE` palette maps with `StatusChip`/`StatusDot`.
  Keep `TaskStatusBadge`/`RunStatusBadge` as thin wrappers or inline `StatusChip`.
- `features/updates/update-tasks-table.tsx` — move `siteNameMap` to `summarize.ts`
  (clears its react-refresh warning). Redesign to Activity-row density: token
  border, mono Target slug + `VersionArrow`, `StatusChip`, mono error, `tabular-nums`,
  drop literal `—`/`→`.
- `features/updates/available-updates-card.tsx` — largest rewrite. `CountBadge`→
  `UpdateChip` (major if core bump present else minor). `AsOf`→`FreshnessBadge
  collectedAt={data.as_of}`. inline `VersionArrow`→shared. `RowStateLine` spinner +
  `✓/✗/↺` glyphs → `LiveIndicator` + lucide `Check`/`X`/`RotateCcw` + `text-success`/
  `text-destructive`/`text-warning-subtle-fg`. inline `role="alert"`→`PageError`.
  Verb-first labels. `Notes ↗`/`Changelog ↗`→lucide `ExternalLink`. KEEP selection
  state, `BulkFooter`, the "take backup first" toggle + `lastHadCore` re-arm,
  and all hooks wiring.
- `routes/_authed/sites/$siteId.updates.tsx` — stays thin; renders redesigned card.
- `routes/_authed/updates/index.tsx` — redesign run list: project empty state (not
  dashed border), mono run-id link, `RunStatusBadge`→`StatusChip`, `tabular-nums`
  task count, `relativeTime` in `<time>`, inline error→`PageError`, optional
  `PageHeader` (no "Manage your …" copy — banned).
- `routes/_authed/updates/$runId.tsx` — `PageHeader` (mono `Run {id.slice(0,8)}`,
  `copyable={run.id}`, badges = `StatusChip` + Dry-run/Live + shared `LiveIndicator`
  from `streamState`, `backTo:/updates`). DELETE the local LiveIndicator copy.
  Progress: `Progress` + `DefinitionList` (`tabular:true`) via `summarizeTasks`.
  Tasks: redesigned `UpdateTasksTable`. inline error/not-found→`PageError` (keep
  `NotFoundError` branch).

## NEW helper file
- `features/updates/summarize.ts` — pure helpers moved out of component modules to
  clear react-refresh: `summarizeTasks`, `siteNameMap`. Import sites type from
  `@wpmgr/api`. Update `features/updates/use-updates.test.ts` import (don't change
  assertions).

## Data deps (reuse, no wire changes)
- Hooks: `useAvailableUpdates`, `useRefreshSiteUpdates`, `RefreshConflictError`;
  `useRowUpdate`, `useCoreRowUpdate`, `buildBulkBody`; `useUpdateRuns`,
  `useUpdateRun`, `useCreateUpdateRun`, `useRunEventStream`, `applyEvent`,
  `NotFoundError`, `RunStreamState`; `useSites`/`useSite`, `useMe`/`canOperate`.
- Query keys: `updatesKeys.{all,lists,list,detail}`,
  `availableUpdatesKeys.{all,detail}`. Keep success-path cache scrub in
  use-row-update.ts:159-172.
- API types: `UpdateRun`, `UpdateTask`, `UpdateItem`, `UpdateRunCreate`,
  `UpdateEvent`. Local `types.ts` (`SiteAvailableUpdates`, `AvailableUpdateItem`,
  `CoreUpdate`, `itemKey`, `CORE_KEY`) stays — availability endpoint not codegen'd.
- SSE: `/api/v1/updates/{runId}/events`, named `task` frames — preserve
  `addEventListener("task")` + `onmessage` fallback + 2s poll safety net.

## Risks
1. update-status.tsx / update-tasks-table.tsx are merge-hot (react-refresh export
   move). Batch 3 OWNS their fix via summarize.ts — the lint workflow was told to
   SKIP features/updates/*. No coordination conflict.
2. Preserve E2E selectors: `data-testid` `available-update-row`, `update-task-row`,
   `update-run-row`, and `data-state` on `RowFrame`.
3. "take backup first" is UI-only (console.info, not in wire schema) — do NOT
   promote to a body field.
4. Don't swap `available-updates` local types to `@wpmgr/api` — not codegen'd yet.
5. Deleting local LiveIndicator changes polling visual to `connecting` tone — pass
   an explicit `label="Polling"`.
6. Run `impeccable detect` on features/updates/ + the three routes as completion gate.

## Ordering for Batches 4-6
Batch 4 (Errors + Security) next — reuses SeverityChip + new VersionArrow, highest
operator value; Security after Errors (shared severity/CVE-mono conventions). Then
Batch 5 (Sites + Settings). Batch 6 (app shell + auth) last — shell already largely
shipped, auth lowest-churn.
