import type { ReactElement, ReactNode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, type RenderOptions } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";

// Shared render helper for component-render tests (Wave 4/5 outcome-test-debt,
// GH #170). Every domain feature test should import `renderWithProviders`
// from here rather than reaching for `@testing-library/react`'s bare
// `render` directly, so the provider stack stays in one place.
//
// Providers wired here:
//   - QueryClientProvider — REQUIRED. Every domain hook in `features/**`
//     (see `use-vuln.ts`, `use-backups.ts`, ...) is a TanStack Query hook;
//     rendering one outside a QueryClientProvider throws immediately.
//   - TanStack Router (RouterProvider) — OPT-IN via `{ withRouter: true }`.
//     Most feature panels/dialogs take their identifiers as props (siteId,
//     snapshotId, ...) and never call `useNavigate`/`useParams`/`<Link>`
//     themselves, so they render fine with no router context at all (this is
//     true for both P0 targets: VulnPanel and RestoreDialog) — leave
//     `withRouter` unset for those. A component that DOES render `<Link>` or
//     call `useNavigate()` (e.g. `features/health/card-plugins.tsx`,
//     `components/dialogs/upgrade-prompt.tsx`) throws immediately without a
//     router context (`Link`/`useNavigate` read it via React context), so
//     opt in with `{ withRouter: true }`.
//
//     The test router is a MINIMAL ad hoc tree (a bare root route whose
//     component renders exactly `ui`, mounted at `initialPath` via
//     `createMemoryHistory`) — never the app's real generated `routeTree`.
//     That is deliberate: the app's route tree drags in every route file's
//     loaders/`beforeLoad`s (including the `_authed` session guard), which
//     the render tests explicitly do NOT want to execute. This means
//     `<Link to="/settings/billing">` resolves to a real anchor with a real
//     `href` (so `getByRole("link", { name: ... })` assertions work exactly
//     like production) but actually clicking it won't match a real route —
//     fine, because no test here asserts on post-navigation content; they
//     only assert the link/nav affordance is present (or absent) and wired.
//     If a future test needs to assert ON a navigation outcome, extend
//     `initialPath`/add a second route rather than reaching for a full
//     app router.
//
//     GOTCHA: `RouterProvider`'s first paint is NOT synchronous — the router
//     resolves its initial match on mount (a microtask), so `ui` is not yet
//     in the DOM the instant `renderWithProviders(..., { withRouter: true })`
//     returns. Assert the FIRST thing you look for with `findBy*`/`waitFor`
//     (not a bare `getBy*`), exactly like an async data fetch; every
//     `getBy*`/`fireEvent` after that first resolved assertion is safe to use
//     synchronously as usual.
//

// Providers intentionally NOT wired here:
//   - Theme provider — the app reflects theme via a `.dark` class on
//     `<html>` (lib/theme-store.ts) driven by a Zustand store, not a React
//     context provider, so there is nothing to wrap. Tests that care about
//     dark-mode-specific class assertions can toggle
//     `document.documentElement.classList` directly.
//
// Server state is mocked at the HOOK boundary (`vi.mock("./use-x")`), never
// via a real network layer (no MSW): this matches the rest of the suite's
// pattern of testing hooks (`use-vuln.test.ts`, etc.) and components
// (`admin-account-ui.test.tsx`) as two separate concerns. A render test
// should mock the feature's `use-*` hook module and assert on the resulting
// DOM, exactly like `vuln-panel.test.tsx` / `restore-dialog.test.tsx`.

/**
 * A fresh QueryClient per render. Retries are disabled (a render test should
 * never hang retrying a mocked rejection) and `gcTime: Infinity` avoids
 * background GC timers outliving the test.
 */
export function createTestQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        retry: false,
        staleTime: 30_000,
        refetchOnWindowFocus: false,
        gcTime: Infinity,
      },
      mutations: {
        retry: false,
      },
    },
  });
}

/**
 * Builds a minimal ad hoc memory router whose ENTIRE tree is a single root
 * route rendering `ui` verbatim — see the module doc above for why this is
 * deliberately not the app's real `routeTree.gen.ts`.
 */
function buildTestRouter(ui: ReactElement, initialPath: string) {
  const rootRoute = createRootRoute({
    component: () => ui,
  });
  return createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: [initialPath] }),
  });
}

export interface RenderWithProvidersOptions extends Omit<RenderOptions, "wrapper"> {
  /** Supply a pre-seeded QueryClient (e.g. to assert cache state after a
   *  mutation) instead of a fresh default one. */
  queryClient?: QueryClient;
  /** Mount `ui` under a TanStack Router memory context — see module doc. */
  withRouter?: boolean;
  /** Starting location for the test router. Defaults to "/". Only used when
   *  `withRouter` is true. */
  initialPath?: string;
}

export interface RenderWithProvidersResult extends ReturnType<typeof render> {
  queryClient: QueryClient;
}

/**
 * Renders `ui` inside the app's required provider stack for a plain
 * component/hook-consumer test (see module doc above for exactly what is,
 * and is not, wired). Returns everything `@testing-library/react`'s
 * `render` returns, plus the `queryClient` instance in case the test wants
 * to assert on cache state directly.
 */
export function renderWithProviders(
  ui: ReactElement,
  options: RenderWithProvidersOptions = {},
): RenderWithProvidersResult {
  const {
    queryClient = createTestQueryClient(),
    withRouter = false,
    initialPath = "/",
    ...rest
  } = options;

  const content = withRouter ? (
    <RouterProvider router={buildTestRouter(ui, initialPath)} />
  ) : (
    ui
  );

  function Providers({ children }: { children: ReactNode }) {
    return (
      <QueryClientProvider client={queryClient}>
        {children}
      </QueryClientProvider>
    );
  }

  return {
    queryClient,
    ...render(content, { wrapper: Providers, ...rest }),
  };
}

// A test file imports `renderWithProviders` from here and everything else
// (`screen`, `fireEvent`, `waitFor`, ...) directly from
// "@testing-library/react" — see `vuln-panel.test.tsx` /
// `restore-dialog.test.tsx`. (A blanket `export *` here would trip
// `react-refresh/only-export-components` since ESLint can't verify a
// wildcard re-export contains only components, so we don't do that.)
