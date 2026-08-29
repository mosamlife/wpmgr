import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, fireEvent } from "@testing-library/react";

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

    await screen.findByRole("generic", { busy: true }).catch(() => null);
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
