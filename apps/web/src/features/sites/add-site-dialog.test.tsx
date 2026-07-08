import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, screen } from "@testing-library/react";
import type { UseMutationResult } from "@tanstack/react-query";
import type { Me } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { mockMutationResult, mockQueryResult } from "@/test/query-mocks";

import { AddSiteDialog } from "./add-site-dialog";
import {
  useCreateSiteFirst,
  type CreateSiteResult,
  type CreateSiteInput,
} from "./use-site-connection";
import { useMe } from "@/features/auth/use-auth";
import { SiteLimitReachedError } from "@/lib/api";

// P1 outcome test — GH #170 Wave 5.
//
// Before this file, nothing rendered `AddSiteDialog`; `lib/api.test.ts` only
// covered the pure `extractSiteLimitReached` extractor, never the UI swap it
// feeds. A regression that dropped the `SiteLimitReachedError` branch (e.g.
// let it fall through to the generic inline error, or forgot to close the
// add-site form when opening the prompt) would pass every existing test.
// Test 1 below fails against exactly that: it asserts the add-site form is
// GONE and `UpgradePrompt` is showing the real limit/usage/plan — see the
// "non-vacuous" notes inline.

vi.mock("./use-site-connection", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-site-connection")>();
  return {
    ...actual,
    useCreateSiteFirst: vi.fn(),
  };
});

vi.mock("@/features/auth/use-auth", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/auth/use-auth")>();
  return {
    ...actual,
    useMe: vi.fn(),
  };
});

const mockedUseCreateSiteFirst = vi.mocked(useCreateSiteFirst);
const mockedUseMe = vi.mocked(useMe);

function buildMe(overrides: Partial<Me> = {}): Me {
  return {
    user: {
      id: "u1",
      email: "owner@example.com",
      name: "Owner",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    },
    memberships: [{ user_id: "u1", tenant_id: "t1", role: "owner" }],
    active_tenant_id: "t1",
    hosted: true,
    ...overrides,
  };
}

/**
 * Drives the dialog from its trigger button through a URL submission that
 * the mocked mutation rejects with `SiteLimitReachedError(3, 3, "free")`.
 */
async function triggerSiteLimitReached() {
  const trigger = await screen.findByRole("button", { name: "Add site" });
  fireEvent.click(trigger);

  const urlInput = await screen.findByLabelText("Site URL");
  fireEvent.change(urlInput, { target: { value: "https://example.com" } });
  fireEvent.click(screen.getByRole("button", { name: "Continue" }));
}

beforeEach(() => {
  vi.clearAllMocks();
  const mutateMock = vi.fn(
    (
      _input: CreateSiteInput,
      opts?: {
        onSuccess?: (r: CreateSiteResult) => void;
        onError?: (err: Error) => void;
      },
    ) => {
      opts?.onError?.(new SiteLimitReachedError(3, 3, "free"));
    },
  );
  mockedUseCreateSiteFirst.mockReturnValue(
    mockMutationResult<CreateSiteResult, CreateSiteInput>({
      // Narrow cast at the mock boundary: `mutateMock` only implements the
      // 1-arg onSuccess/onError callback shape this component actually
      // calls (see AddSiteFlow's UrlStep.submit()), not the full 4-arg
      // MutateOptions signature TanStack Query's real UseMutateFunction
      // type allows for. Same pattern as `mockMutationResult`'s own
      // `unknown` cast (query-mocks.ts) — deliberate and narrow, not an
      // unconstrained escape hatch.
      mutate: mutateMock as UseMutationResult<
        CreateSiteResult,
        Error,
        CreateSiteInput
      >["mutate"],
    }),
  );
});

describe("AddSiteDialog — 402 site_limit_reached swaps to UpgradePrompt", () => {
  it("closes the add-site form and opens UpgradePrompt with the real limit/usage/plan, CTA shown for an owner on a hosted instance", async () => {
    mockedUseMe.mockReturnValue(
      mockQueryResult<Me | null>({ data: buildMe({ hosted: true }) }),
    );

    renderWithProviders(<AddSiteDialog />, { withRouter: true });
    await triggerSiteLimitReached();

    // The add-site form is GONE — never shown stacked with the prompt.
    expect(
      screen.queryByRole("heading", { name: "Add site" }),
    ).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Site URL")).not.toBeInTheDocument();

    // UpgradePrompt is showing the REAL details from the error, not a
    // placeholder — a version that hardcodes different numbers/plan fails.
    expect(
      screen.getByRole("heading", { name: "Site limit reached" }),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/Your Free plan includes 3 sites/),
    ).toBeInTheDocument();
    expect(screen.getByText(/using 3\./)).toBeInTheDocument();

    // Owner + hosted => the upgrade CTA is present.
    expect(
      screen.getByRole("link", { name: "View plans" }),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(/ask your organisation owner/i),
    ).not.toBeInTheDocument();
  });

  it("shows NO upgrade CTA for a viewer (even on a hosted instance)", async () => {
    mockedUseMe.mockReturnValue(
      mockQueryResult<Me | null>({
        data: buildMe({
          hosted: true,
          memberships: [{ user_id: "u1", tenant_id: "t1", role: "viewer" }],
        }),
      }),
    );

    renderWithProviders(<AddSiteDialog />, { withRouter: true });
    await triggerSiteLimitReached();

    expect(
      await screen.findByRole("heading", { name: "Site limit reached" }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("link", { name: "View plans" }),
    ).not.toBeInTheDocument();
    expect(
      screen.getByText(/ask your organisation owner to upgrade/i),
    ).toBeInTheDocument();
  });

  it("shows NO upgrade CTA on a non-hosted (self-hosted) instance, even for an owner", async () => {
    mockedUseMe.mockReturnValue(
      mockQueryResult<Me | null>({ data: buildMe({ hosted: false }) }),
    );

    renderWithProviders(<AddSiteDialog />, { withRouter: true });
    await triggerSiteLimitReached();

    expect(
      await screen.findByRole("heading", { name: "Site limit reached" }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("link", { name: "View plans" }),
    ).not.toBeInTheDocument();
    expect(
      screen.getByText(/ask your organisation owner to upgrade/i),
    ).toBeInTheDocument();
  });
});
