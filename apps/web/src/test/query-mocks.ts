import { vi } from "vitest";
import type { UseMutationResult, UseQueryResult } from "@tanstack/react-query";

// Shared helpers for mocking a domain data hook (`useSiteVulnerabilities`,
// `useCreateRestore`, ...) in a component-render test. See
// `src/test/render.tsx` for the render helper these pair with.
//
// TanStack Query's real `UseQueryResult`/`UseMutationResult` types are large
// discriminated unions (status/isPending/isSuccess/... all move together).
// Building an exact literal for every branch has no test value — what a
// render test actually cares about is the handful of fields the component
// reads (`data`, `isPending`, `isError`, `error`, `refetch`, `mutate`, ...).
// `mockQueryResult`/`mockMutationResult` fill in a safe "loaded, idle"
// default for everything else and cast the result through `unknown` — a
// deliberate, narrow cast at the mock boundary, not an unconstrained `any`
// leaking into test or app code (ADR: no `any`/`unknown` without narrowing).

/**
 * Builds a `UseQueryResult` for mocking a query hook. Defaults to a
 * successful, settled query (`isPending: false`, `isError: false`); pass
 * `{ isPending: true }` or `{ isError: true, error: new Error(...) }` to
 * simulate the other two states a page must render (see PageError /
 * skeleton conventions in CLAUDE.md).
 */
export function mockQueryResult<TData, TError = Error>(
  overrides: Partial<UseQueryResult<TData, TError>> & { data?: TData },
): UseQueryResult<TData, TError> {
  const base = {
    data: undefined,
    error: null,
    isPending: false,
    isLoading: false,
    isLoadingError: false,
    isRefetchError: false,
    isError: false,
    isSuccess: true,
    isFetching: false,
    isFetched: true,
    isFetchedAfterMount: true,
    isStale: false,
    isPlaceholderData: false,
    isRefetching: false,
    isInitialLoading: false,
    isPaused: false,
    status: "success",
    fetchStatus: "idle",
    dataUpdatedAt: Date.now(),
    errorUpdatedAt: 0,
    failureCount: 0,
    failureReason: null,
    errorUpdateCount: 0,
    refetch: vi.fn(),
    ...overrides,
  };
  return base as unknown as UseQueryResult<TData, TError>;
}

/**
 * Builds a `UseMutationResult` for mocking a mutation hook. Defaults to
 * idle (never called). Pass `{ mutate: vi.fn() }` / `{ mutateAsync:
 * vi.fn() }` to capture and assert on calls the component under test makes.
 *
 * `TContext` (the optimistic-update context type from a hook's `onMutate`,
 * e.g. `usePutAlertConfig`'s `{ previous: AlertConfig | null | undefined }`)
 * defaults to `unknown` so the common case — a mutation hook with no
 * explicit context type — needs no 4th type argument at the call site.
 */
export function mockMutationResult<
  TData,
  TVariables,
  TError = Error,
  TContext = unknown,
>(
  overrides: Partial<UseMutationResult<TData, TError, TVariables, TContext>>,
): UseMutationResult<TData, TError, TVariables, TContext> {
  const base = {
    mutate: vi.fn(),
    mutateAsync: vi.fn(),
    isPending: false,
    isError: false,
    isSuccess: false,
    isIdle: true,
    isPaused: false,
    status: "idle",
    data: undefined,
    error: null,
    variables: undefined,
    context: undefined,
    failureCount: 0,
    failureReason: null,
    submittedAt: 0,
    reset: vi.fn(),
    ...overrides,
  };
  return base as unknown as UseMutationResult<TData, TError, TVariables, TContext>;
}
