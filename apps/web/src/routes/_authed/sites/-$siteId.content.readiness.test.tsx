import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import type { AiReadinessCheck, Me, SiteAiReadiness } from "@wpmgr/api";

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import { cellFor } from "@/features/ai-readiness/readiness-cell-model";
import { aiReadinessKeys, useAiReadinessRollup } from "@/features/ai-readiness/use-ai-readiness";
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
  fleetSite,
  okResult,
  readiness,
} from "@/features/ai-readiness/readiness-fixtures";
import { siteAiMode } from "@/features/ai-trust/ai-trust-fixtures";

import { Route as ContentRoute } from "./$siteId.content";

// The AI readiness card, mounted through the real Content route, a real router
// and a real QueryClient. Only the network edge (the generated SDK functions)
// is stubbed. Shapes and codes asserted here:
//   - the readiness body: apps/api/internal/aireadiness/dto.go (siteReadinessDTO)
//     and SiteAiReadiness in packages/openapi-client/src/generated/types.gen.ts;
//   - 409 site_unreachable, 503 ai_readiness_refresh_unavailable, 404
//     site_not_found: apps/api/internal/aireadiness/service.go;
//   - 403 insufficient_permission: apps/api/internal/authz/middleware.go;
//   - the error envelope {code, message}: apps/api/internal/server/httpx/respond.go;
//   - the site's AI mode, read by the AI editing card on the same route:
//     GET /api/v1/sites/{siteId}/ai/mode, apps/api/internal/aitrust/handler.go
//     (getMode), body built by the siteAiMode fixture from aitrust/dto.go.

// The refetch window after "Check again" is part of the owner's contract: every
// 15 seconds, at most 8 times. Written out here, not imported, so the test pins
// the contract instead of following whatever the implementation says.
const POLL_INTERVAL_MS = 15_000;
const POLL_LIMIT = 8;

