import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, fireEvent, render, waitFor, within } from "@testing-library/react";
import { QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";

import { renderWithProviders, createTestQueryClient } from "@/test/render";
import { formatAbsolute } from "@/features/updates/schedule";
import { mockQueryResult } from "@/test/query-mocks";

import { Route } from "@/routes/_authed/ai/connect";
import { MCP_CLIENTS } from "./client-table";
import { useSites, DEFAULT_SITES_LIMIT } from "@/features/sites/use-sites";
import { useTags } from "@/features/tags/use-tags";
import { snapshotFromPage } from "@/features/mcp-consent/site-scope";
import { fleetTotal, scopeCountLabel } from "./site-step";
import { CONNECTIONS_PATH } from "./use-ai-connections";
import type { Site, SiteTag } from "@wpmgr/api";

// Step 3 reads the fleet and the tag registry through the same two hooks the
// consent screen uses, so they are mocked the same way (see
// routes/_authed/-connect.ai.test.tsx). What matters on this screen is that a
// FAILED read and an EMPTY one produce different screens, which is only
// testable if the test can put the hook into each state deliberately.
vi.mock("@/features/sites/use-sites", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/sites/use-sites")>();
  return { ...actual, useSites: vi.fn() };
});
vi.mock("@/features/tags/use-tags", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/tags/use-tags")>();
  return { ...actual, useTags: vi.fn() };
});

const mockedSites = vi.mocked(useSites);
const mockedTags = vi.mocked(useTags);

function fakeSite(n: number, tags: string[] = []): Site {
  return {
    id: `s${n}`,
    name: `site-${n}.example`,
    url: `https://site-${n}.example`,
    tags,
  } as Site;
}

/** A loaded fleet of `count` sites, short of the page limit so it is complete. */
function loadedFleet(count: number, tagsFor: (n: number) => string[] = () => []) {
  mockedSites.mockReturnValue(
    mockQueryResult<Site[]>({
      data: Array.from({ length: count }, (_, i) => fakeSite(i, tagsFor(i))),
    }),
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  loadedFleet(0);
  mockedTags.mockReturnValue(mockQueryResult<SiteTag[]>({ data: [] }));
});

// The wizard, through the real route component, the real router and a real
// QueryClient -- not by calling a hook or rendering a subcomponent in
// isolation. What is being tested is the computation from client to method to
// snippet, and that computation only exists once the route has mounted the
// wizard with a real endpoint.

const ConnectPage = Route.options.component!;

function renderWizard() {
  return renderWithProviders(<ConnectPage />, {
    withRouter: true,
    initialPath: "/ai/connect",
  });
}

async function pickClient(name: string) {
  const card = await screen.findByRole("button", { name: new RegExp(name, "i") });
  fireEvent.click(card);
  return card;
}

function authCard(method: "oauth" | "token"): HTMLButtonElement {
  const el = document.querySelector<HTMLButtonElement>(`button[data-method="${method}"]`);
  // A missing card must fail the test rather than letting every assertion
  // below it be skipped.
  if (el === null) throw new Error(`no auth card rendered for "${method}"`);
  return el;
}

describe("the wizard asks for the client first", () => {
  it("renders every row in the table as a picker card, including the generic one", async () => {
    renderWizard();
    for (const client of MCP_CLIENTS) {
      expect(
        await screen.findByRole("button", { name: new RegExp(client.name, "i") }),
      ).toBeInTheDocument();
    }
    // The generic entry is present and carries the same affordance as the rest,
    // not a smaller one.
    const generic = await screen.findByRole("button", { name: /other \/ generic/i });
    expect(generic).toBeEnabled();
  });

  it("does not offer an auth method before a client is chosen", async () => {
    renderWizard();
    await screen.findByRole("button", { name: /claude code/i });
    expect(document.querySelector('button[data-method="oauth"]')).toBeNull();
  });
});

describe("the method step is computed from the client, with the reason on the card", () => {
  it("disables the token method for Claude Desktop and says exactly why", async () => {
    renderWizard();
    await pickClient("Claude Desktop");

    const token = authCard("token");
    expect(token).toBeDisabled();
    // NEVER A GENERIC "UNAVAILABLE". The card carries the client's own reason.
    expect(within(token).getByText(/no header field/i)).toBeInTheDocument();

    // And the method that DOES work is offered, so the guard is not blanket.
    expect(authCard("oauth")).toBeEnabled();
  });

  it("disables OAuth for VS Code as 'not yet verified by us', with the date", async () => {
    renderWizard();
    await pickClient("VS Code");

    const oauth = authCard("oauth");
    expect(oauth).toBeDisabled();
    // The two halves the design asks for: not verified BY US, and when we last
    // looked, so a stale row looks stale rather than permanent.
    expect(within(oauth).getByText(/not yet verified by us/i)).toBeInTheDocument();
    expect(within(oauth).getByText(/last checked 2026-08-24/i)).toBeInTheDocument();

    expect(authCard("token")).toBeEnabled();
  });

  it("enables both methods for a client that supports both", async () => {
    // The over-fire case: a correct client must not be blocked by the
    // disabling logic.
    renderWizard();
    await pickClient("Cursor");
    expect(authCard("oauth")).toBeEnabled();
    expect(authCard("token")).toBeEnabled();
  });
});

