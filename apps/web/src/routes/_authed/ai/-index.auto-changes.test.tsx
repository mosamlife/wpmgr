import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor, fireEvent } from "@testing-library/react";
import type { QueryClient } from "@tanstack/react-query";
import type { AiAuto, AiConnectionAuto, AiConnectionUsage } from "@wpmgr/api";

import { createTestQueryClient, renderWithProviders } from "@/test/render";
import { authKeys } from "@/features/auth/use-auth";
import { assistantRequestKeys } from "@/features/ai-requests/use-ai-requests";
import { aiTrustKeys } from "@/features/ai-trust/use-ai-trust";

import { Route } from "./index";

// A CONNECTION'S AUTOMATIC CHANGES (design §8.7), through the real /ai route.
//
// The route, the connections list, the panel, the usage and switch hooks, the
// real router (memory history) and the real QueryClient all run. Two things
// are replaced, both at the wire: the hand-shaped connections endpoint
// (a stubbed fetch, as -index.test.tsx does) and the two generated-client
// operations this panel calls, getAiConnectionUsage and putAiConnectionAuto.
// Their answers are typed as the generated AiConnectionUsage and
// AiConnectionAuto, so a contract change fails the typecheck here instead of
// leaving a fixture that no server would send.
//
// Codes asserted: `paused` is the 409 the contract (packages/openapi/openapi.yaml,
// putAIConnectionAuto) gives for saving while the organisation's AI is paused.

const { usageMock, putAutoMock, abilityListMock, toastSuccess, toastError } = vi.hoisted(() => ({
  usageMock: vi.fn(),
  putAutoMock: vi.fn(),
  abilityListMock: vi.fn(),
  toastSuccess: vi.fn(),
  toastError: vi.fn(),
}));

vi.mock("sonner", () => ({ toast: { success: toastSuccess, error: toastError } }));

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return {
    ...actual,
    getAiConnectionUsage: usageMock,
    putAiConnectionAuto: putAutoMock,
    listAbilityRequests: abilityListMock,
  };
});

const AiPage = Route.options.component!;

const TENANT = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const GRANT_ID = "11111111-1111-1111-1111-111111111111";
const OTHER_GRANT_ID = "22222222-2222-2222-2222-222222222222";

let meRole: "owner" | "admin" | "operator" = "admin";

function seedMe(queryClient: QueryClient) {
  queryClient.setQueryData(authKeys.me, {
    id: "user-1",
    email: "priya@example.test",
    name: "Priya",
    scope: "org",
    role: meRole,
    active_tenant_id: TENANT,
    memberships: [{ tenant_id: TENANT, role: meRole, tenant_name: "Example" }],
  });
}

function connectionRow(over: Record<string, unknown> = {}) {
  return {
    id: GRANT_ID,
    name: "Fleet manager",
    status: "active",
    site_scope_mode: "all",
    scopes: ["mcp:read"],
    created_at: "2026-08-01T00:00:00Z",
    reported_client_name: "claude-code",
    reported_client_version: "2.1.0",
    protocol: { state: "recognised", version: "2025-11-25" },
    last_used_at: "2026-08-29T10:00:00Z",
    revoked_at: null,
    capabilities: ["mcp.sites.read"],
    ...over,
  };
}

function urlOf(input: RequestInfo | URL): string {
  if (typeof input === "string") return input;
  if (input instanceof URL) return input.toString();
  return input.url;
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

// Only the connections list is answered. The verification panel's status path
// gets a 404 so "Check connection" opens on its own error state.
function stubConnections(rows: unknown[]) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      const url = urlOf(input);
      if (url.endsWith("/api/v1/mcp/connections")) return Promise.resolve(json({ connections: rows }));
      return Promise.resolve(json({ code: "not_found", message: "not found" }, 404));
    }),
  );
}

function usageFixture(over: Partial<AiConnectionUsage> = {}): AiConnectionUsage {
  return {
    grant_id: GRANT_ID,
    ai_auto: "site_setting",
    auto_set_by_user_id: "user-1",
    auto_set_by_name: "Priya",
    auto_set_by_account_deleted: false,
    auto_set_at: "2026-10-09T09:00:00Z",
    auto_setter_valid: true,
    created_with_api_key: false,
    window_minutes: 60,
    draft_changes: { used: 14, limit: 600 },
    draft_sites: { used: 2, limit: 30 },
    ...over,
  };
}

