import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen } from "@testing-library/react";
import type { Me } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { mockMutationResult, mockQueryResult } from "@/test/query-mocks";

import { Route } from "./smtp";
import {
  useSmtp,
  usePutSmtp,
  useTestSmtp,
  type SmtpSettings,
  type SmtpTestResult,
  type PutSmtpBody,
} from "@/features/settings/use-smtp";

// Instance SMTP capability gating (5ae87b71): GET (and PUT/POST) under
// /api/v1/settings/smtp require instance-level authority, so the page must
// show a plain "ask your instance administrator" state — never the generic
// load-failure PageError with a Retry that can only 403 again — for any
// principal the server does not admit. Rendered via `Route.options.component!`
// (same extraction -route.test.tsx uses) — but unlike that file, THIS one
// needs `withRouter: true`: `tanstackRouter({ autoCodeSplitting: true })`
// (vite.config.ts) wraps every route file's `component` in a `React.lazy`
// (confirmed: `Route.options.component` is a `Lazy` function, not
// `SmtpSettingsPage` itself), and only `RouterProvider` supplies the
// Suspense boundary that resolves it. Rendering the bare element with no
// router leaves React "suspended... act call was not awaited" and an empty
// tree — every assertion below must therefore start with `findBy*`, not a
// synchronous `getBy*` (see src/test/render.tsx's RouterProvider gotcha).

const SmtpSettingsPage = Route.options.component!;

vi.mock("@/features/auth/use-auth", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/auth/use-auth")>();
  return { ...actual, useMe: vi.fn() };
});

vi.mock("@/features/settings/use-smtp", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/settings/use-smtp")>();
  return {
    ...actual,
    useSmtp: vi.fn(),
    usePutSmtp: vi.fn(),
    useTestSmtp: vi.fn(),
  };
});

const { useMe } = await import("@/features/auth/use-auth");
const mockedUseMe = vi.mocked(useMe);
const mockedUseSmtp = vi.mocked(useSmtp);
const mockedUsePutSmtp = vi.mocked(usePutSmtp);
const mockedUseTestSmtp = vi.mocked(useTestSmtp);

const TENANT_ID = "00000000-0000-0000-0000-0000000000aa";

const ORG_OWNER_ME: Me = {
  user: {
    id: "00000000-0000-0000-0000-000000000002",
    email: "owner@wpmgr.test",
    name: "Owner",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  },
  memberships: [{ user_id: "00000000-0000-0000-0000-000000000002", tenant_id: TENANT_ID, role: "owner" }],
  active_tenant_id: TENANT_ID,
};

const SUPERADMIN_NO_ORG_ME: Me = {
  user: {
    id: "00000000-0000-0000-0000-000000000001",
    email: "superadmin@wpmgr.test",
    name: "Superadmin",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    is_superadmin: true,
  },
  memberships: [],
};

function buildSmtpSettings(overrides: Partial<SmtpSettings> = {}): SmtpSettings {
  return {
    enabled: true,
    host: "smtp.example.com",
    port: 587,
    username: "relay",
    from_address: "noreply@example.com",
    from_name: "WPMgr Notifications",
    tls_mode: "starttls",
    allow_insecure_tls: false,
    password_set: true,
    updated_at: "2026-09-01T00:00:00Z",
    ...overrides,
  };
}

beforeEach(() => {
  vi.clearAllMocks();
  mockedUsePutSmtp.mockReturnValue(
    mockMutationResult<SmtpSettings, PutSmtpBody>({}),
  );
  mockedUseTestSmtp.mockReturnValue(
    mockMutationResult<SmtpTestResult, { to_address: string }>({}),
  );
});

