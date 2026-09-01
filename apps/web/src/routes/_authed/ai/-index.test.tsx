import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor, fireEvent } from "@testing-library/react";

import type { QueryClient } from "@tanstack/react-query";

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import { authKeys } from "@/features/auth/use-auth";

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

// The role this page's principal holds, per test. Owner/admin is the default
// because it is the principal every pre-existing test here was written against;
// the role-refused tests set it explicitly.
//
// SEEDED INTO THE QUERY CACHE, NOT MOCKED AT THE HOOK. `vi.mock("use-auth")`
// would stub out canManage itself, and canManage is the thing under test: it
// does more than read a role string, it also requires the active tenant to
// appear in me.memberships, which is how a site-scoped collaborator is refused.
// Seeding authKeys.me runs the real useMe and the real canManage over a real Me
// shape. It is not stubbed at fetch like the connections list next door because
// useMe goes through the generated client rather than raw fetch, and the
// generated client does not read the vi.stubGlobal'd fetch.
let meRole: "owner" | "admin" | "operator" | "viewer" = "admin";

const TENANT = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";

function seedMe(queryClient: QueryClient) {
  queryClient.setQueryData(authKeys.me, {
    id: "user-1",
    email: "priya@example.test",
    name: "Priya",
    scope: "org",
    role: meRole,
    active_tenant_id: TENANT,
    memberships: [{ tenant_id: TENANT, role: meRole, tenant_name: "Example" }],
  });
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
  const queryClient = createTestQueryClient();
  seedMe(queryClient);
  return renderWithProviders(<AiPage />, {
    withRouter: true,
    initialPath: "/ai",
    queryClient,
  });
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

beforeEach(() => {
  vi.clearAllMocks();
  meRole = "admin";
});
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
    expect(screen.queryByText(/you have no ai connections/i)).not.toBeInTheDocument();
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

  it("offers a route into the wizard, which is how that page is reached at all", async () => {
    renderPage();
    await screen.findByTestId("connections-empty");
    const links = await screen.findAllByRole("link", { name: /new connection/i });
    expect(links.length).toBeGreaterThanOrEqual(1);
    expect(links[0]).toHaveAttribute("href", "/ai/connect");
  });
});

// THE CAN/CANNOT CONTRACT.
//
// Every assertion below is on the EXACT string, not on "something rendered".
// The defect this whole block exists over is missing words -- four distinct
// limits collapsed into "It cannot change anything." -- so the words are what
// has to be pinned. A getByRole("heading") or a /cannot/i regex would survive
// the collapse happening again.
describe("what a connection can and cannot do", () => {
  beforeEach(() => stubFetch(() => json({ connections: [] })));

  it("states the lead, and states that nothing is implicit", async () => {
    renderPage();
    expect(
      await screen.findByText(
        "A connection lets one AI client read your fleet, limited to the sites you name. " +
          "Nothing about it is implicit.",
      ),
    ).toBeInTheDocument();
  });

  it("names what a connection can do, in full", async () => {
    renderPage();
    expect(await screen.findByText("What a connection can do")).toBeInTheDocument();
    expect(screen.getByText("Read the sites you put in its scope")).toBeInTheDocument();
    expect(screen.getByText("Report what it found, with its sources")).toBeInTheDocument();
  });

  it("names all four things it can never do, each on its own", async () => {
    // Four separate limits, four separate assertions. Asserting the block
    // renders would pass with three of them deleted, which is the state this
    // page shipped in.
    renderPage();
    expect(await screen.findByText("What it can never do")).toBeInTheDocument();
    expect(screen.getByText("Approve its own change")).toBeInTheDocument();
    expect(screen.getByText("Reach a site outside its scope")).toBeInTheDocument();
    expect(
      screen.getByText("Run PHP, WP-CLI, a shell, or open a file path of its choosing"),
    ).toBeInTheDocument();
    expect(
      screen.getByText("Be granted a “skip approval” setting. There isn’t one."),
    ).toBeInTheDocument();
  });

  it("renders the contract for a principal who cannot create connections", async () => {
    // The contract is a statement about the system, not about the reader, so
    // it does not disappear with the button.
    meRole = "viewer";
    renderPage();
    expect(await screen.findByText("What it can never do")).toBeInTheDocument();
    expect(screen.getByText("What a connection can do")).toBeInTheDocument();
  });

  it("claims no capability the server does not have", async () => {
    // THE GUARD AGAINST A FIDELITY PASS. The design deck draws a "propose
    // changes" capability and a "Produce a change set for you to review" line.
    // apps/api/internal/mcp/policy.go's vocabulary is eight names and every one
    // ends in `.read`; m131's CHECK admits only those eight, so no grant can
    // hold a propose capability and no screen may imply one. Restoring either
    // string off the deck turns this red.
    renderPage();
    await screen.findByText("What a connection can do");
    expect(screen.queryByText(/produce a change set/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/propose/i)).not.toBeInTheDocument();
    expect(document.body.textContent ?? "").not.toMatch(/propose/i);
  });
});

// THE ROLE-REFUSED STATE.
//
// Gate confirmed against the server before it was written, not assumed:
// apps/api/internal/mcp/handler.go:172 mounts POST /mcp/connections behind
// authz.RequirePermission(authz.PermAPIKeyManage), and
// apps/api/internal/authz/role.go:241 maps PermAPIKeyManage to RoleAdmin.
describe("who may create a connection", () => {
  beforeEach(() => stubFetch(() => json({ connections: [] })));

  // PLURAL, DELIBERATELY. This page renders TWO "New connection" links once the
  // list settles: the header action and the empty state's own. A singular
  // findByRole passed here, but only because it resolves on the first poll --
  // while the list is still a skeleton and only the header link exists -- and
  // would start throwing "found multiple elements" the moment anything made the
  // empty state paint sooner. An assertion that depends on which of two
  // renders wins is not pinning the thing it names. Caught in review on #681.
  it("offers the button to an owner", async () => {
    meRole = "owner";
    renderPage();
    await screen.findByTestId("connections-empty");
    expect(screen.getAllByRole("link", { name: /new connection/i }).length).toBeGreaterThan(0);
  });

  it("offers the button to an admin", async () => {
    meRole = "admin";
    renderPage();
    await screen.findByTestId("connections-empty");
    expect(screen.getAllByRole("link", { name: /new connection/i }).length).toBeGreaterThan(0);
  });

  it("shows no create button to an operator, and says why", async () => {
    // A button that can only ever 403 spends the operator's trust when they
    // press it. It is absent, with the reason written where it was.
    meRole = "operator";
    renderPage();
    await screen.findByTestId("connections-empty");
    expect(screen.queryByRole("link", { name: /new connection/i })).not.toBeInTheDocument();
    expect(
      screen.getByText(/creating a connection needs an organisation owner or admin/i),
    ).toBeInTheDocument();
  });

  it("shows no create button to a viewer", async () => {
    meRole = "viewer";
    renderPage();
    await screen.findByTestId("connections-empty");
    expect(screen.queryByRole("link", { name: /new connection/i })).not.toBeInTheDocument();
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
