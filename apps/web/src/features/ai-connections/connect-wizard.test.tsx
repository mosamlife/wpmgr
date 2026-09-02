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
import { resolveCursorPos, WALK_SPEC_ORDER } from "./connect-wizard";
import { authKeys } from "@/features/auth/use-auth";
import { CLIENT_TABLE_VERIFIED_AT, MCP_CLIENTS } from "./client-table";
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

// The role this route's principal holds. The wizard itself is now behind
// canManage, mirroring PermAPIKeyManage -> RoleAdmin
// (apps/api/internal/authz/role.go:241), so every test that wants to see the
// wizard needs a principal who could actually finish it. Seeded into the cache
// rather than mocked at useMe, so the real canManage runs over a real Me shape.
let wizardRole: "owner" | "admin" | "operator" | "viewer" = "admin";

const WIZARD_TENANT = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";

function renderWizard() {
  const queryClient = createTestQueryClient();
  queryClient.setQueryData(authKeys.me, {
    id: "user-1",
    email: "priya@example.test",
    name: "Priya",
    scope: "org",
    role: wizardRole,
    active_tenant_id: WIZARD_TENANT,
    memberships: [{ tenant_id: WIZARD_TENANT, role: wizardRole, tenant_name: "Example" }],
  });
  return renderWithProviders(<ConnectPage />, {
    withRouter: true,
    initialPath: "/ai/connect",
    queryClient,
  });
}

function continueButton(): HTMLButtonElement {
  const el = screen.getByRole("button", { name: /^continue$/i });
  if (!(el instanceof HTMLButtonElement)) throw new Error("Continue is not a button");
  return el;
}

function backButton(): HTMLButtonElement {
  const el = screen.getByRole("button", { name: /^back$/i });
  if (!(el instanceof HTMLButtonElement)) throw new Error("Back is not a button");
  return el;
}

/**
 * Press Continue, and FAIL rather than silently do nothing if it is refused.
 *
 * A disabled button swallows a fireEvent click without complaint, so a helper
 * that just clicked would turn "the wizard refused to advance" into "the
 * assertion after this one failed for an unrelated-looking reason". Every walk
 * below goes through here, so a step that starts refusing advancement reddens
 * at the line that tried to advance.
 */
function goNext() {
  const button = continueButton();
  expect(button).toBeEnabled();
  fireEvent.click(button);
}

/**
 * Leave the contract step, if that is where the walk currently stands.
 *
 * SPECIFIED STEP 1 IS A READING STEP AND IT IS WHERE EVERY WALK NOW STARTS.
 * Guarded rather than unconditional so a helper that calls this after the walk
 * has already moved on cannot advance a step nobody asked it to: the presence
 * of the contract block is the only thing that makes it act.
 */
async function leaveContractStep() {
  // ALREADY PAST IT IS A NO-OP, so a test may call this after `renderWizard`
  // and still hand the walk to `pickClient`, which calls it again.
  if (screen.queryByRole("button", { name: /claude code/i }) !== null) return;
  // AWAITED, NOT PROBED. The route paints a role check and a fleet read before
  // the wizard exists, so a synchronous `queryBy` here answers null for a step
  // that is about to render, silently skips the advance, and leaves every
  // caller timing out on a client card the walk never reached.
  await screen.findByTestId("connection-contract");
  goNext();
}

/**
 * Pick a client AND advance past it, because the wizard shows one step at a
 * time and almost every test below is about a later step. Tests that are about
 * the client step itself use `pickClientOnly`.
 */
async function pickClient(name: string) {
  const card = await pickClientOnly(name);
  goNext();
  return card;
}

async function pickClientOnly(name: string) {
  await leaveContractStep();
  const card = await screen.findByRole("button", { name: new RegExp(name, "i") });
  fireEvent.click(card);
  return card;
}

/**
 * Walk forward to the auth step, answering the two steps that now come before
 * it, and stop there.
 *
 * SITE SCOPE AND CAPABILITIES BOTH PRECEDE THE AUTH METHOD in the specified
 * order, and both gate Continue on their own answer, so a walk to step 5 has
 * to pass through them. All sites is the shortest scope answer that works
 * against any fleet, the empty one included; the capability picker opens with
 * `mcp.sites.read` ticked, so it is already settled and only needs Continue.
 */
function advanceToAuthStep() {
  if (document.querySelector('button[data-method="oauth"]') !== null) return;
  if (screen.queryByTestId("site-step-count") !== null) {
    chooseAllSites();
    goNext();
  }
  if (document.querySelector('button[data-method="oauth"]') === null) goNext();
}


/** All sites: the shortest scope answer that works against any fleet, empty included. */
function chooseAllSites() {
  fireEvent.click(screen.getByRole("radio", { name: /all sites/i }));
}

/**
 * Client chosen, standing ON the site-scope step with nothing answered.
 *
 * IT NO LONGER TAKES AN AUTH METHOD, and that is the reorder rather than a
 * simplification: step 3 comes before step 5, so at this point in the walk no
 * method has been chosen and no test can arrange one. The wizard behaves the
 * same here for every eventual method, because the gate reads the scope answer
 * and nothing else.
 */
async function reachSiteScopeStep(clientName: string) {
  await pickClient(clientName);
  return screen.findByTestId("site-step-count");
}

/**
 * Walk from nothing to the setup artefact for one client and method: contract,
 * client, sites, capabilities, auth, setup.
 *
 * THE SCOPE IS ANSWERED ON EVERY PATH NOW, not just the token one. The wizard
 * refuses Continue on an unanswered scope for everybody, because at step 3 it
 * cannot know which path this will turn out to be, so a walk that skipped it
 * would be testing a screen no operator can reach.
 */
async function reachSetupStep(clientName: string, method: "oauth" | "token") {
  await pickClient(clientName);
  await screen.findByTestId("site-step-count");
  chooseAllSites();
  goNext();
  await screen.findByRole("heading", { name: /^4\. Choose what it may do$/ });
  goNext();
  await screen.findByRole("heading", { name: /^5\. Choose how it authenticates$/ });
  fireEvent.click(authCard(method));
  goNext();
}

/** Back to the auth-method step, from whichever later step the walk is on. */
function backToMethodStep() {
  while (document.querySelector('button[data-method="oauth"]') === null) {
    const button = backButton();
    expect(button).toBeEnabled();
    fireEvent.click(button);
  }
}

/**
 * Back to the site-scope step, and re-open its picker.
 *
 * The picker is a disclosure that opens on "+ add sites" and closes with the
 * step, so a walk that returns here finds it shut. Re-opening it is part of
 * arriving, not part of what any caller is testing.
 */
function backToSiteStep() {
  while (screen.queryByTestId("site-step-count") === null) {
    const button = backButton();
    expect(button).toBeEnabled();
    fireEvent.click(button);
  }
  const reopen = screen.queryByRole("button", { name: /\+ add sites/i });
  if (reopen !== null) fireEvent.click(reopen);
  return screen.getByTestId("site-step-count");
}

/**
 * Forward from the site-scope step to the mint button, answering nothing else
 * except the auth method the mint button only exists for.
 *
 * The auth step is now BETWEEN the capability step and the setup artefact, so
 * a walk to the mint button passes through it. It is answered here rather than
 * left to the caller because a walk that stopped short would never reach the
 * button any caller is asking for.
 */
async function forwardToMintButton() {
  goNext();
  await screen.findByRole("heading", { name: /^4\. Choose what it may do$/ });
  return forwardToMintButtonFromCapabilities();
}

/** Forward from the capability step to the mint button. */
async function forwardToMintButtonFromCapabilities() {
  goNext();
  await screen.findByRole("heading", { name: /^5\. Choose how it authenticates$/ });
  fireEvent.click(authCard("token"));
  goNext();
  return screen.findByRole("button", { name: /generate connection token/i });
}

/**
 * Client chosen and the walk carried to the auth step.
 *
 * The auth cards are at specified step 5 now, with site scope and capabilities
 * between them and the client cards, so a test about a card has to walk there.
 */
async function reachAuthStep(clientName: string) {
  await pickClient(clientName);
  await screen.findByTestId("site-step-count");
  advanceToAuthStep();
  return screen.findByRole("heading", { name: /^5\. Choose how it authenticates$/ });
}

function authCard(method: "oauth" | "token"): HTMLButtonElement {
  const el = document.querySelector<HTMLButtonElement>(`button[data-method="${method}"]`);
  // A missing card must fail the test rather than letting every assertion
  // below it be skipped.
  if (el === null) throw new Error(`no auth card rendered for "${method}"`);
  return el;
}

// HIDING THE LINK IS NOT GUARDING THE ROUTE.
//
// /ai stops offering "New connection" to a principal who cannot mint, and this
// route had no beforeLoad, so the URL still rendered the whole wizard. The
// refusal arrived from the server at the last button, after the operator had
// picked a client, an auth method and a site scope. These tests are the reason
// that cannot come back quietly.
describe("the wizard refuses a principal who could not finish it", () => {
  afterEach(() => {
    wizardRole = "admin";
  });

  it("renders no wizard at all for an operator who types the URL", async () => {
    wizardRole = "operator";
    renderWizard();
    expect(await screen.findByTestId("connect-role-refused")).toBeInTheDocument();
    // NOT "the first step is hidden". No part of the wizard renders, so there
    // is no work to lose and no 403 to walk into.
    expect(screen.queryByRole("button", { name: /claude code/i })).toBeNull();
    expect(document.querySelector('button[data-method="oauth"]')).toBeNull();
  });

  it("renders no wizard for a viewer either", async () => {
    wizardRole = "viewer";
    renderWizard();
    expect(await screen.findByTestId("connect-role-refused")).toBeInTheDocument();
  });

  it("says the refusal arrives before the work, not at the last button", async () => {
    wizardRole = "viewer";
    renderWizard();
    expect(
      await screen.findByText(/nothing has been created and nothing was attempted/i),
    ).toBeInTheDocument();
  });

  it("renders the wizard for an owner, so the guard is not blanket", async () => {
    // The over-fire half. A guard that refuses correct work gets switched off.
    wizardRole = "owner";
    renderWizard();
    await leaveContractStep();
    expect(await screen.findByRole("button", { name: /claude code/i })).toBeInTheDocument();
    expect(screen.queryByTestId("connect-role-refused")).toBeNull();
  });
});

