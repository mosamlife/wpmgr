import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";
import { mockQueryResult } from "@/test/query-mocks";

import { Route } from "./connect.ai";
import {
  CONSENT_APPROVE_PATH,
  CONSENT_AUTHORIZE_PATH,
  navigateTo,
} from "@/features/mcp-consent/use-consent";
import { useSites } from "@/features/sites/use-sites";
import { useTags } from "@/features/tags/use-tags";
import type { Site, SiteTag } from "@wpmgr/api";

// THE WITHHELD-SCOPES NOTE, FROM THE WIRE TO THE SCREEN.
//
// -connect.ai.test.tsx mocks useConsentContext and hands the screen a context
// it built itself, which cannot catch the failure this feature is exposed to:
// the server sends `unregistered_scopes` and the dashboard never shows it,
// because a wire schema that does not list a key drops it without a word.
//
// So this file leaves useConsentContext and useApproveConsent REAL. The route
// mounts on the real router with a real QueryClient, the authorize GET goes
// through the real fetch-and-parse path against a stubbed `fetch`, and the
// approval POST is read back off the same stub. Only navigateTo (the browser
// hand-off) and the two unrelated list hooks are replaced, as in the sibling.

vi.mock("@/features/mcp-consent/use-consent", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("@/features/mcp-consent/use-consent")>();
  return { ...actual, navigateTo: vi.fn() };
});
vi.mock("@/features/sites/use-sites", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/sites/use-sites")>();
  return { ...actual, useSites: vi.fn() };
});
vi.mock("@/features/tags/use-tags", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/tags/use-tags")>();
  return { ...actual, useTags: vi.fn() };
});

const mockedNavigate = vi.mocked(navigateTo);
const mockedSites = vi.mocked(useSites);
const mockedTags = vi.mocked(useTags);

const ConnectAiPage = Route.options.component!;

// What an MCP client sends when it copies the advertised scope list into its
// sign-in request: all three scopes, space separated.
const LAUNCH =
  "/connect/ai?response_type=code&client_id=c_old&redirect_uri=https%3A%2F%2Fclient.example%2Fcb" +
  "&scope=mcp%3Aread%20mcp%3Asite%20mcp%3Acache&state=opaque-csrf-token" +
  "&code_challenge=cc&code_challenge_method=S256";

const NOTICE =
  "This AI app was connected before WPMgr offered site tools. To give it site tools, remove WPMgr from the app and add it again.";

// Deliberately not a tidy token, as in the sibling: the approval must hand it
// back byte for byte.
const TICKET = " v1.eyJzY29wZXMiOlsibWNwOnJlYWQiXX0.c1gN4tUr3-_~+/=AbC ";

// consentResponseDTO (apps/api/internal/mcp/dto.go), key for key, as the
// authorize endpoint answers for a registration that holds mcp:read alone and
// was asked for the advertised list. The two Go assertions that pin these
// values are in apps/api/internal/mcp/advertised_scopes_test.go
// (TestAuthorizeHandler_NamesTheWithheldScopes): scopes is ["mcp:read"] and
// unregistered_scopes is ["mcp:site","mcp:cache"], and [] when nothing was
// withheld.
function wire(unregistered: readonly string[]) {
  return {
    client_id: "c_old",
    client_name_unverified: "Claude Code",
    client_uri_unverified: "",
    identity_verified: false,
    redirect_uri: "https://client.example/cb",
    redirect_host: "client.example",
    scopes: ["mcp:read"],
    state: "opaque-csrf-token",
    code_challenge: "cc",
    code_challenge_method: "S256",
    consent_ticket: TICKET,
    grant_lifetime_days: 90,
    conferrable_capabilities: [
      { name: "mcp.sites.read", effect: "read" },
      { name: "mcp.uptime.read", effect: "read" },
      { name: "mcp.backups.read", effect: "read" },
    ],
    unregistered_scopes: unregistered,
  };
}

