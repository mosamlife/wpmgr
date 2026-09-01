import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor, fireEvent } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";

import { Route } from "./index";
import { MCP_TRANSPORT_PATH } from "@/features/ai-connections/client-table";
import { CONNECTIONS_PATH } from "@/features/ai-connections/use-ai-connections";

// FOUND BY THE MUTATION SWEEP. This route had no test at all, so the one
// decision it makes was pinned by nothing. The sweep's "red" for that mutation
// was vitest exiting 1 on "No test files found" -- a guard finding nothing and
// being scored as a catch, which is this project's signature defect reproduced
// inside the tool checking for it.
//
// Now that /ai does a REAL READ, the wiring is what this file covers: which
// state the page hands down for each answer the endpoint can give. The states
// themselves are covered in features/ai-connections/connections-list.test.tsx,
// and the wire mapping in use-ai-connections.test.ts.
//
// The fetch is stubbed at the network boundary rather than the hook being
// mocked, so the real queryFn, the real zod parse and the real state mapping
// all run. A hook mock here would assert that the component renders whatever it
// is handed, which is not the question.

const AiPage = Route.options.component!;

/**
 * Resolve a fetch input to its URL.
 *
 * String(input) would stringify a Request object as "[object Object]" and the
 * URL assertions below would then pass or fail for the wrong reason. Every
 * branch is handled explicitly.
 */
function urlOf(input: RequestInfo | URL): string {
  if (typeof input === "string") return input;
  if (input instanceof URL) return input.toString();
  return input.url;
}

function stubFetch(impl: (url: string) => Response | Promise<Response>) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => Promise.resolve(impl(urlOf(input)))),
  );
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function renderPage() {
  return renderWithProviders(<AiPage />, { withRouter: true, initialPath: "/ai" });
}

const ROW = {
  id: "11111111-1111-1111-1111-111111111111",
  name: "Fleet manager",
  status: "active",
  site_scope_mode: "all",
  scopes: ["mcp:read"],
  created_at: "2026-08-01T00:00:00Z",
  reported_client_name: "claude-code",
  reported_client_version: "2.1.0",
  protocol: { state: "recognised", version: "2025-11-25" },
  last_used_at: "2026-08-29T10:00:00Z",
  revoked_at: null,
  capabilities: ["mcp.sites.read"],
};

beforeEach(() => vi.clearAllMocks());
afterEach(() => vi.unstubAllGlobals());

describe("/ai reads the real connections endpoint", () => {
  it("asks the path the API actually mounts", async () => {
    const seen: string[] = [];
    stubFetch((url) => {
      seen.push(url);
      return json({ connections: [] });
    });
    renderPage();
    await screen.findByTestId("connections-empty");
    // apps/api/internal/mcp/discovery.go: ConnectionsPath = "/api/v1" +
    // "/mcp/connections". A wrong path here would 404 and render as a failure,
    // which is at least honest, but it would never show a connection.
    expect(seen).toContain(CONNECTIONS_PATH);
    expect(CONNECTIONS_PATH).toBe("/api/v1/mcp/connections");
  });

  it("renders the connections it was given", async () => {
    stubFetch(() => json({ connections: [ROW] }));
    renderPage();
    expect(await screen.findByText("Fleet manager")).toBeInTheDocument();
    expect(screen.getByText(/claude-code 2\.1\.0/)).toBeInTheDocument();
  });

  it("renders a failed load as a failure, never as an empty list", async () => {
    // The whole reason this page exists in the shape it does.
    stubFetch(() => json({ code: "internal", message: "the server said 500" }, 500));
    renderPage();
    expect(await screen.findByText(/could not load your ai connections/i)).toBeInTheDocument();
    expect(screen.queryByText(/no ai clients are connected/i)).not.toBeInTheDocument();
    expect(screen.queryByTestId("connections-empty")).not.toBeInTheDocument();
  });

  it("renders a 403 with the reason, not as an empty org", async () => {
    // The route carries RequirePermission(PermAPIKeyRead), which refuses any
    // site-constrained principal outright, so this is a real expected answer
    // rather than a hypothetical.
    stubFetch(() => json({ code: "forbidden", message: "" }, 403));
    renderPage();
    expect(await screen.findByText(/could not load your ai connections/i)).toBeInTheDocument();
    expect(screen.getByText(/needs an admin/i)).toBeInTheDocument();
    expect(screen.queryByTestId("connections-empty")).not.toBeInTheDocument();
  });

  it("renders a malformed 200 as a failure rather than a confident list", async () => {
    // A 200 whose body is not the promised shape. Rendering it half-parsed
    // would be a partial list shown as a complete one.
    stubFetch(() => json({ connections: [{ id: "x" }] }));
    renderPage();
    expect(await screen.findByText(/could not load your ai connections/i)).toBeInTheDocument();
  });

  it("renders a genuinely empty organisation as empty", async () => {
    // The over-fire half: correct empty work must still say "none".
    stubFetch(() => json({ connections: [] }));
    renderPage();
    expect(await screen.findByTestId("connections-empty")).toBeInTheDocument();
    expect(screen.queryByText(/could not load/i)).not.toBeInTheDocument();
  });

  it("no longer claims the feature does not exist", async () => {
    // It was hardcoded to `unavailable` while there was no endpoint. Shipping
    // that now would tell an operator the feature is missing while the endpoint
    // sits there working.
    stubFetch(() => json({ connections: [ROW] }));
    renderPage();
    await screen.findByText("Fleet manager");
    expect(screen.queryByTestId("connections-unavailable")).not.toBeInTheDocument();
  });
});

