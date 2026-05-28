# apps/web — WPMgr dashboard (React 19 + Vite)

Control-plane SPA for WPMgr. The app shell, routing, and data layer are wired
against the generated API client with **real session-based authentication
(M1)**: email+password and OIDC login, a `GET /auth/me` session, an API-keys
management page, and the Sites list/detail flow.

**M2** adds the site **enrollment** UX (one-time agent pairing codes) plus
expanded site metadata, health, and an installed-component (plugins/themes)
inventory.

**M3** adds **bulk updates**: a wizard to update WordPress core / named
plugins / themes across many sites (selected by checkbox or by tag), a
dry-run preview, optional scheduling, and a run-detail page with **live
progress** over Server-Sent Events (with a polling fallback).

**M4** adds **backups, restore, and scheduling**: a Backups section on the site
detail page (snapshot list, "Back up now", and a schedule editor), a dedicated
snapshot-detail route with a manifest summary, and a destructive **Restore**
dialog (full / by-path / by-table). In-flight backups and restores are tracked
by polling until they reach a terminal state.

## Stack

| Concern        | Choice                                                              |
| -------------- | ------------------------------------------------------------------- |
| Build / dev    | Vite 6 + React 19 + TypeScript (strict)                             |
| Router         | TanStack Router — **file-based** via `@tanstack/router-plugin/vite` |
| Server state   | TanStack Query v5 (REST + SSE for live update progress)            |
| API client     | `@wpmgr/api` — generated from the OpenAPI spec with Hey API         |
| UI components  | shadcn/ui (Radix + Tailwind), manual setup                          |
| Styling        | **Tailwind v4** via `@tailwindcss/vite` (CSS-based config)          |
| Heavy tables   | `@tanstack/react-table` (headless)                                  |
| Forms          | react-hook-form + `@hookform/resolvers` + Zod 4                     |
| Auth / session | session cookie + TanStack Query (`GET /auth/me`) — **server state** |
| Client/UI state| Zustand (theme only) — **no server state here**                     |
| Dark mode      | class strategy (`.dark` on `<html>`), persisted                     |
| E2E            | Playwright (Chromium)                                               |

### Tailwind / shadcn notes

- **Tailwind v4** (the current shadcn-supported default). There is **no
  `tailwind.config.ts`** — v4 is configured in CSS. Design tokens live in
  `src/styles/globals.css` (`@import "tailwindcss"`, `@theme inline`, light +
  dark CSS variables).
- shadcn was set up **manually** (no interactive `init`): `components.json`, the
  `cn` util in `src/lib/utils.ts`, and the primitives actually used
  (`button`, `input`, `label`, `card`, `table`, `badge`, plus the M3
  `checkbox` and `progress` primitives) under `src/components/ui/`.

## Routes

- `/` → redirects to `/sites`
- `/login` — react-hook-form + Zod login form posting to `POST /auth/login`.
  Invalid credentials (401) render inline. A **Sign in with SSO** button does a
  full-page redirect to `/api/auth/oidc/login`; if OIDC is unconfigured the
  backend returns 501 and the user can navigate back (we don't probe config).
- `/register` — first-run bootstrap form (`POST /auth/register`). Creates the
  first user + tenant + owner; returns 403 once any user exists.
- `/settings/api-keys` — list/create/revoke tenant API keys (admin/owner only).
- `/sites` — sites list (TanStack Query + TanStack Table) with enrollment +
  health badges, a relative "last seen", tag chips, a **tag filter** (`?tag=`),
  and an **Add site** action (operator+). Empty state points to Add site.
- `/sites/$siteId` — site detail: metadata (WP/PHP/server/multisite/active
  theme/enrolled/last-seen/health), an editable **tags** section (PUT tags,
  optimistic), a table of installed plugins/themes, and a **Backups** section
  (M4: snapshot list, "Back up now", schedule editor). Loading / error /
  not-found states throughout.
- `/backups/$snapshotId` — snapshot detail: status/kind badges, a manifest
  summary (size, chunk count, entries, age recipient), an entries table, and a
  **Restore** action (operator+). Polls while a backup/restore is in flight.
- `/updates` — list of recent bulk-update runs (status, dry-run/live, task
  count, created, link to detail).
- `/updates/$runId` — run detail with a **live** task table (site, target,
  from→to version, color-coded status) and a progress summary. Subscribes to
  the SSE event stream; falls back to polling if SSE fails.

### Bulk updates (M3)

The bulk-update **wizard** (operator+) is launched from `/sites`:

1. Select one or more sites with the per-row checkboxes (a "Select all"
   header checkbox is provided), **or** apply a tag filter and choose "Update
   all tagged …". Both paths open the same wizard.
2. The wizard collects what to update — **WordPress core**, plugins/themes
   seeded from the selected sites' reported components (checkboxes), and/or
   extra plugin slugs typed free-form — each defaulting to version `latest`.
3. A **dry-run** toggle (default **on**) makes the first submit a safe preview;
   an optional **schedule** (`datetime-local`, sent as ISO `schedule_at`) defers
   the run. Submitting POSTs `/api/v1/updates` and navigates to the run detail.

The wizard is gated to operator/admin/owner via `canOperate()`; the backend
enforces the role regardless.

#### Live progress (SSE + cache reconciliation + fallback)

The run-detail page reconciles two sources into the **same** TanStack Query
cache entry (`["updates","detail",runId]`):

- An initial (and `useUpdateRun`) **GET `/updates/{runId}`** seeds the run + its
  tasks. `useCreateUpdateRun` also pre-seeds this entry from the POST response,
  so the detail page renders instantly after submit.
- `useRunEventStream` opens a browser **`EventSource`** against
  `/api/v1/updates/{runId}/events`. It is same-origin (Vite proxies `/api`), so
  the `wpmgr_session` cookie flows automatically (`withCredentials: true`) — the
  generated fetch client does not model SSE, hence the raw `EventSource`.
- Each `data:` line is a JSON **`UpdateEvent`**
  (`{ run_id, task_id, site_id, target_type, target_slug, status, from_version?,
  to_version?, detail?, run_status }`). On each event we **patch** the cached run
  in place (`applyEvent`): the matching task's status/versions are updated (or a
  new task appended), and the run-level `status` is set from `run_status`.
  Heartbeat/`:`-comment frames parse-fail and are ignored.