describe("the wizard asks for the client first", () => {
  it("renders every row in the table as a picker card, including the generic one", async () => {
    renderWizard();
    await leaveContractStep();
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
    await leaveContractStep();
    await screen.findByRole("button", { name: /claude code/i });
    expect(document.querySelector('button[data-method="oauth"]')).toBeNull();
  });
});

// STEP "HOW IT SIGNS IN", BROUGHT TO ITS SPECIFIED COPY.
//
// Every assertion is on the exact sentence. The two cards used to carry
// one-line summaries ("Connection token", "Nothing to copy or keep secret"),
// which are true but leave the two decisions this screen actually asks an
// operator to make -- what is shown to me, and where does the secret live --
// to be inferred. The words are the deliverable, so the words are pinned.
describe("the auth cards say what each method actually does", () => {
  it("states, on the browser card, that nothing secret is ever shown", async () => {
    renderWizard();
    await leaveContractStep();
    await reachAuthStep("Claude Code");
    const oauth = authCard("oauth");
    expect(within(oauth).getByText("Sign in through your browser")).toBeInTheDocument();
    expect(
      within(oauth).getByText(
        "You approve the connection on a WPMgr page and the client stores the token it is issued. Nothing secret is ever shown to you or pasted anywhere. That token does not refresh itself, so the connection stops working when it expires.",
      ),
    ).toBeInTheDocument();
  });

  it("promises no refresh on any auth card, because the server mints none", async () => {
    // THE SAME GUARD AS CONTRACT_FORBIDDEN, ON THE OTHER SCREEN THAT CAN MAKE
    // THIS CLAIM. apps/api/internal/mcp/service.go:140: "There is no
    // refresh_token grant: the connection token's lifetime is the connection's,
    // and nothing here mints a refresh token", and discovery_test.go:256 drives
    // the grant-type validator with "refresh_token" to prove the discovery
    // document refuses it. The design frame promises a self-refreshing token;
    // the deck is wrong and the server is right, so a fidelity pass that
    // restores the frame's wording turns this red.
    //
    // The word, not the sentence. "refreshes itself", "auto-refresh" and "will
    // be refreshed" are the same false claim in three shapes, and pinning one
    // phrasing would let the next two through.
    renderWizard();
    await leaveContractStep();
    await reachAuthStep("Claude Code");

    // The card is ALLOWED to say "does not refresh itself" and nothing else
    // about refreshing. Striking the denial and then looking for the word is
    // what makes this fire on an affirmative claim while passing on the
    // correction -- a bare /refresh/i assertion would reject the true sentence
    // along with the false one and get switched off by the next person here.
    const oauthText = (authCard("oauth").textContent ?? "").replace(
      /does not refresh itself/gi,
      "",
    );
    expect(oauthText).not.toMatch(/refresh/i);
    expect(authCard("token").textContent ?? "").not.toMatch(/refresh/i);

    // And it says what DOES happen, so the guard cannot be satisfied by
    // deleting the sentence rather than correcting it.
    expect(
      within(authCard("oauth")).getByText(/stops working when it expires/i),
    ).toBeInTheDocument();
  });

  it("states, on the token card, that it is documented and not a fallback", async () => {
    renderWizard();
    await leaveContractStep();
    await reachAuthStep("Claude Code");
    const token = authCard("token");
    expect(within(token).getByText("Use a connection token")).toBeInTheDocument();
    expect(
      within(token).getByText(
        "We show a token once. You put it in your environment, and the client sends it as a header. This is the documented path for CI, containers and SSH, not a fallback for when sign-in fails.",
      ),
    ).toBeInTheDocument();
  });

  it("marks which card is the answer when both methods are open", async () => {
    // Claude Code offers both, so there is a recommendation to make.
    renderWizard();
    await leaveContractStep();
    await reachAuthStep("Claude Code");
    expect(within(authCard("oauth")).getByText("Recommended for Claude Code")).toBeInTheDocument();
    expect(
      within(authCard("token")).getByText("The documented headless path"),
    ).toBeInTheDocument();
  });

  it("says 'the only route' rather than recommending, when there is one route", async () => {
    // Claude Desktop's add-connector dialog has no header field, so the token
    // card is disabled and the browser card is not a preference.
    renderWizard();
    await leaveContractStep();
    await reachAuthStep("Claude Desktop");
    expect(
      within(authCard("oauth")).getByText("The only route for this client"),
    ).toBeInTheDocument();
    // A recommendation on a control nobody can press is noise.
    expect(within(authCard("token")).queryByText(/recommended|only route|documented headless/i))
      .toBeNull();
    expect(within(authCard("token")).getByText("not possible here")).toBeInTheDocument();
  });

  it("says why there is no device-code option, with the date it was checked", async () => {
    // Without this, a disabled browser card on a headless client reads as an
    // oversight and the token reads as our fallback. It is neither.
    renderWizard();
    await leaveContractStep();
    await reachAuthStep("Claude Code");
    expect(
      await screen.findByText("Why there is no “enter this code on another device” option"),
    ).toBeInTheDocument();
    expect(screen.getByText(/no MCP client implements one/i)).toBeInTheDocument();
    // The date is rendered from the client table's own constant, so it goes
    // stale visibly. A literal here would pass while the table moved on.
    expect(screen.getByText(new RegExp(CLIENT_TABLE_VERIFIED_AT))).toBeInTheDocument();
  });
});

// ---------------------------------------------------------------------------
// The step rail: aria-current placement (defect 2) and the ten specified
// steps (defect 3, design S29 "ADD MCP CONNECTION: THE TEN STEPS").
// ---------------------------------------------------------------------------

/**
 * The single element the rail marks as the operator's current step.
 *
 * READ FROM `aria-current`, NOT FROM `data-step-state`. Position and
 * appearance are two facts: the operator stands on exactly one step, and that
 * step may be one whose gate is refusing, in which case its state carries the
 * REASON ("unselected", "loading") rather than the word "current". Querying
 * the state would find no current step at all on any frame where the step in
 * front of the operator is unanswered, which is every wizard's opening frame.
 */
function currentRailStep(): HTMLElement {
  const els = document.querySelectorAll('[aria-current="step"]');
  // Never zero, and never more than one: a rail agreeing with itself about
  // where the operator is is the entire point of aria-current.
  expect(els).toHaveLength(1);
  const el = els[0];
  if (!(el instanceof HTMLElement)) throw new Error("current step element is not an HTMLElement");
  return el;
}

