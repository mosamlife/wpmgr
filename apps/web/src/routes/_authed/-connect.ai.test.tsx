import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, fireEvent, waitFor, within } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";
import { mockQueryResult } from "@/test/query-mocks";

import { Route } from "./connect.ai";
import {
  useConsentContext,
  navigateTo,
  OAuthRequestError,
} from "@/features/mcp-consent/use-consent";
import { parseConsentContext, SCOPE_CACHE, SCOPE_READ } from "@/features/mcp-consent/consent-context";
import { capabilityLabel } from "@/features/ai-connections/capabilities";
import { useSites } from "@/features/sites/use-sites";
import { useTags } from "@/features/tags/use-tags";
import type { ConsentContext } from "@/features/mcp-consent/consent-context";
import type { Site, SiteTag } from "@wpmgr/api";

// A FAILED LOAD MUST NOT RENDER AN APPROVABLE SCREEN.
//
// The house defect class is a failure or absence quietly coerced into a
// plausible value. On this screen the plausible value is a consent form with
// nothing in it, and the coercion costs the user fleet-wide read access to a
// stranger, because a form with an enabled button is a form people press.

vi.mock("@/features/mcp-consent/use-consent", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("@/features/mcp-consent/use-consent")>();
  return { ...actual, useConsentContext: vi.fn(), navigateTo: vi.fn() };
});
vi.mock("@/features/sites/use-sites", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/sites/use-sites")>();
  return { ...actual, useSites: vi.fn() };
});
vi.mock("@/features/tags/use-tags", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/tags/use-tags")>();
  return { ...actual, useTags: vi.fn() };
});

const mockedConsent = vi.mocked(useConsentContext);
const mockedNavigate = vi.mocked(navigateTo);
const mockedSites = vi.mocked(useSites);
const mockedTags = vi.mocked(useTags);

const ConnectAiPage = Route.options.component!;

// The route reads its OAuth parameters from the URL, so every case mounts the
// real router at a real path rather than passing props.
const LAUNCH =
  "/connect/ai?response_type=code&client_id=c1&redirect_uri=https%3A%2F%2Fx.example%2Fcb&scope=mcp%3Aread";

beforeEach(() => {
  vi.clearAllMocks();
  mockedSites.mockReturnValue(mockQueryResult<Site[]>({ data: [] }));
  mockedTags.mockReturnValue(mockQueryResult<SiteTag[]>({ data: [] }));
});

describe("/connect/ai — a failed load is not approvable", () => {
  it("renders an error and NO approve control when the authorize call fails", async () => {
    mockedConsent.mockReturnValue(
      mockQueryResult<ConsentContext>({
        isError: true,
        isSuccess: false,
        error: new OAuthRequestError("invalid_client", "no such client", 401),
      }),
    );

    renderWithProviders(<ConnectAiPage />, { withRouter: true, initialPath: LAUNCH });

    expect(await screen.findByText(/nothing here to approve/i)).toBeTruthy();
    expect(screen.queryByTestId("consent-approve")).toBeNull();
    expect(screen.queryByTestId("consent-deny")).toBeNull();
  });

  it("renders no approve control when the call 'succeeds' with no data", async () => {
    // The shape that gets missed: not an error, just nothing. A screen built
    // from it would have empty identity, empty permissions and a live button.
    mockedConsent.mockReturnValue(
      mockQueryResult<ConsentContext>({ isError: false, isPending: false }),
    );

    renderWithProviders(<ConnectAiPage />, { withRouter: true, initialPath: LAUNCH });

    expect(await screen.findByText(/nothing here to approve/i)).toBeTruthy();
    expect(screen.queryByTestId("consent-approve")).toBeNull();
  });

  it("renders a skeleton with no approve control while loading", async () => {
    mockedConsent.mockReturnValue(
      mockQueryResult<ConsentContext>({ isPending: true }),
    );

    const { container } = renderWithProviders(<ConnectAiPage />, {
      withRouter: true,
      initialPath: LAUNCH,
    });

    // ASSERTED, NOT SWALLOWED. The previous version wrapped this in a .catch,
    // which made the whole test unfailable -- it passed against a build with no
    // skeleton at all. findByRole rejects on absence, and that rejection is now
    // the test's verdict rather than something it recovers from.
    const busy = await screen.findByRole("status", { busy: true });
    expect(busy).toBeTruthy();
    expect(container.querySelector('[data-testid="consent-approve"]')).toBeNull();
  });

  it("refuses an incomplete launch instead of filling in the missing parameters", async () => {
    mockedConsent.mockReturnValue(
      mockQueryResult<ConsentContext>({ isPending: false }),
    );

    // No `scope`. A screen that defaulted it to mcp:read would be consenting on
    // the client's behalf to a permission the client never asked for.
    renderWithProviders(<ConnectAiPage />, {
      withRouter: true,
      initialPath: "/connect/ai?response_type=code&client_id=c1&redirect_uri=https%3A%2F%2Fx.example%2Fcb",
    });

    expect(await screen.findByText(/connection request is incomplete/i)).toBeTruthy();
    expect(screen.queryByTestId("consent-approve")).toBeNull();
    // And it never asked the server, because there was no question to ask.
    expect(mockedConsent).toHaveBeenCalledWith(null);
  });
});


