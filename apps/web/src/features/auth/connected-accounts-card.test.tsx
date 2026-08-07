import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";

// The connected accounts card's ONE job that cannot be got wrong: never let
// somebody walk into an account with no way to sign in.
//
// The server refuses that unlink regardless (decideUnlink, and it is refused
// inside a locked transaction so two tabs cannot race past it). What is tested
// here is the half the server cannot do: that a person is not offered the
// button in the first place, and that when they somehow reach the refusal
// anyway, the server's message survives to the screen instead of being
// flattened into a generic failure. The message is the only thing that tells
// them what to do instead.

const { listMyIdentitiesMock, unlinkMyIdentityMock, setMyInitialPasswordMock } =
  vi.hoisted(() => ({
    listMyIdentitiesMock: vi.fn(),
    unlinkMyIdentityMock: vi.fn(),
    setMyInitialPasswordMock: vi.fn(),
  }));

vi.mock("@wpmgr/api", () => ({
  client: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), delete: vi.fn() },
  getMe: vi.fn(async () => ({ data: null, error: undefined, response: { status: 401 } })),
  login: vi.fn(),
  logout: vi.fn(),
  register: vi.fn(),
  listMyIdentities: listMyIdentitiesMock,
  unlinkMyIdentity: unlinkMyIdentityMock,
  setMyInitialPassword: setMyInitialPasswordMock,
}));

vi.mock("@/components/toast", () => ({
  toast: { success: vi.fn(), error: vi.fn(), warning: vi.fn(), info: vi.fn() },
}));

// The "connect another provider" block asks the server which providers this
// install offers. Not the subject of these tests, and it fetches over bare
// `fetch`, so it is stubbed to "none configured".
vi.mock("@/features/auth/social-buttons", () => ({
  useSignInMethods: () => ({ data: { providers: [], sso: false } }),
}));

const { ConnectedAccountsCard } = await import("./connected-accounts-card");
const { useUnlinkIdentity } = await import("./use-connected-accounts");

const GOOGLE = {
  provider: "google",
  email: "someone@example.test",
  email_verified: true,
  created_at: "2026-01-05T10:00:00Z",
  last_login_at: "2026-08-01T10:00:00Z",
};
const GITHUB = { ...GOOGLE, provider: "github" };

beforeEach(() => {
  listMyIdentitiesMock.mockReset();
  unlinkMyIdentityMock.mockReset();
  setMyInitialPasswordMock.mockReset();
});

describe("ConnectedAccountsCard: the last sign-in method", () => {
  it("offers no Disconnect for a sole provider on a passwordless account, and says why", async () => {
    listMyIdentitiesMock.mockResolvedValue({
      data: { has_password: false, can_unlink: false, items: [GOOGLE] },
      error: undefined,
    });

    renderWithProviders(<ConnectedAccountsCard />);

    await screen.findByText("Google");
    expect(screen.queryByRole("button", { name: /disconnect google/i })).toBeNull();
    // The way out has to be named, or the missing button reads as a bug.
    expect(
      screen.getByText(/only sign-in method\. set a password to disconnect it\./i),
    ).toBeInTheDocument();
  });

  it("offers Disconnect once a password exists", async () => {
    listMyIdentitiesMock.mockResolvedValue({
      data: { has_password: true, can_unlink: true, items: [GOOGLE] },
      error: undefined,
    });

    renderWithProviders(<ConnectedAccountsCard />);

    expect(
      await screen.findByRole("button", { name: /disconnect google/i }),
    ).toBeInTheDocument();
  });

  it("offers Disconnect on a passwordless account with two providers", async () => {
    listMyIdentitiesMock.mockResolvedValue({
      data: { has_password: false, can_unlink: true, items: [GOOGLE, GITHUB] },
      error: undefined,
    });

    renderWithProviders(<ConnectedAccountsCard />);

    expect(
      await screen.findByRole("button", { name: /disconnect google/i }),
    ).toBeInTheDocument();
    expect(
      await screen.findByRole("button", { name: /disconnect github/i }),
    ).toBeInTheDocument();
  });

  it("shows the set-a-password form only when there is no password", async () => {
    listMyIdentitiesMock.mockResolvedValue({
      data: { has_password: false, can_unlink: false, items: [GOOGLE] },
      error: undefined,
    });
    const { unmount } = renderWithProviders(<ConnectedAccountsCard />);
    expect(await screen.findByLabelText("New password")).toBeInTheDocument();
    unmount();

    listMyIdentitiesMock.mockResolvedValue({
      data: { has_password: true, can_unlink: true, items: [GOOGLE] },
      error: undefined,
    });
    renderWithProviders(<ConnectedAccountsCard />);
    await screen.findByText("Google");
    expect(screen.queryByLabelText("New password")).toBeNull();
  });
});

describe("useUnlinkIdentity: the server's 409 refusal", () => {
  it("surfaces the server's message verbatim rather than a generic failure", async () => {
    const serverMessage =
      "disconnecting Google would leave you no way to sign in, because this account " +
      "has no password. Set a password first, then disconnect Google.";
    unlinkMyIdentityMock.mockResolvedValue({
      data: undefined,
      error: { code: "last_sign_in_method", message: serverMessage },
      response: { status: 409 },
    });

    // Rendered through a component so the hook has a QueryClient, matching how
    // it is actually used.
    function Probe() {
      const unlink = useUnlinkIdentity();
      return (
        <button type="button" onClick={() => unlink.mutate("google")}>
          {unlink.isError ? unlink.error.message : "go"}
        </button>
      );
    }
    renderWithProviders(<Probe />);
    screen.getByRole("button", { name: "go" }).click();

    await waitFor(() => {
      // Non-vacuous: a generic `toError` fallback would render the envelope's
      // code or a stock string, not the sentence naming the next step.
      expect(screen.getByRole("button").textContent).toContain("Set a password first");
    });
  });
});
