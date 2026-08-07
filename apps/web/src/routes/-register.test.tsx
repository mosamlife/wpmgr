import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRouteWithContext,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import type { UseMutationResult } from "@tanstack/react-query";
import type { Me, RegisterRequest } from "@wpmgr/api";

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import { mockMutationResult } from "@/test/query-mocks";
import { authKeys } from "@/features/auth/use-auth";
import type { RouterContext } from "@/router";

import { Route as RegisterRoute } from "./register";
import {
  useRegister,
  useResendVerification,
  type RegisterResult,
} from "@/features/auth/use-auth";
import { readPendingPlan } from "@/features/billing/pending-plan";

// M16 Phase C2 — signup-to-premium, the /register half: a `?plan=` carried
// from the marketing pricing page threads through to the register call, the
// same-browser stash, and (on the rare first-account bootstrap path) a
// direct redirect into checkout. Mounts the REAL route component (`Route`'s
// own `component`/`validateSearch`/`beforeLoad`) re-attached to a throwaway
// root — same pattern as routes/_authed/settings/-billing.test.tsx and
// routes/_authed/admin/accounts/-index.test.tsx (see those files' module
// docs for why: `Route.useSearch()`/`useNavigate()` are bound to this file's
// own exported `Route` singleton).

vi.mock("@/features/auth/use-auth", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/auth/use-auth")>();
  return { ...actual, useRegister: vi.fn(), useResendVerification: vi.fn() };
});

const mockedUseRegister = vi.mocked(useRegister);
const mockedUseResendVerification = vi.mocked(useResendVerification);

const MINIMAL_ME: Me = {
  user: {
    id: "00000000-0000-0000-0000-000000000001",
    email: "new@wpmgr.test",
    name: "New Owner",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  },
  memberships: [
    {
      user_id: "00000000-0000-0000-0000-000000000001",
      tenant_id: "11111111-1111-1111-1111-111111111111",
      role: "owner",
    },
  ],
  active_tenant_id: "11111111-1111-1111-1111-111111111111",
};

/**
 * Attaches register.tsx's own exported `Route` to a throwaway root, plus a
 * `/sites` and `/welcome/checkout` stub so a real `navigate({ to: ... })`
 * resolves and its search params are observable via `router.state.location`.
 */
function buildRegisterRouter(
  initialPath: string,
  queryClient: ReturnType<typeof createTestQueryClient>,
) {
  const rootRoute = createRootRouteWithContext<RouterContext>()({});
  type UpdateOptions = Parameters<typeof RegisterRoute.update>[0];
  const registerRoute = RegisterRoute.update({
    id: "/register",
    path: "/register",
    getParentRoute: () => rootRoute,
  } as unknown as UpdateOptions);
  const sitesRoute = createRoute({
    path: "/sites",
    getParentRoute: () => rootRoute,
    component: () => <div>Sites stub</div>,
  });
  const welcomeCheckoutRoute = createRoute({
    path: "/welcome/checkout",
    getParentRoute: () => rootRoute,
    validateSearch: (search: Record<string, unknown>) => search,
    component: () => <div>Welcome checkout stub</div>,
  });
  const routeTree = rootRoute.addChildren([registerRoute, sitesRoute, welcomeCheckoutRoute]);
  return createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [initialPath] }),
    context: { queryClient },
  });
}

function renderRegisterPage(initialPath = "/register") {
  const queryClient = createTestQueryClient();
  // Unauthenticated — register.tsx's own beforeLoad redirects to /sites when
  // `ensureMe` resolves a real session, so seed `null` (the "no session"
  // convention fetchMe uses on a 401) to keep the register form reachable.
  queryClient.setQueryData(authKeys.me, null);
  const router = buildRegisterRouter(initialPath, queryClient);
  renderWithProviders(<RouterProvider router={router} />, { queryClient });
  return router;
}

/** A `mutateAsync` spy that resolves with `result`, WITHOUT firing onSuccess. */
function mutateAsyncResolving<TData>(
  result: TData,
): UseMutationResult<TData, Error, RegisterRequest>["mutateAsync"] {
  return vi.fn().mockResolvedValue(result);
}

/** A `mutateAsync` spy that resolves with `result` AND fires the caller's `onSuccess`. */
function mutateAsyncFiringOnSuccess<TData>(
  result: TData,
): UseMutationResult<TData, Error, RegisterRequest>["mutateAsync"] {
  return vi.fn(
    (_vars: RegisterRequest, opts?: { onSuccess?: (result: TData) => void }) => {
      opts?.onSuccess?.(result);
      return Promise.resolve(result);
    },
  ) as unknown as UseMutationResult<TData, Error, RegisterRequest>["mutateAsync"];
}

