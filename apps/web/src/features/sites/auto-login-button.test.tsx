import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, fireEvent } from "@testing-library/react";
import type { Me } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { mockMutationResult } from "@/test/query-mocks";

import { AutoLoginButton } from "./auto-login-button";
import { useAutoLogin } from "./use-autologin";
import { useMe } from "@/features/auth/use-auth";

// GH #286 (web half): AutoLoginButton's default-login-user affordances.
//
// Three states the locked design names explicitly:
//   1. `defaultLoginUser` absent (undefined): renders exactly like before
//      the feature shipped, with no tooltip and no dropdown header row.
//      This is what every list surface (sites-list rows, bulk drawer,
//      incident dialog) gets, since they never pass the prop.
//   2. `defaultLoginUser=""`: a policy exists but no default is configured.
//   3. `defaultLoginUser="editor-jane"`: the configured default.
//
// The dropdown's header row is the deterministic assertion (Radix
// DropdownMenu renders its content synchronously once open, no animation
// wait needed). The hover/focus Tooltip is also exercised once, via a
// focus event: Radix opens a Tooltip immediately on trigger focus (no
// `delayDuration`, that timer only gates pointer-hover opens).

vi.mock("./use-autologin", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-autologin")>();
  return { ...actual, useAutoLogin: vi.fn() };
});

vi.mock("@/features/auth/use-auth", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/auth/use-auth")>();
  return { ...actual, useMe: vi.fn() };
});

vi.mock("@/components/toast", () => ({
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn(), warning: vi.fn() },
}));

const mockedUseAutoLogin = vi.mocked(useAutoLogin);
const mockedUseMe = vi.mocked(useMe);

function buildMe(overrides: Partial<Me> = {}): Me {
  return {
    user: {
      id: "u1",
      email: "owner@example.com",
      name: "Owner",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    },
    memberships: [{ user_id: "u1", tenant_id: "t1", role: "owner" }],
    active_tenant_id: "t1",
    hosted: true,
    ...overrides,
  };
}

/** Matches an element whose OWN trimmed text content equals `expected`,
 *  regardless of how the JSX split it across child nodes (e.g. a `<span>`
 *  for the font-mono username). Plain string matchers only match a single
 *  text node, so a split label needs the function-matcher form (same
 *  pattern as `bulk-action-drawer.test.tsx`). */
function byExactText(expected: string) {
  return (_content: string, node: Element | null) =>
    node?.textContent?.trim() === expected;
}

function openChevronMenu() {
  const trigger = screen.getByRole("button", { name: /more log-in options/i });
  fireEvent.keyDown(trigger, { key: "Enter" });
  return screen.findByRole("menuitem", { name: /open dashboard/i });
}

beforeEach(() => {
  vi.clearAllMocks();
  mockedUseMe.mockReturnValue(
    // Only `data` is read by `canAutoLogin`/consumers here.
    { data: buildMe() } as ReturnType<typeof useMe>,
  );
  mockedUseAutoLogin.mockReturnValue(
    mockMutationResult({ mutate: vi.fn() }) as ReturnType<typeof useAutoLogin>,
  );
});

describe("AutoLoginButton: defaultLoginUser absent (list surfaces)", () => {
  it("renders no tooltip on focus and no dropdown header row", async () => {
    renderWithProviders(<AutoLoginButton siteId="site-1" siteName="Example" />);

    fireEvent.focus(screen.getByRole("button", { name: /log in to site/i }));
    expect(screen.queryByText(/logs in as/i)).not.toBeInTheDocument();

    await openChevronMenu();
    expect(screen.queryByText(/logs in as/i)).not.toBeInTheDocument();
    // First item is the real first menu entry, no header row was inserted.
    const items = screen.getAllByRole("menuitem");
    expect(items[0]).toHaveTextContent("Open Dashboard");
  });
});

describe("AutoLoginButton: defaultLoginUser is an empty string (policy known, no default set)", () => {
  it("dropdown header row reads the first-administrator fallback", async () => {
    renderWithProviders(
      <AutoLoginButton siteId="site-1" siteName="Example" defaultLoginUser="" />,
    );

    await openChevronMenu();
    expect(
      screen.getByText(byExactText("Logs in as the first administrator")),
    ).toBeInTheDocument();
  });

  it("tooltip opens on focus with the first-administrator copy", () => {
    renderWithProviders(
      <AutoLoginButton siteId="site-1" siteName="Example" defaultLoginUser="" />,
    );

    fireEvent.focus(screen.getByRole("button", { name: /log in to site/i }));
    expect(
      screen.getAllByText(byExactText("Logs in as the first administrator")).length,
    ).toBeGreaterThan(0);
  });
});

describe("AutoLoginButton: defaultLoginUser is a configured username", () => {
  it("dropdown header row names the configured user in font-mono", async () => {
    renderWithProviders(
      <AutoLoginButton
        siteId="site-1"
        siteName="Example"
        defaultLoginUser="editor-jane"
      />,
    );

    await openChevronMenu();
    const row = screen.getByText(byExactText("Logs in as editor-jane"));
    expect(row).toBeInTheDocument();
    expect(row.querySelector("span.font-mono")).toHaveTextContent("editor-jane");
  });

  it("tooltip opens on focus naming the configured user", () => {
    renderWithProviders(
      <AutoLoginButton
        siteId="site-1"
        siteName="Example"
        defaultLoginUser="editor-jane"
      />,
    );

    fireEvent.focus(screen.getByRole("button", { name: /log in to site/i }));
    expect(
      screen.getAllByText(byExactText("Logs in as editor-jane")).length,
    ).toBeGreaterThan(0);
  });
});
