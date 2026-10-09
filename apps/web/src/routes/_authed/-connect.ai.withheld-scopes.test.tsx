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

// The two sentences, written out in full and never imported from the screen,
// so editing the copy there reddens this file. Which one shows is decided by
// whether mcp:site itself is in the withheld list: an app that holds mcp:site
// and lacks only mcp:cache is already offered site tools, and must not be told
// to reinstall to get them.
const SITE_NOTICE =
  "This AI app was connected before WPMgr offered site tools. To give it site tools, remove WPMgr from the app and add it again.";
const OTHER_NOTICE =
  "This AI app was connected before WPMgr offered some of the permissions it is asking for. To give it those, remove WPMgr from the app and add it again.";

// Deliberately not a tidy token, as in the sibling: the approval must hand it
// back byte for byte.
const TICKET = " v1.eyJzY29wZXMiOlsibWNwOnJlYWQiXX0.c1gN4tUr3-_~+/=AbC ";

// consentResponseDTO (apps/api/internal/mcp/dto.go), key for key, as the
// authorize endpoint answers for a registration asked for the advertised list.
// The default is a registration that holds mcp:read alone: the two Go
// assertions that pin those values are in
// apps/api/internal/mcp/advertised_scopes_test.go
// (TestAuthorizeHandler_NamesTheWithheldScopes): scopes is ["mcp:read"] and
// unregistered_scopes is ["mcp:site","mcp:cache"], and [] when nothing was
// withheld.
//
// `held` is what the registration holds and so what `scopes` carries. A
// registration holding mcp:site also confers the two ability capabilities
// (scopeCapabilities in apps/api/internal/mcp/policy.go: mcp.ability.read is a
// read, mcp.ability.request is a request), and the screen offers its site-tools
// box from them.
function wire(unregistered: readonly string[], held: readonly string[] = ["mcp:read"]) {
  return {
    client_id: "c_old",
    client_name_unverified: "Claude Code",
    client_uri_unverified: "",
    identity_verified: false,
    redirect_uri: "https://client.example/cb",
    redirect_host: "client.example",
    scopes: held,
    state: "opaque-csrf-token",
    code_challenge: "cc",
    code_challenge_method: "S256",
    consent_ticket: TICKET,
    grant_lifetime_days: 90,
    conferrable_capabilities: [
      { name: "mcp.sites.read", effect: "read" },
      { name: "mcp.uptime.read", effect: "read" },
      { name: "mcp.backups.read", effect: "read" },
      ...(held.includes("mcp:site")
        ? [
            { name: "mcp.ability.read", effect: "read" },
            { name: "mcp.ability.request", effect: "request" },
          ]
        : []),
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

/** The URL a fetch call was made with, whichever of the three forms it took. */
function urlOf(input: RequestInfo | URL): string {
  if (typeof input === "string") return input;
  if (input instanceof URL) return input.href;
  return input.url;
}

/** A fetch that answers the two OAuth endpoints and refuses anything else. */
function oauthFetch(authorizeBody: unknown) {
  return vi.fn((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = urlOf(input);
    if (url.startsWith(`${CONSENT_AUTHORIZE_PATH}?`) && (init?.method ?? "GET") === "GET") {
      return Promise.resolve(json(authorizeBody));
    }
    if (url === CONSENT_APPROVE_PATH && init?.method === "POST") {
      return Promise.resolve(
        json({
          grant_id: "g1",
          code: "auth-code",
          redirect_uri: "https://client.example/cb",
          state: "opaque-csrf-token",
        }),
      );
    }
    return Promise.reject(new Error(`unexpected fetch ${init?.method ?? "GET"} ${url}`));
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
  it("shows the site sentence when the server's payload names mcp:site as withheld", async () => {
    const fetchMock = oauthFetch(wire(["mcp:site", "mcp:cache"]));
    vi.stubGlobal("fetch", fetchMock);

    renderWithProviders(<ConnectAiPage />, { withRouter: true, initialPath: LAUNCH });

    const note = await screen.findByTestId("consent-unregistered-scopes");
    expect(note.textContent).toBe(SITE_NOTICE);

    // The real hook ran: it asked the authorize endpoint for the three scopes
    // the client asked for, not for the narrowed set.
    const asked = new URL(urlOf(fetchMock.mock.calls[0]![0]), "https://dashboard.example");
    expect(asked.pathname).toBe(CONSENT_AUTHORIZE_PATH);
    expect(asked.searchParams.get("scope")).toBe("mcp:read mcp:site mcp:cache");
  });

  it.each([
    { label: "mcp:site alone", unregistered: ["mcp:site"] },
    { label: "mcp:cache listed before mcp:site", unregistered: ["mcp:cache", "mcp:site"] },
  ])(
    "shows the site sentence wherever mcp:site sits in the withheld list: $label",
    async ({ unregistered }) => {
      vi.stubGlobal("fetch", oauthFetch(wire(unregistered)));

      renderWithProviders(<ConnectAiPage />, { withRouter: true, initialPath: LAUNCH });

      const note = await screen.findByTestId("consent-unregistered-scopes");
      expect(note.textContent).toBe(SITE_NOTICE);
      expect(screen.getAllByTestId("consent-unregistered-scopes")).toHaveLength(1);
    },
  );

  it("does not promise site tools to an app that already holds them, when only mcp:cache was withheld", async () => {
    // A registration holding mcp:read and mcp:site and lacking mcp:cache: the
    // authorize endpoint keeps mcp:site in `scopes`, so the screen below offers
    // the site-tools box, and names only mcp:cache as withheld. Telling this
    // user to remove the app to get site tools would send them to discard
    // access they already have.
    vi.stubGlobal("fetch", oauthFetch(wire(["mcp:cache"], ["mcp:read", "mcp:site"])));

    renderWithProviders(<ConnectAiPage />, { withRouter: true, initialPath: LAUNCH });

    const note = await screen.findByTestId("consent-unregistered-scopes");
    expect(note.textContent).toBe(OTHER_NOTICE);

    // The positive control for why the site sentence would have been wrong:
    // the same screen is offering site tools, and no cache box.
    expect(screen.getByTestId("consent-site-capability")).toBeTruthy();
    expect(screen.queryByTestId("consent-cache-capability")).toBeNull();
  });

  it("shows the general sentence and none of the scope strings for a scope it has never heard of", async () => {
    // Membership picks a fixed sentence. Nothing the server sends in the list
    // is ever rendered, so an unfamiliar value cannot reach the page.
    vi.stubGlobal("fetch", oauthFetch(wire(["mcp:future-scope"])));

    renderWithProviders(<ConnectAiPage />, { withRouter: true, initialPath: LAUNCH });

    const note = await screen.findByTestId("consent-unregistered-scopes");
    expect(note.textContent).toBe(OTHER_NOTICE);
    expect(document.body.textContent).not.toContain("mcp:future-scope");
  });

  it("shows no note, on a screen that did load, when nothing was withheld", async () => {
    // The over-fire case, with its positive control: the approve button is
    // what proves the screen rendered, so the absent note is an absence and
    // not a screen that failed to mount. The pattern covers both sentences.
    vi.stubGlobal("fetch", oauthFetch(wire([])));

    renderWithProviders(<ConnectAiPage />, { withRouter: true, initialPath: LAUNCH });

    expect(await screen.findByTestId("consent-approve")).toBeTruthy();
    expect(screen.queryByTestId("consent-unregistered-scopes")).toBeNull();
    expect(screen.queryByText(/connected before WPMgr offered/i)).toBeNull();
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
      ([url, init]) => urlOf(url) === CONSENT_APPROVE_PATH && init?.method === "POST",
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
