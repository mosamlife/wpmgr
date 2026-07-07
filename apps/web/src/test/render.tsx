import type { ReactElement, ReactNode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, type RenderOptions } from "@testing-library/react";

// Shared render helper for component-render tests (Wave 4 outcome-test-debt,
// GH #170). Every domain feature test should import `renderWithProviders`
// from here rather than reaching for `@testing-library/react`'s bare
// `render` directly, so the provider stack stays in one place.
//
// Providers wired here:
//   - QueryClientProvider — REQUIRED. Every domain hook in `features/**`
//     (see `use-vuln.ts`, `use-backups.ts`, ...) is a TanStack Query hook;
//     rendering one outside a QueryClientProvider throws immediately.
//
// Providers intentionally NOT wired here:
//   - TanStack Router (RouterProvider) — most feature panels/dialogs take
//     their identifiers as props (siteId, snapshotId, ...) and never call
//     `useNavigate`/`useParams`/`<Link>` themselves, so they render fine with
//     no router context at all (this is true for both P0 targets: VulnPanel
//     and RestoreDialog). A component that DOES render `<Link>` (e.g.
//     `features/health/card-plugins.tsx`) needs a router context or a mock
//     of `@tanstack/react-router`'s `Link`/`useNavigate` — add that
//     per-test-file rather than forcing every render test through a router,
//     matching how the app itself only mounts the router-consuming layout
//     under `_authed/` (see routes/__root.tsx + lib/router.tsx).
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

export interface RenderWithProvidersOptions extends Omit<RenderOptions, "wrapper"> {
  /** Supply a pre-seeded QueryClient (e.g. to assert cache state after a
   *  mutation) instead of a fresh default one. */
  queryClient?: QueryClient;
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
  const { queryClient = createTestQueryClient(), ...rest } = options;

  function Providers({ children }: { children: ReactNode }) {
    return (
      <QueryClientProvider client={queryClient}>
        {children}
      </QueryClientProvider>
    );
  }

  return {
    queryClient,
    ...render(ui, { wrapper: Providers, ...rest }),
  };
}

// A test file imports `renderWithProviders` from here and everything else
// (`screen`, `fireEvent`, `waitFor`, ...) directly from
// "@testing-library/react" — see `vuln-panel.test.tsx` /
// `restore-dialog.test.tsx`. (A blanket `export *` here would trip
// `react-refresh/only-export-components` since ESLint can't verify a
// wildcard re-export contains only components, so we don't do that.)