describe("the setup artefact is generated per client", () => {
  it("emits the required http type for Claude Code", async () => {
    renderWizard();
    await pickClient("Claude Code");
    fireEvent.click(authCard("oauth"));

    const block = await screen.findByText(/"mcpServers"/);
    const text = block.textContent ?? "";
    expect(text).toContain('"type": "http"');
    expect(text).toContain('"url"');
    // The reason for that line is on screen, not only in a comment.
    expect(screen.getByText(/read as a local process/i)).toBeInTheDocument();
  });

  it("emits httpUrl and no url for Gemini CLI", async () => {
    renderWizard();
    await pickClient("Gemini CLI");
    fireEvent.click(authCard("oauth"));

    const text = (await screen.findByText(/"mcpServers"/)).textContent ?? "";
    expect(text).toContain('"httpUrl"');
    expect(text).not.toContain('"url"');
  });

  it("emits the servers wrapper for VS Code", async () => {
    renderWizard();
    await pickClient("VS Code");
    fireEvent.click(authCard("token"));

    const text = (await screen.findByText(/"servers"/)).textContent ?? "";
    expect(text).toContain('"servers"');
    expect(text).not.toContain('"mcpServers"');
  });

  it("emits no type key for Cursor", async () => {
    renderWizard();
    await pickClient("Cursor");
    fireEvent.click(authCard("oauth"));

    const text = (await screen.findByText(/"mcpServers"/)).textContent ?? "";
    expect(text).not.toContain('"type"');
  });

  it("renders the endpoint and a spec link for the generic entry, with no config block", async () => {
    renderWizard();
    await pickClient("Other / generic");
    fireEvent.click(authCard("oauth"));

    expect(await screen.findByText(/endpoint for other \/ generic/i)).toBeInTheDocument();
    expect(screen.queryByText(/"mcpServers"/)).not.toBeInTheDocument();
    expect(
      screen.getByRole("link", { name: /streamable http specification/i }),
    ).toBeInTheDocument();
  });

  it("gives GUI clients in-app steps rather than a file to edit", async () => {
    renderWizard();
    await pickClient("Claude Desktop");
    fireEvent.click(authCard("oauth"));

    expect(await screen.findByText(/set this up inside claude desktop/i)).toBeInTheDocument();
    expect(screen.queryByText(/"mcpServers"/)).not.toBeInTheDocument();
  });

  it("never prints a Windows path", async () => {
    renderWizard();
    await pickClient("Claude Code");
    fireEvent.click(authCard("oauth"));
    await screen.findByText(/"mcpServers"/);
    // Every source documented POSIX only; a Windows path here would be invented.
    expect(document.body.textContent ?? "").not.toMatch(/[A-Z]:\\|%APPDATA%/);
  });

  it("shows the config placeholder alongside a real mint button, not the old refusal", async () => {
    // The endpoint this refusal predated now exists (POST CONNECTIONS_PATH),
    // so the stale "you cannot mint a token here yet" copy is gone. The config
    // block above still shows the placeholder -- buildSnippet emits the real
    // token only when one has actually been minted, and none has yet here.
    renderWizard();
    await pickClient("Cursor");
    fireEvent.click(authCard("token"));

    const text = (await screen.findByText(/"mcpServers"/)).textContent ?? "";
    expect(text).toContain("YOUR_CONNECTION_TOKEN");
    expect(screen.queryByText(/cannot mint a token here yet/i)).not.toBeInTheDocument();
    expect(
      await screen.findByRole("button", { name: /generate connection token/i }),
    ).toBeInTheDocument();
  });
});

describe("the wizard does not promise things it cannot deliver", () => {
  it("does not claim the entered name appears on the approval screen", async () => {
    renderWizard();
    await pickClient("Claude Code");
    fireEvent.click(authCard("oauth"));
    await screen.findByText(/"mcpServers"/);

    // The old copy said "shown on the approval screen". Nothing carries the
    // name there -- the client starts the OAuth flow itself -- so an operator
    // would go looking for something that is not there. Wrong prose is a defect.
    expect(screen.queryByText(/shown on the approval screen/i)).not.toBeInTheDocument();
    expect(screen.getByText(/used as the server key in the config/i)).toBeInTheDocument();
    expect(screen.getByText(/asks you to name\s+the connection separately/i)).toBeInTheDocument();
  });

  it("states the self-hosted proxy requirement beside the endpoint it printed", async () => {
    renderWizard();
    await pickClient("Claude Code");
    fireEvent.click(authCard("oauth"));
    await screen.findByText(/"mcpServers"/);

    // The URL is derived from the origin, which does not prove anything
    // forwards it: infra/nginx/nginx.conf has no /mcp location and
    // apps/web/vite.config.ts does not proxy it, so on a self-hosted install
    // or in dev the copied URL reaches the SPA. Saying nothing would hand
    // someone a web page and let them debug their AI client.
    expect(screen.getByText(/reverse proxy must forward \/mcp to the API/i)).toBeInTheDocument();
  });
});

// ---------------------------------------------------------------------------
// Step 3 -- "Choose which sites this connection may touch"
// ---------------------------------------------------------------------------

/** Get as far as step 3, which needs a client and a method behind it. */
async function reachSiteStep() {
  renderWizard();
  await pickClient("Claude Code");
  fireEvent.click(authCard("oauth"));
  return await screen.findByTestId("site-step-count");
}

describe("step 3 exists at all, and sits before capabilities", () => {
  it("renders a site step between the method and the setup artefact", async () => {
    await reachSiteStep();
    // The three modes the schema permits, on screen, as one radiogroup.
    const modes = screen.getByRole("radiogroup", { name: /site scope/i });
    for (const label of ["All sites", "By tag", "Named sites"]) {
      expect(within(modes).getByText(label)).toBeInTheDocument();
    }
  });

  it("names the ordering as a decision rather than leaving it implied", async () => {
    await reachSiteStep();
    expect(screen.getByText(/chosen before capabilities, on purpose/i)).toBeInTheDocument();
  });

  it("numbers the setup artefact after it, so the rail and the page agree", async () => {
    await reachSiteStep();
    expect(screen.getByRole("heading", { name: /^4\. Set it up$/ })).toBeInTheDocument();
    // And the rail no longer claims sites are chosen somewhere else.
    expect(screen.queryByText(/4\. Choose sites and permissions/i)).not.toBeInTheDocument();
  });

  it("does not invent a numbered step for capabilities, which no completion path lets an operator choose today", async () => {
    // reachSiteStep exercises the OAuth path (it clicks the oauth auth card),
    // where "this wizard never creates the grant" is true: Approve
    // (apps/api/internal/mcp/service.go:607) takes no capability field. The
    // token path differs -- the mint endpoint already accepts one
    // (dto.go:264) and only MintConnectionInput lacks the field
    // (use-ai-connections.ts:228-233) -- but neither path has a picker in
    // this frontend, so the assertion below holds for both. Not a claim
    // about the backend either way: the vocabulary and both endpoints exist.
    // See GH #660 for where a picker should live.
    await reachSiteStep();
    expect(screen.queryByRole("heading", { name: /what this connection may do/i })).toBeNull();
    expect(screen.getByText(/permissions are chosen on the approval screen/i)).toBeInTheDocument();
  });
});