/**
 * Fills and submits the registration form. Awaits the email field first —
 * `RouterProvider`'s first paint resolves in a microtask (see
 * src/test/render.tsx's module doc), so a bare `getByLabelText` right after
 * `renderRegisterPage` races an empty DOM.
 */
async function fillAndSubmit(email = "new@wpmgr.test", password = "supersecretpassword") {
  const emailInput = await screen.findByLabelText("Email");
  fireEvent.change(emailInput, { target: { value: email } });
  fireEvent.change(screen.getByLabelText("Password"), { target: { value: password } });
  fireEvent.click(screen.getByRole("button", { name: "Create account" }));
}

beforeEach(() => {
  vi.clearAllMocks();
  window.localStorage.clear();
  document.cookie = "wpmgr_pending_plan=; max-age=0; path=/";
  mockedUseResendVerification.mockReturnValue(
    mockMutationResult<void, { email: string }>({}),
  );
});

describe("RegisterPage — the form asks for two things", () => {
  // 2.8. The form used to also ask for a display name, an organization name,
  // and an organization slug, all optional, all at the point of highest
  // drop-off. These two tests are what stops them coming back: one pins what
  // the visitor sees, the other pins what the API is sent, because a field
  // could be removed from the markup while a stale default still rides along
  // in the request body.
  it("renders exactly the email and password fields", async () => {
    mockedUseRegister.mockReturnValue(
      mockMutationResult<RegisterResult, RegisterRequest>({
        mutateAsync: mutateAsyncResolving<RegisterResult>({ pending: true }),
      }),
    );

    renderRegisterPage("/register");
    await screen.findByLabelText("Email");

    const form = screen.getByRole("button", { name: "Create account" }).closest("form");
    const inputs = [...(form?.querySelectorAll("input") ?? [])];
    expect(inputs.map((i) => i.id)).toEqual(["email", "password"]);

    for (const gone of ["Your name (optional)", "Organization name (optional)", "Organization slug (optional)"]) {
      expect(screen.queryByLabelText(gone)).toBeNull();
    }
  });

  it("sends only email, password and plan, never a name or tenant field", async () => {
    // Declared with an explicit signature rather than via mutateAsyncResolving
    // so `.mock.calls` carries a real type: this assertion reads the request
    // body back, which the objectContaining style used elsewhere in this file
    // cannot do (it would pass with extra fields present, which is the exact
    // regression being guarded against).
    const mutateAsyncMock = vi
      .fn<(vars: RegisterRequest, opts?: unknown) => Promise<RegisterResult>>()
      .mockResolvedValue({ pending: true });
    mockedUseRegister.mockReturnValue(
      mockMutationResult<RegisterResult, RegisterRequest>({
        mutateAsync: mutateAsyncMock as unknown as UseMutationResult<
          RegisterResult,
          Error,
          RegisterRequest
        >["mutateAsync"],
      }),
    );

    renderRegisterPage("/register");
    await fillAndSubmit();

    await waitFor(() => expect(mutateAsyncMock).toHaveBeenCalledTimes(1));
    const body = mutateAsyncMock.mock.calls[0]![0];
    expect(Object.keys(body).sort()).toEqual(["email", "password", "plan"]);
  });
});

