import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, screen, within } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import type { Me, SiteAiReadiness } from "@wpmgr/api";

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import {
  ADAPTER_WARNING,
  ELEMENTOR_WARNING,
  FLOORS,
  SITE_ID,
  baseChecks,
  baseGroup,
  bricksAllPassing,
  builderGroup,
  chk,
  elementorAllPassing,
  failResult,
  okResult,
  readiness,
} from "@/features/ai-readiness/readiness-fixtures";

import { Route as ContentRoute } from "./$siteId.content";

// The AI readiness card, mounted through the real Content route, a real router
// and a real QueryClient. Only the network edge (the generated SDK functions)
// is stubbed. Shapes and codes asserted here:
//   - the readiness body: apps/api/internal/aireadiness/dto.go (siteReadinessDTO)
//     and SiteAiReadiness in packages/openapi-client/src/generated/types.gen.ts;
//   - 409 site_unreachable, 503 ai_readiness_refresh_unavailable, 404
//     site_not_found: apps/api/internal/aireadiness/service.go;
//   - 403 insufficient_permission: apps/api/internal/authz/middleware.go;
//   - the error envelope {code, message}: apps/api/internal/server/httpx/respond.go.

// The refetch window after "Check again" is part of the owner's contract: every
// 15 seconds, at most 8 times. Written out here, not imported, so the test pins
// the contract instead of following whatever the implementation says.
const POLL_INTERVAL_MS = 15_000;
const POLL_LIMIT = 8;

const getReadiness = vi.fn();
const refreshReadiness = vi.fn();
const getEditing = vi.fn();
const listReqs = vi.fn();
const getInv = vi.fn();

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return {
    ...actual,
    getSiteAiReadiness: (...a: unknown[]): unknown => getReadiness(...a),
    refreshSiteAiReadiness: (...a: unknown[]): unknown => refreshReadiness(...a),
    getSiteContentEditing: (...a: unknown[]): unknown => getEditing(...a),
    listSiteAbilityRequests: (...a: unknown[]): unknown => listReqs(...a),
    getSiteContentInventory: (...a: unknown[]): unknown => getInv(...a),
  };
});

const ME_KEY = ["test", "me"] as const;
vi.mock("@/features/auth/use-auth", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/auth/use-auth")>();
  const { useQuery } = await import("@tanstack/react-query");
  return {
    ...actual,
    useMe: () => useQuery({ queryKey: ME_KEY, queryFn: () => null, staleTime: Infinity }),
  };
});
vi.mock("@/features/sites/use-sites", () => ({
  useSite: () => ({ data: { url: "https://shop.example.com" } }),
}));

const TENANT = "00000000-0000-0000-0000-0000000000aa";
function meWithRole(role: "operator" | "viewer"): Me {
  return {
    user: { id: "u", email: "a@b.test", name: "A" },
    active_tenant_id: TENANT,
    memberships: [{ tenant_id: TENANT, role, tenant_name: "Acme" }],
  } as unknown as Me;
}

function renderTab(role: "operator" | "viewer" = "operator") {
  const queryClient = createTestQueryClient();
  queryClient.setQueryData(ME_KEY, meWithRole(role));
  const rootRoute = createRootRoute({});
  type UpdateOptions = Parameters<typeof ContentRoute.update>[0];
  const contentRoute = ContentRoute.update({
    id: "/sites/$siteId/content",
    path: "/sites/$siteId/content",
    getParentRoute: () => rootRoute,
  } as unknown as UpdateOptions);
  const router = createRouter({
    routeTree: rootRoute.addChildren([contentRoute]),
    history: createMemoryHistory({ initialEntries: [`/sites/${SITE_ID}/content`] }),
  });
  renderWithProviders(<RouterProvider router={router} />, { queryClient });
  return queryClient;
}

beforeEach(() => {
  for (const m of [getReadiness, refreshReadiness, getEditing, listReqs, getInv]) m.mockReset();
  getReadiness.mockResolvedValue(okResult(readiness()));
  getEditing.mockResolvedValue(okResult({ site_id: SITE_ID, enabled: true }));
  listReqs.mockResolvedValue(okResult({ requests: [], limit: 50, offset: 0 }));
  getInv.mockResolvedValue(
    okResult({
      state: "ok",
      min_agent_version: "0.62.0",
      last_checked_at: null,
      titles_included: true,
      truncated: false,
      next_after_post_id: null,
      pages: [],
    }),
  );
});

afterEach(() => {
  vi.useRealTimers();
});

async function card() {
  return within(await screen.findByRole("region", { name: "AI readiness" }));
}