// ---------------------------------------------------------------------------
// Refusal reaches the client.
// ---------------------------------------------------------------------------

const CONSENT = parseConsentContext({
  client_id: "c1",
  client_name_unverified: "Some Client",
  identity_verified: false,
  redirect_uri: "https://client.example/cb",
  redirect_host: "client.example",
  scopes: [SCOPE_READ],
  grant_lifetime_days: 90,
  state: "opaque-csrf-token",
});

describe("/connect/ai — 'Do not connect' answers the client", () => {
  it("redirects to the client with access_denied and the original state", async () => {
    mockedConsent.mockReturnValue(mockQueryResult<ConsentContext>({ data: CONSENT }));

    renderWithProviders(<ConnectAiPage />, { withRouter: true, initialPath: LAUNCH });

    fireEvent.click(await screen.findByTestId("consent-deny"));

    expect(mockedNavigate).toHaveBeenCalledTimes(1);
    const target = new URL(mockedNavigate.mock.calls[0]![0]);
    expect(target.origin + target.pathname).toBe("https://client.example/cb");
    expect(target.searchParams.get("error")).toBe("access_denied");
    expect(target.searchParams.get("state")).toBe("opaque-csrf-token");
    expect(target.searchParams.has("code")).toBe(false);
  });

  it("does NOT use browser history, which is empty in a freshly opened tab", async () => {
    // An OAuth client opens this URL in a new tab or window. There is no
    // history entry to go back to, so history.back() there does nothing at all
    // and the user's refusal simply vanishes.
    const back = vi.spyOn(window.history, "back");
    mockedConsent.mockReturnValue(mockQueryResult<ConsentContext>({ data: CONSENT }));

    renderWithProviders(<ConnectAiPage />, { withRouter: true, initialPath: LAUNCH });

    fireEvent.click(await screen.findByTestId("consent-deny"));

    expect(back).not.toHaveBeenCalled();
    expect(mockedNavigate).toHaveBeenCalledTimes(1);
    back.mockRestore();
  });
});


describe("/connect/ai — a failed tags load is not an empty tag registry", () => {
  it("does not tell the operator the organisation has no tags", async () => {
    // The same collapse the site list carries a FleetSnapshot to avoid: "we
    // could not ask" rendered as "there are none", stated as a fact about the
    // organisation on the screen where facts drive an irreversible decision.
    mockedConsent.mockReturnValue(mockQueryResult<ConsentContext>({ data: CONSENT }));
    mockedTags.mockReturnValue(
      mockQueryResult<SiteTag[]>({ isError: true, isSuccess: false, error: new Error("boom") }),
    );

    renderWithProviders(<ConnectAiPage />, { withRouter: true, initialPath: LAUNCH });

    fireEvent.click(await screen.findByText("Sites with a tag"));

    expect(screen.queryByTestId("consent-tags-empty")).toBeNull();
    expect(screen.getByTestId("consent-tags-failed").textContent).toMatch(
      /could not load this organisation's tags/i,
    );
    expect(screen.getByTestId("consent-approve").hasAttribute("disabled")).toBe(true);
  });

  it("still says 'no tags yet' when the registry really is empty", async () => {
    // The over-fire case. An org with no tags is a real state and must keep its
    // own true sentence, or the fix has only moved the lie.
    mockedConsent.mockReturnValue(mockQueryResult<ConsentContext>({ data: CONSENT }));
    mockedTags.mockReturnValue(mockQueryResult<SiteTag[]>({ data: [] }));

    renderWithProviders(<ConnectAiPage />, { withRouter: true, initialPath: LAUNCH });

    fireEvent.click(await screen.findByText("Sites with a tag"));

    expect(screen.queryByTestId("consent-tags-failed")).toBeNull();
    expect(screen.getByTestId("consent-tags-empty").textContent).toMatch(/no tags yet/i);
  });
});