describe("RegisterPage — plan summary chip", () => {
  it("shows the Agency plan chip when the URL carries ?plan=agency", async () => {
    mockedUseRegister.mockReturnValue(
      mockMutationResult<RegisterResult, RegisterRequest>({
        mutateAsync: mutateAsyncResolving<RegisterResult>({ pending: true }),
      }),
    );

    renderRegisterPage("/register?plan=agency");

    expect(
      await screen.findByText(/You're signing up for the Agency plan/),
    ).toBeInTheDocument();
  });

  it("shows no chip and no plan field for a plain signup (no ?plan=)", async () => {
    mockedUseRegister.mockReturnValue(
      mockMutationResult<RegisterResult, RegisterRequest>({
        mutateAsync: mutateAsyncResolving<RegisterResult>({ pending: true }),
      }),
    );

    renderRegisterPage("/register");

    expect(await screen.findByRole("heading", { name: "Create an account" })).toBeInTheDocument();
    expect(screen.queryByText(/signing up for the/)).not.toBeInTheDocument();
  });

  it("ignores an unknown/free plan value rather than erroring the page (?plan=free)", async () => {
    mockedUseRegister.mockReturnValue(
      mockMutationResult<RegisterResult, RegisterRequest>({
        mutateAsync: mutateAsyncResolving<RegisterResult>({ pending: true }),
      }),
    );

    renderRegisterPage("/register?plan=free");

    expect(await screen.findByRole("heading", { name: "Create an account" })).toBeInTheDocument();
    expect(screen.queryByText(/signing up for the/)).not.toBeInTheDocument();
  });
});

describe("RegisterPage — threads plan into the register call", () => {
  it("sends plan: 'agency' in the register body when ?plan=agency", async () => {
    const mutateAsyncMock = mutateAsyncResolving<RegisterResult>({ pending: true });
    mockedUseRegister.mockReturnValue(
      mockMutationResult<RegisterResult, RegisterRequest>({ mutateAsync: mutateAsyncMock }),
    );

    renderRegisterPage("/register?plan=agency");
    await screen.findByText(/You're signing up for the Agency plan/);
    await fillAndSubmit();

    await waitFor(() => expect(mutateAsyncMock).toHaveBeenCalledTimes(1));
    expect(mutateAsyncMock).toHaveBeenCalledWith(
      expect.objectContaining({
        email: "new@wpmgr.test",
        plan: "agency",
      }),
      expect.anything(),
    );
  });

  it("omits plan from the register body for a plain signup", async () => {
    const mutateAsyncMock = mutateAsyncResolving<RegisterResult>({ pending: true });
    mockedUseRegister.mockReturnValue(
      mockMutationResult<RegisterResult, RegisterRequest>({ mutateAsync: mutateAsyncMock }),
    );

    renderRegisterPage("/register");
    await fillAndSubmit();

    await waitFor(() => expect(mutateAsyncMock).toHaveBeenCalledTimes(1));
    expect(mutateAsyncMock).toHaveBeenCalledWith(
      expect.objectContaining({ plan: undefined }),
      expect.anything(),
    );
  });
});

describe("RegisterPage — pending branch stashes the plan (same-browser fast path)", () => {
  it("stashes { plan, currency } once the register call reports pending", async () => {
    const mutateAsyncMock = mutateAsyncFiringOnSuccess<RegisterResult>({ pending: true });
    mockedUseRegister.mockReturnValue(
      mockMutationResult<RegisterResult, RegisterRequest>({ mutateAsync: mutateAsyncMock }),
    );

    renderRegisterPage("/register?plan=agency&currency=INR");
    await fillAndSubmit();

    await screen.findByRole("heading", { name: "Check your email" });
    expect(readPendingPlan()).toEqual({ plan: "agency", currency: "INR" });
  });

  it("stashes nothing for a plain signup (no plan chosen)", async () => {
    const mutateAsyncMock = mutateAsyncFiringOnSuccess<RegisterResult>({ pending: true });
    mockedUseRegister.mockReturnValue(
      mockMutationResult<RegisterResult, RegisterRequest>({ mutateAsync: mutateAsyncMock }),
    );

    renderRegisterPage("/register");
    await fillAndSubmit();

    await screen.findByRole("heading", { name: "Check your email" });
    expect(readPendingPlan()).toBeNull();
  });
});

describe("RegisterPage — first-account bootstrap branch", () => {
  it("navigates to /welcome/checkout?plan=agency when the bootstrap response carries desired_plan on a hosted instance", async () => {
    const mutateAsyncMock = mutateAsyncFiringOnSuccess<RegisterResult>({
      pending: false,
      me: { ...MINIMAL_ME, hosted: true, desired_plan: "agency" },
    });
    mockedUseRegister.mockReturnValue(
      mockMutationResult<RegisterResult, RegisterRequest>({ mutateAsync: mutateAsyncMock }),
    );

    const router = renderRegisterPage("/register?plan=agency");
    await fillAndSubmit();

    await waitFor(() =>
      expect(router.state.location.pathname).toBe("/welcome/checkout"),
    );
    expect(router.state.location.search).toMatchObject({ plan: "agency" });
  });

  it("navigates to /sites (unregressed) when the bootstrap response carries no desired_plan", async () => {
    const mutateAsyncMock = mutateAsyncFiringOnSuccess<RegisterResult>({
      pending: false,
      me: { ...MINIMAL_ME, hosted: true },
    });
    mockedUseRegister.mockReturnValue(
      mockMutationResult<RegisterResult, RegisterRequest>({ mutateAsync: mutateAsyncMock }),
    );

    const router = renderRegisterPage("/register");
    await fillAndSubmit();

    await waitFor(() => expect(router.state.location.pathname).toBe("/sites"));
  });

  it("navigates to /sites when desired_plan is present but the instance is not hosted (self-host safety)", async () => {
    const mutateAsyncMock = mutateAsyncFiringOnSuccess<RegisterResult>({
      pending: false,
      me: { ...MINIMAL_ME, hosted: false, desired_plan: "agency" },
    });
    mockedUseRegister.mockReturnValue(
      mockMutationResult<RegisterResult, RegisterRequest>({ mutateAsync: mutateAsyncMock }),
    );

    const router = renderRegisterPage("/register?plan=agency");
    await fillAndSubmit();

    await waitFor(() => expect(router.state.location.pathname).toBe("/sites"));
  });
});