function savedFixture(over: Partial<AiConnectionAuto> = {}): AiConnectionAuto {
  return {
    grant_id: GRANT_ID,
    ai_auto: "never",
    auto_set_by_user_id: null,
    auto_set_by_name: null,
    auto_set_by_account_deleted: false,
    auto_set_at: null,
    auto_setter_valid: true,
    created_with_api_key: false,
    ...over,
  };
}

function ok(data: unknown) {
  return Promise.resolve({ data, error: undefined, response: { status: 200 } });
}

// A stand-in for the control plane's side of the contract, so a re-read after a
// save sees what the save did. Saving records the caller as the person who
// allowed it, and an allowed switch holds again (putAIConnectionAuto).
let server: AiConnectionUsage;

function authOf(u: AiConnectionUsage): AiConnectionAuto {
  return {
    grant_id: u.grant_id,
    ai_auto: u.ai_auto,
    auto_set_by_user_id: u.auto_set_by_user_id,
    auto_set_by_name: u.auto_set_by_name,
    auto_set_by_account_deleted: u.auto_set_by_account_deleted,
    auto_set_at: u.auto_set_at,
    auto_setter_valid: u.auto_setter_valid,
    created_with_api_key: u.created_with_api_key,
  };
}

function serveUsage(over: Partial<AiConnectionUsage> = {}) {
  server = usageFixture(over);
  usageMock.mockImplementation(() => ok(server));
  putAutoMock.mockImplementation(({ body }: { body: { ai_auto: AiAuto } }) => {
    server = {
      ...server,
      ai_auto: body.ai_auto,
      auto_setter_valid: true,
      auto_set_by_user_id: body.ai_auto === "site_setting" ? "user-1" : server.auto_set_by_user_id,
    };
    return ok(authOf(server));
  });
}

function fail(status: number, error: { code: string; message: string }) {
  return Promise.resolve({ data: undefined, error, response: { status } });
}

function renderPage() {
  const queryClient = createTestQueryClient();
  seedMe(queryClient);
  queryClient.setQueryData(assistantRequestKeys.list(), {
    requests: [],
    pending_count: 0,
    limit: 50,
    offset: 0,
  });
  const view = renderWithProviders(<AiPage />, { withRouter: true, initialPath: "/ai", queryClient });
  return { ...view, queryClient };
}

async function openPanel() {
  await screen.findByText("Fleet manager");
  fireEvent.click(screen.getByRole("button", { name: "Automatic changes" }));
  return screen.findByTestId("connection-auto-panel");
}

const SITE_SETTING = () => screen.getByRole("radio", { name: "Where each site allows it" });
const NEVER = () => screen.getByRole("radio", { name: "Never (always ask)" });
const SAVE = () => screen.getByRole("button", { name: "Save" });

beforeEach(() => {
  vi.clearAllMocks();
  meRole = "admin";
  stubConnections([connectionRow()]);
  usageMock.mockReset();
  putAutoMock.mockReset();
  abilityListMock.mockReset();
  abilityListMock.mockReturnValue(ok({ requests: [], pending_count: 0, limit: 1, offset: 0 }));
  serveUsage();
});
afterEach(() => vi.unstubAllGlobals());