- The stream is closed on unmount and once `run_status === "completed"`.
- If the `EventSource` errors (proxy/unsupported), we close it and flip the
  transport state to **polling**, which enables `useUpdateRun({ poll: true })`
  to refetch the detail every 2s until the run completes. A small live/polling
  indicator reflects the current transport.

Task statuses are color-coded: succeeded = green, failed = red, rolled_back =
amber, running = blue (pulsing), skipped/pending = muted.

### Backups, restore & scheduling (M4)

The **Backups** section on `/sites/$siteId` (in `src/features/backups/`) covers
the whole snapshot lifecycle. The data hooks (`use-backups.ts`) wrap the
generated `createBackup` / `listBackups` / `getBackup` / `createRestore` /
`getBackupSchedule` / `putBackupSchedule` operations as TanStack Query hooks.

- **Snapshot list** — `useBackups(siteId)` lists snapshots with kind, a status
  badge (pending / running / completed / failed), size, chunk count, and
  relative created/finished times. The list **polls every 3s** while any
  snapshot is still in flight, so a freshly triggered backup advances without a
  manual reload. Each row links to the snapshot detail.
- **Back up now** (operator+) — a kind selector (full / files / db) plus a
  button that POSTs `/api/v1/sites/{siteId}/backups`; on success the list is
  invalidated so the new pending snapshot appears.
- **Backup schedule** (operator+) — `BackupScheduleEditor` GETs the current
  schedule (404 → render defaults) and PUTs changes via react-hook-form + Zod:
  enable toggle, cadence (daily / weekly / monthly), kind, rolling-window
  retention days, and a count of monthly archives to keep. The cadence/kind/
  retention fields are disabled when scheduling is off.

The **snapshot detail** route (`/backups/$snapshotId`) shows the manifest
summary and an entries table (path, file/db, table name, size, chunks). It
**polls every 2s** via `useBackup` while the snapshot (or an in-progress
restore) is pending/running, stopping at a terminal state.

The **Restore** dialog (operator+, opened from the snapshot detail) is
deliberately destructive-by-acknowledgement: it offers a **full** restore, a
partial restore **by file path**, or **by database table** (textarea, one entry
per line/comma-separated; known db tables from the manifest are listed as a
hint). The submit button stays disabled until the user ticks an explicit "this
overwrites the live site and cannot be undone" checkbox. Submitting POSTs
`/api/v1/backups/{snapshotId}/restore` (`{ full } | { paths } | { db_tables }`),
seeds the detail cache with the returned snapshot, and lets polling track the
restore to completion.

All backup actions are gated to operator/admin/owner via `canOperate()`; the
backend enforces the role regardless. Viewers see the snapshot list only.

### Site enrollment (pairing codes)

There is no manual "create site" form in the UI; sites are **enrolled** by the
WPMgr Agent plugin using a one-time pairing code:

