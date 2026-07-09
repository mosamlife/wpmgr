import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, fireEvent } from "@testing-library/react";
import type { Site } from "@wpmgr/api";
import type { UseMutateFunction } from "@tanstack/react-query";

import { renderWithProviders } from "@/test/render";
import { mockMutationResult } from "@/test/query-mocks";

import { SiteRowActions } from "./site-row-actions";
import { useRefreshScreenshot } from "./use-sites";
import { useRecheckConnection } from "./use-site-connection";
import { toast } from "@/components/toast";

// GH #187 (FE half) caller test — the row-action "Refresh screenshot" menu
// item is the one and only place this mutation is fired from in the sites
// list/grid. Pins: (a) the click fires the mutation with the site id and the
// honest "queued" toast on the 202, and (c) whatever message the mutation
// hook's error mapping produces (409/500/501/503, see use-sites.test.ts)
// reaches the operator verbatim in the error toast's description.

vi.mock("./use-sites", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-sites")>();
  return { ...actual, useRefreshScreenshot: vi.fn() };
});

vi.mock("./use-site-connection", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-site-connection")>();
  return { ...actual, useRecheckConnection: vi.fn() };
});

vi.mock("@/components/toast", () => ({
  toast: {
    success: vi.fn(),
    error: vi.fn(),
    warning: vi.fn(),
    info: vi.fn(),
    destructive: vi.fn(),
  },
}));

const mockedUseRefreshScreenshot = vi.mocked(useRefreshScreenshot);
const mockedUseRecheckConnection = vi.mocked(useRecheckConnection);
const mockedToastSuccess = vi.mocked(toast.success);
const mockedToastError = vi.mocked(toast.error);

function buildSite(overrides: Partial<Site> = {}): Site {
  return {
    id: "site-1",
    tenant_id: "tenant-1",
    url: "https://example.com",
    name: "Example",
    status: "active",
    wp_version: "6.8",
    php_version: "8.3",
    health_status: "healthy",
    multisite: false,
    tags: [],
    ...overrides,
  } as unknown as Site;
}

type SimpleMutateOpts = {
  onSuccess?: () => void;
  onError?: (err: Error) => void;
};
type SimpleMutate = (siteId: string, opts?: SimpleMutateOpts) => void;

/** `SiteRowActions` only ever calls `mutate(siteId, { onSuccess, onError })`
 *  with zero/one-arg callbacks (see the real call site), so the test double
 *  only needs to model that shape — cast through `unknown` to the real
 *  `UseMutateFunction` signature rather than reproducing its full 4-arg
 *  onSuccess/onError callback types (mirrors the `as unknown as Site`
 *  pattern already used for test fixtures in sites-filter.test.ts). */
function asMutateFn(fn: SimpleMutate): UseMutateFunction<void, Error, string> {
  return fn as unknown as UseMutateFunction<void, Error, string>;
}

function openMenuAndFindRefreshItem() {
  const trigger = screen.getByRole("button", { name: /more actions/i });
  // Radix DropdownMenuTrigger opens on Enter/Space (keyboard) or pointerdown;
  // Enter is the deterministic jsdom path (matches
  // routes/_authed/settings/-route.test.tsx's precedent).
  fireEvent.keyDown(trigger, { key: "Enter" });
  return screen.findByRole("menuitem", { name: /refresh screenshot/i });
}

beforeEach(() => {
  vi.clearAllMocks();
  mockedUseRecheckConnection.mockReturnValue(
    mockMutationResult({ mutate: vi.fn() }) as ReturnType<
      typeof useRecheckConnection
    >,
  );
});

describe("SiteRowActions — Refresh screenshot wiring (GH #187)", () => {
  it("fires the mutation with the site id and shows the honest 'queued' toast on the 202 ack", async () => {
    const mutate = vi.fn<SimpleMutate>((_siteId, opts) => {
      opts?.onSuccess?.();
    });
    mockedUseRefreshScreenshot.mockReturnValue(
      mockMutationResult<void, string>({ mutate: asMutateFn(mutate) }),
    );

    renderWithProviders(
      <SiteRowActions site={buildSite()} connectionState="connected" />,
    );

    const item = await openMenuAndFindRefreshItem();
    fireEvent.click(item);

    expect(mutate).toHaveBeenCalledTimes(1);
    expect(mutate.mock.calls[0]?.[0]).toBe("site-1");
    expect(mockedToastSuccess).toHaveBeenCalledTimes(1);
    const [title, opts] = mockedToastSuccess.mock.calls[0] as [
      string,
      { description?: string }?,
    ];
    expect(title).toBe("Screenshot queued");
    expect(opts?.description).toContain("shortly");
  });

  it.each([
    ["The screenshot service isn't running.", 503],
    ["Screenshots aren't configured on this server.", 500],
    ["Screenshots aren't configured on this server.", 501],
    ["Site is not enrolled; cannot capture screenshot.", 409],
  ] as const)(
    "surfaces the mutation's specific error message in the error toast (status %2$s)",
    async (message, _status) => {
      const mutate = vi.fn<SimpleMutate>((_siteId, opts) => {
        opts?.onError?.(new Error(message));
      });
      mockedUseRefreshScreenshot.mockReturnValue(
        mockMutationResult<void, string>({ mutate: asMutateFn(mutate) }),
      );

      renderWithProviders(
        <SiteRowActions site={buildSite()} connectionState="connected" />,
      );

      const item = await openMenuAndFindRefreshItem();
      fireEvent.click(item);

      expect(mockedToastError).toHaveBeenCalledWith(
        "Could not refresh screenshot",
        expect.objectContaining({ description: message }),
      );
    },
  );
});