describe("the step rail names all ten specified steps and marks the right one current", () => {
  it("renders all ten specified steps, in specified order, on every step of the walk", async () => {
    // RULING 15: the stepper is persistent. It never shortens, so an operator
    // sees the whole path from the first frame and the rail does not change
    // length under them halfway through.
    renderWizard();
    await leaveContractStep();
    await screen.findByRole("button", { name: /claude code/i });
    expect(railNumbers()).toEqual(Array.from({ length: 10 }, (_, i) => String(i + 1)));

    await reachSetupStep("Cursor", "token");
    expect(railNumbers()).toEqual(Array.from({ length: 10 }, (_, i) => String(i + 1)));
  });

  it("names exactly six built steps, and the other four as not yet available", async () => {
    renderWizard();
    await leaveContractStep();
    await screen.findByRole("button", { name: /claude code/i });

    expect(railStateNs("not-built")).toEqual(["7", "8", "9", "10"]);
    for (const n of ["7", "8", "9", "10"]) {
      expect(railSegment(n)).not.toHaveAttribute("aria-current", "step");
    }
  });

  // ---------------------------------------------------------------------------
  // THE INVARIANT, ASSERTED AS A PROPERTY RATHER THAN AS A POSITION.
  //
  // The wizard clamps the cursor to the first step whose gate refuses, so three
  // things hold at EVERY reachable point of EVERY walk. These assert them as
  // such rather than pinning the segment a particular sequence of clicks lands
  // on: a test written against the walk's internals passes until the walk is
  // reordered; one written against the property survives it.
  //
  //   1. Exactly one segment is current.
  //   2. A step with no section on this path is never that segment.
  //   3. Continue is offered exactly when the current segment's state does not
  //      carry a refusal -- one predicate, read by the rail and the button.
  // ---------------------------------------------------------------------------

  /** The four states a segment carries when its own gate is refusing. */
  const BLOCKED_STATES = ["loading", "failed", "tags-unresolved", "unselected"];

  /**
   * Assert the invariants against whatever the rail is showing now.
   *
   * NOTE WHAT THIS DELIBERATELY DOES NOT DO: assert where in the walk the
   * operator is. These are properties that hold at every point of every walk;
   * position claims are made in the individual tests below, and the walk's
   * order itself is asserted as a sequence in "the walk is the specified order".
   */
  function expectRailIsCoherent() {
    const current = currentRailStep();

    for (const el of railSegments()) {
      const state = el.dataset.stepState;
      if (state !== "not-built") continue;
      expect(el).not.toHaveAttribute("aria-current", "step");
      expect(state).not.toBe("completed");
    }

    const refused = BLOCKED_STATES.includes(current.dataset.stepState ?? "");
    const advance = screen.queryByRole("button", { name: /^continue$/i });
    if (advance !== null) {
      expect(advance).toHaveProperty("disabled", refused);
    }
    return current;
  }

  it("keeps exactly one segment current at every step of a token walk", async () => {
    loadedFleet(3);
    renderWizard();
    await leaveContractStep();

    await screen.findByRole("button", { name: /claude code/i });
    expectRailIsCoherent();

    await pickClientOnly("Cursor");
    expectRailIsCoherent();
    goNext();

    await screen.findByTestId("site-step-count");
    expectRailIsCoherent();
    chooseAllSites();
    expectRailIsCoherent();
    goNext();

    await screen.findByRole("heading", { name: /^4\. Choose what it may do$/ });
    expectRailIsCoherent();
    goNext();

    await screen.findByRole("heading", { name: /^5\. Choose how it authenticates$/ });
    expectRailIsCoherent();
    fireEvent.click(authCard("token"));
    expectRailIsCoherent();
    goNext();

    await screen.findByRole("button", { name: /generate connection token/i });
    expectRailIsCoherent();
  });

  it("advances the rail only when the operator does, never on an answer alone", async () => {
    // The old wizard derived its position from the answers, so answering a
    // question moved the rail by itself while five steps' content stayed on
    // screen. Now the answer settles the gate and Continue moves the cursor;
    // they are two acts and the rail follows the second.
    renderWizard();
    await leaveContractStep();
    await pickClientOnly("Cursor");

    expect(currentRailStep()).toHaveAttribute("data-step-n", "2");
    goNext();
    expect(currentRailStep()).toHaveAttribute("data-step-n", "3");
    expect(railSegment("2")).toHaveAttribute("data-step-state", "completed");
  });

  it("walks the specified numbers in order, 1 through 6, and never skips one", async () => {
    // THE DEFECT THIS FILE WAS REPORTED FOR, ASSERTED AS A SEQUENCE. The wizard
    // used to walk 2, 5, 3, 4, 6, so an operator leaving step 2 landed on step
    // 5. Recording the walk and comparing the WHOLE list is what makes a future
    // reorder fail here: a test that asked "is step 3 reachable" would pass
    // against any ordering that contains it.
    loadedFleet(3);
    renderWizard();
    // NOT `leaveContractStep` here: the first step has to be RECORDED before
    // it is left, and a helper that leaves it would drop it from the sequence.
    await screen.findByTestId("connection-contract");

    const visited: string[] = [currentRailStep().dataset.stepN!];
    goNext();
    visited.push(currentRailStep().dataset.stepN!);
    await pickClientOnly("Cursor");
    goNext();
    visited.push(currentRailStep().dataset.stepN!);
    await screen.findByTestId("site-step-count");
    chooseAllSites();
    goNext();
    await screen.findByRole("heading", { name: /^4\. Choose what it may do$/ });
    visited.push(currentRailStep().dataset.stepN!);
    goNext();
    await screen.findByRole("heading", { name: /^5\. Choose how it authenticates$/ });
    visited.push(currentRailStep().dataset.stepN!);
    fireEvent.click(authCard("token"));
    goNext();
    await screen.findByRole("button", { name: /generate connection token/i });
    visited.push(currentRailStep().dataset.stepN!);

    // The step the walk STARTS on is step 1, not step 2, and it is in the list
    // for that reason: the contract is the first thing an operator reads.
    expect(visited).toEqual(["1", "2", "3", "4", "5", "6"]);
    // The same sequence as the module's own written statement of the walk, so
    // the two cannot drift into disagreeing about what the wizard does.
    expect(WALK_SPEC_ORDER).toEqual([1, 2, 3, 4, 5, 6]);
  });

  it("marks every step behind the operator complete, in that same order", async () => {
    loadedFleet(3);
    renderWizard();
    await leaveContractStep();
    await reachSetupStep("Cursor", "oauth");

    expect(currentRailStep()).toHaveAttribute("data-step-n", "6");
    for (const n of ["1", "2", "3", "4", "5"]) {
      expect(railSegment(n)).toHaveAttribute("data-step-state", "completed");
    }
  });

  it("re-blocks a step whose answer is taken away again, rather than latching it done", async () => {
    // THE GATE IS RE-EVALUATED ON EVERY RENDER, NEVER LATCHED AT THE MOMENT
    // CONTINUE WAS PRESSED. An operator who walks back and removes the answer
    // that let them through must be held again: the mint that answer was
    // gating would now be refused, so a rail still calling step 3 complete
    // would be asserting a readiness the button does not have.
    loadedFleet(3);
    renderWizard();
    await leaveContractStep();
    await advanceToCapabilityStep();
    await forwardToMintButtonFromCapabilities();
    expect(railSegment("3")).toHaveAttribute("data-step-state", "completed");

    backToSiteStep();
    fireEvent.click(pickerBoxes()[0]!);

    const current = expectRailIsCoherent();
    expect(current).toHaveAttribute("data-step-n", "3");
    expect(current).toHaveAttribute("data-step-state", "unselected");
    expect(continueButton()).toBeDisabled();
    expect(railSegment("4")).toHaveAttribute("data-step-state", "upcoming");
    expect(railSegment("6")).toHaveAttribute("data-step-state", "upcoming");
    expect(screen.queryByRole("button", { name: /generate connection token/i })).toBeNull();
  });

  // ---------------------------------------------------------------------------
  // The four ways the site-scope step can be unsettled, each kept distinct
  // rather than collapsed into one muted "not done yet". An operator told
  // nothing when a read has actually failed keeps waiting for a state that is
  // never coming, and one told "loading" for their own unmade selection is told
  // something false about themselves.
  // ---------------------------------------------------------------------------

  it.each([
    [
      "loading",
      /\(loading\)/i,
      () =>
        mockedSites.mockReturnValue(mockQueryResult<Site[]>({ data: undefined, isPending: true })),
      undefined,
    ],
    [
      "failed",
      /\(failed to load\)/i,
      () =>
        mockedSites.mockReturnValue(
          mockQueryResult<Site[]>({ data: undefined, isPending: false, isError: true }),
        ),
      undefined,
    ],
    [
      "tags-unresolved",
      /\(tags still loading\)/i,
      () => {
        loadedFleet(3);
        mockedTags.mockReturnValue(mockQueryResult<SiteTag[]>({ data: undefined, isPending: true }));
      },
      () => fireEvent.click(screen.getByRole("radio", { name: /by tag/i })),
    ],
    ["unselected", /\(not chosen yet\)/i, () => loadedFleet(3), undefined],
  ] as const)(
    "holds the operator on site scope, and says it is %s, while a token mint would be refused",
    async (readiness, annotation, seed, answer) => {
      seed();
      renderWizard();
    await leaveContractStep();
      await reachSiteScopeStep("Cursor");
      answer?.();

      // THE REFUSAL IS REAL FIRST. Everything below is vacuous if the walk was
      // never actually held here.
      const current = expectRailIsCoherent();
      expect(current).toHaveAttribute("data-step-n", "3");
      expect(continueButton()).toBeDisabled();

      // The reason is named, and it is THIS reason, not a generic one.
      expect(current).toHaveAttribute("data-step-state", readiness);
      expect(screen.getByTestId("step-label-3")).toHaveTextContent(annotation);
      // NO RING AND NO FILL on a step whose action is being refused -- the
      // style table maps the state and nothing reads a second boolean.
      expect(screen.getByTestId("step-circle-3").className).not.toContain("border-2");
      // And the setup step behind it is neither reached nor reachable.
      expect(railSegment("6")).toHaveAttribute("data-step-state", "upcoming");
      expect(screen.queryByRole("button", { name: /generate connection token/i })).toBeNull();
    },
  );

  it("lets the walk through the moment the scope is actually answered", async () => {
    // THE OVER-FIRE ARM of the four cases above. A gate that never opens is not
    // a gate; it just moves the false state from "prematurely done" to
    // "permanently stuck".
    loadedFleet(3);
    renderWizard();
    await leaveContractStep();
    await reachSiteScopeStep("Cursor");
    expect(continueButton()).toBeDisabled();

    chooseAllSites();

    expect(continueButton()).toBeEnabled();
    expect(currentRailStep()).toHaveAttribute("data-step-state", "current");
    goNext();
    expect(currentRailStep()).toHaveAttribute("data-step-n", "4");
    expect(railSegment("3")).toHaveAttribute("data-step-state", "completed");
  });

  // ---------------------------------------------------------------------------
  // THE SITE-SCOPE GATE DOES NOT DEPEND ON THE AUTH METHOD, and it cannot: the
  // method is answered two steps later. This block used to assert the opposite
  // -- that a browser sign-in walk was waved through an unanswered scope --
  // which was only expressible while the auth step came second. Waving it
  // through now would refuse the operator at step 6 with a sentence about step
  // 3, a refusal pointing backwards at a step they were told was settled.
  // ---------------------------------------------------------------------------

  it("holds every walk at site scope, whichever method is chosen afterwards", async () => {
    loadedFleet(3);
    renderWizard();
    await leaveContractStep();
    await reachSiteScopeStep("Cursor");

    const current = expectRailIsCoherent();
    expect(current).toHaveAttribute("data-step-n", "3");
    expect(continueButton()).toBeDisabled();

    // The over-fire arm, and it carries the walk all the way to the browser
    // sign-in path: the gate opens on the answer, and nothing about the method
    // chosen two steps later reopens it.
    chooseAllSites();
    expect(continueButton()).toBeEnabled();
    goNext();
    await screen.findByRole("heading", { name: /^4\. Choose what it may do$/ });
    goNext();
    await screen.findByRole("heading", { name: /^5\. Choose how it authenticates$/ });
    fireEvent.click(authCard("oauth"));
    goNext();
    expect(currentRailStep()).toHaveAttribute("data-step-n", "6");
    expect(railSegment("3")).toHaveAttribute("data-step-state", "completed");
  });

  // ---------------------------------------------------------------------------
  // Specified step 4 is asked of everyone. Ruling 15 keeps the stepper
  // persistent, and with the specified order there is nothing left that could
  // shorten the walk: no answer given before step 4 can remove it.
  // ---------------------------------------------------------------------------

  it("keeps all ten segments, and asks step 4 whatever the eventual method", async () => {
    loadedFleet(3);
    renderWizard();
    await leaveContractStep();
    await reachSiteScopeStep("Cursor");

    expect(railNumbers()).toHaveLength(10);
    expect(railSegment("4")).toHaveAttribute("data-step-state", "upcoming");
    // An unbuilt step still reads differently from one merely ahead of the
    // operator: two different facts, and nobody should have to guess which.
    expect(railSegment("7")).toHaveAttribute("data-step-state", "not-built");
    expect(screen.getByTestId("step-label-7").className).toContain("italic");
    expect(screen.getByTestId("step-label-4").className).not.toContain("italic");

    chooseAllSites();
    goNext();
    expect(currentRailStep()).toHaveAttribute("data-step-n", "4");
    expect(screen.getByRole("heading", { name: /^4\. Choose what it may do$/ })).toBeInTheDocument();
  });

  it("never marks a built step as one the walk will not ask", async () => {
    // The rail has no state for "built, but not asked of you" any more, and it
    // must not acquire one by accident: every built segment is either behind
    // the operator, under them, or ahead of them.
    renderWizard();
    await leaveContractStep();
    await pickClientOnly("Cursor");
    for (const n of ["1", "2", "3", "4", "5", "6"]) {
      expect(railSegment(n)).not.toHaveAttribute("data-step-state", "not-built");
      expect(screen.getByTestId(`step-label-${n}`).className).not.toContain("line-through");
    }
  });
});