describe("the count, and the fleet size it is entitled to claim", () => {
  it("opens on the wireframe's empty ratio against a fleet it could read", async () => {
    loadedFleet(60);
    const count = await reachSiteStep();

    // Derived from the same function the component calls, so a reworded
    // sentence does not redden this and a wrong NUMBER does.
    const expected = scopeCountLabel(
      { kind: "none", because: "no-selection" },
      fleetTotal(snapshotFromPage(Array.from({ length: 60 }, () => null as never), DEFAULT_SITES_LIMIT)),
    );
    expect(count).toHaveTextContent(expected);
    expect(count.textContent?.startsWith("0 of 60")).toBe(true);
  });

  it("counts up as sites are picked, and back down as they are removed", async () => {
    loadedFleet(60);
    await reachSiteStep();

    fireEvent.click(screen.getByRole("button", { name: /add sites/i }));
    const picker = await screen.findByTestId("site-step-picker");
    const boxes = within(picker).getAllByRole("checkbox");
    fireEvent.click(boxes[0]!);
    fireEvent.click(boxes[1]!);
    expect(screen.getByTestId("site-step-count").textContent?.startsWith("2 of 60")).toBe(true);

    // The token is the removal affordance, and removing must move the count.
    fireEvent.click(screen.getAllByRole("button", { name: /^remove /i })[0]!);
    expect(screen.getByTestId("site-step-count").textContent?.startsWith("1 of 60")).toBe(true);
  });

  it("refuses to state a fleet size when the page it read came back full", async () => {
    // A full page is byte-identical to a full page with more behind it, so the
    // denominator is a floor and the copy has to say so.
    loadedFleet(DEFAULT_SITES_LIMIT);
    const count = await reachSiteStep();
    expect(count.textContent).toMatch(/at least/i);
  });

  it("states the fleet size plainly when the page was short", async () => {
    loadedFleet(7);
    const count = await reachSiteStep();
    expect(count.textContent).not.toMatch(/at least/i);
    expect(count.textContent?.startsWith("0 of 7")).toBe(true);
  });
});

describe("an empty scope is a working state, not an error", () => {
  it("says so in as many words, and styles nothing as a failure", async () => {
    loadedFleet(60);
    await reachSiteStep();

    const empty = screen.getByTestId("site-step-empty");
    expect(empty).toHaveTextContent(/working state, not an error/i);
    expect(empty).toHaveTextContent(/can read nothing/i);
    expect(empty).not.toHaveTextContent(/propose/i);
    // Not an alert, because it is not one. An empty scope announced by a
    // screen reader as an error is the same defect as rendering it in red.
    expect(screen.queryByTestId("site-step-blocked")).toBeNull();
    expect(empty.getAttribute("role")).toBeNull();
  });

  it("does not block the wizard on it", async () => {
    loadedFleet(60);
    await reachSiteStep();
    // The setup artefact is reachable with nothing selected. An earlier
    // revision of this surface disabled Continue here; that is the behaviour
    // being corrected.
    expect(screen.getByRole("heading", { name: /^4\. Set it up$/ })).toBeInTheDocument();
    expect(await screen.findByText(/"mcpServers"/)).toBeInTheDocument();
  });
});

describe("a failed load is not an empty fleet", () => {
  it("holds the step and says the list did not load, rather than showing a zero", async () => {
    mockedSites.mockReturnValue(mockQueryResult<Site[]>({ data: undefined }));
    await reachSiteStep();

    expect(screen.getByTestId("site-step-blocked")).toHaveTextContent(/could not read/i);
    // And no count is asserted over the top of it.
    expect(screen.getByTestId("site-step-count").textContent).not.toMatch(/\d/);
    // The empty-state sentence must NOT appear: "you have chosen nothing" and
    // "we could not read the list" are different facts.
    expect(screen.queryByTestId("site-step-empty")).toBeNull();
  });

  it("offers no site picker to choose from when the fleet is unknown", async () => {
    mockedSites.mockReturnValue(mockQueryResult<Site[]>({ data: undefined }));
    await reachSiteStep();
    fireEvent.click(screen.getByRole("button", { name: /add sites/i }));
    expect(await screen.findByTestId("site-step-sites-failed")).toHaveTextContent(
      /not the same as having no sites/i,
    );
    expect(screen.queryByTestId("site-step-picker")).toBeNull();
  });

  it("says the read is still running rather than that it failed, while it is still running", async () => {
    // `fleet === null` covers two different worlds -- we asked and it failed,
    // and we have not finished asking -- and the picker used to render the
    // failure copy for both. That is this feature's recurring defect: a load
    // in progress stated as a fact about the organisation. It is worse here
    // than a bare spinner would be, because the add control stays live beside
    // it, so an operator can act on a failure that has not happened.
    mockedSites.mockReturnValue(mockQueryResult<Site[]>({ data: undefined, isPending: true }));
    await reachSiteStep();
    fireEvent.click(screen.getByRole("button", { name: /add sites/i }));

    expect(await screen.findByTestId("site-step-sites-loading")).toHaveTextContent(
      /still reading/i,
    );
    expect(screen.queryByTestId("site-step-sites-failed")).toBeNull();
    // Still no picker: nothing is tickable yet either way. The claim being
    // corrected is about WHY, not about what is offered.
    expect(screen.queryByTestId("site-step-picker")).toBeNull();
  });

  it("still says the read failed once it is no longer running", async () => {
    // The other half of the guard. A loading branch that swallowed the failure
    // branch would be the same defect pointing the other way, and this is the
    // case that would catch it.
    mockedSites.mockReturnValue(
      mockQueryResult<Site[]>({ data: undefined, isPending: false, isError: true }),
    );
    await reachSiteStep();
    fireEvent.click(screen.getByRole("button", { name: /add sites/i }));

    expect(await screen.findByTestId("site-step-sites-failed")).toBeInTheDocument();
    expect(screen.queryByTestId("site-step-sites-loading")).toBeNull();
  });

  it("distinguishes a failed tag registry from an organisation with no tags", async () => {
    loadedFleet(3);
    mockedTags.mockReturnValue(mockQueryResult<SiteTag[]>({ data: undefined }));
    await reachSiteStep();

    fireEvent.click(within(screen.getByRole("radiogroup", { name: /site scope/i })).getByText("By tag"));
    fireEvent.click(screen.getByRole("button", { name: /add tags/i }));
    expect(await screen.findByTestId("site-step-tags-failed")).toBeInTheDocument();
    expect(screen.queryByTestId("site-step-tags-empty")).toBeNull();
  });
});

