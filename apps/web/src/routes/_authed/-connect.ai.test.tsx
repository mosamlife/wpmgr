import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";
import { mockQueryResult } from "@/test/query-mocks";

import { Route } from "./connect.ai";
import {
  useConsentContext,
  navigateTo,
  OAuthRequestError,
} from "@/features/mcp-consent/use-consent";
import { parseConsentContext, SCOPE_READ } from "@/features/mcp-consent/consent-context";
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

/** Drive the screen to a real approval POST and return the body it sent. */
async function postedApprovalBody(
  consent: ConsentContext,
  fetchMock: ReturnType<typeof vi.fn>,
): Promise<Record<string, unknown>> {
  mockedConsent.mockReturnValue(mockQueryResult<ConsentContext>({ data: consent }));
  vi.stubGlobal("fetch", fetchMock);

  renderWithProviders(<ConnectAiPage />, { withRouter: true, initialPath: LAUNCH });

  fireEvent.change(await screen.findByLabelText(/name this connection/i), {
    target: { value: "Desktop" },
  });
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