const getReadiness = vi.fn();
const getFleetReadiness = vi.fn();
const refreshReadiness = vi.fn();
const getEditing = vi.fn();
const enableEditing = vi.fn();
const listReqs = vi.fn();
const getInv = vi.fn();
// The AI editing card below the readiness card reads the site's AI mode. It is
// stubbed with the rest of the network edge, so a Retry button on the page is
// the readiness card's and never a mode request that went to a real fetch.
const getAiMode = vi.fn();

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return {
    ...actual,
    getSiteAiReadiness: (...a: unknown[]): unknown => getReadiness(...a),
    getFleetAiReadiness: (...a: unknown[]): unknown => getFleetReadiness(...a),
    refreshSiteAiReadiness: (...a: unknown[]): unknown => refreshReadiness(...a),
    getSiteContentEditing: (...a: unknown[]): unknown => getEditing(...a),
    enableSiteContentEditing: (...a: unknown[]): unknown => enableEditing(...a),
    listSiteAbilityRequests: (...a: unknown[]): unknown => listReqs(...a),
    getSiteContentInventory: (...a: unknown[]): unknown => getInv(...a),
    getSiteAiMode: (...a: unknown[]): unknown => getAiMode(...a),
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

/**
 * A collaborator who reaches the site through a share: no organisation
 * membership, scope "site", and the share's role (Me in the generated client).
 */
function siteScopedMe(role: "operator" | "viewer"): Me {
  return {
    user: { id: "u", email: "a@b.test", name: "A" },
    active_tenant_id: TENANT,
    memberships: [],
    scope: "site",
    role,
  } as unknown as Me;
}

/**
 * Stands in for the Sites list while the check runs: it reads the fleet rollup
 * through the real hook and the real cell model, as the AI column does. The
 * list is a different route and is never mounted together with the Content
 * tab, so a test that wants to see the rollup move has to put a reader of it
 * next to the card.
 */
function FleetReader() {
  const cell = cellFor(useAiReadinessRollup(), SITE_ID);
  return <p data-testid="fleet-reader">{cell.kind === "result" ? cell.label : cell.kind}</p>;
}

function renderTab(who: "operator" | "viewer" | Me = "operator", opts: { fleetReader?: boolean } = {}) {
  const queryClient = createTestQueryClient();
  queryClient.setQueryData(ME_KEY, typeof who === "string" ? meWithRole(who) : who);
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
  renderWithProviders(
    <>
      <RouterProvider router={router} />
      {opts.fleetReader ? <FleetReader /> : null}
    </>,
    { queryClient },
  );
  return queryClient;
}

beforeEach(() => {
  for (const m of [getReadiness, getFleetReadiness, refreshReadiness, getEditing, enableEditing, listReqs, getInv, getAiMode]) {
    m.mockReset();
  }
  getReadiness.mockResolvedValue(okResult(readiness()));
  getAiMode.mockResolvedValue(okResult(siteAiMode(SITE_ID)));
  getFleetReadiness.mockResolvedValue(okResult({ sites: [fleetSite({ site_id: SITE_ID })] }));
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

/** The icon beside the card's status line. Its colour is the colour of the header. */
function headerIcon(c: Awaited<ReturnType<typeof card>>): Element {
  const icon = c.getByTestId("ai-readiness-status").parentElement?.querySelector("svg");
  if (!icon) throw new Error("the status line has no icon");
  return icon;
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

  it("keeps the loaded checklist on screen when a later refetch fails", async () => {
    refreshReadiness.mockResolvedValue(okResult({ metadata: true, abilities: true }));
    const queryClient = renderTab();
    const c = await card();
    expect(c.getByText("WordPress 7.1.")).toBeInTheDocument();
    // The AI editing card below has read its mode. Without this wait, the
    // absence of a Retry button further down could be that card still loading.
    await screen.findByTestId("ai-mode-card");

    // "Check again" refetches the card as soon as the site has been asked, and
    // that read fails.
    getReadiness.mockResolvedValue(failResult(500, "internal_error", "internal server error"));
    fireEvent.click(c.getByRole("button", { name: "Check again" }));

    // Wait until the query is in error with its checklist still in hand, so the
    // assertions below look at that state and not at the moment before it.
    await waitFor(() =>
      expect(queryClient.getQueryState(aiReadinessKeys.site(SITE_ID))?.status).toBe("error"),
    );
    expect(queryClient.getQueryData(aiReadinessKeys.site(SITE_ID))).toBeDefined();

    expect(screen.queryByText("Could not load AI readiness.")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    expect(c.getByTestId("ai-readiness-status")).toHaveTextContent("Ready.");
    expect(c.getByText("WordPress 7.1.")).toBeInTheDocument();
    expect(c.getAllByRole("img", { name: "Passed" })).toHaveLength(4);
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
    // The header is red when the control plane says there is something to fix:
    // the control for the "header is not red" checks on the Bricks AI row below.
    expect(headerIcon(c)).toHaveClass("text-[var(--color-destructive)]");
    expect(headerIcon(c)).not.toHaveClass("text-[var(--color-success)]");
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

describe("a reason the card cannot read", () => {
  it("renders a reason it does not know on a check it does know as grey, never red", async () => {
    // A fail with a reason from a newer control plane, on a check this page knows.
    const newer = readiness({
      status: "needs_attention",
      fix_count: 1,
      groups: [
        baseGroup(),
        builderGroup("elementor", {
          installed: true,
          version: "9.0",
          checks: [
            { id: "elementor_version", state: "fail", reason: "too_new", observed: "9.0" },
            chk("elementor_mcp_switch", "not_applicable", "needs_elementor"),
            chk("elementor_atomic", "not_applicable", "needs_elementor"),
          ] as unknown as AiReadinessCheck[],
        }),
        builderGroup("bricks", { installed: false }),
      ],
    });
    getReadiness.mockResolvedValue(okResult(newer));
    renderTab();
    const c = await card();
    const row = within(c.getByText("Elementor 4.3 or later").closest("li") as HTMLElement);
    expect(row.getByRole("img", { name: "Not checked" })).toBeInTheDocument();
    expect(row.getByText("Not checked yet.")).toBeInTheDocument();
    expect(row.queryByRole("img", { name: "Needs fixing" })).not.toBeInTheDocument();
    expect(c.queryAllByRole("img", { name: "Needs fixing" })).toHaveLength(0);
  });

  it("shows a pre-release WordPress build as a fix, in its own words", async () => {
    const prerelease = {
      id: "wp_version",
      state: "fail",
      reason: "prerelease_build",
      observed: "7.1-beta2",
    } as unknown as AiReadinessCheck;
    getReadiness.mockResolvedValue(
      okResult(
        readiness({
          status: "needs_attention",
          fix_count: 1,
          groups: [
            baseGroup(baseChecks({ wp_version: prerelease })),
            builderGroup("elementor", { installed: false }),
            builderGroup("bricks", { installed: false }),
          ],
        }),
      ),
    );
    renderTab();
    const c = await card();
    const row = within(c.getByText("WordPress 7.1 or later").closest("li") as HTMLElement);
    expect(row.getByRole("img", { name: "Needs fixing" })).toBeInTheDocument();
    expect(
      row.getByText(
        "This is a development or pre-release build of WordPress. WPMgr's AI tools need a released version, 7.1 or later.",
      ),
    ).toBeInTheDocument();
    expect(c.getAllByRole("img", { name: "Needs fixing" })).toHaveLength(1);
    expect(c.getByTestId("ai-readiness-status")).toHaveTextContent(
      "1 thing to fix before the AI can work here.",
    );
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
    // The Bricks AI row is derived from the tool list and unconfirmed, so a
    // pass is never shown as a confirmed On.
    expect(
      c.getByText(
        "Derived, unconfirmed. Bricks tools are listed on this site. Not yet checked on a licensed Bricks install.",
      ),
    ).toBeInTheDocument();
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

describe("a builder that is installed but not active", () => {
  // The control plane sends the version row as not_applicable with reason
  // inactive (observed keeps the version), the rows that wait on it as
  // not_applicable needs_*, and counts none of them as a fix: packages/openapi/
  // openapi.yaml AIReadinessCheck, apps/api/internal/aireadiness/evaluate.go.
  function inactiveBuilder(builder: "elementor" | "bricks", version: string): SiteAiReadiness {
    const dependsOn = builder === "elementor" ? "needs_elementor" : "needs_bricks";
    const checks =
      builder === "elementor"
        ? [
            chk("elementor_version", "not_applicable", "inactive", version),
            chk("elementor_mcp_switch", "not_applicable", dependsOn),
            chk("elementor_atomic", "not_applicable", dependsOn),
          ]
        : [
            chk("bricks_version", "not_applicable", "inactive", version),
            chk("bricks_abilities", "not_applicable", dependsOn),
          ];
    return readiness({
      status: "ready",
      fix_count: 0,
      groups: [
        baseGroup(),
        builderGroup("elementor", builder === "elementor" ? { installed: true, version, checks } : { installed: false }),
        builderGroup("bricks", builder === "bricks" ? { installed: true, version, checks } : { installed: false }),
      ],
    });
  }

  it("shows Elementor as grey 'Installed, not active.' and counts no fix", async () => {
    getReadiness.mockResolvedValue(okResult(inactiveBuilder("elementor", "4.3.4")));
    renderTab();
    const c = await card();
    expect(c.getByText("Version 4.3.4")).toBeInTheDocument();
    expect(c.getByText("Installed, not active.")).toBeInTheDocument();
    expect(c.getByRole("img", { name: "Not active" })).toBeInTheDocument();
    expect(c.queryAllByRole("img", { name: "Needs fixing" })).toHaveLength(0);
    expect(c.getByTestId("ai-readiness-status")).toHaveTextContent("Ready.");
  });

  it("says the rows that wait on Elementor need it active, not a newer version", async () => {
    getReadiness.mockResolvedValue(okResult(inactiveBuilder("elementor", "4.3.4")));
    renderTab();
    const c = await card();
    expect(c.getAllByText("Needs Elementor to be active first.")).toHaveLength(2);
    expect(c.getAllByRole("img", { name: "Not applicable" })).toHaveLength(2);
    expect(c.queryByText(/Needs Elementor 4\.3 or later first/)).not.toBeInTheDocument();
  });

  it("shows Bricks as grey 'Installed, not active.' and its AI row as waiting for it", async () => {
    getReadiness.mockResolvedValue(okResult(inactiveBuilder("bricks", "2.4.1")));
    renderTab();
    const c = await card();
    expect(c.getByText("Version 2.4.1")).toBeInTheDocument();
    expect(c.getByText("Installed, not active.")).toBeInTheDocument();
    expect(c.getByText("Needs Bricks to be active first.")).toBeInTheDocument();
    expect(c.queryAllByRole("img", { name: "Needs fixing" })).toHaveLength(0);
    expect(c.getByTestId("ai-readiness-status")).toHaveTextContent("Ready.");
  });

  it("still marks a builder that is active but too old red, and asks for the newer version", async () => {
    getReadiness.mockResolvedValue(
      okResult(
        readiness({
          status: "needs_attention",
          fix_count: 1,
          groups: [
            baseGroup(),
            builderGroup("elementor", {
              installed: true,
              version: "4.2.9",
              checks: [
                chk("elementor_version", "fail", "too_old", "4.2.9"),
                chk("elementor_mcp_switch", "not_applicable", "needs_elementor"),
                chk("elementor_atomic", "not_applicable", "needs_elementor"),
              ],
            }),
            builderGroup("bricks", { installed: false }),
          ],
        }),
      ),
    );
    renderTab();
    const c = await card();
    expect(c.getAllByRole("img", { name: "Needs fixing" })).toHaveLength(1);
    expect(c.getByText("Elementor 4.2.9. Its AI tools need 4.3 or later. Update Elementor.")).toBeInTheDocument();
    expect(c.getAllByText("Needs Elementor 4.3 or later first.")).toHaveLength(2);
    expect(c.queryByText("Installed, not active.")).not.toBeInTheDocument();
    expect(c.queryByText(/to be active first/)).not.toBeInTheDocument();
  });
});

describe("the Bricks AI row is derived and unconfirmed", () => {
  // bricks_abilities is a pass when a tool list read found a Bricks tool and a
  // fail when it found none (evaluate.go switchCheck). Neither is confirmed
  // against a licensed Bricks install, so neither reads as On or Off.
  //
  // This is the payload for a site where that row is the only one that is not
  // a pass: status ready and fix_count 0, written out here rather than left to
  // the fixture's defaults. The row keeps its state in the group but is never
  // counted (Check.unconfirmed and the loop that skips it in evaluate.go;
  // Result.Failing in model.go leaves it out). The header reads status and
  // fix_count, so a grey row never sits under a header that counts a fix.
  function bricksWith(abilities: ReturnType<typeof chk>): SiteAiReadiness {
    return readiness({
      status: "ready",
      fix_count: 0,
      groups: [
        baseGroup(),
        builderGroup("elementor", { installed: false }),
        builderGroup("bricks", {
          installed: true,
          version: "2.4.1",
          checks: [chk("bricks_version", "pass", null, "2.4.1"), abilities],
        }),
      ],
    });
  }

  it("is grey with a short note when Bricks tools are listed", async () => {
    getReadiness.mockResolvedValue(okResult(bricksWith(chk("bricks_abilities", "pass"))));
    renderTab();
    const c = await card();
    const row = within(c.getByText("Bricks AI abilities").closest("li") as HTMLElement);
    expect(row.getByRole("img", { name: "Unconfirmed" })).toBeInTheDocument();
    expect(
      row.getByText(
        "Derived, unconfirmed. Bricks tools are listed on this site. Not yet checked on a licensed Bricks install.",
      ),
    ).toBeInTheDocument();
    expect(row.queryByRole("img", { name: "Passed" })).not.toBeInTheDocument();
  });

  it("is grey, not a red cross, when none are listed", async () => {
    getReadiness.mockResolvedValue(okResult(bricksWith(chk("bricks_abilities", "fail"))));
    renderTab();
    const c = await card();
    const row = within(c.getByText("Bricks AI abilities").closest("li") as HTMLElement);
    expect(row.getByRole("img", { name: "Unconfirmed" })).toBeInTheDocument();
    expect(row.queryByRole("img", { name: "Needs fixing" })).not.toBeInTheDocument();
    expect(
      row.getByText(
        "Derived, unconfirmed. No Bricks tools are listed on this site. Not yet checked on a licensed Bricks install.",
      ),
    ).toBeInTheDocument();
    // No row anywhere on the card is red because of it.
    expect(c.queryAllByRole("img", { name: "Needs fixing" })).toHaveLength(0);
  });

  it.each(["pass", "fail"] as const)(
    "leaves the header green when the row is a derived %s: ready, nothing to fix",
    async (state) => {
      getReadiness.mockResolvedValue(okResult(bricksWith(chk("bricks_abilities", state))));
      renderTab();
      const c = await card();
      const status = c.getByTestId("ai-readiness-status");
      expect(status).toHaveTextContent("Ready. Everything the AI needs on this site is in place.");
      expect(status).not.toHaveTextContent(/to fix/);
      expect(headerIcon(c)).toHaveClass("text-[var(--color-success)]");
      expect(headerIcon(c)).not.toHaveClass("text-[var(--color-destructive)]");
      // And no row under it is red either.
      expect(c.queryAllByRole("img", { name: "Needs fixing" })).toHaveLength(0);
    },
  );

  it("does not turn the Elementor switch grey: only the Bricks row is a derivation", async () => {
    getReadiness.mockResolvedValue(
      okResult(
        readiness({
          groups: [baseGroup(), elementorAllPassing(), builderGroup("bricks", { installed: false })],
        }),
      ),
    );
    renderTab();
    const c = await card();
    expect(c.queryByRole("img", { name: "Unconfirmed" })).not.toBeInTheDocument();
    expect(c.getAllByRole("img", { name: "Passed" }).length).toBeGreaterThan(4);
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

  // The refresh route needs site.content.refresh (operator and above, or a
  // site-scoped operator share): apps/api/internal/aireadiness/handler.go
  // Register and PermSiteContentRefresh in apps/api/internal/authz/role.go. The
  // Content route hands the card the same gate through canWriteSiteContext, so
  // these go through the real route with the principals that gate admits and
  // refuses. The first table is the positive control for the second: the same
  // render shows the button when it is allowed.
  it.each([
    ["an organisation operator", meWithRole("operator")],
    ["a site-scoped operator", siteScopedMe("operator")],
  ])("is there for %s", async (_who, me) => {
    renderTab(me);
    const c = await card();
    expect(await c.findByRole("button", { name: "Check again" })).toBeEnabled();
  });

  it.each([
    ["an organisation viewer", meWithRole("viewer")],
    ["a site-scoped viewer", siteScopedMe("viewer")],
  ])("is not there for %s, who may read the card but not ask for a refresh", async (_who, me) => {
    renderTab(me);
    const c = await card();
    // The card has loaded: its status line is on screen, so the button is
    // absent and not merely late.
    expect(c.getByTestId("ai-readiness-status")).toBeInTheDocument();
    expect(c.queryByRole("button", { name: /Check again|Checking/ })).not.toBeInTheDocument();
    expect(refreshReadiness).not.toHaveBeenCalled();
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

describe("Check again keeps the Sites list column current", () => {
  // "Check again" marks the fleet rollup stale once, when the request is
  // accepted. The site has not reported yet at that point, so a rollup read
  // right after it still carries the old answer. A reader of the rollup sits
  // beside the card (see FleetReader) and must see the new answer once the
  // card's poll has seen the results.
  const BEFORE_DETAILS = "2026-10-09T10:00:00.000Z";
  const BEFORE_TOOLS = "2026-10-09T10:01:00.000Z";
  const AFTER_DETAILS = "2026-10-09T10:05:00.000Z";
  const AFTER_TOOLS = "2026-10-09T10:05:30.000Z";

  function oneFix(details: string, tools: string): SiteAiReadiness {
    return readiness({
      status: "needs_attention",
      fix_count: 1,
      metadata_as_of: details,
      abilities_as_of: tools,
      groups: [
        baseGroup(baseChecks({ content_editing: chk("content_editing", "fail") })),
        builderGroup("elementor", { installed: false }),
        builderGroup("bricks", { installed: false }),
      ],
    });
  }
  const allClear = (details: string, tools: string) =>
    readiness({ metadata_as_of: details, abilities_as_of: tools });

  const FLEET_ONE_FIX = okResult({
    sites: [fleetSite({ site_id: SITE_ID, status: "needs_attention", fix_count: 1, failing: ["content_editing"] })],
  });
  const FLEET_READY = okResult({ sites: [fleetSite({ site_id: SITE_ID })] });

  async function settle(ms: number) {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(ms);
      // TanStack Query hands a result to React on a zero-delay timer that the
      // advance above has only just scheduled: run it.
      await vi.advanceTimersByTimeAsync(10);
    });
  }

  /** The card and a rollup reader showing one fix; the check is asked for and accepted. */
  async function askForACheck() {
    getReadiness.mockResolvedValue(okResult(oneFix(BEFORE_DETAILS, BEFORE_TOOLS)));
    getFleetReadiness.mockResolvedValue(FLEET_ONE_FIX);
    refreshReadiness.mockResolvedValue(okResult({ metadata: true, abilities: true }));
    renderTab("operator", { fleetReader: true });
    const c = await card();
    expect(await screen.findByTestId("fleet-reader")).toHaveTextContent("1 to fix");

    vi.useFakeTimers();
    fireEvent.click(c.getByRole("button", { name: "Check again" }));
    await settle(0);
    expect(refreshReadiness).toHaveBeenCalledTimes(1);
    return c;
  }

  it("shows the new answer in the rollup once a poll finds the new results", async () => {
    const c = await askForACheck();
    // Accepting the request refetched the rollup: still the old answer.
    expect(screen.getByTestId("fleet-reader")).toHaveTextContent("1 to fix");

    // The site reports. Both the card and the rollup now have a new answer.
    getReadiness.mockResolvedValue(okResult(allClear(AFTER_DETAILS, AFTER_TOOLS)));
    getFleetReadiness.mockResolvedValue(FLEET_READY);
    await settle(POLL_INTERVAL_MS);

    expect(c.getByTestId("ai-readiness-status")).toHaveTextContent("Ready.");
    expect(screen.getByTestId("fleet-reader")).toHaveTextContent("Ready");
  });

  it("leaves the rollup alone while the polls find nothing new", async () => {
    await askForACheck();
    const rollupReads = getFleetReadiness.mock.calls.length;
    const cardReads = getReadiness.mock.calls.length;

    await settle(POLL_INTERVAL_MS * 3);

    expect(getReadiness.mock.calls.length).toBeGreaterThan(cardReads);
    expect(getFleetReadiness).toHaveBeenCalledTimes(rollupReads);
    expect(screen.getByTestId("fleet-reader")).toHaveTextContent("1 to fix");
  });

  it("refreshes the rollup again when the tool list lands after the site details", async () => {
    await askForACheck();

    // The site details land first and change nothing the rollup shows.
    getReadiness.mockResolvedValue(okResult(oneFix(AFTER_DETAILS, BEFORE_TOOLS)));
    const rollupReads = getFleetReadiness.mock.calls.length;
    await settle(POLL_INTERVAL_MS);
    expect(getFleetReadiness.mock.calls.length).toBeGreaterThan(rollupReads);
    expect(screen.getByTestId("fleet-reader")).toHaveTextContent("1 to fix");

    // The tool list lands later and changes the answer.
    getReadiness.mockResolvedValue(okResult(allClear(AFTER_DETAILS, AFTER_TOOLS)));
    getFleetReadiness.mockResolvedValue(FLEET_READY);
    await settle(POLL_INTERVAL_MS);
    expect(screen.getByTestId("fleet-reader")).toHaveTextContent("Ready");
  });
});

describe("turning AI editing on", () => {
  // The readiness body and the fleet rollup are computed from the switch the AI
  // editing section turns on: checkContentEditing in
  // apps/api/internal/aireadiness/evaluate.go reads content_editing_enabled_at
  // (apps/api/db/query/ai_readiness.sql), which MarkSiteContentEditingEnabled
  // (apps/api/db/query/sites.sql) sets and the enable route answers as
  // ContentEditingState.enabled (apps/api/internal/abilityrequest/content_editing.go).
  // No site report is involved, so the stub keeps both report times fixed while
  // the switch flips: nothing but the switch itself moves.
  const DETAILS = "2026-10-09T10:00:00.000Z";
  const TOOLS = "2026-10-09T10:01:00.000Z";

  function siteReadiness(on: boolean): SiteAiReadiness {
    return readiness({
      status: on ? "ready" : "needs_attention",
      fix_count: on ? 0 : 1,
      metadata_as_of: DETAILS,
      abilities_as_of: TOOLS,
      groups: [
        baseGroup(baseChecks({ content_editing: chk("content_editing", on ? "pass" : "fail") })),
        builderGroup("elementor", { installed: false }),
        builderGroup("bricks", { installed: false }),
      ],
    });
  }

  /** One switch, read by the three GETs and set by the POST, as on the server. */
  function serveSwitch(initiallyOn: boolean) {
    let on = initiallyOn;
    getEditing.mockImplementation(() => Promise.resolve(okResult({ site_id: SITE_ID, enabled: on })));
    getReadiness.mockImplementation(() => Promise.resolve(okResult(siteReadiness(on))));
    getFleetReadiness.mockImplementation(() =>
      Promise.resolve(
        okResult({
          sites: [
            on
              ? fleetSite({ site_id: SITE_ID })
              : fleetSite({
                  site_id: SITE_ID,
                  status: "needs_attention",
                  fix_count: 1,
                  failing: ["content_editing"],
                }),
          ],
        }),
      ),
    );
    enableEditing.mockImplementation(() => {
      on = true;
      return Promise.resolve(okResult({ site_id: SITE_ID, enabled: true }));
    });
  }

  async function settle(ms: number) {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(ms);
      // TanStack Query hands a result to React on a zero-delay timer that the
      // advance above has only just scheduled: run it.
      await vi.advanceTimersByTimeAsync(10);
    });
  }

  it("re-reads the card and the Sites list column once it is on", async () => {
    serveSwitch(false);
    renderTab("operator", { fleetReader: true });
    const c = await card();
    expect(c.getByText("Off. Turn it on in AI editing, below.")).toBeInTheDocument();
    expect(c.getByTestId("ai-readiness-status")).toHaveTextContent("1 thing to fix");
    expect(await screen.findByTestId("fleet-reader")).toHaveTextContent("1 to fix");
    expect(getReadiness).toHaveBeenCalledTimes(1);

    fireEvent.click(await screen.findByRole("button", { name: "Turn on" }));
    await waitFor(() =>
      expect(enableEditing).toHaveBeenCalledWith({ path: { siteId: SITE_ID }, body: {} }),
    );

    await waitFor(() =>
      expect(c.getByTestId("ai-readiness-status")).toHaveTextContent(
        "Ready. Everything the AI needs on this site is in place.",
      ),
    );
    expect(getReadiness.mock.calls.length).toBeGreaterThanOrEqual(2);
    expect(c.queryByText("Off. Turn it on in AI editing, below.")).not.toBeInTheDocument();
    expect(c.getByText("On.")).toBeInTheDocument();
    expect(c.queryAllByRole("img", { name: "Needs fixing" })).toHaveLength(0);
    await waitFor(() => expect(screen.getByTestId("fleet-reader")).toHaveTextContent("Ready"));
  });

  it("keeps both current when it is turned on part-way through a Check again window", async () => {
    serveSwitch(false);
    refreshReadiness.mockResolvedValue(okResult({ metadata: true, abilities: true }));
    renderTab("operator", { fleetReader: true });
    const c = await card();
    expect(await screen.findByTestId("fleet-reader")).toHaveTextContent("1 to fix");
    const turnOn = await screen.findByRole("button", { name: "Turn on" });

    vi.useFakeTimers();
    fireEvent.click(c.getByRole("button", { name: "Check again" }));
    await settle(0);
    expect(refreshReadiness).toHaveBeenCalledTimes(1);

    // The window is open and its first poll has found nothing new.
    const beforePoll = getReadiness.mock.calls.length;
    await settle(POLL_INTERVAL_MS);
    expect(getReadiness.mock.calls.length).toBeGreaterThan(beforePoll);
    expect(c.getByTestId("ai-readiness-status")).toHaveTextContent("1 thing to fix");

    // Turned on inside the window. Nothing waits for the next poll.
    fireEvent.click(turnOn);
    await settle(0);
    expect(c.getByTestId("ai-readiness-status")).toHaveTextContent("Ready.");
    expect(c.getByText("On.")).toBeInTheDocument();
    expect(screen.getByTestId("fleet-reader")).toHaveTextContent("Ready");

    // The rest of the window finds the same site reports and the answer holds.
    const afterEnable = getReadiness.mock.calls.length;
    await settle(POLL_INTERVAL_MS * 3);
    expect(getReadiness.mock.calls.length).toBeGreaterThan(afterEnable);
    expect(c.getByTestId("ai-readiness-status")).toHaveTextContent("Ready.");
    expect(c.getByText("On.")).toBeInTheDocument();
    expect(screen.getByTestId("fleet-reader")).toHaveTextContent("Ready");
  });

  it("is not undone by a poll read that was already on its way when it was turned on", async () => {
    serveSwitch(false);
    refreshReadiness.mockResolvedValue(okResult({ metadata: true, abilities: true }));
    renderTab("operator", { fleetReader: true });
    const c = await card();
    expect(await screen.findByTestId("fleet-reader")).toHaveTextContent("1 to fix");
    const turnOn = await screen.findByRole("button", { name: "Turn on" });

    vi.useFakeTimers();
    fireEvent.click(c.getByRole("button", { name: "Check again" }));
    await settle(0);

    // The next poll's read leaves before the switch flips and answers after.
    let answerOldRead!: (v: unknown) => void;
    const oldRead = new Promise((resolve) => (answerOldRead = resolve));
    getReadiness.mockImplementationOnce(() => oldRead);
    await settle(POLL_INTERVAL_MS);

    fireEvent.click(turnOn);
    await settle(0);
    expect(c.getByTestId("ai-readiness-status")).toHaveTextContent("Ready.");

    // The read that left first now answers, with the switch still off.
    await act(async () => {
      answerOldRead(okResult(siteReadiness(false)));
      await vi.advanceTimersByTimeAsync(10);
    });
    await settle(0);
    expect(c.getByTestId("ai-readiness-status")).toHaveTextContent("Ready.");
    expect(c.getByText("On.")).toBeInTheDocument();
    expect(screen.getByTestId("fleet-reader")).toHaveTextContent("Ready");
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