describe("a tag matching nothing in a partial page is not a zero", () => {
  it("holds the step rather than claiming the tag reaches no site", async () => {
    // A full page, and a tag that matches nothing in it. We have not seen every
    // site, so "matches nothing" is a claim we cannot make.
    loadedFleet(DEFAULT_SITES_LIMIT);
    mockedTags.mockReturnValue(
      mockQueryResult<SiteTag[]>({ data: [{ id: "t1", name: "seo-2026" } as SiteTag] }),
    );
    await reachSiteStep();

    fireEvent.click(within(screen.getByRole("radiogroup", { name: /site scope/i })).getByText("By tag"));
    fireEvent.click(screen.getByRole("button", { name: /add tags/i }));
    fireEvent.click(within(await screen.findByTestId("site-step-picker")).getAllByRole("checkbox")[0]!);

    expect(screen.getByTestId("site-step-blocked")).toBeInTheDocument();
    expect(screen.getByTestId("site-step-count").textContent).not.toMatch(/\d/);
  });

  it("resolves the tag to a real set when the fleet is fully in hand", async () => {
    loadedFleet(10, (n) => (n < 4 ? ["client-retainer"] : []));
    mockedTags.mockReturnValue(
      mockQueryResult<SiteTag[]>({ data: [{ id: "t1", name: "client-retainer" } as SiteTag] }),
    );
    await reachSiteStep();

    fireEvent.click(within(screen.getByRole("radiogroup", { name: /site scope/i })).getByText("By tag"));
    fireEvent.click(screen.getByRole("button", { name: /add tags/i }));
    fireEvent.click(within(await screen.findByTestId("site-step-picker")).getAllByRole("checkbox")[0]!);

    expect(screen.getByTestId("site-step-count").textContent?.startsWith("4 of 10")).toBe(true);
    expect(screen.queryByTestId("site-step-blocked")).toBeNull();
    // A tag is re-resolved per request, and the screen says so rather than
    // letting the operator read the list as frozen.
    expect(screen.getByText(/resolved to a site list at every request/i)).toBeInTheDocument();
    // The token carries the tag: prefix so it cannot be misread as a hostname.
    // Scoped to the field, because the picker below renders the same label.
    expect(
      within(screen.getByTestId("site-step-tokenfield")).getByText("tag:client-retainer"),
    ).toBeInTheDocument();
  });
});

describe("the picker discloses what it could not offer", () => {
  it("warns that the choices are not all of them when the page came back full", async () => {
    loadedFleet(DEFAULT_SITES_LIMIT);
    await reachSiteStep();
    fireEvent.click(screen.getByRole("button", { name: /add sites/i }));
    expect(await screen.findByTestId("site-step-picker-truncated")).toHaveTextContent(
      /not all of them/i,
    );
  });

  it("says nothing of the sort when it listed the whole fleet", async () => {
    loadedFleet(9);
    await reachSiteStep();
    fireEvent.click(screen.getByRole("button", { name: /add sites/i }));
    await screen.findByTestId("site-step-picker");
    expect(screen.queryByTestId("site-step-picker-truncated")).toBeNull();
  });

  it("does not offer a never-reach count it cannot compute", async () => {
    loadedFleet(DEFAULT_SITES_LIMIT);
    await reachSiteStep();
    fireEvent.click(screen.getByRole("button", { name: /add sites/i }));
    fireEvent.click(within(await screen.findByTestId("site-step-picker")).getAllByRole("checkbox")[0]!);
    expect(screen.queryByTestId("site-step-excluded")).toBeNull();
  });
});

describe("the wizard does not promise to carry the scope it collected", () => {
  it("says plainly that the approval screen asks again", async () => {
    loadedFleet(5);
    await reachSiteStep();
    expect(
      screen.getByText(/nothing carries this selection to the approval screen/i),
    ).toBeInTheDocument();
  });
});

describe("changing the client recomputes rather than carrying a stale answer", () => {
  it("drops a method the newly chosen client cannot use", async () => {
    renderWizard();
    await pickClient("Cursor");
    fireEvent.click(authCard("token"));
    expect(await screen.findByText(/"mcpServers"/)).toBeInTheDocument();

    // Claude Desktop cannot use a token. The wizard must fall back to asking,
    // not silently keep a selection that produces no valid artefact.
    await pickClient("Claude Desktop");
    expect(screen.queryByText(/set this up inside/i)).not.toBeInTheDocument();
    expect(authCard("token")).toBeDisabled();
  });
});

// ---------------------------------------------------------------------------
// Minting a connection token (step 6's one-time reveal)
//
// The fetch is stubbed at the network boundary, not the mutation hook, for
// the same reason routes/_authed/ai/-index.test.tsx gives: the real queryFn,
// the real zod parse and the real state mapping all run, and a hook mock here
// would only prove the component renders whatever it is handed.
// ---------------------------------------------------------------------------

function urlOf(input: RequestInfo | URL): string {
  if (typeof input === "string") return input;
  if (input instanceof URL) return input.toString();
  return input.url;
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

/** Stub fetch for exactly the mint POST; anything else fails the test loudly. */
function stubMintFetch(impl: (init: RequestInit | undefined) => Response | Promise<Response>) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = urlOf(input);
      const method = init?.method ?? "GET";
      if (url === CONNECTIONS_PATH && method === "POST") {
        return Promise.resolve(impl(init));
      }
      throw new Error(`unstubbed fetch in mint test: ${method} ${url}`);
    }),
  );
}

const MINTED = {
  grant_id: "11111111-1111-1111-1111-111111111111",
  token: "wpm_conn_test_token_value_do_not_reuse",
  token_prefix: "wpm_conn_ab12",
  expires_at: "2026-11-27T00:00:00Z",
  site_scope_mode: "list",
  capabilities: ["read_fleet", "propose_changes"],
};

/** The site checkboxes inside step 3's picker. Throws rather than returning []. */
function pickerBoxes(): HTMLElement[] {
  return within(screen.getByTestId("site-step-picker")).getAllByRole("checkbox");
}

/**
 * Client and method picked, fleet loaded, ONE SITE PICKED, sitting at the mint
 * button.
 *
 * The site is picked deliberately and is not scaffolding. The wizard opens on
 * mode 'list' with nothing selected, and ValidateSiteScopeRequest
 * (apps/api/internal/mcp/scope.go) refuses mode 'list' with no site ids, so a
 * mint from the opening state is a 400 rather than a token. These tests are
 * about what happens to a token that WAS minted, which needs a scope the
 * server would actually accept.
 */
async function reachMintButton() {
  renderWizard();
  return advanceToMintButton();
}

/**
 * The same walk to the mint button, on a wizard that is ALREADY RENDERED. Split
 * out of `reachMintButton` so the route-blocker tests can reach the same state
 * through a router they hold a handle on, rather than duplicating the steps and
 * letting the two copies drift.
 */
async function advanceToMintButton() {
  await pickClient("Cursor");
  fireEvent.click(authCard("token"));
  await screen.findByTestId("site-step-count");
  fireEvent.click(screen.getByRole("button", { name: /\+ add sites/i }));
  fireEvent.click(pickerBoxes()[0]!);
  return screen.findByRole("button", { name: /generate connection token/i });
}

