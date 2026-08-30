import { describe, it, expect } from "vitest";
import { screen } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";

import { Route } from "./index";
import { MCP_TRANSPORT_PATH } from "@/features/ai-connections/client-table";

// FOUND BY THE MUTATION SWEEP. This route had no test at all, so the one
// decision it makes -- pass `unavailable` to the list rather than `empty` --
// was pinned by nothing. The sweep's "red" for that mutation was vitest
// exiting 1 on "No test files found", which is a guard finding nothing and
// being scored as a catch: the exact failure mode this project calls its
// signature defect, reproduced inside the tool checking for it.
//
// The component-level states are covered in
// features/ai-connections/connections-list.test.tsx. What is covered HERE is
// the wiring: which state this page actually hands down.

const AiPage = Route.options.component!;

function renderPage() {
  return renderWithProviders(<AiPage />, { withRouter: true, initialPath: "/ai" });
}

describe("/ai", () => {
  it("says we cannot list connections yet, and never that the operator has none", async () => {
    renderPage();
    expect(await screen.findByTestId("connections-unavailable")).toBeInTheDocument();
    expect(screen.getByText(/cannot list your connections yet/i)).toBeInTheDocument();

    // The sentence that would be a claim about the operator's account made out
    // of a gap in our own API.
    expect(screen.queryByText(/no ai clients are connected/i)).not.toBeInTheDocument();
    expect(screen.queryByTestId("connections-empty")).not.toBeInTheDocument();
    // Nor a failure, which would send someone to retry something that cannot
    // succeed.
    expect(screen.queryByText(/could not load your ai connections/i)).not.toBeInTheDocument();
  });

  it("names the missing endpoint as the reason rather than staying vague", async () => {
    renderPage();
    await screen.findByTestId("connections-unavailable");
    expect(screen.getByText(/does not expose an endpoint/i)).toBeInTheDocument();
  });

  it("publishes this deployment's endpoint, not a hardcoded host", async () => {
    renderPage();
    await screen.findByTestId("connections-unavailable");
    const expected = `${window.location.origin}${MCP_TRANSPORT_PATH}`;
    expect(screen.getByText(expected)).toBeInTheDocument();
  });

  it("states the self-hosted proxy requirement beside the endpoint", async () => {
    renderPage();
    await screen.findByTestId("connections-unavailable");
    // Deriving /mcp from the origin does not prove anything forwards it.
    // infra/urlmap.yaml routes it on hosted; infra/nginx/nginx.conf and
    // apps/web/vite.config.ts do not.
    expect(screen.getByText(/reverse proxy must forward \/mcp to the API/i)).toBeInTheDocument();
  });

  it("offers a route into the wizard, which is how that page is reached at all", async () => {
    renderPage();
    // /ai/connect is deliberately absent from the sidebar, so this link is its
    // only entry point. If it goes, the wizard becomes unreachable in exactly
    // the way the whole slice exists to fix.
    const links = await screen.findAllByRole("link", { name: /connect an ai client/i });
    expect(links.length).toBeGreaterThanOrEqual(1);
    expect(links[0]).toHaveAttribute("href", "/ai/connect");
  });
});
