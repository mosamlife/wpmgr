# apps/web — WPMgr dashboard (React 19 + Vite)

Control-plane SPA for WPMgr. The app shell, routing, and data layer are wired
against the generated API client with **real session-based authentication
(M1)**: email+password and OIDC login, a `GET /auth/me` session, an API-keys
management page, and the Sites list/detail flow.

**M2** adds the site **enrollment** UX (one-time agent pairing codes) plus
expanded site metadata, health, and an installed-component (plugins/themes)
inventory.

## Stack

| Concern        | Choice                                                              |
| -------------- | ------------------------------------------------------------------- |
| Build / dev    | Vite 6 + React 19 + TypeScript (strict)                             |
| Router         | TanStack Router — **file-based** via `@tanstack/router-plugin/vite` |
| Server state   | TanStack Query v5 (REST; SSE later)                                 |
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
  (`button`, `input`, `label`, `card`, `table`, `badge`) under
  `src/components/ui/`.

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
  optimistic), and a table of installed plugins/themes. Loading / error /
  not-found states throughout.

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