describe("SmtpSettingsPage — incapable principal never sees the generic load-error/Retry state", () => {
  it("shows the plain instance-administrator message when can_manage_instance_email is false, even though the (mocked) query looks errored", async () => {
    mockedUseMe.mockReturnValue(
      mockQueryResult<Me | null>({
        data: { ...ORG_OWNER_ME, can_manage_instance_email: false },
      }),
    );
    // Deliberately wired to look like a failed load — if the component
    // checked capability AFTER the error branch, this would render
    // PageError's "Could not load SMTP settings." + Retry instead.
    mockedUseSmtp.mockReturnValue(
      mockQueryResult<SmtpSettings>({
        data: undefined,
        isError: true,
        isPending: false,
        isSuccess: false,
        status: "error",
        error: new Error("forbidden"),
      }),
    );

    renderWithProviders(<SmtpSettingsPage />, {
      withRouter: true,
      initialPath: "/settings/smtp",
    });

    expect(await screen.findByRole("heading", { name: "Email / SMTP" })).toBeInTheDocument();
    expect(
      screen.getByText(
        "Only the instance administrator can change these settings. Ask your instance administrator to make changes.",
      ),
    ).toBeInTheDocument();

    expect(screen.queryByText("Could not load SMTP settings.")).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Reload SMTP settings" }),
    ).not.toBeInTheDocument();
    // Exactly one alert on the page — the incapable message, not a second
    // PageError alongside it.
    expect(screen.getAllByRole("alert")).toHaveLength(1);

    // The GET is never fired for a principal the page already knows will
    // 403 — enabled: false must reach the hook.
    expect(mockedUseSmtp).toHaveBeenCalledWith({ enabled: false });
  });

  it("treats a MISSING can_manage_instance_email the same as false (never defaults open)", async () => {
    mockedUseMe.mockReturnValue(mockQueryResult<Me | null>({ data: ORG_OWNER_ME }));
    mockedUseSmtp.mockReturnValue(
      mockQueryResult<SmtpSettings>({ data: undefined, isPending: true, isSuccess: false }),
    );

    renderWithProviders(<SmtpSettingsPage />, {
      withRouter: true,
      initialPath: "/settings/smtp",
    });

    expect(
      await screen.findByText(
        "Only the instance administrator can change these settings. Ask your instance administrator to make changes.",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByRole("status", { name: "Loading SMTP settings" })).not.toBeInTheDocument();
    expect(mockedUseSmtp).toHaveBeenCalledWith({ enabled: false });
  });
});

describe("SmtpSettingsPage — capable principal sees the real form", () => {
  it("renders the SMTP config form (not the incapable message) for an org owner the server admits", async () => {
    mockedUseMe.mockReturnValue(
      mockQueryResult<Me | null>({
        data: { ...ORG_OWNER_ME, can_manage_instance_email: true },
      }),
    );
    mockedUseSmtp.mockReturnValue(
      mockQueryResult<SmtpSettings>({ data: buildSmtpSettings() }),
    );

    renderWithProviders(<SmtpSettingsPage />, {
      withRouter: true,
      initialPath: "/settings/smtp",
    });

    expect(await screen.findByText("SMTP relay")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save SMTP settings" })).toBeEnabled();
    expect(
      screen.queryByText("Only the instance administrator can change these settings."),
    ).not.toBeInTheDocument();
    expect(mockedUseSmtp).toHaveBeenCalledWith({ enabled: true });
  });

  it("renders the SMTP config form for a superadmin with NO organisation the server admits", async () => {
    mockedUseMe.mockReturnValue(
      mockQueryResult<Me | null>({
        data: { ...SUPERADMIN_NO_ORG_ME, can_manage_instance_email: true },
      }),
    );
    mockedUseSmtp.mockReturnValue(
      mockQueryResult<SmtpSettings>({ data: buildSmtpSettings() }),
    );

    renderWithProviders(<SmtpSettingsPage />, {
      withRouter: true,
      initialPath: "/settings/smtp",
    });

    expect(await screen.findByText("SMTP relay")).toBeInTheDocument();
    expect(screen.getByLabelText("SMTP host")).toHaveValue("smtp.example.com");
  });
});
