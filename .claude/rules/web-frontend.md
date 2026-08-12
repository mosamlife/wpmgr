---
paths:
  - "apps/web/**/*.ts"
  - "apps/web/**/*.tsx"
  - "apps/marketing/**/*.ts"
  - "apps/marketing/**/*.tsx"
  - "apps/tracker/**/*.ts"
  - "packages/**/*.ts"
---

# Web, marketing and tracker

## Assert against codes the server actually returns

A test here asserted three server error codes that did not exist. It passed, and
it tested nothing. Before asserting on a status, error code, field name or
shape, find it in the Go handler or the generated client and quote the file you
found it in. If it is not there, the assertion is fiction.

## The client is generated

```
go generate ./internal/api/gen/...
pnpm -C packages/openapi-client generate
```

Never hand-edit
`packages/openapi-client/src/generated/**` or `apps/web/src/routeTree.gen.ts`.
App code imports from `@wpmgr/api`, never from `./generated`; a new operation
must be added to the facade's re-export lists in
`packages/openapi-client/src/index.ts` or the app cannot see it.

A new route needs `pnpm -C apps/web build` to regenerate `routeTree.gen.ts`
(commit it) **and** a nav entry in `src/components/layout/sidebar.tsx`.
Typecheck alone catches neither.

## Units cross the wire raw

Two shipped bugs were unit errors at the display layer: a CLS scaled by a
thousand shown to clients, and Core Web Vitals axis units and thresholds that
disagreed with the metric. Confirm the unit at the producer, not at the chart.

## Live data is pulled, not pushed

Push is a hint; pull is the truth. Hydrate on mount, refetch on reconnect, keep
a stale backstop. Never pre-set `ConnectionEvent.ID`: UUIDs on the ULID-ordered
bus poisoned the tenant cursor and dropped every live event.

A new server event must be added to `SITE_EVENT_TYPES` **and** get its own
`addEventListener` in `features/sites/use-site-events.ts`. A type missing from
the zod enum is dropped by frame validation before any consumer sees it. Never
open an `EventSource` inside a queryFn; it belongs in a `useEffect` with
teardown.

## Concurrent edits to shared lists

Tri-state and bulk controls over a shared list need a lost-update guard.
Adversarial review caught this on site tags before it shipped.

## Route placement is load-bearing

Public flows (login, register, reset, verify, invite, legal) are top-level route
files with no guard. Anything tenant-scoped lives under `routes/_authed/`. A
reset page under `_authed/` dead-ends the flow; a tenant page outside it renders
for logged-out users and 403s on every call.

## Copy rules are CI-enforced

No em dashes, no en dashes, no competitor plugin names in shipped copy or
comments. `node apps/marketing/scripts/check-copy.mjs` runs in `ci.yml` over the
marketing site, `apps/agent/readme.txt` (the public wordpress.org listing page),
the plugin header and the agent's user-facing strings.

`apps/marketing` has its own `ci.yml` job (`JS (marketing)`: typecheck then
build) and its own gates: `typecheck`, `build`, `check-copy`, `impeccable`. Its
hero badge and `/changelog` are hand-maintained and name the **agent** version.

`apps/tracker` builds the RUM collector that ships inside the agent zip as
`assets/wpmgr-rum.js`; a change there is an agent-facing change.