describe("loading and load failure", () => {
  it("shows a skeleton while the card loads", async () => {
    getReadiness.mockReturnValue(new Promise(() => {}));
    renderTab();
    expect(await screen.findByRole("status", { name: "Loading AI readiness" })).toBeInTheDocument();
  });

  it("shows PageError with the server's message and a Retry that recovers", async () => {
    getReadiness.mockResolvedValueOnce(failResult(500, "internal_error", "internal server error"));
    renderTab();
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Could not load AI readiness.");
    expect(alert).toHaveTextContent("internal server error");

    fireEvent.click(within(alert).getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("region", { name: "AI readiness" })).toBeInTheDocument();
    expect(getReadiness).toHaveBeenCalledTimes(2);
  });

  it("explains a missing site in plain words instead of echoing 'site not found'", async () => {
    getReadiness.mockResolvedValue(failResult(404, "site_not_found", "site not found"));
    renderTab();
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Could not load AI readiness.");
    expect(alert).toHaveTextContent("This site is not connected to WPMgr, so there is nothing to check yet.");
    expect(alert).not.toHaveTextContent("site not found");
  });
});

describe("a ready site", () => {
  it("renders the title, sub-line, freshness line, status line and four passing rows", async () => {
    renderTab();
    const c = await card();
    expect(c.getByRole("heading", { name: "AI readiness" })).toBeInTheDocument();
    expect(c.getByText("Whether an AI assistant connected to WPMgr can work on this site.")).toBeInTheDocument();
    expect(c.getByText("Site details from 5m ago. Tool list from 2h ago.")).toBeInTheDocument();
    expect(c.getByTestId("ai-readiness-status")).toHaveTextContent(
      "Ready. Everything the AI needs on this site is in place.",
    );

    expect(c.getByText("WordPress 7.1 or later")).toBeInTheDocument();
    expect(c.getByText("WordPress 7.1.")).toBeInTheDocument();
    expect(c.getByText("WordPress abilities")).toBeInTheDocument();
    expect(c.getByText("Available.")).toBeInTheDocument();
    expect(c.getByText("WPMgr plugin 0.61.158 or later")).toBeInTheDocument();
    expect(c.getByText("Version 0.61.159.")).toBeInTheDocument();
    expect(c.getByText("AI page creation")).toBeInTheDocument();
    expect(c.getAllByRole("img", { name: "Passed" })).toHaveLength(4);
    expect(c.queryAllByRole("img", { name: "Needs fixing" })).toHaveLength(0);
  });

  it("says a builder that is not installed has nothing to check, with no 'coming' note", async () => {
    renderTab();
    const c = await card();
    expect(c.getByText("Elementor is not installed on this site. Nothing to check.")).toBeInTheDocument();
    expect(c.getByText("Bricks is not installed on this site. Nothing to check.")).toBeInTheDocument();
    expect(c.getAllByText("Not installed")).toHaveLength(2);
    expect(c.queryByText(/WPMgr cannot build/)).not.toBeInTheDocument();
  });

  it("sits above the AI editing section", async () => {
    renderTab();
    const readinessHeading = (await card()).getByRole("heading", { name: "AI readiness" });
    const editingHeading = await screen.findByRole("heading", { name: "AI editing" });
    expect(
      readinessHeading.compareDocumentPosition(editingHeading) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });
});

describe("a site with things to fix", () => {
  function twoFixes(): SiteAiReadiness {
    return readiness({
      status: "needs_attention",
      fix_count: 2,
      groups: [
        baseGroup(
          baseChecks({
            wp_version: chk("wp_version", "fail", null, "7.0.9"),
            content_editing: chk("content_editing", "fail"),
          }),
        ),
        builderGroup("elementor", { installed: false }),
        builderGroup("bricks", { installed: false }),
      ],
    });
  }

  it("counts them, and marks exactly those rows red", async () => {
    getReadiness.mockResolvedValue(okResult(twoFixes()));
    renderTab();
    const c = await card();
    expect(c.getByTestId("ai-readiness-status")).toHaveTextContent(
      "2 things to fix before the AI can work here.",
    );
    expect(c.getAllByRole("img", { name: "Needs fixing" })).toHaveLength(2);
    expect(c.getAllByRole("img", { name: "Passed" })).toHaveLength(2);
    expect(
      c.getByText("WordPress 7.0.9. Builder tools need 7.1 or later. Update WordPress on this site."),
    ).toBeInTheDocument();
  });

  it("uses the singular for one", async () => {
    getReadiness.mockResolvedValue(
      okResult(
        readiness({
          status: "needs_attention",
          fix_count: 1,
          groups: [
            baseGroup(baseChecks({ content_editing: chk("content_editing", "fail") })),
            builderGroup("elementor", { installed: false }),
            builderGroup("bricks", { installed: false }),
          ],
        }),
      ),
    );
    renderTab();
    expect((await card()).getByTestId("ai-readiness-status")).toHaveTextContent(
      "1 thing to fix before the AI can work here.",
    );
  });

  it("tells an operator to turn AI page creation on below, and a viewer to ask an operator", async () => {
    getReadiness.mockResolvedValue(okResult(twoFixes()));
    renderTab("operator");
    expect(await screen.findByText("Off. Turn it on in AI editing, below.")).toBeInTheDocument();
  });

  it("does not send a viewer to a button they do not have", async () => {
    getReadiness.mockResolvedValue(okResult(twoFixes()));
    renderTab("viewer");
    expect(await screen.findByText("Off. An operator can turn it on in AI editing, below.")).toBeInTheDocument();
    expect(screen.queryByText("Off. Turn it on in AI editing, below.")).not.toBeInTheDocument();
  });
});

describe("a site whose checks have not run", () => {
  it("shows grey 'Not checked' rows and never a red cross", async () => {
    getReadiness.mockResolvedValue(
      okResult(
        readiness({
          status: "incomplete",
          fix_count: 0,
          metadata_as_of: null,
          abilities_as_of: null,
          groups: [
            baseGroup([
              chk("wp_version", "unknown", "not_reported"),
              chk("abilities_api", "unknown", "inventory_never_run"),
              chk("agent_version", "unknown", "not_reported"),
              chk("content_editing", "pass"),
            ]),
            builderGroup("elementor", {
              installed: true,
              version: "4.3.4",
              checks: [
                chk("elementor_version", "pass", null, "4.3.4"),
                chk("elementor_mcp_switch", "unknown", "inventory_never_run"),
                chk("elementor_atomic", "unknown", "agent_too_old_for_fact"),
              ],
            }),
            builderGroup("bricks", { installed: false }),
          ],
        }),
      ),
    );
    renderTab();
    const c = await card();
    expect(c.getByTestId("ai-readiness-status")).toHaveTextContent("Some checks have not run yet.");
    expect(c.getByText("Site details not reported yet. Tool list not read yet.")).toBeInTheDocument();
    expect(c.queryAllByRole("img", { name: "Needs fixing" })).toHaveLength(0);
    expect(c.getAllByRole("img", { name: "Not checked" })).toHaveLength(5);
    expect(c.getByText("WordPress has not reported its version yet.")).toBeInTheDocument();
    expect(c.getByText("Update the WPMgr plugin to 0.61.159 or later to check this.")).toBeInTheDocument();
  });

  it("renders codes it does not know as grey, never as a failure", async () => {
    const odd = readiness({
      status: "needs_attention",
      fix_count: 1,
      groups: [
        baseGroup([
          // A check id, a state and a reason from a newer control plane.
          { id: "elementor_role_access", state: "fail", reason: null, observed: null },
          { id: "wp_version", state: "exploded", reason: null, observed: null },
          { id: "abilities_api", state: "unknown", reason: "brand_new_reason", observed: null },
          chk("agent_version", "pass", null, "0.61.159"),
          chk("content_editing", "pass"),
        ] as unknown as SiteAiReadiness["groups"][number]["checks"]),
        builderGroup("elementor", { installed: false }),
        builderGroup("bricks", { installed: false }),
      ],
    });
    getReadiness.mockResolvedValue(okResult(odd));
    renderTab();
    const c = await card();
    expect(c.queryAllByRole("img", { name: "Needs fixing" })).toHaveLength(0);
    expect(c.getAllByRole("img", { name: "Not checked" })).toHaveLength(3);
    expect(c.getByText("Another check")).toBeInTheDocument();
  });
});

describe("builder groups", () => {
  it("shows the version, the rows and the 'coming' note for an installed builder", async () => {
    getReadiness.mockResolvedValue(
      okResult(
        readiness({
          groups: [baseGroup(), elementorAllPassing("4.3.4"), bricksAllPassing("2.4.1")],
        }),
      ),
    );
    renderTab();
    const c = await card();
    expect(c.getByText("Version 4.3.4")).toBeInTheDocument();
    expect(c.getByText("Version 2.4.1")).toBeInTheDocument();
    expect(c.getByText("Elementor 4.3.4, active.")).toBeInTheDocument();
    expect(c.getByText("Bricks 2.4.1, active theme.")).toBeInTheDocument();
    expect(
      c.getByText("WPMgr cannot build Elementor pages yet. These checks show whether this site will be ready."),
    ).toBeInTheDocument();
    expect(
      c.getByText("WPMgr cannot build Bricks pages yet. These checks show whether this site will be ready."),
    ).toBeInTheDocument();
    expect(c.getByText("On. Bricks advises keeping this off on live sites while it is experimental.")).toBeInTheDocument();
  });

  it("drops the 'coming' note once WPMgr can build with the builder", async () => {
    getReadiness.mockResolvedValue(
      okResult(
        readiness({
          groups: [
            baseGroup(),
            builderGroup("elementor", {
              installed: true,
              version: "4.3.4",
              support: "available",
              checks: [
                chk("elementor_version", "pass", null, "4.3.4"),
                chk("elementor_mcp_switch", "pass"),
                chk("elementor_atomic", "pass"),
              ],
            }),
            builderGroup("bricks", { installed: false }),
          ],
        }),
      ),
    );
    renderTab();
    const c = await card();
    expect(c.getByText("Elementor 4.3.4, active.")).toBeInTheDocument();
    expect(c.queryByText(/WPMgr cannot build Elementor pages yet/)).not.toBeInTheDocument();
  });

  it("names a child theme the plugin is too old to see through", async () => {
    getReadiness.mockResolvedValue(
      okResult(
        readiness({
          status: "incomplete",
          groups: [
            baseGroup(),
            builderGroup("elementor", { installed: false }),
            builderGroup("bricks", {
              installed: true,
              version: "2.4.1",
              checks: [
                chk("bricks_version", "unknown", "agent_too_old_for_fact", "2.4.1"),
                chk("bricks_abilities", "unknown", "needs_bricks"),
              ],
            }),
          ],
        }),
      ),
    );
    renderTab();
    const c = await card();
    expect(c.getByText("Update the WPMgr plugin to 0.61.159 or later to check this.")).toBeInTheDocument();
    expect(c.getByText("Needs Bricks 2.4 or later first.")).toBeInTheDocument();
    expect(c.queryAllByRole("img", { name: "Needs fixing" })).toHaveLength(0);
  });
});

describe("warnings", () => {
  it("shows an amber notice for each open AI connection point, and none when there is none", async () => {
    getReadiness.mockResolvedValue(
      okResult(
        readiness({
          warnings: [{ code: ADAPTER_WARNING }, { code: ELEMENTOR_WARNING }],
          groups: [baseGroup(), elementorAllPassing(), builderGroup("bricks", { installed: false })],
        }),
      ),
    );
    renderTab();
    const c = await card();
    const notes = c.getAllByRole("note");
    expect(notes).toHaveLength(2);
    expect(notes[0]).toHaveTextContent(
      "The WordPress MCP Adapter plugin is active on this site. It lets any logged-in user of the site connect an AI tool directly, without WPMgr's approvals, undo or audit. WPMgr does not need it. If nobody uses it, consider deactivating it.",
    );
    expect(notes[1]).toHaveTextContent(
      "Elementor's AI tools switch also opens Elementor's own AI connection point on this site. Any logged-in user can connect an AI tool to it, without WPMgr's approvals, undo or audit.",
    );
    for (const note of notes) expect(note.textContent?.toLowerCase()).not.toContain("install");
    // A warning is advice: the status line is untouched.
    expect(c.getByTestId("ai-readiness-status")).toHaveTextContent("Ready.");
  });

  it("shows no notice for a site with no open connection point", async () => {
    renderTab();
    const c = await card();
    expect(c.queryAllByRole("note")).toHaveLength(0);
  });
});

describe("Check again", () => {
  async function pressCheckAgain(role: "operator" | "viewer" = "operator") {
    renderTab(role);
    const c = await card();
    const button = c.getByRole("button", { name: "Check again" });
    return { c, button };
  }

  it("is there for a viewer, who may read the site", async () => {
    const { button } = await pressCheckAgain("viewer");
    expect(button).toBeEnabled();
  });

  it("posts an empty JSON body to the site's refresh route and says what happened", async () => {
    let resolve!: (v: unknown) => void;
    refreshReadiness.mockReturnValue(new Promise((r) => (resolve = r)));
    const { c, button } = await pressCheckAgain();

    fireEvent.click(button);
    const checking = await c.findByRole("button", { name: "Checking…" });
    expect(checking).toBeDisabled();
    expect(refreshReadiness).toHaveBeenCalledWith({ path: { siteId: SITE_ID }, body: {} });

    await act(async () => {
      resolve(okResult({ metadata: true, abilities: true }));
      await Promise.resolve();
    });
    expect(
      await c.findByText("Asked the site to report again. Results update within a couple of minutes."),
    ).toBeInTheDocument();
    expect(c.getByRole("button", { name: "Check again" })).toBeEnabled();
  });

  it("refetches the card at once after a 202", async () => {
    refreshReadiness.mockResolvedValue(okResult({ metadata: true, abilities: false }));
    const { c, button } = await pressCheckAgain();
    expect(getReadiness).toHaveBeenCalledTimes(1);
    fireEvent.click(button);
    await c.findByText(/Asked the site to report again/);
    expect(getReadiness.mock.calls.length).toBeGreaterThanOrEqual(2);
  });

  it("refetches every 15 seconds, exactly 8 times, and then stops", async () => {
    refreshReadiness.mockResolvedValue(okResult({ metadata: true, abilities: true }));
    const { c, button } = await pressCheckAgain();
    await c.findByRole("button", { name: "Check again" });

    vi.useFakeTimers();
    const settle = async (ms: number) => {
      await act(async () => {
        await vi.advanceTimersByTimeAsync(ms);
      });
    };

    fireEvent.click(button);
    await settle(0);
    expect(refreshReadiness).toHaveBeenCalledTimes(1);
    const baseline = getReadiness.mock.calls.length;

    for (let i = 1; i <= POLL_LIMIT; i += 1) {
      await settle(POLL_INTERVAL_MS);
      expect(getReadiness).toHaveBeenCalledTimes(baseline + i);
    }
    // The window is spent: minutes later, still no further request.
    await settle(POLL_INTERVAL_MS * 6);
    expect(getReadiness).toHaveBeenCalledTimes(baseline + POLL_LIMIT);
  });

  it("says the site could not be reached on a 409 site_unreachable, not the server's wording", async () => {
    refreshReadiness.mockResolvedValue(
      failResult(409, "site_unreachable", "site agent heartbeat is stale; cannot refresh now"),
    );
    const { c, button } = await pressCheckAgain();
    fireEvent.click(button);
    const alert = await c.findByRole("alert");
    expect(alert).toHaveTextContent(
      "WPMgr could not reach this site, so nothing was checked. Try again when the site is back online.",
    );
    expect(alert).not.toHaveTextContent("heartbeat");
  });

  it("says so on a 403", async () => {
    refreshReadiness.mockResolvedValue(
      failResult(403, "insufficient_permission", "your role does not permit this action"),
    );
    const { c, button } = await pressCheckAgain();
    fireEvent.click(button);
    expect(await c.findByRole("alert")).toHaveTextContent("You do not have permission to run this check.");
  });

  it("passes the server's message through on any other failure", async () => {
    refreshReadiness.mockResolvedValue(
      failResult(
        503,
        "ai_readiness_refresh_unavailable",
        "Asking the site to report again is not available on this install.",
      ),
    );
    const { c, button } = await pressCheckAgain();
    fireEvent.click(button);
    expect(await c.findByRole("alert")).toHaveTextContent(
      "Could not ask the site to report. Asking the site to report again is not available on this install.",
    );
  });

  it("handles a request that never got an answer", async () => {
    refreshReadiness.mockRejectedValue(new TypeError("Failed to fetch"));
    const { c, button } = await pressCheckAgain();
    fireEvent.click(button);
    expect(await c.findByRole("alert")).toHaveTextContent(
      "Could not ask the site to report. Check your connection and try again.",
    );
    expect(c.getByRole("button", { name: "Check again" })).toBeEnabled();
  });

  it("does not poll after a refused check", async () => {
    refreshReadiness.mockResolvedValue(failResult(409, "site_unreachable", "site is not enrolled with an agent"));
    const { c, button } = await pressCheckAgain();
    vi.useFakeTimers();
    fireEvent.click(button);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    const before = getReadiness.mock.calls.length;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 3);
    });
    expect(getReadiness).toHaveBeenCalledTimes(before);
    expect(c.getByRole("alert")).toBeInTheDocument();
  });
});

describe("floors", () => {
  it("writes the floors the control plane sent, not numbers of its own", async () => {
    getReadiness.mockResolvedValue(
      okResult(readiness({ floors: { ...FLOORS, wp: "7.2", agent: "0.61.200" } })),
    );
    renderTab();
    const c = await card();
    expect(c.getByText("WordPress 7.2 or later")).toBeInTheDocument();
    expect(c.getByText("WPMgr plugin 0.61.200 or later")).toBeInTheDocument();
  });
});