describe("/ai's static surfaces", () => {
  beforeEach(() => stubFetch(() => json({ connections: [] })));

  it("states the self-hosted proxy requirement beside the endpoint", async () => {
    renderPage();
    await screen.findByTestId("connections-empty");
    expect(screen.getByText(/reverse proxy must forward \/mcp to the API/i)).toBeInTheDocument();
  });

  it("publishes this deployment's endpoint, not a hardcoded host", async () => {
    renderPage();
    await screen.findByTestId("connections-empty");
    expect(
      screen.getByText(`${window.location.origin}${MCP_TRANSPORT_PATH}`),
    ).toBeInTheDocument();
  });

  it("tells the truth about what a connection can do: read, scoped to sites, nothing else", async () => {
    // Nothing in the product can propose anything (no proposal table, no
    // approval queue, no approval screen anywhere in the route tree). The
    // subline used to say "It can propose changes; it can never approve
    // them." -- a false capability claim. It must now say only what is true.
    renderPage();
    const subline = await screen.findByText(
      /let an ai client read your fleet through one endpoint/i,
    );
    expect(subline).toHaveTextContent(/limited to the sites you scope it to/i);
    expect(subline).toHaveTextContent(/it cannot change anything/i);
    expect(subline).not.toHaveTextContent(/propose/i);
  });

  it("offers a route into the wizard, which is how that page is reached at all", async () => {
    renderPage();
    await screen.findByTestId("connections-empty");
    const links = await screen.findAllByRole("link", { name: /connect an ai client/i });
    expect(links.length).toBeGreaterThanOrEqual(1);
    expect(links[0]).toHaveAttribute("href", "/ai/connect");
  });
});

describe("revoke", () => {
  it("says what revoking actually does, on the client's next request", async () => {
    stubFetch(() => json({ connections: [ROW] }));
    renderPage();
    await screen.findByText("Fleet manager");
    fireEvent.click(screen.getByRole("button", { name: /^revoke$/i }));

    // NOT "will no longer have access". The cascade kills the tokens and the
    // grant is re-checked per request, so there is no expiry delay to imply.
    await waitFor(() =>
      expect(screen.getByText(/stops working on its/i)).toBeInTheDocument(),
    );
    expect(screen.getByText(/next request/i)).toBeInTheDocument();
  });

  it("does not carry a failed revoke from one connection to the next", async () => {
    // A's revoke fails; the operator closes and opens B. B must not be shown
    // A's failure -- that tells them a revoke failed for something nobody
    // tried to revoke. Same family as everything else on this page: state
    // belonging to one subject presented as a fact about another.
    const rowB = { ...ROW, id: "22222222-2222-2222-2222-222222222222", name: "CI reader" };
    stubFetch((url) =>
      url.includes("/revoke")
        ? json({ code: "conflict", message: "REVOKE FAILED FOR FLEET MANAGER" }, 409)
        : json({ connections: [ROW, rowB] }),
    );
    renderPage();
    await screen.findByText("Fleet manager");

    const [revokeA] = screen.getAllByRole("button", { name: /^revoke$/i });
    fireEvent.click(revokeA!);
    fireEvent.change(await screen.findByRole("textbox"), {
      target: { value: "Fleet manager" },
    });
    fireEvent.click(screen.getByRole("button", { name: /revoke connection/i }));
    expect(await screen.findByText(/REVOKE FAILED FOR FLEET MANAGER/i)).toBeInTheDocument();

    // Close, then open the OTHER connection's dialog.
    fireEvent.click(screen.getByRole("button", { name: /keep it connected/i }));
    await waitFor(() =>
      expect(screen.queryByText(/REVOKE FAILED FOR FLEET MANAGER/i)).not.toBeInTheDocument(),
    );
    const revokeB = screen.getAllByRole("button", { name: /^revoke$/i })[1];
    fireEvent.click(revokeB!);
    await screen.findByText(/next request/i);

    expect(screen.queryByText(/REVOKE FAILED FOR FLEET MANAGER/i)).not.toBeInTheDocument();
  });

  it("does not revoke anything merely by opening the dialog", async () => {
    const posts: string[] = [];
    stubFetch((url) => {
      if (url.includes("/revoke")) posts.push(url);
      return json({ connections: [ROW] });
    });
    renderPage();
    await screen.findByText("Fleet manager");
    fireEvent.click(screen.getByRole("button", { name: /^revoke$/i }));
    await screen.findByText(/next request/i);
    // Opening a destructive dialog must not perform the destructive thing.
    expect(posts).toEqual([]);
  });
});
