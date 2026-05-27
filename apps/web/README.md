# apps/web — WPMgr dashboard (React 19 + Vite)

Control-plane SPA for WPMgr. This is the **Phase 4 frontend skeleton**: the app
shell, routing, data layer, and a Sites list/detail flow wired against the
generated API client. Real authentication and the remaining feature domains
land in Phase 5 / M1.

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
| Client/UI state| Zustand (session stub + theme) — **no server state here**           |
| Dark mode      | class strategy (`.dark` on `<html>`), persisted                     |
| E2E            | Playwright (Chromium)                                               |

### Tailwind / shadcn notes

- **Tailwind v4** (the current shadcn-supported default). There is **no
  `tailwind.config.ts`** — v4 is configured in CSS. Design tokens live in
  `src/styles/globals.css` (`@import "tailwindcss"`, `@theme inline`, light +
  dark CSS variables).
- shadcn was set up **manually** (no interactive `init`): `components.json`, the
  `cn` util in `src/lib/utils.ts`, and the primitives actually used
  (`button`, `input`, `label`, `card`, `table`) under `src/components/ui/`.

## Routes

- `/` → redirects to `/sites`
- `/login` — react-hook-form + Zod login form. **Stub only:** sets a fake
  session in Zustand and routes to `/sites` (no auth endpoint exists yet).
- `/sites` — sites list (TanStack Query + TanStack Table); has a logout action.
- `/sites/$siteId` — site detail with loading / error / not-found states.

The pathless `_authed` layout guards all `/sites*` routes: an empty session
`beforeLoad`-redirects to `/login`.

## API client (`@wpmgr/api`)

`packages/openapi-client` generates a typed fetch client from
`packages/openapi/openapi.yaml` with Hey API (`@hey-api/openapi-ts`). The whole
fetch runtime is generated locally (no npm runtime dep). The generated tree
(`src/generated/**`) is committed and re-exported through a thin, swappable
facade in `packages/openapi-client/src/index.ts`.

In the app, `src/lib/api.ts` points the client `baseUrl` at `/api` (proxied to
the backend in dev via `vite.config.ts`). The Sites query hooks
(`src/features/sites/use-sites.ts`) call the generated `listSites` / `getSite` /
`deleteSite` operations and adapt them into TanStack Query
`useSites()` / `useSite(id)` / `useDeleteSite()` hooks.

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