/**
 * The wizard inside a REAL router with a REAL second route, returning the
 * router so a test can assert where the operator actually ended up.
 *
 * This is not `renderWithProviders({ withRouter: true })` and cannot be. That
 * harness builds a bare root route deliberately (see its module doc), so every
 * path resolves to no matched route, and the blocker under test is route
 * behaviour: a test that cannot observe the location cannot tell "the
 * navigation was refused" from "the navigation happened and the notice
 * rendered anyway". Two matched routes make the difference observable, and
 * `router.state.location.pathname` is the assertion that a rendered banner
 * cannot fake.
 */
function renderWizardInRouter() {
  const rootRoute = createRootRoute();
  const connectRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/ai/connect",
    component: ConnectPage,
  });
  const listRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/ai",
    component: () => <p>the AI connections list</p>,
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([connectRoute, listRoute]),
    history: createMemoryHistory({ initialEntries: ["/ai/connect"] }),
  });
  render(
    <QueryClientProvider client={createTestQueryClient()}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  return router;
}

/**
 * Attempt to leave for /ai, the way the page header's back link does.
 *
 * The promise is caught rather than awaited. A blocked navigation is not an
 * error, but nothing in the router's contract promises this settles when the
 * navigation never happens, and a bare `await` on it would hang the test
 * instead of failing it. What the test waits on is the observable outcome.
 */
function attemptLeave(router: { navigate: (opts: { to: string }) => Promise<void> }) {
  void router.navigate({ to: "/ai" }).catch(() => {});
}

/**
 * A mint whose response is held open until the returned `release` is called.
 *
 * Every in-flight test uses this rather than racing a resolved stub. A test
 * that assumed the operator acted before the response landed would pass
 * against a component that does the wrong thing, because the wrong thing would
 * never get the chance to happen.
 */