describe("the usage line", () => {
  it("is not requested until the panel is opened", async () => {
    renderPage();
    await screen.findByText("Fleet manager");
    expect(usageMock).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Automatic changes" }));
    await screen.findByTestId("connection-draft-usage");
    expect(usageMock).toHaveBeenCalledTimes(1);
    expect(usageMock).toHaveBeenCalledWith({ path: { grantId: GRANT_ID } });
  });

  it("states all four numbers exactly as the server sent them", async () => {
    // Numbers chosen to differ from the design's drawing, so a literal in the
    // component cannot satisfy this. One site, to pin the singular.
    serveUsage({ draft_changes: { used: 3, limit: 123 }, draft_sites: { used: 1, limit: 45 } });
    renderPage();
    await openPanel();
    expect((await screen.findByTestId("connection-draft-usage")).textContent).toBe(
      "This hour: 3 draft changes (limit 123) on 1 site (limit 45).",
    );
    expect(
      screen.getByText("Above a limit, changes wait for you instead of running. WPMgr sets these limits."),
    ).toBeInTheDocument();
  });

  it("names the window when it is not an hour, rather than saying this hour", async () => {
    serveUsage({ window_minutes: 30 });
    renderPage();
    await openPanel();
    const line = (await screen.findByTestId("connection-draft-usage")).textContent ?? "";
    expect(line.startsWith("In the last 30 minutes:")).toBe(true);
  });

  it("shows a skeleton while loading, and no switch to choose from yet", async () => {
    usageMock.mockReturnValue(new Promise(() => {}));
    renderPage();
    await openPanel();
    expect(screen.getByLabelText("Loading this connection's usage")).toBeInTheDocument();
    expect(screen.queryByRole("radio")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
  });

  it("says the usage could not be loaded, with no usage line, and the switch still works", async () => {
    usageMock.mockReturnValue(fail(500, { code: "internal", message: "the server said 500" }));
    putAutoMock.mockReturnValue(ok(savedFixture()));
    renderPage();
    await openPanel();
    expect(await screen.findByText("Could not load this connection's usage.")).toBeInTheDocument();
    expect(screen.queryByTestId("connection-draft-usage")).not.toBeInTheDocument();

    // The switch still works with the read failed: nothing is chosen, any
    // choice can be saved.
    expect(SITE_SETTING()).not.toBeChecked();
    expect(NEVER()).not.toBeChecked();
    expect(SAVE()).toBeDisabled();
    fireEvent.click(NEVER());
    expect(SAVE()).toBeEnabled();
    fireEvent.click(SAVE());
    await waitFor(() => expect(putAutoMock).toHaveBeenCalledTimes(1));
    expect(putAutoMock).toHaveBeenCalledWith({ path: { grantId: GRANT_ID }, body: { ai_auto: "never" } });
  });

  it("reads again on Try again and then shows the usage", async () => {
    usageMock.mockReturnValueOnce(fail(500, { code: "internal", message: "the server said 500" }));
    renderPage();
    await openPanel();
    expect(await screen.findByText("Could not load this connection's usage.")).toBeInTheDocument();
    expect(usageMock).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByRole("button", { name: "Try again" }));

    expect((await screen.findByTestId("connection-draft-usage")).textContent).toContain("14 draft changes");
    expect(usageMock).toHaveBeenCalledTimes(2);
    expect(screen.queryByText("Could not load this connection's usage.")).not.toBeInTheDocument();
    // With the read back, the stored choice is shown.
    expect(SITE_SETTING()).toBeChecked();
  });
});