// ---------------------------------------------------------------------------
// The consent ticket survives the browser round trip.
// ---------------------------------------------------------------------------
//
// The consent flow goes out through the browser and comes back, so every
// authorize-time fact used to arrive on the approval POST as caller input --
// including the scope set the operator was actually shown -- and the server
// stored what the body said. The ticket is the server's own signed record of
// those facts, and the dashboard's entire job is to hand it back untouched.
//
// These run through the REAL useApproveConsent (the module mock spreads
// `...actual` and replaces only useConsentContext and navigateTo), mounted on
// the real router with a real QueryClient, so what is asserted is the bytes
// that would leave the browser and not a hook's arguments.

// Deliberately not a tidy token. The leading and trailing whitespace is the
// point: a `.trim()` anywhere between the authorize response and the POST body
// still yields a present, plausible, non-empty string, so a test asserting
// presence would sail straight over it. The server verifies a signature over
// these exact bytes, so the assertion is on these exact bytes.
const TICKET = " v1.eyJzY29wZXMiOlsibWNwOnJlYWQiXX0.c1gN4tUr3-_~+/=AbC ";

const TICKETED_WIRE = {
  client_id: "c1",
  client_name_unverified: "Some Client",
  identity_verified: false,
  redirect_uri: "https://client.example/cb",
  redirect_host: "client.example",
  scopes: [SCOPE_READ],
  grant_lifetime_days: 90,
  state: "opaque-csrf-token",
};

function approveResponse(): Response {
  return new Response(
    JSON.stringify({
      grant_id: "g1",
      code: "auth-code",
      redirect_uri: "https://client.example/cb",
      state: "opaque-csrf-token",
    }),
    { status: 200, headers: { "Content-Type": "application/json" } },
  );
}

/**
 * Drive the screen to a real approval POST and return the body it sent.
 * `interact` runs after the screen is up and before Approve is pressed, so a
 * case can tick and clear boxes exactly as an operator would.
 */
async function postedApprovalBody(
  consent: ConsentContext,
  fetchMock: ReturnType<typeof vi.fn>,
  opts: { tickCache?: boolean; interact?: () => void } = {},
): Promise<Record<string, unknown>> {
  mockedConsent.mockReturnValue(mockQueryResult<ConsentContext>({ data: consent }));
  vi.stubGlobal("fetch", fetchMock);

  renderWithProviders(<ConnectAiPage />, { withRouter: true, initialPath: LAUNCH });

  fireEvent.change(await screen.findByLabelText(/name this connection/i), {
    target: { value: "Desktop" },
  });
  if (opts.tickCache) {
    fireEvent.click(within(screen.getByTestId("consent-cache-capability")).getByRole("checkbox"));
  }
  opts.interact?.();
  fireEvent.click(screen.getByTestId("consent-approve"));

  await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
  const init = fetchMock.mock.calls[0]![1] as RequestInit;
  return JSON.parse(init.body as string) as Record<string, unknown>;
}

describe("/connect/ai — the consent ticket is carried back unread", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("posts the server's ticket back byte for byte", async () => {
    const fetchMock = vi.fn().mockResolvedValue(approveResponse());

    const body = await postedApprovalBody(
      parseConsentContext({ ...TICKETED_WIRE, consent_ticket: TICKET }),
      fetchMock,
    );

    // THE EXACT STRING. Not toBeDefined, not toContain, not a length check.
    // A ticket that arrives emptied, trimmed or re-encoded is still "present",
    // and the failure it causes downstream is a signature rejection that reads
    // like an unrelated auth bug to whoever debugs it next.
    expect(body.consent_ticket).toBe(TICKET);
  });

  it("sends NO ticket key at all against a server that issues none", async () => {
    // THE BACKWARD-COMPATIBILITY CASE, and the whole reason this may deploy
    // ahead of the API. The server in production today issues no ticket and
    // reads none, so the key must be ABSENT -- not null, not "" -- and the
    // approval must still complete. This is the one most likely to be broken
    // later by someone "tidying up" the conditional into a `?? ""`.
    const fetchMock = vi.fn().mockResolvedValue(approveResponse());

    const body = await postedApprovalBody(parseConsentContext(TICKETED_WIRE), fetchMock);

    expect("consent_ticket" in body).toBe(false);
    // The rest of the body is unchanged, so an old server sees exactly the
    // request it has always seen.
    expect(body.client_id).toBe("c1");
    expect(body.name).toBe("Desktop");
    // And the approval really did succeed rather than erroring out for want of
    // a field this server has never sent.
    await waitFor(() => expect(mockedNavigate).toHaveBeenCalledTimes(1));
  });
});