describe("the method step is computed from the client, with the reason on the card", () => {
  it("disables the token method for Claude Desktop and says exactly why", async () => {
    renderWizard();
    await leaveContractStep();
    await reachAuthStep("Claude Desktop");

    const token = authCard("token");
    expect(token).toBeDisabled();
    // NEVER A GENERIC "UNAVAILABLE". The card carries the client's own reason.
    expect(within(token).getByText(/no header field/i)).toBeInTheDocument();

    // And the method that DOES work is offered, so the guard is not blanket.
    expect(authCard("oauth")).toBeEnabled();
  });

  it("disables OAuth for VS Code as 'not yet verified by us', with the date", async () => {
    renderWizard();
    await leaveContractStep();
    await reachAuthStep("VS Code");

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
    await leaveContractStep();
    await reachAuthStep("Cursor");
    expect(authCard("oauth")).toBeEnabled();
    expect(authCard("token")).toBeEnabled();
  });
});

describe("the setup artefact is generated per client", () => {
  it("emits the required http type for Claude Code", async () => {
    renderWizard();
    await leaveContractStep();
    await reachSetupStep("Claude Code", "oauth");

    const block = await screen.findByText(/"mcpServers"/);
    const text = block.textContent ?? "";
    expect(text).toContain('"type": "http"');
    expect(text).toContain('"url"');
    // The reason for that line is on screen, not only in a comment.
    expect(screen.getByText(/read as a local process/i)).toBeInTheDocument();
  });

  it("emits httpUrl and no url for Gemini CLI", async () => {
    renderWizard();
    await leaveContractStep();
    await reachSetupStep("Gemini CLI", "oauth");

    const text = (await screen.findByText(/"mcpServers"/)).textContent ?? "";
    expect(text).toContain('"httpUrl"');
    expect(text).not.toContain('"url"');
  });

  it("emits the servers wrapper for VS Code", async () => {
    renderWizard();
    await leaveContractStep();
    await reachSetupStep("VS Code", "token");

    const text = (await screen.findByText(/"servers"/)).textContent ?? "";
    expect(text).toContain('"servers"');
    expect(text).not.toContain('"mcpServers"');
  });

  it("emits no type key for Cursor", async () => {
    renderWizard();
    await leaveContractStep();
    await reachSetupStep("Cursor", "oauth");

    const text = (await screen.findByText(/"mcpServers"/)).textContent ?? "";
    expect(text).not.toContain('"type"');
  });

  it("renders the endpoint and a spec link for the generic entry, with no config block", async () => {
    renderWizard();
    await leaveContractStep();
    await reachSetupStep("Other / generic", "oauth");

    expect(await screen.findByText(/endpoint for other \/ generic/i)).toBeInTheDocument();
    expect(screen.queryByText(/"mcpServers"/)).not.toBeInTheDocument();
    expect(
      screen.getByRole("link", { name: /streamable http specification/i }),
    ).toBeInTheDocument();
  });

  it("gives GUI clients in-app steps rather than a file to edit", async () => {
    renderWizard();
    await leaveContractStep();
    await reachSetupStep("Claude Desktop", "oauth");

    expect(await screen.findByText(/set this up inside claude desktop/i)).toBeInTheDocument();
    expect(screen.queryByText(/"mcpServers"/)).not.toBeInTheDocument();
  });

  it("never prints a Windows path", async () => {
    renderWizard();
    await leaveContractStep();
    await reachSetupStep("Claude Code", "oauth");
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
    await leaveContractStep();
    await reachSetupStep("Cursor", "token");

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
    await leaveContractStep();
    await reachSetupStep("Claude Code", "oauth");
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
    await leaveContractStep();
    await reachSetupStep("Claude Code", "oauth");
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
  // NO AUTH METHOD IS CHOSEN HERE ANY MORE. Step 3 comes before step 5, so
  // arriving on the site step with a method already picked is not a state the
  // wizard can be in.
  await pickClient("Claude Code");
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

  it("numbers the capability step after it, so the rail and the page agree", async () => {
    // ONE STEP AT A TIME, so the ordering is proved by walking it rather than
    // by finding two headings on one page: site scope is where the operator
    // lands, and capabilities is the step after it.
    await reachSiteStep();
    expect(
      screen.getByRole("heading", { name: /^3\. Choose which sites$/ }),
    ).toBeInTheDocument();
    expect(currentRailStep()).toHaveAttribute("data-step-n", "3");

    chooseAllSites();
    goNext();
    expect(screen.getByRole("heading", { name: /^4\. Choose what it may do$/ })).toBeInTheDocument();
    expect(currentRailStep()).toHaveAttribute("data-step-n", "4");
    // And the rail no longer claims sites are chosen somewhere else.
    expect(screen.queryByText(/4\. Choose sites and permissions/i)).not.toBeInTheDocument();
  });

  it("says where the scope answer goes, as the forward conditional it is", async () => {
    // The method is chosen at step 5, AFTER this step, so this footnote cannot
    // state one path's outcome flatly. It used to, which was only true while
    // the auth step came second, and it is the class of false claim the
    // reorder exposes.
    await reachSiteStep();
    expect(screen.getByText(/depends on the sign-in method you choose at step 5/i)).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: /choose what it may do/i })).toBeNull();
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

  it("is a sentence about the selection, not a failure that stops the walk", async () => {
    loadedFleet(60);
    await reachSiteStep();
    // Nothing about the empty state is styled or announced as an error, and
    // the remedy is one click away rather than a wall: All sites settles it.
    expect(screen.queryByRole("alert")).toBeNull();

    chooseAllSites();
    expect(continueButton()).toBeEnabled();
    goNext();
    expect(screen.getByRole("heading", { name: /^4\. Choose what it may do$/ })).toBeInTheDocument();
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
  it("says plainly that the approval screen asks again, on the path where it does", async () => {
    loadedFleet(5);
    await reachSiteStep();
    expect(
      screen.getByText(/nothing carries this selection there and it asks for the scope again/i),
    ).toBeInTheDocument();
  });
});

describe("changing the client recomputes rather than carrying a stale answer", () => {
  it("drops a method the newly chosen client cannot use", async () => {
    loadedFleet(3);
    renderWizard();
    await leaveContractStep();
    await reachSetupStep("Cursor", "token");
    expect(await screen.findByText(/"mcpServers"/)).toBeInTheDocument();

    // Claude Desktop cannot use a token. The wizard must fall back to asking,
    // not silently keep a selection that produces no valid artefact. Changing
    // the client means walking back to the step that owns that answer, which
    // is also where the operator is told the method has been dropped.
    while (screen.queryByRole("button", { name: /claude desktop/i }) === null) {
      fireEvent.click(backButton());
    }
    await pickClientOnly("Claude Desktop");
    expect(screen.queryByText(/set this up inside/i)).not.toBeInTheDocument();

    // The dropped method leaves step 2 unanswered, so the walk cannot run past
    // it: the cursor is held at the method step and the token card is disabled
    // with the client's own reason.
    goNext();
    await screen.findByTestId("site-step-count");
    chooseAllSites();
    goNext();
    await screen.findByRole("heading", { name: /^4\. Choose what it may do$/ });
    goNext();
    expect(currentRailStep()).toHaveAttribute("data-step-n", "5");
    expect(authCard("token")).toBeDisabled();
    expect(continueButton()).toBeDisabled();
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
async function advanceToMintButton(answerScope?: () => void) {
  await advanceToCapabilityStep(answerScope);
  // Past the capability picker, which opens on sites-read and is therefore
  // already settled -- nothing here has to touch it to get through -- and then
  // through the auth step, which now sits between it and the mint button.
  goNext();
  await screen.findByRole("heading", { name: /^5\. Choose how it authenticates$/ });
  fireEvent.click(authCard("token"));
  goNext();
  return screen.findByRole("button", { name: /generate connection token/i });
}

/**
 * Client chosen and A SITE PICKED, standing on the capability step.
 *
 * The site is picked deliberately and is not scaffolding: mode 'list' with
 * nothing selected is refused by ValidateSiteScopeRequest
 * (apps/api/internal/mcp/scope.go), and the wizard refuses Continue on the
 * same predicate, so this is the shortest honest walk past step 3. No auth
 * method is chosen: it is answered at step 5, after this one.
 */
async function advanceToCapabilityStep(answerScope?: () => void) {
  await pickClient("Cursor");
  await screen.findByTestId("site-step-count");
  if (answerScope === undefined) {
    fireEvent.click(screen.getByRole("button", { name: /\+ add sites/i }));
    fireEvent.click(pickerBoxes()[0]!);
  } else {
    answerScope();
  }
  goNext();
  await screen.findByRole("heading", { name: /^4\. Choose what it may do$/ });
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
  // Seeded for the same reason renderWizard is: the route is behind canManage
  // now, and these tests are about the navigation block around an open mint,
  // which only a principal who can mint ever reaches.
  const queryClient = createTestQueryClient();
  queryClient.setQueryData(authKeys.me, {
    id: "user-1",
    email: "priya@example.test",
    name: "Priya",
    scope: "org",
    role: "admin",
    active_tenant_id: WIZARD_TENANT,
    memberships: [{ tenant_id: WIZARD_TENANT, role: "admin", tenant_name: "Example" }],
  });
  render(
    <QueryClientProvider client={queryClient}>
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

// ---------------------------------------------------------------------------
// Step 4 -- "Choose what it may do" (spec S29 step 4), TOKEN PATH ONLY.
// ---------------------------------------------------------------------------

/** Client and token method picked, sitting at the capability picker. */
async function reachCapabilityStep() {
  renderWizard();
  // All sites, because these tests render the default empty fleet and there is
  // no site to pick in it. What the scope IS does not matter here; getting
  // past the step that gates this one does.
  await advanceToCapabilityStep(chooseAllSites);
  return screen.getByRole("heading", { name: /choose what it may do/i });
}

describe("choosing what a token may do (step 4, token path only)", () => {
  it("asks the capability step on a browser sign-in walk too, and says where the answer goes", async () => {
    // The picker is asked before the method is chosen, so it cannot be scoped
    // to one path. What differs is what becomes of the answer, and the step
    // states that rather than the wizard silently dropping it.
    loadedFleet(3);
    renderWizard();
    await leaveContractStep();
    await pickClient("Cursor");
    await screen.findByTestId("site-step-count");
    chooseAllSites();
    goNext();

    expect(screen.getByRole("heading", { name: /^4\. Choose what it may do$/ })).toBeInTheDocument();
    expect(
      screen.getByText(/with browser sign-in the approval screen has no channel for it yet/i),
    ).toBeInTheDocument();
    expect(railSegment("4")).toHaveAttribute("data-step-state", "current");
  });

  it("renders every conferrable capability with a real description, and Content disabled with its reason", async () => {
    await reachCapabilityStep();

    // The seven conferrable rows, each carrying label AND description --
    // never a bare label, which would leave "what it permits" to be guessed.
    // findAllByRole rather than a singular query: a still-loading render could
    // otherwise let this pass against a skeleton.
    const boxes = await screen.findAllByRole("checkbox", { name: /.+/ });
    expect(boxes.length).toBe(8); // seven conferrable + the disabled Content row

    expect(screen.getByRole("checkbox", { name: /^Sites/i })).toBeInTheDocument();
    expect(
      screen.getByText(/see the fleet inventory: site names, urls and tags/i),
    ).toBeInTheDocument();
    expect(screen.getByText(/see uptime checks and outage history/i)).toBeInTheDocument();
    expect(screen.getByText(/see backup runs, their status/i)).toBeInTheDocument();

    // Content: seated in the vocabulary, never conferrable -- disabled, with
    // the reason stated in an operator's words, not "coming soon".
    const content = screen.getByRole("checkbox", { name: /^Content/i });
    expect(content).toBeDisabled();
    expect(
      screen.getByText(/not available yet.*no content tools.*for a connection to call/i),
    ).toBeInTheDocument();
  });

  it("states the negative space once: nothing here can change WordPress content or configuration", async () => {
    await reachCapabilityStep();
    expect(
      screen.getByText(/no capability on this screen can change wordpress content or configuration/i),
    ).toBeInTheDocument();
  });

  it("opens with only sites-read checked -- the server's own default for an omitted field, never empty and never all seven", async () => {
    await reachCapabilityStep();
    expect(screen.getByRole("checkbox", { name: /^Sites/i })).toBeChecked();
    expect(screen.getByRole("checkbox", { name: /^Uptime/i })).not.toBeChecked();
    expect(screen.getByRole("checkbox", { name: /^Backups/i })).not.toBeChecked();
    expect(screen.getByRole("checkbox", { name: /^Security/i })).not.toBeChecked();
  });

  it("refuses to mint when every capability is deselected, rather than defaulting or sending nothing", async () => {
    // THE GUARD UNDER MUTATION TEST. See the PR description for the red/green
    // proof: temporarily changing mintCapabilitiesRequest's `selected.length
    // === 0` to `selected.length === -1` turns this red, and restoring it
    // turns it green again.
    loadedFleet(3);
    renderWizard();
    await leaveContractStep();
    // A valid site scope, so the ONLY thing left blocking is the capability
    // deselection this test is actually about.
    await advanceToCapabilityStep();

    fireEvent.click(screen.getByRole("checkbox", { name: /^Sites/i }));

    // EXACTLY ONE, not merely present. WizardNav owns the one refusal panel
    // for every step; a private panel inside this section rendering the same
    // sentence a second time shipped to production once and a `findAllByText
    // .length > 0` assertion here did not catch it, because two matches
    // satisfy "greater than zero" as easily as one does.
    expect(await screen.findAllByText(/no capability is selected/i)).toHaveLength(1);
    // REFUSED AT CONTINUE NOW, not at a mint button one step further on. The
    // same predicate makes both refusals, so moving the assertion to the
    // control the operator is standing in front of tests the same guard at the
    // point it now fires.
    expect(continueButton()).toBeDisabled();
    expect(screen.queryByRole("button", { name: /generate connection token/i })).toBeNull();
  });

  it("re-enables minting the moment a capability is checked again -- the over-fire arm of the guard above", async () => {
    loadedFleet(3);
    renderWizard();
    await leaveContractStep();
    await advanceToCapabilityStep();

    const sites = screen.getByRole("checkbox", { name: /^Sites/i });
    fireEvent.click(sites);
    expect(continueButton()).toBeDisabled();

    fireEvent.click(screen.getByRole("checkbox", { name: /^Uptime/i }));
    expect(screen.queryByText(/no capability is selected/i)).toBeNull();
    expect(continueButton()).toBeEnabled();
    // And the walk really does open again, rather than merely un-refusing.
    expect(await forwardToMintButtonFromCapabilities()).toBeInTheDocument();
  });

  it("never sends `capabilities: []` on the wire, and sends exactly the selected set", async () => {
    // Asserted on the ACTUAL SERIALIZED FETCH BODY, not on hook-internal state
    // or React state -- a hook-level assertion could pass while a stray `?? []`
    // still reached the network.
    loadedFleet(3);
    let capturedBody: unknown = null;
    stubMintFetch((init) => {
      capturedBody = typeof init?.body === "string" ? (JSON.parse(init.body) as unknown) : null;
      return jsonResponse(MINTED, 201);
    });

    fireEvent.click(await reachMintButton());
    await screen.findByText(/this is the only time this token is shown/i);

    expect(capturedBody).not.toBeNull();
    const body = capturedBody as Record<string, unknown>;
    expect(Array.isArray(body.capabilities)).toBe(true);
    expect(body.capabilities).toEqual(["mcp.sites.read"]);
    expect(body.capabilities).not.toEqual([]);
  });

  it("sends every capability actually checked, not only the default", async () => {
    loadedFleet(3);
    let capturedBody: unknown = null;
    stubMintFetch((init) => {
      capturedBody = typeof init?.body === "string" ? (JSON.parse(init.body) as unknown) : null;
      return jsonResponse(MINTED, 201);
    });

    // The checkboxes live on the capability step, one before the mint button,
    // so they are ticked there and carried forward -- which is also the
    // property being relied on: an answer survives leaving the step that
    // collected it.
    renderWizard();
    await leaveContractStep();
    await advanceToCapabilityStep();
    fireEvent.click(screen.getByRole("checkbox", { name: /^Uptime/i }));
    fireEvent.click(screen.getByRole("checkbox", { name: /^Backups/i }));
    fireEvent.click(await forwardToMintButtonFromCapabilities());
    await screen.findByText(/this is the only time this token is shown/i);

    const body = capturedBody as Record<string, unknown>;
    expect(body.capabilities).toEqual(["mcp.sites.read", "mcp.uptime.read", "mcp.backups.read"]);
  });
});

/**
 * ONE REFUSAL PER BLOCKED STEP, AS A COUNT, FOR EVERY STEP THAT CAN REFUSE.
 *
 * WizardNav (`connect-wizard.tsx`) is the one place a step's `gate.refusal`
 * is rendered. A step section that also renders its own copy of the same
 * sentence -- reachable because the section reads the same underlying value
 * the gate does -- puts the identical text on screen twice. A `getAllByText
 * / findAllByText .length > 0` assertion is blind to that: a second match
 * satisfies "at least one" exactly as well as one match does, which is how
 * the capability step's private panel shipped and stayed unnoticed. Every
 * test below asserts the exact count instead, on every local step whose gate
 * can carry a refusal (1, 2, 3 and 4 -- the setup artefact step never
 * refuses, see `stepGate`'s final branch).
 */
describe("a blocked step's refusal renders exactly once, never twice", () => {
  it("step 1 -- no client chosen", async () => {
    renderWizard();
    await leaveContractStep();
    await screen.findByRole("button", { name: /cursor/i });
    expect(
      screen.getAllByText(/pick the client that will connect before going on/i),
    ).toHaveLength(1);
    expect(continueButton()).toBeDisabled();
  });

  it("step 5 -- no sign-in method chosen", async () => {
    loadedFleet(3);
    renderWizard();
    await leaveContractStep();
    await reachAuthStep("Cursor");
    expect(
      screen.getAllByText(/choose how this client signs in before going on/i),
    ).toHaveLength(1);
    expect(continueButton()).toBeDisabled();
  });

  it("step 3 -- token path, list mode, no site picked", async () => {
    loadedFleet(3);
    renderWizard();
    await leaveContractStep();
    await reachSiteScopeStep("Cursor");
    expect(screen.getAllByText(/no site is picked/i)).toHaveLength(1);
    expect(continueButton()).toBeDisabled();
  });

  it("step 4 -- token path, every capability deselected", async () => {
    loadedFleet(3);
    renderWizard();
    await leaveContractStep();
    await advanceToCapabilityStep();
    fireEvent.click(screen.getByRole("checkbox", { name: /^Sites/i }));
    expect(await screen.findAllByText(/no capability is selected/i)).toHaveLength(1);
    expect(continueButton()).toBeDisabled();
  });
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
    // And the scope too: the other half of what configKey watched. It lives on
    // an earlier step now, so changing it means walking back to it -- which is
    // the stronger version of this test, because the reveal has to survive a
    // step change as well as a keystroke.
    backToSiteStep();
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

    // Walking back to the method step and switching to OAuth unmounts the mint
    // panel completely: the setup step renders NextSteps instead, and the
    // capability step drops out of the walk altogether.
    backToMethodStep();
    fireEvent.click(authCard("oauth"));
    goNext();

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

    // THE CONTROLS THAT CAN NOW UNMOUNT THE PANEL ARE BACK AND CONTINUE. The
    // client and auth cards sit on earlier steps and are not on screen at all,
    // so the navigation controls are what the lock has to hold: walking back
    // mid-request would unmount the panel exactly as switching the method used
    // to, with the same outcome -- a live credential whose plaintext nobody saw.
    expect(backButton()).toBeDisabled();
    expect(screen.queryByRole("button", { name: /^continue$/i })).toBeNull();
    // Said out loud, not silently greyed out.
    expect(screen.getByRole("status")).toHaveTextContent(/strand a live credential nobody holds/i);

    // Clicking anyway changes nothing, which is what "refuses" has to mean.
    fireEvent.click(backButton());
    expect(await screen.findByRole("button", { name: /minting/i })).toBeInTheDocument();

    release();

    expect(await screen.findByText(MINTED.token)).toBeInTheDocument();
    // And the control comes back once nothing is outstanding -- the lock is for
    // the duration of the request, not for the rest of the session. A block
    // that outlived its request would strand the operator on a page they cannot
    // leave, which is worse than the defect it prevents.
    expect(backButton()).toBeEnabled();
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

  it("describes the scope the token was minted FOR, not the scope chosen after it", async () => {
    // THE WORST OUTCOME THIS SCREEN CAN PRODUCE, AND WHY THE TEST NOW REACHES
    // IT DIFFERENTLY. A reveal that read the site scope at RENDER time would
    // pair a real, shown-once credential with a description of access it does
    // not carry -- on the one screen whose whole job is saying what was just
    // made, at the only moment the token is ever visible.
    //
    // The original construction widened the scope WHILE the mint request was
    // open. That race is no longer reachable through the UI: the scope controls
    // are on an earlier step, and Back and Continue are both locked for the
    // duration of the request (see the in-flight lock test above). Unreachable
    // is not the same as untrue, so the property is pinned through the route
    // that IS reachable -- the operator walks back after the mint and changes
    // the scope -- and MintedReveal must still describe what it was minted for.
    // Nothing about the reveal is re-derived from live state; that is the whole
    // point of it carrying its own configuration.
    loadedFleet(3);
    stubMintFetch(() => jsonResponse(MINTED, 201));

    renderWizard();
    await leaveContractStep();
    await reachSiteScopeStep("Cursor");
    fireEvent.click(screen.getByRole("button", { name: /\+ add sites/i }));
    fireEvent.click(pickerBoxes()[0]!);
    expect(await screen.findByTestId("site-step-summary")).toHaveTextContent(
      /1 site, listed below/i,
    );

    fireEvent.click(await forwardToMintButton());
    expect(await screen.findByText(MINTED.token)).toBeInTheDocument();

    // The operator walks back and widens the scope from one site to three.
    backToSiteStep();
    fireEvent.click(pickerBoxes()[1]!);
    fireEvent.click(pickerBoxes()[2]!);
    expect(await screen.findByTestId("site-step-summary")).toHaveTextContent(
      /3 sites, listed below/i,
    );

    const scopeLine = screen.getByText(/Capabilities:/);
    expect(scopeLine).toHaveTextContent(/1 site, listed below/i);
    expect(scopeLine).not.toHaveTextContent(/3 sites/i);
    // And the live step shows the operator's newer, wider selection, so the
    // reveal is not merely lagging the whole screen -- the two disagree on
    // purpose, because they describe two different things.
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
    await leaveContractStep();
    await reachSiteScopeStep("Cursor");
    fireEvent.click(screen.getByRole("radio", { name: /by tag/i }));
    fireEvent.click(screen.getByRole("button", { name: /\+ add tags/i }));
    fireEvent.click(
      within(await screen.findByTestId("site-step-picker")).getAllByRole("checkbox")[0]!,
    );
    fireEvent.click(await forwardToMintButton());

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
    await leaveContractStep();
    await reachSiteScopeStep("Cursor");
    fireEvent.click(screen.getByRole("radio", { name: /by tag/i }));

    // THE REFUSAL MOVED EARLIER, IT DID NOT SOFTEN. One step is on screen at a
    // time, so this scope is refused at Continue rather than at a mint button
    // three steps later -- the same predicate, read by the control the operator
    // is actually standing in front of. The mint button is not merely disabled;
    // it cannot be reached at all.
    expect(await screen.findByText(/tag scope could not be resolved/i)).toBeInTheDocument();
    expect(continueButton()).toBeDisabled();
    expect(screen.queryByRole("button", { name: /generate connection token/i })).toBeNull();
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
    await leaveContractStep();
    await reachSiteScopeStep("Cursor");

    // Refused at Continue, on the step that owns the answer, rather than at a
    // mint button the operator would have walked three more steps to find.
    expect(continueButton()).toBeDisabled();
    expect(screen.getByText(/no site is picked/i)).toHaveTextContent(
      /pick at least one site in step 3, or switch that step to all sites/i,
    );
    // The remedy is the operator's own, not "the server said no".
    expect(screen.queryByText(/the server refused this request/i)).toBeNull();
    expect(screen.queryByRole("button", { name: /generate connection token/i })).toBeNull();
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
    await leaveContractStep();
    await reachSiteScopeStep("Cursor");
    fireEvent.click(screen.getByRole("radio", { name: /by tag/i }));

    expect(continueButton()).toBeDisabled();
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

    expect(await reachMintButton()).toBeEnabled();
    // Back to the step that owns the scope, the way an operator changes an
    // answer now, and forward again. "Sites were picked first" is exactly what
    // that walk leaves behind, which is the leak this test is about.
    backToSiteStep();
    fireEvent.click(screen.getByRole("radio", { name: /all sites/i }));
    fireEvent.click(await forwardToMintButton());

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
    backToSiteStep();
    fireEvent.click(screen.getByRole("radio", { name: /by tag/i }));
    fireEvent.click(screen.getByRole("button", { name: /\+ add tags/i }));
    fireEvent.click(
      within(await screen.findByTestId("site-step-picker")).getAllByRole("checkbox")[0]!,
    );
    fireEvent.click(await forwardToMintButton());

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

// ---------------------------------------------------------------------------
// THE STEPPER ITSELF. The owner looked at the shipped screen and said it was
// neither a stepper nor a wizard, and he was right about the rail: it printed
// the ten LONG frame titles run together with slashes, which is a paragraph in
// a stepper's place, and it disagreed with the numbers on the sections below
// it. These tests hold the component the deck actually draws.
// ---------------------------------------------------------------------------

/** Every rail segment, in the order the DOM has them. */
function railSegments(): HTMLElement[] {
  return Array.from(document.querySelectorAll<HTMLElement>("[data-step-n]"));
}

function railNumbers(): string[] {
  return railSegments().map((s) => s.dataset.stepN ?? "");
}

function railSegment(n: string): HTMLElement {
  const el = document.querySelector<HTMLElement>(`[data-step-n="${n}"]`);
  // A missing segment must fail here rather than letting every assertion
  // against it be silently skipped over a null.
  if (el === null) throw new Error(`no rail segment for specified step ${n}`);
  return el;
}

/** The specified step numbers currently in one rail state. */
function railStateNs(state: string): string[] {
  return railSegments()
    .filter((s) => s.dataset.stepState === state)
    .map((s) => s.dataset.stepN ?? "");
}

describe("the rail is a stepper, not a paragraph", () => {
  it("labels the rail with the deck's SHORT labels and never the long frame titles", async () => {
    renderWizard();
    await leaveContractStep();
    await screen.findByRole("button", { name: /claude code/i });

    // Ruling 15, verbatim in its ordering: "Start, Client, Sites,
    // Capabilities, Auth, Setup, Authorize, Confirm, Test, Done".
    // The label's own text node, without the sr-only "(not yet available)"
    // that an unbuilt step carries for a screen reader -- that suffix is
    // asserted by the "not-built" tests above and is not part of the label.
    expect(
      railSegments().map(
        (s) => screen.getByTestId(`step-label-${s.dataset.stepN}`).childNodes[0]?.textContent,
      ),
    ).toEqual([
      "Start",
      "Client",
      "Sites",
      "Capabilities",
      "Auth",
      "Setup",
      "Authorize",
      "Confirm",
      "Test",
      "Done",
    ]);

    // And the long frame titles are gone from the rail specifically. Asserting
    // their absence from the whole document would be wrong: "Choose what it
    // may do" is a legitimate SECTION heading, which is exactly where ruling
    // 15 puts the long form.
    const railText = railSegments()
      .map((s) => s.textContent ?? "")
      .join(" ");
    for (const long of [
      "Name it, pick the AI client",
      "Choose how it authenticates",
      "Get the setup artefact",
      "WPMgr confirms connection is live",
    ]) {
      expect(railText).not.toContain(long);
    }
  });

  it("draws a numbered circle per step and a connector between every pair", async () => {
    renderWizard();
    await leaveContractStep();
    await screen.findByRole("button", { name: /claude code/i });

    // Ten circles, each carrying its own specified number -- the number lives
    // in the circle now, not in a "6. Get the setup artefact" run of prose.
    expect(
      Array.from({ length: 10 }, (_, i) => screen.getByTestId(`step-circle-${i + 1}`).textContent),
    ).toEqual(["1", "2", "3", "4", "5", "6", "7", "8", "9", "10"]);

    // Nine connectors for ten steps: one before every segment except the
    // first. A connector before step 1 would draw a line coming from nowhere.
    expect(document.querySelectorAll('[data-testid^="step-line-"]')).toHaveLength(9);
    expect(screen.queryByTestId("step-line-1")).toBeNull();
  });

  it("shortens the connector below the deck's 640px breakpoint", async () => {
    renderWizard();
    await leaveContractStep();
    await screen.findByRole("button", { name: /claude code/i });

    // The deck: `.step .line { width: 44px }` with
    // `@media (max-width: 640px) { .step .line { width: 16px } }`. In this
    // project that is the unprefixed utility for the narrow case and the `sm:`
    // variant for the wide one -- w-4 is 16px, w-11 is 44px.
    const line = screen.getByTestId("step-line-2");
    expect(line.className).toContain("w-4");
    expect(line.className).toContain("sm:w-11");
    expect(line.className).toContain("mx-1.5");
    expect(line.className).toContain("sm:mx-2.5");
  });

  it("puts `sm:` at the 640px the deck specifies, so those classes mean what they say", async () => {
    // THE HALF THAT MAKES THE TEST ABOVE ABOUT BEHAVIOUR RATHER THAN SPELLING.
    // `sm:w-11` only implements the deck's breakpoint while `sm` IS 640px;
    // Tailwind's default is 40rem/640px and this app must not have moved it.
    // Moving it would silently change where the connector shortens, which no
    // class-name assertion could see.
    const fs = await import("node:fs");
    const nodePath = await import("node:path");
    // Resolved from the process, and a MISSING file throws rather than
    // letting this test pass over nothing -- a check that cannot find its
    // input has to go red, not green.
    const candidates = [
      nodePath.resolve(process.cwd(), "src/styles/globals.css"),
      nodePath.resolve(process.cwd(), "apps/web/src/styles/globals.css"),
    ];
    const found = candidates.find((p) => fs.existsSync(p));
    if (found === undefined) throw new Error(`globals.css not found at ${candidates.join(" or ")}`);
    const css = fs.readFileSync(found, "utf8");
    // The file has to have actually been read, or the regex below matches
    // nothing and the assertion is skipped over an empty string.
    expect(css).toContain("@theme");
    const override = /--breakpoint-sm:\s*([^;]+);/.exec(css)?.[1];
    // Either untouched (Tailwind's own 40rem = 640px), or explicitly set to
    // the same value. Anything else moves the design's boundary.
    if (override !== undefined) expect(override.trim()).toBe("40rem");
  });

  it("fills the circle only for a step actually behind the operator, and rings the current one", async () => {
    // WALKED AS FAR AS A SETTLED STEP, on purpose. The ring is the "you are
    // here and this step is in hand" signal, and a step whose gate is still
    // refusing does not get it -- that rule is asserted next door, in "a
    // blocked step never renders as the current one". So this test stands the
    // operator on the OAuth site-scope step, where the gate is settled from
    // the moment it is reached (ruling 4), and the ring is the honest answer.
    loadedFleet(3);
    renderWizard();
    await leaveContractStep();
    await reachSiteScopeStep("Cursor");
    // ANSWERED, because a step whose gate still refuses does not get the ring
    // -- that rule is asserted next door, in "a blocked step never renders as
    // the current one". The ring is only the honest answer once step 3 is
    // settled, which it now is for every walk rather than only the OAuth one.
    chooseAllSites();

    // Steps 1 and 2 are behind them: filled. The fill is the strongest "done"
    // signal on the screen and it is drawn from the rail's own state, so it
    // cannot claim progress the wizard has not made.
    for (const n of ["1", "2"]) {
      expect(document.querySelector(`[data-step-n="${n}"]`)).toHaveAttribute(
        "data-step-state",
        "completed",
      );
      expect(screen.getByTestId(`step-circle-${n}`).className).toContain(
        "bg-[var(--color-primary)]",
      );
    }

    // Step 3 is where they are: ringed, never filled.
    expect(screen.getByTestId("step-circle-3").className).toContain("border-2");
    expect(screen.getByTestId("step-circle-3").className).not.toContain(
      "bg-[var(--color-primary)]",
    );

    // Step 8 does not exist yet. No fill, no ring: an unbuilt step must never
    // render as done or current.
    expect(document.querySelector('[data-step-n="8"]')).toHaveAttribute(
      "data-step-state",
      "not-built",
    );
    expect(screen.getByTestId("step-circle-8").className).not.toContain(
      "bg-[var(--color-primary)]",
    );
    expect(screen.getByTestId("step-circle-8").className).not.toContain("border-2");
  });
});

describe("the rail and the section heading agree on which step this is", () => {
  // Two numbering systems on one screen contradict each other as soon as the
  // page order stops matching the spine: the rail calling the setup section
  // step 6 while the heading over it says "4. Set it up". Every heading here
  // is now numbered with the specified step it answers, so a section and its
  // rail segment cannot name the same step differently.

  /** Every "N. Title" heading on screen, as its leading number. */
  function headingNumbers(): string[] {
    return screen
      .getAllByRole("heading", { level: 2 })
      .map((h) => /^(\d+)\./.exec(h.textContent ?? "")?.[1])
      .filter((n): n is string => n !== undefined);
  }

  it("numbers the client section 2, the way the rail does, before anything is picked", async () => {
    renderWizard();
    await leaveContractStep();
    await screen.findByRole("button", { name: /claude code/i });

    expect(screen.getByRole("heading", { name: /^2\. Pick your client$/ })).toBeInTheDocument();
    expect(currentRailStep()).toHaveAttribute("data-step-n", "2");
    // Nothing on screen is numbered 1: local position numbering is gone.
    expect(headingNumbers()).toEqual(["2"]);
  });

  it("numbers every revealed section with a step the rail also names, on the token path", async () => {
    // COLLECTED ACROSS THE WALK, one step at a time, because that is how the
    // operator meets them now. The section number is the specified number and
    // the walk is in that order, so the two cannot disagree.
    loadedFleet(3);
    renderWizard();
    await screen.findByTestId("connection-contract");

    const seen: string[] = [];
    const step = () => {
      // Exactly one section is on screen at a time, which is the property the
      // whole navigation model exists for.
      expect(headingNumbers()).toHaveLength(1);
      seen.push(headingNumbers()[0]!);
      // And the heading's number is the segment the rail marks current, so
      // there is exactly one numbering system on the screen.
      expect(currentRailStep()).toHaveAttribute("data-step-n", headingNumbers()[0]!);
    };

    step();
    goNext();

    step();
    await pickClientOnly("Claude Code");
    goNext();

    await screen.findByTestId("site-step-count");
    step();
    chooseAllSites();
    goNext();

    await screen.findByRole("heading", { name: /^4\. Choose what it may do$/ });
    step();
    goNext();

    await screen.findByRole("heading", { name: /^5\. Choose how it authenticates$/ });
    step();
    fireEvent.click(authCard("token"));
    goNext();

    await screen.findByRole("button", { name: /generate connection token/i });
    step();

    expect(seen).toEqual(["1", "2", "3", "4", "5", "6"]);
  });

  it("gives the setup section the same number the rail marks current", async () => {
    renderWizard();
    await leaveContractStep();
    await reachSetupStep("Claude Code", "oauth");

    const current = currentRailStep();
    expect(current).toHaveAttribute("data-step-n", "6");
    expect(screen.getByTestId("step-label-6")).toHaveTextContent("Setup");
    // The heading over the section the operator is looking at carries that
    // same 6, not the "4" its position on the page would give it.
    expect(screen.getByRole("heading", { name: /^6\. Get the setup artefact$/ })).toBeInTheDocument();
  });
});

// ---------------------------------------------------------------------------
// The stepper rail: one row, one current step, one canonical heading.
// ---------------------------------------------------------------------------

describe("the rail is one row, so no connector can start a row", () => {
  it("never wraps, at any width", async () => {
    renderWizard();
    await leaveContractStep();
    await screen.findByRole("button", { name: /claude code/i });

    // The orphan-connector defect is only possible if the rail can wrap: a
    // connector travels with the step that follows it, so the first step on
    // any row after the first would open with a line coming from nowhere. CSS
    // cannot tell a flex item that it starts a visual row, so the rail does
    // not wrap at all. This is the class-level half; the layout half is
    // e2e/ai-connect-stepper.spec.ts, which measures the rendered rows at
    // 390px, because no jsdom assertion can see a wrap.
    const rail = screen.getByTestId("step-rail");
    expect(rail.className).toContain("flex-nowrap");
    expect(rail.className).not.toContain("flex-wrap");
    // And it stays reachable when it does not fit, rather than clipping.
    expect(rail.className).toContain("overflow-x-auto");
  });
});

describe("a blocked step never renders as the current one", () => {
  // A RAIL MUST NOT PRESENT A STEP AS IN HAND WHILE ITS ACTION IS REFUSED.
  // While site scope is unresolved the rail correctly points at step 3, so the
  // POSITIONAL "is this the current step" answer is true -- and the ring used
  // to render off that boolean, telling the operator step 3 was in hand while
  // the mint button was refusing it. Every visual now reads the state value
  // and nothing else.

  async function reachBlockedSiteScopeOnTokenPath() {
    renderWizard();
    await leaveContractStep();
    // Standing ON the scope step with nothing answered -- reachSetupStep would
    // answer it in order to get past, which is the state this test is not about.
    await reachSiteScopeStep("Claude Code");
    const siteStep = document.querySelector('[data-step-n="3"]');
    if (!(siteStep instanceof HTMLElement)) throw new Error("no step 3 segment");
    return siteStep;
  }

  it("gives the ring to no step at all while site scope is unselected", async () => {
    const siteStep = await reachBlockedSiteScopeOnTokenPath();
    // The rail points here: this IS where the operator is standing.
    expect(siteStep).toHaveAttribute("data-step-state", "unselected");
    expect(siteStep).toHaveAttribute("aria-current", "step");

    // And it is drawn as blocked, not as current: no ring, no fill.
    const circle = screen.getByTestId("step-circle-3");
    expect(circle.className).not.toContain("border-2");
    expect(circle.className).not.toContain("bg-[var(--color-primary)]");

    // No OTHER segment picked up the ring either -- the styling did not move
    // to a step further along and call that step current instead.
    expect(document.querySelectorAll('[data-step-state="current"]')).toHaveLength(0);
    for (const n of [1, 2, 3, 4, 5, 6, 7, 8, 9, 10]) {
      expect(screen.getByTestId(`step-circle-${n}`).className).not.toContain("border-2");
    }
  });

  it("gives the ring to no step while the fleet read is still loading", async () => {
    mockedSites.mockReturnValue(mockQueryResult<Site[]>({ data: undefined, isPending: true }));
    renderWizard();
    await leaveContractStep();
    await reachSiteScopeStep("Claude Code");

    const siteStep = document.querySelector('[data-step-n="3"]');
    expect(siteStep).toHaveAttribute("data-step-state", "loading");
    expect(siteStep).toHaveAttribute("aria-current", "step");
    expect(screen.getByTestId("step-circle-3").className).not.toContain("border-2");
    expect(document.querySelectorAll('[data-step-state="current"]')).toHaveLength(0);
  });
});

describe("the heading a section renders is the canonical one", () => {
  // `heading` on the spec entry was canonical and `title` on the
  // Section was what actually rendered, so a step's name was written twice
  // with nothing making the two agree. The expected strings below are the
  // deck's own frame titles, written out here rather than imported, so this
  // test reddens on drift from the DECK and not merely on drift within the
  // file.
  it("renders the deck's frame title over every section it reveals", async () => {
    // COLLECTED ONE STEP AT A TIME, because that is how the operator meets
    // them. Exactly one heading is on screen at any moment, and the sequence
    // below is what the walk produces in order.
    loadedFleet(3);
    renderWizard();
    await screen.findByTestId("connection-contract");

    const headings: (string | null)[] = [];
    const capture = () => {
      const level2 = screen.getAllByRole("heading", { level: 2 });
      expect(level2).toHaveLength(1);
      headings.push(level2[0]!.textContent);
    };

    capture();
    goNext();

    capture();
    await pickClientOnly("Claude Code");
    goNext();

    await screen.findByTestId("site-step-count");
    capture();
    chooseAllSites();
    goNext();

    await screen.findByRole("heading", { name: /^4\. Choose what it may do$/ });
    capture();
    goNext();

    await screen.findByRole("heading", { name: /^5\. Choose how it authenticates$/ });
    capture();
    fireEvent.click(authCard("token"));
    goNext();

    await screen.findByRole("button", { name: /generate connection token/i });
    capture();

    expect(headings).toEqual([
      "1. Start a connection",
      // The single declared narrowing, and the only one: this section picks
      // the client and does not yet ask for the name step 2's frame title
      // promises, so it says what it does.
      "2. Pick your client",
      "3. Choose which sites",
      "4. Choose what it may do",
      "5. Choose how it authenticates",
      "6. Get the setup artefact",
    ]);
  });
});

describe("exactly one segment answers for the operator's position", () => {
  /**
   * The count of segments claiming to be current. A COUNT and not a presence
   * check: a rail with two current steps still has "a" current step, so
   * `getByRole`-style presence passes over the very defect this asserts
   * against.
   */
  function currentCount(): number {
    return document.querySelectorAll('[data-step-n][aria-current="step"]').length;
  }

  it("keeps one current segment when site scope AND capabilities are both blocked", async () => {
    // BOTH BLOCKING AT ONCE, reached the way an operator reaches it now: the
    // capability step is answered badly first, then the scope behind it is
    // taken away. Each blocked step used to promote its own segment, giving the
    // rail two positions and handing the scroll ref to the later one.
    loadedFleet(3);
    renderWizard();
    await leaveContractStep();
    await advanceToCapabilityStep();

    fireEvent.click(screen.getByRole("checkbox", { name: /^Sites/i }));
    expect(screen.getByRole("checkbox", { name: /^Sites/i })).not.toBeChecked();
    expect(continueButton()).toBeDisabled();

    // Now walk back and remove the scope answer too, so step 3 refuses as well.
    backToSiteStep();
    fireEvent.click(pickerBoxes()[0]!);

    // ONLY THE STEP THE OPERATOR IS ON REPORTS A REASON, and that is the new
    // truth rather than a softened assertion. A reason is a message to someone
    // standing in front of the step; step 4 is now behind the wall at step 3
    // and the operator cannot act on it, so advertising "not chosen yet" there
    // would be telling them about a refusal they cannot reach. Both gates ARE
    // still refusing, which the walk proves below.
    expect(document.querySelector('[data-step-n="3"]')).toHaveAttribute(
      "data-step-state",
      "unselected",
    );
    expect(document.querySelector('[data-step-n="4"]')).toHaveAttribute(
      "data-step-state",
      "upcoming",
    );
    expect(continueButton()).toBeDisabled();

    // ONE position, and it is the earlier blocked step.
    expect(currentCount()).toBe(1);
    const current = document.querySelector('[data-step-n][aria-current="step"]');
    expect(current).toHaveAttribute("data-step-n", "3");

    // AND THE SECOND GATE IS GENUINELY STILL REFUSING, so the "both blocked"
    // premise is not vacuous: answering the scope moves the operator to step 4
    // and it refuses there in its own right, still with exactly one position.
    chooseAllSites();
    goNext();
    expect(currentCount()).toBe(1);
    const afterScope = document.querySelector('[data-step-n][aria-current="step"]');
    expect(afterScope).toHaveAttribute("data-step-n", "4");
    expect(afterScope).toHaveAttribute("data-step-state", "unselected");
    expect(continueButton()).toBeDisabled();
  });

  it("keeps one current segment in every state this wizard can reach", async () => {
    // The invariant, not one scenario of it: at no point in the walk from an
    // empty screen to a fully answered token path does the rail hold two
    // positions.
    loadedFleet(3);
    renderWizard();
    await leaveContractStep();
    await screen.findByRole("button", { name: /claude code/i });
    expect(currentCount()).toBe(1);

    await pickClientOnly("Claude Code");
    expect(currentCount()).toBe(1);
    goNext();

    await screen.findByTestId("site-step-count");
    expect(currentCount()).toBe(1);
    chooseAllSites();
    expect(currentCount()).toBe(1);
    goNext();

    await screen.findByRole("heading", { name: /^4\. Choose what it may do$/ });
    expect(currentCount()).toBe(1);

    // Blocked, and still exactly one position.
    fireEvent.click(screen.getByRole("checkbox", { name: /^Sites/i }));
    expect(currentCount()).toBe(1);
    fireEvent.click(screen.getByRole("checkbox", { name: /^Sites/i }));
    expect(currentCount()).toBe(1);
    goNext();

    await screen.findByRole("heading", { name: /^5\. Choose how it authenticates$/ });
    expect(currentCount()).toBe(1);
    fireEvent.click(authCard("token"));
    expect(currentCount()).toBe(1);
    goNext();

    await screen.findByRole("button", { name: /generate connection token/i });
    expect(currentCount()).toBe(1);
  });

  it("numbers every specified step exactly once, which is what makes one current possible", async () => {
    // The premise the invariant above rests on. `aria-current` is placed by
    // comparing a segment's specified number against one number; that yields
    // at most one match only while the specified numbers are unique. If a
    // duplicate `n` were ever added to SPEC_STEPS, two segments could match
    // again -- so the uniqueness is asserted rather than assumed.
    renderWizard();
    await leaveContractStep();
    await screen.findByRole("button", { name: /claude code/i });

    const ns = Array.from(document.querySelectorAll<HTMLElement>("[data-step-n]")).map(
      (s) => s.dataset.stepN,
    );
    expect(new Set(ns).size).toBe(ns.length);
  });
});

// ---------------------------------------------------------------------------
// The cursor clamp, tested directly.
//
// Its sharpest case -- a position requested PAST the first blocked step -- is
// not reachable through the rendered wizard: it needs an answer to go bad
// behind the operator while they stand further on, which in production is the
// fleet query refetching into a failure on a window focus or a reconnect, and
// which no mounted test can produce without navigating (and navigating is what
// resets the request). Removing the clamp leaves every rendered test in this
// file green. These are the tests that actually hold it.
// ---------------------------------------------------------------------------

describe("the cursor is clamped to the first step the answers do not support", () => {
  const settled = { readiness: "resolved", refusal: null } as const;
  const refused = { readiness: "unselected", refusal: "pick something" } as const;
  /** The token walk: client, method, sites, capabilities, setup. */
  const walk = [
    [1, 2],
    [2, 5],
    [3, 3],
    [4, 4],
    [5, 6],
  ] as [1 | 2 | 3 | 4 | 5, number][];

  it("gives the operator the position they asked for when everything before it is settled", () => {
    const gates = [settled, settled, settled, settled, settled];
    expect(resolveCursorPos(walk, gates, 5)).toBe(4);
    expect(resolveCursorPos(walk, gates, 3)).toBe(2);
    expect(resolveCursorPos(walk, gates, 1)).toBe(0);
  });

  it("holds them AT a blocked step they are standing on", () => {
    const gates = [settled, settled, refused, settled, settled];
    expect(resolveCursorPos(walk, gates, 3)).toBe(2);
  });

  it("pulls them BACK when a step behind them stops being answered", () => {
    // THE CASE THE RENDERED TESTS CANNOT REACH. The operator legitimately
    // walked to the setup step, and then the read behind site scope failed.
    // The mint they were about to press would now be refused, so leaving them
    // standing on it -- with every step before it marked complete -- would be
    // the rail asserting a readiness the button does not have.
    const gates = [settled, settled, refused, settled, settled];
    expect(resolveCursorPos(walk, gates, 5)).toBe(2);
    expect(resolveCursorPos(walk, gates, 4)).toBe(2);
  });

  it("reports the EARLIEST blocked step when more than one refuses", () => {
    // An operator who has answered neither is working on the earlier one.
    // Naming the later would tell them they are stuck on a step they have not
    // been let reach in any meaningful sense.
    const gates = [settled, settled, refused, refused, settled];
    expect(resolveCursorPos(walk, gates, 5)).toBe(2);
  });

  it("falls back to the wall, not to the start, when the path no longer asks the requested step", () => {
    // The OAuth walk does not include the capability picker. An operator who
    // was on it and went back to choose browser sign-in keeps every answer
    // they gave rather than being sent to the beginning.
    const oauthWalk = [
      [1, 2],
      [2, 5],
      [3, 3],
      [5, 6],
    ] as [1 | 2 | 3 | 4 | 5, number][];
    expect(resolveCursorPos(oauthWalk, [settled, settled, settled, settled], 4)).toBe(3);
    expect(resolveCursorPos(oauthWalk, [settled, settled, refused, settled], 4)).toBe(2);
  });

  it("never returns a position outside the walk", () => {
    const gates = [settled, settled, settled, settled, settled];
    for (const requested of [1, 2, 3, 4, 5] as const) {
      const pos = resolveCursorPos(walk, gates, requested);
      expect(pos).toBeGreaterThanOrEqual(0);
      expect(pos).toBeLessThan(walk.length);
    }
  });
});