function heldMint(body: unknown = MINTED, status = 201) {
  let release!: () => void;
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  stubMintFetch(async () => {
    await held;
    return jsonResponse(body, status);
  });
  return { release: () => release() };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("minting a connection token", () => {
  it("reveals the plaintext exactly once, stating the scope and capabilities it covers", async () => {
    loadedFleet(3);
    stubMintFetch(() => jsonResponse(MINTED, 201));

    fireEvent.click(await reachMintButton());

    expect(
      await screen.findByText(/this is the only time this token is shown/i),
    ).toBeInTheDocument();
    expect(screen.getByText(MINTED.token)).toBeInTheDocument();
    expect(screen.getByText(/read_fleet, propose_changes/i)).toBeInTheDocument();
    // No "mint again" affordance beside a token that is supposedly shown once.
    expect(
      screen.queryByRole("button", { name: /generate connection token/i }),
    ).not.toBeInTheDocument();
  });

  // -------------------------------------------------------------------------
  // A MINTED TOKEN IS NEVER LOST. Three routes reached the same outcome: a
  // live organisation credential that authenticates, whose plaintext was shown
  // to nobody, that nobody knows to revoke because nobody knows it exists.
  // Each is pinned separately, because they are three different mechanisms and
  // a single test would let two of them come back.
  // -------------------------------------------------------------------------

  it("keeps a revealed token when an input it was minted for changes", async () => {
    // THE TRIPWIRE THAT USED TO POINT THE OTHER WAY. This test previously
    // asserted that editing the name CLEARED the reveal, which was written to
    // stop a token being shown beside a configuration it was not minted for.
    // MintedReveal made that unconstructible -- the reveal carries the scope
    // and client the request was sent with -- and what the clearing did after
    // that was destroy the only copy of a live credential on one keystroke.
    // The property being pinned is the inverse, and with the same force.
    loadedFleet(3);
    stubMintFetch(() => jsonResponse(MINTED, 201));

    fireEvent.click(await reachMintButton());
    await screen.findByText(/this is the only time this token is shown/i);

    fireEvent.change(screen.getByLabelText(/name this connection/i), {
      target: { value: "Fleet manager, renamed" },
    });
    // And the scope too: the other half of what configKey watched.
    fireEvent.click(pickerBoxes()[1]!);
    expect(await screen.findByTestId("site-step-summary")).toHaveTextContent(/2 sites/i);

    expect(screen.getByText(MINTED.token)).toBeInTheDocument();
    expect(screen.getByText(/this is the only time this token is shown/i)).toBeInTheDocument();
    // Still labelled with what it was minted FOR, not with the newer scope --
    // surviving the edit must not mean drifting into describing it.
    expect(screen.getByText(/Capabilities:/)).toHaveTextContent(/1 site, listed below/i);
  });

  it("gives up the token only when the operator says they have saved it", async () => {
    // The over-fire case for the test above. A reveal that can never be
    // dismissed is its own defect: the operator cannot mint a second token and
    // the screen is stuck.
    loadedFleet(3);
    stubMintFetch(() => jsonResponse(MINTED, 201));

    fireEvent.click(await reachMintButton());
    await screen.findByText(MINTED.token);

    fireEvent.click(screen.getByRole("button", { name: /i have saved this token/i }));

    expect(screen.queryByText(MINTED.token)).not.toBeInTheDocument();
    expect(
      await screen.findByRole("button", { name: /generate connection token/i }),
    ).toBeInTheDocument();
  });

  it("keeps the token on screen after the step that minted it is gone", async () => {
    // THE UNMOUNT DEFECT, PINNED AT ITS ROOT. The reveal used to live in
    // TokenMintPanel, which step 4 renders only for method 'token'. Anything
    // that unmounted that panel took the token with it. Switching to OAuth
    // here unmounts the panel completely -- step 4 renders NextSteps instead --
    // and the token must still be on screen, because a credential the server
    // has already created cannot be un-created by a click.
    loadedFleet(3);
    stubMintFetch(() => jsonResponse(MINTED, 201));

    fireEvent.click(await reachMintButton());
    await screen.findByText(MINTED.token);

    fireEvent.click(authCard("oauth"));

    // The panel is genuinely gone, so this is not passing by nothing happening.
    expect(await screen.findByText(/start the connection in cursor/i)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /generate connection token/i })).toBeNull();
    // And the token survived it.
    expect(screen.getByText(MINTED.token)).toBeInTheDocument();
    expect(screen.getByText(/this is the only time this token is shown/i)).toBeInTheDocument();
  });

  it("refuses to unmount the panel while its request is still in the air", async () => {
    // THE OUTER LAYER. Lifting the state is what makes the bad outcome
    // impossible; this is what stops the operator walking into it in the first
    // place. Both are here because a guard that depends on the operator not
    // clicking during a round trip is not a guard, and state that survives an
    // unmount is not a reason to offer the unmount.
    loadedFleet(3);
    const { release } = heldMint();

    fireEvent.click(await reachMintButton());
    // IN FLIGHT, AND PROVABLY SO. Every assertion below is vacuous if the
    // response has already landed.
    expect(await screen.findByRole("button", { name: /minting/i })).toBeInTheDocument();

    expect(authCard("oauth")).toBeDisabled();
    expect(authCard("token")).toBeDisabled();
    expect(screen.getByRole("button", { name: /claude desktop/i })).toBeDisabled();
    // Said out loud, not silently greyed out.
    expect(screen.getByRole("status")).toHaveTextContent(/strand a live credential nobody holds/i);

    // Clicking anyway changes nothing, which is what "refuses" has to mean.
    fireEvent.click(authCard("oauth"));
    expect(await screen.findByRole("button", { name: /minting/i })).toBeInTheDocument();

    release();

    expect(await screen.findByText(MINTED.token)).toBeInTheDocument();
    // And the controls come back once nothing is outstanding -- the lock is
    // for the duration of the request, not for the rest of the session.
    expect(authCard("oauth")).toBeEnabled();
    expect(screen.getByRole("button", { name: /claude desktop/i })).toBeEnabled();
  });

  // ---------------------------------------------------------------------------
  // THE FOURTH ROUTE OUT, AND THE ONE THE OTHER THREE DO NOT COVER: leaving the
  // page. Lifting the mint to the wizard makes the response land on a mounted
  // surface for every click INSIDE the screen, and does nothing at all when the
  // screen itself goes away. The back link, a sidebar entry and the browser's
  // back button all unmount the wizard with the POST still open, and the
  // credential the server has already created is then a live grant whose
  // plaintext was shown to no one.
  //
  // Every test below holds the response open on a promise and proves the
  // in-flight state was genuinely reached before it acts. A test that raced a
  // resolved stub would pass against a component with no blocker at all.
  // ---------------------------------------------------------------------------

  it("refuses to leave the route while a mint is in the air, and says why", async () => {
    loadedFleet(3);
    const { release } = heldMint();
    const router = renderWizardInRouter();

    fireEvent.click(await advanceToMintButton());
    // IN FLIGHT, AND PROVABLY SO. Everything below is vacuous otherwise.
    expect(await screen.findByRole("button", { name: /minting/i })).toBeInTheDocument();
    // And nothing is claiming to hold anyone yet, so the assertion after the
    // navigation is about the navigation and not about a banner that was
    // always there.
    expect(screen.queryByTestId("navigation-held")).toBeNull();

    attemptLeave(router);

    // THE REASON. A refusal the operator cannot see is indistinguishable from
    // a broken link.
    await waitFor(() => {
      expect(screen.getByTestId("navigation-held")).toBeInTheDocument();
    });
    expect(screen.getByTestId("navigation-held")).toHaveTextContent(/shown once/i);
    expect(screen.getByTestId("navigation-held")).toHaveTextContent(
      /live credential .* nobody holds/i,
    );
    // THE REFUSAL ITSELF, which the banner cannot fake: the location never
    // moved and the other route never rendered.
    expect(router.state.location.pathname).toBe("/ai/connect");
    expect(screen.queryByText(/the ai connections list/i)).toBeNull();
    expect(screen.getByRole("button", { name: /minting/i })).toBeInTheDocument();

    release();
    expect(await screen.findByText(MINTED.token)).toBeInTheDocument();
  });

  it("lifts the block when the mint succeeds", async () => {
    // A blocker that outlives its request strands the operator on a page they
    // cannot leave, which is a worse defect than the one it was added to fix.
    loadedFleet(3);
    const { release } = heldMint();
    const router = renderWizardInRouter();

    fireEvent.click(await advanceToMintButton());
    expect(await screen.findByRole("button", { name: /minting/i })).toBeInTheDocument();
    attemptLeave(router);
    await waitFor(() => {
      expect(screen.getByTestId("navigation-held")).toBeInTheDocument();
    });

    release();
    expect(await screen.findByText(MINTED.token)).toBeInTheDocument();
    // The reason goes with the request that caused it, rather than sitting
    // there telling the operator they are held when they are not.
    expect(screen.queryByTestId("navigation-held")).toBeNull();

    attemptLeave(router);
    expect(await screen.findByText(/the ai connections list/i)).toBeInTheDocument();
    expect(router.state.location.pathname).toBe("/ai");
  });

  it("lifts the block when the mint fails", async () => {
    // The half that a blocker keyed on "a mint was started" would get wrong.
    // A 500 creates no credential and leaves nothing to protect, so holding
    // the operator after it would be a trap with no purpose at all.
    loadedFleet(3);
    const { release } = heldMint({ error: "internal" }, 500);
    const router = renderWizardInRouter();

    fireEvent.click(await advanceToMintButton());
    expect(await screen.findByRole("button", { name: /minting/i })).toBeInTheDocument();
    attemptLeave(router);
    await waitFor(() => {
      expect(screen.getByTestId("navigation-held")).toBeInTheDocument();
    });
    expect(router.state.location.pathname).toBe("/ai/connect");

    release();
    // The request settled as a failure, and provably so: the button came back
    // and no token was revealed.
    expect(
      await screen.findByRole("button", { name: /generate connection token/i }),
    ).toBeInTheDocument();
    expect(screen.queryByText(MINTED.token)).toBeNull();
    expect(screen.queryByTestId("navigation-held")).toBeNull();

    attemptLeave(router);
    expect(await screen.findByText(/the ai connections list/i)).toBeInTheDocument();
    expect(router.state.location.pathname).toBe("/ai");
  });

  it("does not block a route change when no mint is outstanding", async () => {
    // THE OVER-FIRE CASE. A blocker that fires on a screen with nothing in the
    // air makes the wizard a room with no door, and the first operator it
    // traps is the one who opened it to read the instructions.
    loadedFleet(3);
    const router = renderWizardInRouter();

    // All the way to the mint button, and deliberately no further: every input
    // the wizard has is filled in and still nothing has been sent.
    await advanceToMintButton();

    attemptLeave(router);

    expect(await screen.findByText(/the ai connections list/i)).toBeInTheDocument();
    expect(router.state.location.pathname).toBe("/ai");
    expect(screen.queryByTestId("navigation-held")).toBeNull();
  });

  it("names the credential it just made, so a lost plaintext is still revocable", async () => {
    // If anything goes wrong between this screen and the operator's password
    // manager, the prefix is what lets them find and kill this exact
    // credential. Without it, "revoke the one I just made" is a guess.
    loadedFleet(3);
    stubMintFetch(() => jsonResponse(MINTED, 201));

    fireEvent.click(await reachMintButton());

    await screen.findByText(MINTED.token);
    expect(screen.getByText(MINTED.token_prefix)).toBeInTheDocument();
  });

  it("dates the expiry for a person to read, not in the wire format", async () => {
    // `expires_at` arrives as ISO-8601 and was printed straight into an
    // English sentence, so the operator read "2026-11-27T00:00:00Z" mid-line
    // on the one screen they see once and cannot return to.
    loadedFleet(3);
    stubMintFetch(() => jsonResponse(MINTED, 201));

    fireEvent.click(await reachMintButton());
    await screen.findByText(MINTED.token);

    // Not merely joined by a formatted copy elsewhere: the machine string is
    // off the screen entirely.
    expect(document.body.textContent).not.toContain(MINTED.expires_at);
    // And what replaced it is the app's shared formatter, zone included,
    // rather than a second hand-rolled one that could drift from the rest of
    // the app.
    expect(screen.getByText(/Listed as/)).toHaveTextContent(
      formatAbsolute(MINTED.expires_at),
    );
    // The guard on the guard: if formatAbsolute were a passthrough, the
    // assertion above would hold while the defect stood.
    expect(formatAbsolute(MINTED.expires_at)).not.toContain("T00:00:00Z");
  });

  it("describes the scope the token was minted FOR when the scope changed mid-flight", async () => {
    // THE RACE, AND THE WORST OUTCOME THIS SCREEN CAN PRODUCE. The mint is a
    // round trip; the site scope is an input the operator can go on clicking
    // while it is open. A reveal that reads the scope at RESPONSE time pairs a
    // real, shown-once credential with a description of access it does not
    // carry -- on the one screen whose whole job is saying what was just made,
    // at the only moment the token is ever visible. Nothing downstream can
    // correct it: the operator reads that line and deploys the token.
    //
    // The response is held open deliberately rather than raced against a
    // timer. A test that depended on the operator being slower than a stubbed
    // fetch would pass on a fixed component and on a broken one.
    loadedFleet(3);
    let release!: () => void;
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    stubMintFetch(async () => {
      await held;
      return jsonResponse(MINTED, 201);
    });

    renderWizard();
    await pickClient("Cursor");
    fireEvent.click(authCard("token"));
    await screen.findByTestId("site-step-count");
    fireEvent.click(screen.getByRole("button", { name: /\+ add sites/i }));

    const boxes = () =>
      within(screen.getByTestId("site-step-picker")).getAllByRole("checkbox");
    fireEvent.click(boxes()[0]!);
    expect(await screen.findByTestId("site-step-summary")).toHaveTextContent(
      /1 site, listed below/i,
    );

    fireEvent.click(await screen.findByRole("button", { name: /generate connection token/i }));
    // In flight, and provably so: the assertions below mean nothing if the
    // scope was widened after the response had already landed.
    expect(await screen.findByRole("button", { name: /minting/i })).toBeInTheDocument();

    // The operator widens the scope from one site to three, mid-request.
    fireEvent.click(boxes()[1]!);
    fireEvent.click(boxes()[2]!);
    expect(await screen.findByTestId("site-step-summary")).toHaveTextContent(
      /3 sites, listed below/i,
    );

    release();

    expect(await screen.findByText(MINTED.token)).toBeInTheDocument();
    const scopeLine = screen.getByText(/Capabilities:/);
    expect(scopeLine).toHaveTextContent(/1 site, listed below/i);
    expect(scopeLine).not.toHaveTextContent(/3 sites/i);
    // And the live step still shows the operator's newer, wider selection, so
    // the reveal is not merely lagging the whole screen -- the two disagree on
    // purpose, because they are describing two different things.
    expect(screen.getByTestId("site-step-summary")).toHaveTextContent(/3 sites, listed below/i);
  });

  it("sends the tag ids the registry resolved, never the tag names the operator clicked", async () => {
    loadedFleet(3, () => ["prod"]);
    mockedTags.mockReturnValue(
      mockQueryResult<SiteTag[]>({ data: [{ id: "tag-uuid-1", name: "prod" } as SiteTag] }),
    );
    const requestBodies: Record<string, unknown>[] = [];
    stubMintFetch((init) => {
      // The mutation always sends a JSON string body (use-ai-connections.ts
      // calls JSON.stringify), so this narrows rather than stringifying an
      // arbitrary BodyInit.
      requestBodies.push(JSON.parse(init?.body as string) as Record<string, unknown>);
      return jsonResponse({ ...MINTED, site_scope_mode: "tags" }, 201);
    });

    renderWizard();
    await pickClient("Cursor");
    fireEvent.click(authCard("token"));
    await screen.findByTestId("site-step-count");
    fireEvent.click(screen.getByRole("radio", { name: /by tag/i }));
    fireEvent.click(screen.getByRole("button", { name: /\+ add tags/i }));
    fireEvent.click(within(await screen.findByTestId("site-step-picker")).getAllByRole("checkbox")[0]!);
    fireEvent.click(await screen.findByRole("button", { name: /generate connection token/i }));

    await screen.findByText(/this is the only time this token is shown/i);
    expect(requestBodies).toHaveLength(1);
    const sentBody = requestBodies[0];
    expect(sentBody?.scope_tag_ids).toEqual(["tag-uuid-1"]);
    expect(JSON.stringify(sentBody)).not.toContain("prod");
  });

  it("refuses to mint on an unresolved tag scope, with a remedy distinct from a server failure", async () => {
    // tags stays null (the registry has not finished loading) while mode is
    // "tags" -- resolveTagIds returns null regardless of what is selected, so
    // this is the client-side refusal the mint panel exists to make loud
    // rather than silently narrowing the request.
    loadedFleet(3);
    mockedTags.mockReturnValue(mockQueryResult<SiteTag[]>({ data: undefined, isPending: true }));
    renderWizard();
    await pickClient("Cursor");
    fireEvent.click(authCard("token"));
    await screen.findByTestId("site-step-count");
    fireEvent.click(screen.getByRole("radio", { name: /by tag/i }));

    expect(await screen.findByText(/tag scope could not be resolved/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /generate connection token/i })).toBeDisabled();
  });

  // -------------------------------------------------------------------------
  // THE PAYLOAD THE SERVER WOULD REFUSE IS NEVER SENT. ValidateSiteScopeRequest
  // (apps/api/internal/mcp/scope.go lines 168-200) refuses mode 'tags' with no
  // tag ids, mode 'list' with no site ids, and mode 'all' that carries either.
  // The gate on this screen asked isScopeApprovable instead, which holds an
  // empty scope to be a working state, so the button was enabled for requests
  // the server answers with a 400 the operator can do nothing about.
  // -------------------------------------------------------------------------

  it("refuses to mint from the state the wizard opens in, with a local remedy", async () => {
    // Reachable by doing nothing: the wizard opens on mode 'list' with no
    // sites picked, so this was pick a client, pick the token method, press
    // the button, get a 400.
    loadedFleet(3);
    renderWizard();
    await pickClient("Cursor");
    fireEvent.click(authCard("token"));

    const button = await screen.findByRole("button", { name: /generate connection token/i });
    expect(button).toBeDisabled();
    expect(screen.getByText(/no site is picked/i)).toHaveTextContent(
      /pick at least one site in step 3, or switch that step to all sites/i,
    );
    // The remedy is the operator's own, not "the server said no".
    expect(screen.queryByText(/the server refused this request/i)).toBeNull();
  });

  it("refuses a by-tag scope with no tag picked, distinctly from an unreadable registry", async () => {
    // The registry HAS loaded and does contain tags. Nothing is unresolved
    // here; the operator has simply picked none, and telling them to go and
    // fix a tag registry that is working is a remedy they cannot follow.
    loadedFleet(3, () => ["prod"]);
    mockedTags.mockReturnValue(
      mockQueryResult<SiteTag[]>({ data: [{ id: "tag-uuid-1", name: "prod" } as SiteTag] }),
    );
    renderWizard();
    await pickClient("Cursor");
    fireEvent.click(authCard("token"));
    await screen.findByTestId("site-step-count");
    fireEvent.click(screen.getByRole("radio", { name: /by tag/i }));

    expect(
      await screen.findByRole("button", { name: /generate connection token/i }),
    ).toBeDisabled();
    expect(screen.getByText(/no tag is picked/i)).toBeInTheDocument();
    expect(screen.queryByText(/tag scope could not be resolved/i)).toBeNull();
  });

  it("sends no site ids under 'all sites', even when sites were picked first", async () => {
    // Step 3 KEEPS the other mode's selection when the segmented control is
    // flipped, on purpose, so a mode changed by accident does not throw away
    // the other answer. That is right for the picker and wrong for the wire:
    // mode 'all' carrying site ids is refused outright.
    loadedFleet(3);
    const requestBodies: Record<string, unknown>[] = [];
    stubMintFetch((init) => {
      requestBodies.push(JSON.parse(init?.body as string) as Record<string, unknown>);
      return jsonResponse({ ...MINTED, site_scope_mode: "all" }, 201);
    });

    const button = await reachMintButton();
    expect(button).toBeEnabled();
    fireEvent.click(screen.getByRole("radio", { name: /all sites/i }));
    fireEvent.click(await screen.findByRole("button", { name: /generate connection token/i }));

    await screen.findByText(MINTED.token);
    expect(requestBodies).toHaveLength(1);
    expect(requestBodies[0]?.site_scope_mode).toBe("all");
    expect(requestBodies[0]?.scope_site_ids).toEqual([]);
    expect(requestBodies[0]?.scope_tag_ids).toEqual([]);
  });

  it("sends no site ids under 'by tag', even when sites were picked first", async () => {
    // The same leak in the other direction: mode 'tags' must not also name
    // sites, and the picked site ids were going out beside the tag ids.
    loadedFleet(3, () => ["prod"]);
    mockedTags.mockReturnValue(
      mockQueryResult<SiteTag[]>({ data: [{ id: "tag-uuid-1", name: "prod" } as SiteTag] }),
    );
    const requestBodies: Record<string, unknown>[] = [];
    stubMintFetch((init) => {
      requestBodies.push(JSON.parse(init?.body as string) as Record<string, unknown>);
      return jsonResponse({ ...MINTED, site_scope_mode: "tags" }, 201);
    });

    await reachMintButton();
    fireEvent.click(screen.getByRole("radio", { name: /by tag/i }));
    fireEvent.click(screen.getByRole("button", { name: /\+ add tags/i }));
    fireEvent.click(within(await screen.findByTestId("site-step-picker")).getAllByRole("checkbox")[0]!);
    fireEvent.click(await screen.findByRole("button", { name: /generate connection token/i }));

    await screen.findByText(MINTED.token);
    expect(requestBodies).toHaveLength(1);
    expect(requestBodies[0]?.scope_tag_ids).toEqual(["tag-uuid-1"]);
    expect(requestBodies[0]?.scope_site_ids).toEqual([]);
  });

  it("renders a network failure as a network failure, never a confident empty state", async () => {
    loadedFleet(3);
    vi.stubGlobal("fetch", vi.fn(() => Promise.reject(new TypeError("Failed to fetch"))));

    fireEvent.click(await reachMintButton());

    expect(await screen.findByText(/did not reach the server/i)).toBeInTheDocument();
    expect(screen.queryByText(/this is the only time this token is shown/i)).not.toBeInTheDocument();
  });

  it("renders the 403 org-scope refusal with its own remedy, distinct from every other failure", async () => {
    loadedFleet(3);
    stubMintFetch(() =>
      jsonResponse(
        {
          code: "mcp_org_scope_required",
          message:
            "an AI connection is an organisation-wide credential, so listing or revoking one requires full organisation membership. This is a refusal, not an empty list.",
        },
        403,
      ),
    );

    fireEvent.click(await reachMintButton());

    expect(await screen.findByText(/cannot mint a connection token/i)).toBeInTheDocument();
    expect(screen.getByText(/organisation-wide credential/i)).toBeInTheDocument();
    // Not the 429 copy, not the network copy, not the tag copy.
    expect(screen.queryByText(/too many connection tokens/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/did not reach the server/i)).not.toBeInTheDocument();
  });

  it("renders the 429 rate limit with the server's own retry-after, distinct from a 403", async () => {
    loadedFleet(3);
    stubMintFetch(() =>
      jsonResponse(
        {
          code: "mcp_mint_rate_limited",
          message: "too many connection tokens minted; retry shortly",
          details: { retry_after_seconds: 42 },
        },
        429,
      ),
    );

    fireEvent.click(await reachMintButton());

    expect(await screen.findByText(/too many connection tokens minted recently/i)).toBeInTheDocument();
    expect(screen.getByText(/wait about 42 seconds/i)).toBeInTheDocument();
    expect(screen.queryByText(/organisation-wide credential/i)).not.toBeInTheDocument();
  });
});