describe("/connect/ai — the approval carries the consented capabilities", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  const CACHE_WIRE = {
    ...TICKETED_WIRE,
    scopes: [SCOPE_READ, SCOPE_CACHE],
    conferrable_capabilities: [
      { name: "mcp.sites.read", effect: "read" },
      { name: "mcp.uptime.read", effect: "read" },
      { name: "mcp.cache.purge", effect: "request" },
    ],
  };

  it("sends only Sites, the wizard's default, and NOT mcp.cache.purge when nothing is changed", async () => {
    // The server offers Sites and Uptime here. Only Sites is ticked on open, so
    // only Sites is sent: Uptime is on offer, not on the screen as ticked.
    const fetchMock = vi.fn().mockResolvedValue(approveResponse());
    const body = await postedApprovalBody(parseConsentContext(CACHE_WIRE), fetchMock);

    const caps = body.capabilities as string[];
    expect([...caps].sort()).toEqual(["mcp.sites.read"]);
    expect(caps).not.toContain("mcp.cache.purge");
  });

  it("adds mcp.cache.purge only when the box is ticked", async () => {
    const fetchMock = vi.fn().mockResolvedValue(approveResponse());
    const body = await postedApprovalBody(parseConsentContext(CACHE_WIRE), fetchMock, {
      tickCache: true,
    });

    expect([...(body.capabilities as string[])].sort()).toEqual([
      "mcp.cache.purge",
      "mcp.sites.read",
    ]);
  });

  it("never sends an empty list", async () => {
    const fetchMock = vi.fn().mockResolvedValue(approveResponse());
    const body = await postedApprovalBody(
      parseConsentContext({ ...TICKETED_WIRE, conferrable_capabilities: [] }),
      fetchMock,
    );

    expect("capabilities" in body).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// The read picker: what is ticked is exactly what is posted (GH #660).
// ---------------------------------------------------------------------------
//
// Through the real useApproveConsent, with fetch stubbed, so what is asserted
// is the JSON body that would leave the browser. Before the picker the screen
// posted every read the server offered whatever the operator had on screen.

// What the server lists for a request holding mcp:read and mcp:cache:
// scopeCapabilities[ScopeRead] and [ScopeCache] in
// apps/api/internal/mcp/policy.go, written out rather than derived from the
// dashboard's vocabulary.
const SERVER_READS = [
  "mcp.activity.read",
  "mcp.backups.read",
  "mcp.diagnostics.read",
  "mcp.performance.read",
  "mcp.security.read",
  "mcp.sites.read",
  "mcp.uptime.read",
];

const PICKER_WIRE = {
  ...TICKETED_WIRE,
  scopes: [SCOPE_READ, SCOPE_CACHE],
  conferrable_capabilities: [
    ...SERVER_READS.map((name) => ({ name, effect: "read" })),
    { name: "mcp.cache.purge", effect: "request" },
  ],
};

const picker = () => screen.getByRole("group", { name: "It will be able to read" });

const row = (cap: string) =>
  within(picker()).getByRole<HTMLInputElement>("checkbox", {
    name: new RegExp(`^${capabilityLabel(cap)}\\b`),
  });

describe("/connect/ai, the approval posts exactly what is ticked", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("posts Backups and Security, and not Sites, when those are the ticks", async () => {
    const fetchMock = vi.fn().mockResolvedValue(approveResponse());
    const body = await postedApprovalBody(parseConsentContext(PICKER_WIRE), fetchMock, {
      interact: () => {
        fireEvent.click(row("mcp.sites.read"));
        fireEvent.click(row("mcp.backups.read"));
        fireEvent.click(row("mcp.security.read"));
      },
    });

    expect([...(body.capabilities as string[])].sort()).toEqual([
      "mcp.backups.read",
      "mcp.security.read",
    ]);
  });

  it("posts the reads that are ticked on screen, whatever mix the operator left", async () => {
    const fetchMock = vi.fn().mockResolvedValue(approveResponse());
    let shown: string[] = [];
    const body = await postedApprovalBody(parseConsentContext(PICKER_WIRE), fetchMock, {
      interact: () => {
        fireEvent.click(row("mcp.diagnostics.read"));
        fireEvent.click(row("mcp.activity.read"));
        fireEvent.click(row("mcp.activity.read"));
        fireEvent.click(row("mcp.performance.read"));
        shown = SERVER_READS.filter((cap) => row(cap).checked).sort();
      },
    });

    expect(shown).toEqual(["mcp.diagnostics.read", "mcp.performance.read", "mcp.sites.read"]);
    expect([...(body.capabilities as string[])].sort()).toEqual(shown);
  });

  it("posts every offered read for Read everything, and neither the cache request nor mcp.content.read", async () => {
    const fetchMock = vi.fn().mockResolvedValue(approveResponse());
    const body = await postedApprovalBody(parseConsentContext(PICKER_WIRE), fetchMock, {
      interact: () => {
        fireEvent.click(within(picker()).getByRole("button", { name: "Read everything" }));
      },
    });

    const caps = body.capabilities as string[];
    expect([...caps].sort()).toEqual([...SERVER_READS].sort());
    expect(caps).not.toContain("mcp.cache.purge");
    expect(caps).not.toContain("mcp.content.read");
  });

  it("posts the cache request beside the ticked reads, and only when its box is ticked", async () => {
    const fetchMock = vi.fn().mockResolvedValue(approveResponse());
    const body = await postedApprovalBody(parseConsentContext(PICKER_WIRE), fetchMock, {
      tickCache: true,
      interact: () => {
        fireEvent.click(row("mcp.sites.read"));
        fireEvent.click(row("mcp.uptime.read"));
      },
    });

    expect([...(body.capabilities as string[])].sort()).toEqual([
      "mcp.cache.purge",
      "mcp.uptime.read",
    ]);
  });

  it("posts nothing when nothing is ticked, and says why", async () => {
    const fetchMock = vi.fn().mockResolvedValue(approveResponse());
    mockedConsent.mockReturnValue(
      mockQueryResult<ConsentContext>({ data: parseConsentContext(PICKER_WIRE) }),
    );
    vi.stubGlobal("fetch", fetchMock);
    renderWithProviders(<ConnectAiPage />, { withRouter: true, initialPath: LAUNCH });

    await screen.findByLabelText(/name this connection/i);
    fireEvent.click(row("mcp.sites.read"));

    expect(screen.getByTestId("consent-approve")).toBeDisabled();
    expect(screen.getByTestId("consent-nothing-to-confer").textContent).toBe(
      "Nothing is ticked, so this connection could do nothing: tick a box above, or deny the request.",
    );
    // Not by the button alone: submitting the form is refused too, and no
    // request leaves the browser, least of all one with an empty list in it.
    fireEvent.submit(screen.getByTestId("consent-approve").closest("form")!);
    expect(fetchMock).not.toHaveBeenCalled();
    expect(mockedNavigate).not.toHaveBeenCalled();
  });

  it("shows the server's refusal and stays on the page, with the ticks as the operator left them", async () => {
    // The consent endpoint answers a refused narrowing with HTTP 400, error
    // "invalid_request" and the Go message as the description (CapabilitySet.
    // NarrowTo in policy.go, rendered by the oauthError default arm in dto.go).
    const refusal = new Response(
      JSON.stringify({
        error: "invalid_request",
        error_description:
          'capability "mcp.uptime.read" is not held by this organisation\'s default, so a connection cannot be granted it; a connection may only ever be narrower than the organisation default',
      }),
      { status: 400, headers: { "Content-Type": "application/json" } },
    );
    const fetchMock = vi.fn().mockResolvedValue(refusal);
    mockedConsent.mockReturnValue(
      mockQueryResult<ConsentContext>({ data: parseConsentContext(PICKER_WIRE) }),
    );
    vi.stubGlobal("fetch", fetchMock);
    renderWithProviders(<ConnectAiPage />, { withRouter: true, initialPath: LAUNCH });

    await screen.findByLabelText(/name this connection/i);
    fireEvent.click(row("mcp.uptime.read"));
    fireEvent.click(screen.getByTestId("consent-approve"));

    expect(await screen.findByText(/is not held by this organisation's default/i)).toBeTruthy();
    expect(mockedNavigate).not.toHaveBeenCalled();
    expect(row("mcp.uptime.read").checked).toBe(true);
    expect(screen.getByTestId("consent-approve")).toBeEnabled();
  });
});