1. On `/sites`, an operator+ clicks **Add site** and (optionally) supplies a
   site name and tags via a react-hook-form + Zod dialog.
2. The app POSTs `/api/v1/sites/pairing-codes` and shows the returned **code
   exactly once** in a dialog: copyable, with a "shown once" warning, a live
   **expiry countdown**, and install instructions (install the Agent plugin,
   enter the control-plane URL, paste the code).

The action is gated to operator/admin/owner via the active-tenant role from
`/auth/me` (`canOperate()`); the backend enforces the role regardless.

Health renders as a color-coded `Badge` (healthy = green, unreachable = red,
unknown = gray); enrollment as Enrolled / Pending. Components are flattened into
one table (name, type, version, active).

### Auth & the route guard

Auth is **server state**, so the session lives in TanStack Query (not Zustand),
per the ADRs. The single source of truth is `GET /auth/me`, authenticated by the
HttpOnly `wpmgr_session` cookie. The generated `@wpmgr/api` client is configured
with `credentials: "include"` (in `packages/openapi-client/src/client.config.ts`)
so the cookie flows on every request; there is no `X-Tenant-ID` header.

- `useMe()` reads `/auth/me`; a 401 resolves to `null` (not authenticated).
- `useLogin()` posts to `/auth/login`, seeds the `me` cache, and navigates to
  `/sites` (or the `?redirect=` target).
- `useLogout()` posts to `/auth/logout` and `queryClient.clear()`s all server
  state, then routes to `/login`.

The pathless `_authed` layout guard (`beforeLoad`) calls `ensureMe()` (a cached
`/auth/me` read via the router's QueryClient context). When unauthenticated it
`redirect`s to `/login` carrying the attempted URL in `?redirect=`. The header
shows the logged-in user, their active-tenant role, and a working logout; the
API-keys nav entry and the create/revoke controls are hidden for non-admins
(the backend enforces the role regardless).

## API client (`@wpmgr/api`)

`packages/openapi-client` generates a typed fetch client from
`packages/openapi/openapi.yaml` with Hey API (`@hey-api/openapi-ts`). The whole
fetch runtime is generated locally (no npm runtime dep). The generated tree
(`src/generated/**`) is committed and re-exported through a thin, swappable
facade in `packages/openapi-client/src/index.ts`.

In the app, `src/lib/api.ts` points the client `baseUrl` at `/api` (proxied to
the backend in dev via `vite.config.ts`). The Sites query hooks
(`src/features/sites/use-sites.ts`) call the generated `listSites` / `getSite` /
`deleteSite` / `createPairingCode` / `setSiteTags` operations and adapt them
into TanStack Query hooks: `useSites(tag?)` / `useSite(id)` / `useDeleteSite()`
/ `usePairingCode()` / `useSetSiteTags()` (optimistic). Server state stays in
TanStack Query — never Zustand.

The Updates hooks (`src/features/updates/use-updates.ts`) similarly wrap the
generated `createUpdateRun` / `listUpdateRuns` / `getUpdateRun` operations
(`useCreateUpdateRun()` / `useUpdateRuns()` / `useUpdateRun(id, { poll })`).
The SSE stream is **not** part of the generated client — it is consumed via the
browser `EventSource` in `useRunEventStream`, which patches `UpdateEvent` deltas
straight into the run-detail cache (see "Live progress" above).

The Backups hooks (`src/features/backups/use-backups.ts`) wrap the generated
`createBackup` / `listBackups` / `getBackup` / `createRestore` /
`getBackupSchedule` / `putBackupSchedule` operations as
`useCreateBackup(siteId)` / `useBackups(siteId)` / `useBackup(snapshotId)` /
`useCreateRestore(snapshotId)` / `useBackupSchedule(siteId)` /
`usePutBackupSchedule(siteId)`. The list and detail hooks use Query
`refetchInterval` to poll while a backup/restore is in flight.

Regenerate the client after the contract changes:

```bash
pnpm --filter @wpmgr/api generate
```

## Commands

Node 22 is required. All commands run from the repo root.

```bash
pnpm install                       # install workspace deps
pnpm --filter @wpmgr/api generate  # regenerate the API client
pnpm --filter @wpmgr/web dev       # dev server on :5173 (proxies /api)
pnpm --filter @wpmgr/web build     # tsc --noEmit && vite build
pnpm --filter @wpmgr/web typecheck # strict type check
pnpm --filter @wpmgr/web lint      # eslint
pnpm --filter @wpmgr/web e2e:install  # playwright install chromium
pnpm --filter @wpmgr/web e2e          # run Playwright smoke test
```