describe("the switch", () => {
  it("opens on the stored choice with nothing to save", async () => {
    renderPage();
    await openPanel();
    await screen.findByTestId("connection-draft-usage");
    expect(SITE_SETTING()).toBeChecked();
    expect(NEVER()).not.toBeChecked();
    expect(SAVE()).toBeDisabled();
  });

  it("saves the other choice, says so, and shows the saved choice before the re-read arrives", async () => {
    const view = renderPage();
    await openPanel();
    await screen.findByTestId("connection-draft-usage");

    // The re-read after the save never answers, so the radios can only show
    // the saved choice by taking it from the save's own answer.
    usageMock.mockReturnValue(new Promise(() => {}));
    putAutoMock.mockReturnValue(ok(savedFixture({ ai_auto: "never" })));

    fireEvent.click(NEVER());
    expect(SAVE()).toBeEnabled();
    fireEvent.click(SAVE());

    await waitFor(() => expect(toastSuccess).toHaveBeenCalledWith("Saved. Never (always ask)."));
    expect(putAutoMock).toHaveBeenCalledWith({ path: { grantId: GRANT_ID }, body: { ai_auto: "never" } });
    await waitFor(() => expect(usageMock).toHaveBeenCalledTimes(2));
    expect(NEVER()).toBeChecked();
    expect(SITE_SETTING()).not.toBeChecked();
    expect(SAVE()).toBeDisabled();
    // The counts the save did not carry are kept, not blanked.
    const cached = view.queryClient.getQueryData<AiConnectionUsage>(aiTrustKeys.usage(GRANT_ID));
    expect(cached?.draft_changes).toEqual({ used: 14, limit: 600 });
    expect(cached?.ai_auto).toBe("never");
  });

  it("says the connection is unchanged when the save fails, and keeps the choice made", async () => {
    putAutoMock.mockReturnValue(fail(500, { code: "internal", message: "boom" }));
    renderPage();
    await openPanel();
    await screen.findByTestId("connection-draft-usage");

    fireEvent.click(NEVER());
    fireEvent.click(SAVE());

    const alert = await screen.findByTestId("connection-auto-save-failed");
    expect(alert).toHaveAttribute("role", "alert");
    expect(alert.textContent).toBe("WPMgr could not save this. The connection is unchanged.");
    expect(toastSuccess).not.toHaveBeenCalled();
    expect(NEVER()).toBeChecked();

    // Choosing again clears the failure rather than leaving it standing.
    fireEvent.click(SITE_SETTING());
    expect(screen.queryByTestId("connection-auto-save-failed")).not.toBeInTheDocument();
  });

  it("says the organisation's AI is paused when the server refuses with `paused`", async () => {
    serveUsage({ ai_auto: "never" });
    putAutoMock.mockReturnValue(fail(409, { code: "paused", message: "paused" }));
    renderPage();
    await openPanel();
    await screen.findByTestId("connection-draft-usage");

    fireEvent.click(SITE_SETTING());
    fireEvent.click(SAVE());

    const alert = await screen.findByTestId("connection-auto-save-failed");
    expect(alert.textContent).toBe(
      "AI is paused for your organisation. Nothing runs, automatic or not, until an owner resumes it.",
    );
  });

  it("is offered to an owner as well as an admin", async () => {
    meRole = "owner";
    renderPage();
    await openPanel();
    await screen.findByTestId("connection-draft-usage");
    expect(SITE_SETTING()).toBeInTheDocument();
  });

  it("is not offered to someone who cannot manage connections, who still sees the usage", async () => {
    meRole = "operator";
    renderPage();
    await openPanel();
    expect((await screen.findByTestId("connection-draft-usage")).textContent).toContain("14 draft changes");
    expect(screen.queryByRole("radio")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
  });

  it("is read-only on a revoked connection, which says so", async () => {
    stubConnections([connectionRow({ status: "revoked", revoked_at: "2026-09-01T00:00:00Z" })]);
    renderPage();
    await openPanel();
    expect((await screen.findByTestId("connection-draft-usage")).textContent).toContain("14 draft changes");
    expect((await screen.findByTestId("connection-auto-revoked")).textContent).toBe(
      "This connection is revoked.",
    );
    expect(screen.queryByRole("radio")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
  });
});

describe("a connection created with an API key", () => {
  it("shows Never selected with the reason a person must allow it", async () => {
    serveUsage({
      ai_auto: "never",
      created_with_api_key: true,
      auto_set_by_user_id: null,
      auto_set_by_name: null,
      auto_set_at: null,
    });
    renderPage();
    await openPanel();
    expect((await screen.findByTestId("connection-auto-key-minted")).textContent).toBe(
      "Created with an API key. A person must allow automatic changes.",
    );
    expect(NEVER()).toBeChecked();
    expect(SITE_SETTING()).not.toBeChecked();
    expect(SAVE()).toBeDisabled();
    // A person can allow it, and the save names the choice.
    fireEvent.click(SITE_SETTING());
    fireEvent.click(SAVE());
    await waitFor(() =>
      expect(putAutoMock).toHaveBeenCalledWith({ path: { grantId: GRANT_ID }, body: { ai_auto: "site_setting" } }),
    );
    // Once allowed, the reason no longer applies.
    await waitFor(() => expect(screen.queryByTestId("connection-auto-key-minted")).not.toBeInTheDocument());
  });

  it("does not say it of a connection a person created and set to Never", async () => {
    serveUsage({ ai_auto: "never", created_with_api_key: false });
    renderPage();
    await openPanel();
    await screen.findByTestId("connection-draft-usage");
    expect(NEVER()).toBeChecked();
    expect(screen.queryByTestId("connection-auto-key-minted")).not.toBeInTheDocument();
  });

  it("does not say it once a person has allowed it", async () => {
    serveUsage({ ai_auto: "site_setting", created_with_api_key: true });
    renderPage();
    await openPanel();
    await screen.findByTestId("connection-draft-usage");
    expect(SITE_SETTING()).toBeChecked();
    expect(screen.queryByTestId("connection-auto-key-minted")).not.toBeInTheDocument();
  });
});

describe("a switch whose person can no longer manage connections", () => {
  const LINE =
    "The person who allowed this can no longer manage AI connections, so every change from this connection waits for you. Save to allow it again.";

  it("still shows Where each site allows it, says why changes wait, and can be saved unchanged", async () => {
    serveUsage({ ai_auto: "site_setting", auto_setter_valid: false });
    renderPage();
    await openPanel();

    expect((await screen.findByTestId("connection-auto-setter-invalid")).textContent).toBe(LINE);
    expect(SITE_SETTING()).toBeChecked();
    // Nothing else changed, and Save is still enabled.
    expect(SAVE()).toBeEnabled();

    // Saving records the viewer as the new person; the answer says it holds again.
    fireEvent.click(SAVE());
    await waitFor(() =>
      expect(putAutoMock).toHaveBeenCalledWith({ path: { grantId: GRANT_ID }, body: { ai_auto: "site_setting" } }),
    );
    await waitFor(() => expect(screen.queryByTestId("connection-auto-setter-invalid")).not.toBeInTheDocument());
    expect(SAVE()).toBeDisabled();
  });

  it("shows nothing of the kind while the person still holds the access", async () => {
    serveUsage({ auto_setter_valid: true });
    renderPage();
    await openPanel();
    await screen.findByTestId("connection-draft-usage");
    expect(screen.queryByTestId("connection-auto-setter-invalid")).not.toBeInTheDocument();
    expect(SAVE()).toBeDisabled();
  });
});

describe("opening panels from the list", () => {
  it("opens beside Check connection and toggles by its own label", async () => {
    renderPage();
    await screen.findByText("Fleet manager");
    const button = screen.getByRole("button", { name: "Automatic changes" });
    expect(button).toHaveAttribute("aria-expanded", "false");
    expect(screen.getByRole("button", { name: "Check connection" })).toBeInTheDocument();

    fireEvent.click(button);
    expect(await screen.findByTestId("connection-auto-panel")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Hide automatic changes" })).toHaveAttribute(
      "aria-expanded",
      "true",
    );

    fireEvent.click(screen.getByRole("button", { name: "Hide automatic changes" }));
    expect(screen.queryByTestId("connection-auto-panel")).not.toBeInTheDocument();
  });

  it("closes the automatic-changes panel when Check connection opens, and the reverse", async () => {
    renderPage();
    await screen.findByText("Fleet manager");
    fireEvent.click(screen.getByRole("button", { name: "Automatic changes" }));
    await screen.findByTestId("connection-auto-panel");

    fireEvent.click(screen.getByRole("button", { name: "Check connection" }));
    expect(screen.getByRole("button", { name: "Hide check" })).toBeInTheDocument();
    expect(screen.queryByTestId("connection-auto-panel")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Automatic changes" })).toHaveAttribute("aria-expanded", "false");

    fireEvent.click(screen.getByRole("button", { name: "Automatic changes" }));
    await screen.findByTestId("connection-auto-panel");
    expect(screen.getByRole("button", { name: "Check connection" })).toHaveAttribute("aria-expanded", "false");
  });

  it("shows the panel of the connection it was opened on, not another", async () => {
    stubConnections([
      connectionRow(),
      connectionRow({ id: OTHER_GRANT_ID, name: "Second client" }),
    ]);
    usageMock.mockImplementation(({ path }: { path: { grantId: string } }) =>
      ok(
        usageFixture({
          grant_id: path.grantId,
          draft_changes: { used: path.grantId === OTHER_GRANT_ID ? 7 : 14, limit: 600 },
        }),
      ),
    );
    renderPage();
    await screen.findByText("Second client");
    const buttons = screen.getAllByRole("button", { name: "Automatic changes" });
    expect(buttons).toHaveLength(2);
    fireEvent.click(buttons[1]!);

    const panel = await screen.findByTestId("connection-auto-panel");
    expect(panel.textContent).toContain("Automatic changes: Second client");
    expect((await screen.findByTestId("connection-draft-usage")).textContent).toContain("7 draft changes");
    expect(usageMock).toHaveBeenCalledWith({ path: { grantId: OTHER_GRANT_ID } });
    expect(usageMock).not.toHaveBeenCalledWith({ path: { grantId: GRANT_ID } });
  });
});
