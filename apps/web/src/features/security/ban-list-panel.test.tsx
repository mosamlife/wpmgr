import { describe, it, expect, vi } from "vitest";
import { fireEvent, screen } from "@testing-library/react";
import type { UseMutationResult } from "@tanstack/react-query";

import { renderWithProviders } from "@/test/render";
import { mockMutationResult, mockQueryResult } from "@/test/query-mocks";

import { BanListPanel } from "./ban-list-panel";
import {
  useBans,
  useCreateBan,
  type Ban,
  type BanCreate,
  type BanCreateResponse,
} from "./use-hardening";

// P1 outcome test — GH #170 Wave 5.
//
// Before this file, `use-hardening.test.ts` covered the viewer-gate ONLY via
// a LOCAL re-implementation (`simulateBanListPanelSubmit(canWrite)`) that
// never rendered `BanListPanel` — a regression that showed the real add-ban
// form to a real viewer would pass that test unchanged. This file renders the
// real component and asserts on the real DOM: the add-ban form (and its
// `useCreateBan` wiring) is absent for a viewer, present and wired for an
// owner/admin.

vi.mock("./use-hardening", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-hardening")>();
  return {
    ...actual,
    useBans: vi.fn(),
    useCreateBan: vi.fn(),
  };
});

const mockedUseBans = vi.mocked(useBans);
const mockedUseCreateBan = vi.mocked(useCreateBan);

function buildBan(overrides: Partial<Ban> = {}): Ban {
  return {
    id: "ban-1",
    type: "ip",
    value: "203.0.113.42",
    comment: "Known scanner",
    created_at: "2026-07-01T00:00:00Z",
    ...overrides,
  };
}

describe("BanListPanel — role gating (viewer must never see the add-ban form)", () => {
  it("hides the add-ban form entirely for a viewer (canWrite=false)", () => {
    mockedUseBans.mockReturnValue(mockQueryResult<Ban[]>({ data: [buildBan()] }));
    const mutate = vi.fn();
    mockedUseCreateBan.mockReturnValue(
      mockMutationResult<BanCreateResponse, BanCreate>({ mutate }),
    );

    renderWithProviders(<BanListPanel siteId="site-1" canWrite={false} />);

    expect(screen.queryByLabelText(/ban type/i)).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /add ban/i }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("form", { name: /add a ban/i }),
    ).not.toBeInTheDocument();

    // The read-only table still renders — viewer sees data, just can't act.
    expect(screen.getByText("203.0.113.42")).toBeInTheDocument();
    // No delete affordance either.
    expect(
      screen.queryByRole("button", { name: /remove ban/i }),
    ).not.toBeInTheDocument();

    // And since there is no way to reach it, the create mutation is never
    // invoked for a viewer.
    expect(mutate).not.toHaveBeenCalled();
  });

  it("shows the add-ban form for an owner/admin (canWrite=true), wired to fire useCreateBan with the entered fields", () => {
    mockedUseBans.mockReturnValue(mockQueryResult<Ban[]>({ data: [] }));
    const mutate = vi.fn(
      (
        _body: BanCreate,
        opts?: { onSuccess?: (r: BanCreateResponse) => void },
      ) => {
        opts?.onSuccess?.({ ban: buildBan() });
      },
    );
    mockedUseCreateBan.mockReturnValue(
      mockMutationResult<BanCreateResponse, BanCreate>({
        // Narrow cast at the mock boundary (same pattern as
        // add-site-dialog.test.tsx): `mutate` only implements the 1-arg
        // onSuccess callback shape the component actually calls, not
        // TanStack Query's full 4-arg MutateOptions signature.
        mutate: mutate as UseMutationResult<
          BanCreateResponse,
          Error,
          BanCreate
        >["mutate"],
      }),
    );

    renderWithProviders(<BanListPanel siteId="site-1" canWrite />);

    const valueInput = screen.getByPlaceholderText("203.0.113.42");
    fireEvent.change(valueInput, { target: { value: "198.51.100.7" } });

    const commentInput = screen.getByPlaceholderText("Optional note");
    fireEvent.change(commentInput, { target: { value: "Repeat offender" } });

    fireEvent.click(screen.getByRole("button", { name: /add ban/i }));

    // Non-vacuous: this is the exact form <-> hook wiring a viewer must never
    // reach. A regression that showed the form but forgot to call the real
    // mutation (or dropped a field) fails here.
    expect(mutate).toHaveBeenCalledTimes(1);
    expect(mutate).toHaveBeenCalledWith(
      { type: "ip", value: "198.51.100.7", comment: "Repeat offender" },
      expect.anything(),
    );
  });
});