function json(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

/** A fetch that answers the two OAuth endpoints and refuses anything else. */
function oauthFetch(authorizeBody: unknown) {
  return vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (url.startsWith(`${CONSENT_AUTHORIZE_PATH}?`) && (init?.method ?? "GET") === "GET") {
      return json(authorizeBody);
    }
    if (url === CONSENT_APPROVE_PATH && init?.method === "POST") {
      return json({
        grant_id: "g1",
        code: "auth-code",
        redirect_uri: "https://client.example/cb",
        state: "opaque-csrf-token",
      });
    }
    throw new Error(`unexpected fetch ${init?.method ?? "GET"} ${url}`);
  });
}

beforeEach(() => {
  vi.clearAllMocks();
  mockedSites.mockReturnValue(mockQueryResult<Site[]>({ data: [] }));
  mockedTags.mockReturnValue(mockQueryResult<SiteTag[]>({ data: [] }));
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("/connect/ai, an app whose registration predates site tools", () => {
  it("shows the note when the server's payload names withheld scopes", async () => {
    const fetchMock = oauthFetch(wire(["mcp:site", "mcp:cache"]));
    vi.stubGlobal("fetch", fetchMock);

    renderWithProviders(<ConnectAiPage />, { withRouter: true, initialPath: LAUNCH });

    const note = await screen.findByTestId("consent-unregistered-scopes");
    expect(note.textContent).toBe(NOTICE);

    // The real hook ran: it asked the authorize endpoint for the three scopes
    // the client asked for, not for the narrowed set.
    const asked = new URL(String(fetchMock.mock.calls[0]![0]), "https://dashboard.example");
    expect(asked.pathname).toBe(CONSENT_AUTHORIZE_PATH);
    expect(asked.searchParams.get("scope")).toBe("mcp:read mcp:site mcp:cache");
  });

  it("shows no note, on a screen that did load, when nothing was withheld", async () => {
    // The over-fire case, with its positive control: the approve button is
    // what proves the screen rendered, so the absent note is an absence and
    // not a screen that failed to mount.
    vi.stubGlobal("fetch", oauthFetch(wire([])));

    renderWithProviders(<ConnectAiPage />, { withRouter: true, initialPath: LAUNCH });

    expect(await screen.findByTestId("consent-approve")).toBeTruthy();
    expect(screen.queryByTestId("consent-unregistered-scopes")).toBeNull();
    expect(screen.queryByText(/connected before WPMgr offered site tools/i)).toBeNull();
  });

  it("approves only the scopes the server sealed, never a withheld one", async () => {
    // The note is copy. What the real approval POST carries is the sealed
    // ticket, the narrowed scopes and the reads: no site tools and no cache
    // clear, which the registration does not hold.
    const fetchMock = oauthFetch(wire(["mcp:site", "mcp:cache"]));
    vi.stubGlobal("fetch", fetchMock);

    renderWithProviders(<ConnectAiPage />, { withRouter: true, initialPath: LAUNCH });

    await screen.findByTestId("consent-unregistered-scopes");
    fireEvent.click(screen.getByTestId("consent-approve"));

    await waitFor(() => expect(mockedNavigate).toHaveBeenCalledTimes(1));
    const post = fetchMock.mock.calls.find(
      ([url, init]) => String(url) === CONSENT_APPROVE_PATH && init?.method === "POST",
    );
    expect(post).toBeDefined();
    const body = JSON.parse(post![1]!.body as string) as Record<string, unknown>;

    expect(body.scopes).toEqual(["mcp:read"]);
    expect(body.consent_ticket).toBe(TICKET);
    const capabilities = body.capabilities as string[];
    expect(capabilities).toContain("mcp.sites.read");
    expect(capabilities).not.toContain("mcp.ability.read");
    expect(capabilities).not.toContain("mcp.ability.request");
    expect(capabilities).not.toContain("mcp.cache.purge");
    // The field the note reads from is not part of what is sent back.
    expect("unregistered_scopes" in body).toBe(false);
  });
});
