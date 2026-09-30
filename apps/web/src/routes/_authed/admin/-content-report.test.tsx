import { beforeEach, describe, expect, it, vi } from "vitest";
import { screen } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";

import { createTestQueryClient, renderWithProviders } from "@/test/render";

import { Route as ReportRoute } from "./content-report";

// Shape: ContentFleetReport in packages/openapi-client/src/generated/types.gen.ts.
const getReport = vi.fn();
vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return { ...actual, getAdminContentFleetReport: (...a: unknown[]) => getReport(...a) };
});

async function renderReport() {
  const rootRoute = createRootRoute({});
  type UpdateOptions = Parameters<typeof ReportRoute.update>[0];
  const r = ReportRoute.update({
    id: "/admin/content-report",
    path: "/admin/content-report",
    getParentRoute: () => rootRoute,
  } as unknown as UpdateOptions);
  const router = createRouter({
    routeTree: rootRoute.addChildren([r]),
    history: createMemoryHistory({ initialEntries: ["/admin/content-report"] }),
  });
  renderWithProviders(<RouterProvider router={router} />, {
    queryClient: createTestQueryClient(),
  });
}

beforeEach(() => getReport.mockReset());

describe("fleet page-editor report", () => {
  it("renders totals, builders and verdict shares, unknown verdicts as Not available yet", async () => {
    getReport.mockResolvedValue({
      data: {
        pages: 200,
        by_verdict: [
          { verdict: "classic", route_number: 1, pages: 120, sites: 9 },
          { verdict: "unrecognised_builder", route_number: 3, pages: 50, sites: 4 },
          { verdict: "from_the_future", route_number: 3, pages: 30, sites: 2 },
        ],
        by_builder: [{ integration_id: "elementor", version: "3.20.1", pages: 70, sites: 5 }],
      },
      error: undefined,
      response: { status: 200 },
    });
    await renderReport();
    expect(await screen.findByText("Pages checked: 200")).toBeInTheDocument();
    expect(screen.getByText("elementor")).toBeInTheDocument();
    expect(screen.getByText("3.20.1")).toBeInTheDocument();
    expect(screen.getByText("Classic editor")).toBeInTheDocument();
    expect(screen.getByText("A page builder, not confirmed")).toBeInTheDocument();
    expect(screen.getByText("Not available yet")).toBeInTheDocument();
    expect(screen.getByText("60%")).toBeInTheDocument();
  });

  it("shows an error state", async () => {
    getReport.mockResolvedValue({ data: undefined, error: { code: "x", message: "no" }, response: { status: 500 } });
    await renderReport();
    expect(await screen.findByText("Could not load the page editor report")).toBeInTheDocument();
  });
});
