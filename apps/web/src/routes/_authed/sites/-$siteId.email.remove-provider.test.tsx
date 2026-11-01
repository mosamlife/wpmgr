import { beforeEach, describe, expect, it, vi } from "vitest";
import { configure, fireEvent, screen, waitFor, within } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import type {
  EmailConnection,
  EmailProviderCatalog,
  RemoveSiteEmailConfigResult,
  SiteEmailConfig,
} from "@wpmgr/api";

import { createTestQueryClient, renderWithProviders } from "@/test/render";

import { Route as EmailRoute } from "./$siteId.email";

// GH #968: a site can remove its OWN email provider from the dashboard.
//
// Rendered through the real route, the real router and the real QueryClient,
// with the real hooks and the real page. Only the @wpmgr/api wire boundary is
// faked, and it is faked as a small server: `stored` is what GET
// /sites/{siteId}/email/config would answer, and a removal changes it, so the
// page is proven to show what the server holds after the removal rather than
// whatever it held before.
//
// Where each fact in the fakes comes from:
//  - The error body is {code, message}: apps/api/internal/server/httpx/respond.go.
//  - The removal's statuses, codes and sentences: RemoveSiteConfig in
//    apps/api/internal/email/service.go (404 email_site_config_not_found; 409
//    email_agent_outdated and email_config_changed; 503 email_agent_clear_failed,
//    email_agent_unwired and email_delete_site_config_failed; 403
//    email_site_config_delete_forbidden), with the response schema
//    RemoveSiteEmailConfigResult in packages/openapi/openapi.yaml.
//  - A site with neither a config of its own nor an organisation default gets
//    404 email_config_not_found from GET: GetConfig in the same service.go.
//    An inheriting site gets the organisation row with inherited: true.

configure({ asyncUtilTimeout: 4000 });

const {
  getConfigMock,
  listProvidersMock,
  listConnectionsMock,
  deleteConfigMock,
  syncMock,
  toastSuccess,
  toastWarning,
  toastError,
} = vi.hoisted(() => ({
  getConfigMock: vi.fn(),
  listProvidersMock: vi.fn(),
  listConnectionsMock: vi.fn(),
  deleteConfigMock: vi.fn(),
  syncMock: vi.fn(),
  toastSuccess: vi.fn(),
  toastWarning: vi.fn(),
  toastError: vi.fn(),
}));

vi.mock("@wpmgr/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@wpmgr/api")>()),
  getSiteEmailConfig: getConfigMock,
  listEmailProviders: listProvidersMock,
  listEmailConnections: listConnectionsMock,
  deleteSiteEmailConfig: deleteConfigMock,
  syncSiteEmailConfig: syncMock,
}));
vi.mock("@/components/toast", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/components/toast")>();
  return {
    ...actual,
    toast: { ...actual.toast, success: toastSuccess, warning: toastWarning, error: toastError },
  };
});

const SITE_ID = "11111111-1111-1111-1111-111111111111";

const PROVIDERS: EmailProviderCatalog = {
  providers: [
    {
      slug: "smtp",
      label: "Generic SMTP",
      fields: [
        { key: "host", label: "SMTP Host", type: "text", is_secret: false, is_required: true },
        { key: "password", label: "Password", type: "password", is_secret: true, is_required: false },
      ],
    },
    {
      slug: "ses",
      label: "Amazon SES",
      fields: [
        { key: "region", label: "AWS Region", type: "text", is_secret: false, is_required: true },
        { key: "secret_key", label: "Secret Access Key", type: "password", is_secret: true, is_required: true },
      ],
    },
    {
      slug: "sendgrid",
      label: "SendGrid",
      fields: [{ key: "api_key", label: "API Key", type: "password", is_secret: true, is_required: true }],
    },
  ],
};

function emailConfig(over: Partial<SiteEmailConfig> = {}): SiteEmailConfig {
  return {
    id: "cfg-site",
    tenant_id: "t1",
    site_id: SITE_ID,
    provider: "smtp",
    from_address: "site@example.test",
    from_name: "Site Sender",
    force_from_email: false,
    force_from_name: false,
    return_path: false,
    secret_set: true,
    log_emails: true,
    store_body: false,
    retention_days: 30,
    config: { host: "smtp.site.example.test" },
    mappings: {},
    created_at: "2026-10-01T00:00:00Z",
    updated_at: "2026-10-02T00:00:00Z",
    ...over,
  };
}

/** A named connection of the site's own provider, as the connections list returns it. */
function connection(over: Partial<EmailConnection> = {}): EmailConnection {
  return {
    id: "conn-backup",
    tenant_id: "t1",
    config_id: "cfg-site",
    connection_key: "backup",
    provider: "ses",
    from_address: "backup@example.test",
    from_name: "Backup Sender",
    secret_set: true,
    created_at: "2026-10-01T00:00:00Z",
    updated_at: "2026-10-02T00:00:00Z",
    ...over,
  };
}

/** The site's own provider, as GET returns it for a site that has one. */
const OWN = emailConfig();
/** The organisation default, as GET returns it for a site that has none of its own. */
const ORG_DEFAULT = emailConfig({
  id: "cfg-org",
  provider: "ses",
  from_address: "org@example.test",
  from_name: "Org Sender",
  config: { region: "eu-west-1" },
  inherited: true,
});

/** What GET /sites/{siteId}/email/config answers right now; null is the 404. */
let stored: SiteEmailConfig | null;

function ok(data: unknown) {
  return Promise.resolve({ data, error: undefined, response: { status: 200 } });
}
function fail(status: number, code: string, message: string) {
  return Promise.resolve({ data: undefined, error: { code, message }, response: { status } });
}
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

const REMOVED_ORG_DEFAULT: RemoveSiteEmailConfigResult = {
  removed_provider: "smtp",
  fallback: "org_default",
  org_default_synced: true,
  detail: "This site now uses the organisation's default email settings.",
};
const REMOVED_WORDPRESS: RemoveSiteEmailConfigResult = {
  removed_provider: "smtp",
  fallback: "wordpress",
  org_default_synced: false,
  detail: "This site now sends email through WordPress's own mailer.",
};
const REMOVED_UNSYNCED: RemoveSiteEmailConfigResult = {
  removed_provider: "smtp",
  fallback: "org_default",
  org_default_synced: false,
  detail:
    "This site now uses the organisation's default email settings, but they could not be sent to it yet. " +
    "Until they are it uses WordPress's own mailer; use Sync to site to try again.",
};
const REMOVED_UNKNOWN: RemoveSiteEmailConfigResult = {
  removed_provider: "smtp",
  fallback: "unknown",
  org_default_synced: false,
  detail:
    "This site's email provider was removed, but the organisation's default email settings could not be read. " +
    "Until they are sent to the site it uses WordPress's own mailer; reload the email settings and use Sync to site.",
};

/** The server accepts the removal, and from then on holds `after`. */
function removalSucceeds(result: RemoveSiteEmailConfigResult, after: SiteEmailConfig | null) {
  deleteConfigMock.mockImplementation(() => {
    stored = after;
    return ok(result);
  });
}

function renderEmailTab() {
  const queryClient = createTestQueryClient();
  const rootRoute = createRootRoute({});
  type UpdateOptions = Parameters<typeof EmailRoute.update>[0];
  const route = EmailRoute.update({
    id: "/sites/$siteId/email",
    path: "/sites/$siteId/email",
    getParentRoute: () => rootRoute,
  } as unknown as UpdateOptions);
  const router = createRouter({
    routeTree: rootRoute.addChildren([route]),
    history: createMemoryHistory({ initialEntries: [`/sites/${SITE_ID}/email`] }),
  });
  renderWithProviders(<RouterProvider router={router} />, { queryClient });
  return { queryClient, router };
}

const REMOVE = { name: "Remove provider" } as const;

// The two refusals that find the page out of date, in RemoveSiteConfig's words:
// the not-found message (apps/api/internal/email/service.go line 580) and
// removeChangedMessage (line 523).
const GONE = "This site has no email provider of its own to remove.";
const CHANGED = "The email settings changed while removing them; nothing was removed.";

/** Waits for the page, then opens the confirmation. */
async function openConfirmation() {
  fireEvent.click(await screen.findByRole("button", REMOVE));
  return screen.findByRole("dialog");
}

/** Types the provider's name into the open confirmation and confirms. */
function confirmRemoval(dialog: HTMLElement, typed = "Generic SMTP") {
  fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: typed } });
  fireEvent.click(within(dialog).getByRole("button", REMOVE));
}

beforeEach(() => {
  stored = OWN;
  getConfigMock.mockReset();
  getConfigMock.mockImplementation(() =>
    stored === null
      ? fail(404, "email_config_not_found", "no email config for this site or org")
      : ok(stored),
  );
  listProvidersMock.mockReset();
  listProvidersMock.mockImplementation(() => ok(PROVIDERS));
  listConnectionsMock.mockReset();
  listConnectionsMock.mockImplementation(() => ok({ connections: [] }));
  deleteConfigMock.mockReset();
  syncMock.mockReset();
  syncMock.mockImplementation(() => ok({ ok: true }));
  toastSuccess.mockReset();
  toastWarning.mockReset();
  toastError.mockReset();
});

describe("Remove provider is offered only for a provider the site owns", () => {
  it("is offered for a site that has a provider of its own", async () => {
    renderEmailTab();
    expect(await screen.findByRole("button", REMOVE)).toBeEnabled();
    expect(screen.getByRole("combobox", { name: "Provider" })).toHaveValue("smtp");
  });

  it("is not offered for a site that uses the organisation default", async () => {
    stored = ORG_DEFAULT;
    renderEmailTab();
    // The positive control: the page has loaded and says whose settings these are.
    expect(await screen.findByText(/These are your organisation.s email settings/)).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Provider" })).toHaveValue("ses");
    expect(screen.queryByRole("button", REMOVE)).toBeNull();
  });

  it("is not offered for a site that has no email configuration at all", async () => {
    stored = null;
    renderEmailTab();
    expect(await screen.findByText(/Email is not configured for this site yet/)).toBeInTheDocument();
    expect(screen.queryByRole("button", REMOVE)).toBeNull();
  });
});

describe("the confirmation", () => {
  it("says what will happen and sends nothing until the provider is named and confirmed", async () => {
    renderEmailTab();
    const dialog = await openConfirmation();

    // It names the provider and both places the site's mail can go afterwards.
    expect(within(dialog).getByRole("heading", { name: /Remove Generic SMTP from this site/ })).toBeInTheDocument();
    expect(dialog).toHaveTextContent(/organisation.s default email settings/);
    expect(dialog).toHaveTextContent(/WordPress.s own mailer/);
    // It says what is deleted, so nobody learns it afterwards.
    expect(dialog).toHaveTextContent(/credential/);
    expect(dialog).toHaveTextContent(/email log is kept/);

    const confirm = within(dialog).getByRole("button", REMOVE);
    expect(confirm).toBeDisabled();
    fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: "smtp" } });
    expect(confirm).toBeDisabled();
    expect(deleteConfigMock).not.toHaveBeenCalled();

    fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: "Generic SMTP" } });
    expect(confirm).toBeEnabled();
    expect(deleteConfigMock).not.toHaveBeenCalled();
  });

  it("names the provider the site has saved, not the one picked in the form and not yet saved", async () => {
    renderEmailTab();
    fireEvent.change(await screen.findByRole("combobox", { name: "Provider" }), { target: { value: "sendgrid" } });

    const dialog = await openConfirmation();

    expect(within(dialog).getByRole("heading", { name: /Remove Generic SMTP from this site/ })).toBeInTheDocument();
    expect(dialog).not.toHaveTextContent("SendGrid");
  });

  it("closes without sending anything when the operator keeps the provider", async () => {
    renderEmailTab();
    const dialog = await openConfirmation();

    fireEvent.click(within(dialog).getByRole("button", { name: "Keep provider" }));

    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(deleteConfigMock).not.toHaveBeenCalled();
    expect(screen.getByRole("button", REMOVE)).toBeInTheDocument();
  });

  it("sends the DELETE for this site once, and only on confirmation", async () => {
    removalSucceeds(REMOVED_ORG_DEFAULT, ORG_DEFAULT);
    renderEmailTab();
    const dialog = await openConfirmation();

    confirmRemoval(dialog);

    await waitFor(() => expect(deleteConfigMock).toHaveBeenCalledTimes(1));
    expect(deleteConfigMock).toHaveBeenCalledWith({ path: { siteId: SITE_ID } });
  });

  it("shows a spinner and cannot be dismissed or sent twice while the removal is in flight", async () => {
    const request = deferred<unknown>();
    deleteConfigMock.mockReturnValue(request.promise);
    renderEmailTab();
    const dialog = await openConfirmation();

    confirmRemoval(dialog);

    const pending = await within(dialog).findByRole("button", { name: "Removing…" });
    expect(pending).toBeDisabled();
    expect(within(dialog).getByRole("button", { name: "Keep provider" })).toBeDisabled();
    fireEvent.keyDown(dialog, { key: "Escape" });
    expect(dialog).toBeInTheDocument();
    expect(deleteConfigMock).toHaveBeenCalledTimes(1);

    stored = ORG_DEFAULT;
    request.resolve(await ok(REMOVED_ORG_DEFAULT));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });
});

describe("after a removal the page shows where the site's mail goes now", () => {
  it("org_default, delivered: the organisation's settings are shown as inherited and the action is gone", async () => {
    removalSucceeds(REMOVED_ORG_DEFAULT, ORG_DEFAULT);
    renderEmailTab();
    const dialog = await openConfirmation();

    confirmRemoval(dialog);

    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(await screen.findByText(/These are your organisation.s email settings/)).toBeInTheDocument();
    // The form carries the organisation's values, not the settings just removed.
    expect(screen.getByRole("combobox", { name: "Provider" })).toHaveValue("ses");
    expect(screen.getByLabelText("From address")).toHaveValue("org@example.test");
    expect(screen.getByLabelText("From name")).toHaveValue("Org Sender");
    expect(screen.queryByRole("button", REMOVE)).toBeNull();
    // Told what happened, in the server's own sentence, as a success.
    expect(toastSuccess).toHaveBeenCalledWith("Email provider removed", { description: REMOVED_ORG_DEFAULT.detail });
    expect(toastWarning).not.toHaveBeenCalled();
    expect(toastError).not.toHaveBeenCalled();
    // Nothing is left to do, so no follow-up is offered.
    expect(screen.queryByRole("status", { name: "Email provider removed" })).toBeNull();
  });

  it("wordpress: the page returns to its not-configured state with an empty form", async () => {
    removalSucceeds(REMOVED_WORDPRESS, null);
    renderEmailTab();
    const dialog = await openConfirmation();

    confirmRemoval(dialog);

    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(await screen.findByText(/Email is not configured for this site yet/)).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Provider" })).toHaveValue("");
    expect(screen.getByLabelText("From address")).toHaveValue("");
    expect(screen.getByLabelText("From name")).toHaveValue("");
    expect(screen.queryByRole("button", REMOVE)).toBeNull();
    expect(toastSuccess).toHaveBeenCalledWith("Email provider removed", { description: REMOVED_WORDPRESS.detail });
    // org_default_synced is false here too, but there is no organisation default to send.
    expect(screen.queryByRole("status", { name: "Email provider removed" })).toBeNull();
  });

  it("org_default, not delivered: says so and offers Sync to site, which clears once it works", async () => {
    removalSucceeds(REMOVED_UNSYNCED, ORG_DEFAULT);
    renderEmailTab();
    const dialog = await openConfirmation();

    confirmRemoval(dialog);

    const notice = await screen.findByRole("status", { name: "Email provider removed" });
    expect(notice).toHaveTextContent(REMOVED_UNSYNCED.detail);
    // Removed, but not finished: a warning, never the plain success.
    expect(toastWarning).toHaveBeenCalledWith("Email provider removed", { description: REMOVED_UNSYNCED.detail });
    expect(toastSuccess).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", REMOVE)).toBeNull();

    fireEvent.click(within(notice).getByRole("button", { name: "Sync to site" }));

    await waitFor(() => expect(syncMock).toHaveBeenCalledWith({ path: { siteId: SITE_ID } }));
    await waitFor(() => expect(screen.queryByRole("status", { name: "Email provider removed" })).toBeNull());
  });

  it("org_default, not delivered: the offer stays when the sync itself fails", async () => {
    removalSucceeds(REMOVED_UNSYNCED, ORG_DEFAULT);
    syncMock.mockImplementation(() => ok({ ok: false, detail: "The site did not answer in time." }));
    renderEmailTab();
    const dialog = await openConfirmation();
    confirmRemoval(dialog);

    const notice = await screen.findByRole("status", { name: "Email provider removed" });
    fireEvent.click(within(notice).getByRole("button", { name: "Sync to site" }));

    await waitFor(() => expect(syncMock).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(toastError).toHaveBeenCalled());
    expect(screen.getByRole("status", { name: "Email provider removed" })).toBeInTheDocument();
  });

  it("unknown: the removal is reported, the organisation's settings are read again, and Sync to site is offered", async () => {
    removalSucceeds(REMOVED_UNKNOWN, ORG_DEFAULT);
    renderEmailTab();
    const dialog = await openConfirmation();
    const readsBefore = getConfigMock.mock.calls.length;

    confirmRemoval(dialog);

    const notice = await screen.findByRole("status", { name: "Email provider removed" });
    expect(notice).toHaveTextContent(REMOVED_UNKNOWN.detail);
    expect(within(notice).getByRole("button", { name: "Sync to site" })).toBeEnabled();
    expect(toastWarning).toHaveBeenCalledWith("Email provider removed", { description: REMOVED_UNKNOWN.detail });
    expect(toastSuccess).not.toHaveBeenCalled();
    // The page asked the server again rather than trusting what it held before.
    expect(getConfigMock.mock.calls.length).toBeGreaterThan(readsBefore);
    expect(screen.getByRole("combobox", { name: "Provider" })).toHaveValue("ses");
  });

  it("unknown: no Sync to site is offered when, read again, there is no organisation default to send", async () => {
    removalSucceeds(REMOVED_UNKNOWN, null);
    renderEmailTab();
    const dialog = await openConfirmation();

    confirmRemoval(dialog);

    expect(await screen.findByText(/Email is not configured for this site yet/)).toBeInTheDocument();
    expect(screen.queryByRole("status", { name: "Email provider removed" })).toBeNull();
    expect(toastWarning).toHaveBeenCalledTimes(1);
  });
});

describe("a refused removal shows the server's message and never a success", () => {
  // Statuses, codes and sentences are RemoveSiteConfig's, in
  // apps/api/internal/email/service.go. The last two reasons in the 503s are
  // DescribeAttemptError's, in apps/api/internal/agentcmd/command_error.go.
  const REFUSALS: Array<[string, number, string, string]> = [
    [
      "the agent is too old for the command",
      409,
      "email_agent_outdated",
      "Update the WPMgr agent on this site to remove its email provider.",
    ],
    [
      "the site did not confirm it stopped",
      503,
      "email_agent_clear_failed",
      "The site did not confirm it stopped using its email provider, so nothing was removed: The site did not answer in time. Try again.",
    ],
    [
      "agent commands are not configured",
      503,
      "email_agent_unwired",
      "The agent command client is not configured, so the site cannot be told to stop using its email provider. Nothing was removed.",
    ],
    [
      "the stored settings could not be deleted",
      503,
      "email_delete_site_config_failed",
      "The site stopped using its email provider, but its stored settings could not be removed. Remove the email provider again to finish.",
    ],
    [
      "the operator may not delete the stored settings",
      403,
      "email_site_config_delete_forbidden",
      "The site stopped using its email provider, but you cannot remove its stored settings. " +
        "Ask an organisation member to remove them, or use Sync to site to restore them.",
    ],
  ];

  it.each(REFUSALS)("%s (%i %s)", async (_name, status, code, message) => {
    deleteConfigMock.mockImplementation(() => fail(status, code, message));
    renderEmailTab();
    const dialog = await openConfirmation();

    confirmRemoval(dialog);

    expect(await within(dialog).findByRole("alert")).toHaveTextContent(message);
    // Still open, still the same question, ready for another try.
    expect(dialog).toBeInTheDocument();
    expect(within(dialog).getByRole("textbox")).toHaveValue("Generic SMTP");
    expect(within(dialog).getByRole("button", REMOVE)).toBeEnabled();
    // No success of any kind was announced.
    expect(toastSuccess).not.toHaveBeenCalled();
    expect(toastWarning).not.toHaveBeenCalled();

    // And the page behind it still shows the provider as the site's own.
    fireEvent.click(within(dialog).getByRole("button", { name: "Keep provider" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.getByRole("button", REMOVE)).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Provider" })).toHaveValue("smtp");
    expect(screen.queryByRole("status", { name: "Email provider removed" })).toBeNull();
  });

  it("lets the operator try again after the stored settings could not be deleted, and finishes the removal", async () => {
    deleteConfigMock.mockImplementationOnce(() =>
      fail(
        503,
        "email_delete_site_config_failed",
        "The site stopped using its email provider, but its stored settings could not be removed. Remove the email provider again to finish.",
      ),
    );
    removalSucceeds(REMOVED_WORDPRESS, null);
    renderEmailTab();
    const dialog = await openConfirmation();
    confirmRemoval(dialog);
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(/Remove the email provider again to finish/);

    fireEvent.click(within(dialog).getByRole("button", REMOVE));

    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(deleteConfigMock).toHaveBeenCalledTimes(2);
    expect(await screen.findByText(/Email is not configured for this site yet/)).toBeInTheDocument();
    expect(toastSuccess).toHaveBeenCalledTimes(1);
  });

  it("does not carry an old refusal into the next time the confirmation opens", async () => {
    deleteConfigMock.mockImplementation(() =>
      fail(409, "email_agent_outdated", "Update the WPMgr agent on this site to remove its email provider."),
    );
    renderEmailTab();
    const first = await openConfirmation();
    confirmRemoval(first);
    await within(first).findByRole("alert");
    fireEvent.click(within(first).getByRole("button", { name: "Keep provider" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

    const second = await openConfirmation();

    expect(within(second).queryByRole("alert")).toBeNull();
    expect(within(second).getByRole("textbox")).toHaveValue("");
  });
});

describe("a refusal that finds the page out of date also brings the page up to date", () => {
  it("404 email_site_config_not_found: the provider was already removed elsewhere", async () => {
    // Someone else removed it first; the server now holds only the organisation default.
    stored = OWN;
    deleteConfigMock.mockImplementation(() => {
      stored = ORG_DEFAULT;
      return fail(404, "email_site_config_not_found", GONE);
    });
    renderEmailTab();
    const dialog = await openConfirmation();

    confirmRemoval(dialog);

    // The question the confirmation asked is over, so it closes by itself, and
    // the server's sentence is on the page to say why.
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(await screen.findByRole("alert")).toHaveTextContent(GONE);
    expect(toastSuccess).not.toHaveBeenCalled();
    expect(await screen.findByText(/These are your organisation.s email settings/)).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Provider" })).toHaveValue("ses");
    expect(screen.queryByRole("button", REMOVE)).toBeNull();
  });

  it("409 email_config_changed: the settings saved meanwhile are what the page shows", async () => {
    deleteConfigMock.mockImplementation(() => {
      stored = emailConfig({ from_address: "changed@example.test", updated_at: "2026-10-03T00:00:00Z" });
      return fail(409, "email_config_changed", CHANGED);
    });
    renderEmailTab();
    const dialog = await openConfirmation();

    confirmRemoval(dialog);

    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(await screen.findByRole("alert")).toHaveTextContent(CHANGED);
    expect(toastSuccess).not.toHaveBeenCalled();
    await waitFor(() => expect(screen.getByLabelText("From address")).toHaveValue("changed@example.test"));
    // It is still the site's own provider, so it can still be removed.
    expect(screen.getByRole("button", REMOVE)).toBeInTheDocument();
  });

  it("409 email_config_changed after the provider was switched: the next confirmation names the new provider and starts empty", async () => {
    deleteConfigMock.mockImplementation(() => {
      // Someone else switched the site to SendGrid while this confirmation was open.
      stored = emailConfig({ provider: "sendgrid", config: {}, updated_at: "2026-10-03T00:00:00Z" });
      return fail(409, "email_config_changed", CHANGED);
    });
    renderEmailTab();
    const first = await openConfirmation();

    confirmRemoval(first);

    // The question was about Generic SMTP, which the site no longer has, so it
    // is closed rather than left standing with its name and its typed answer.
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(await screen.findByRole("alert")).toHaveTextContent(CHANGED);
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Provider" })).toHaveValue("sendgrid"));
    expect(deleteConfigMock).toHaveBeenCalledTimes(1);

    // A new removal opens a new question, about the provider the site has now.
    const second = await openConfirmation();
    expect(within(second).getByRole("heading", { name: /Remove SendGrid from this site/ })).toBeInTheDocument();
    expect(second).not.toHaveTextContent("Generic SMTP");
    expect(within(second).getByRole("textbox")).toHaveValue("");
    expect(within(second).queryByRole("alert")).toBeNull();
    // The earlier refusal is spent: nothing of it greets the new question.
    expect(screen.queryByText(CHANGED)).toBeNull();
    // The name typed for the old provider does not remove the new one.
    fireEvent.change(within(second).getByRole("textbox"), { target: { value: "Generic SMTP" } });
    expect(within(second).getByRole("button", REMOVE)).toBeDisabled();
    expect(deleteConfigMock).toHaveBeenCalledTimes(1);
  });
});

describe("a refusal that finds the page out of date also drops the connections that went with the provider", () => {
  // A site's connections are rows of its own provider's config and go with it:
  // config_id REFERENCES site_email_config (id) ON DELETE CASCADE, at line 26 of
  // apps/api/migrations/20260625000000_m62_email_v1_completion.sql.
  const DELETE_BACKUP = { name: "Delete connection backup" } as const;

  const STALE: Array<[string, number, string, string, SiteEmailConfig]> = [
    ["404 email_site_config_not_found", 404, "email_site_config_not_found", GONE, ORG_DEFAULT],
    [
      "409 email_config_changed",
      409,
      "email_config_changed",
      CHANGED,
      emailConfig({ from_address: "changed@example.test", updated_at: "2026-10-03T00:00:00Z" }),
    ],
  ];

  it.each(STALE)("%s: the Routing tab reads the connections again instead of showing the cached list", async (_name, status, code, message, after) => {
    // `connections` is what GET /sites/{siteId}/email/connections answers right now.
    let connections = [connection()];
    listConnectionsMock.mockImplementation(() => ok({ connections }));
    deleteConfigMock.mockImplementation(() => {
      // The settings were changed or removed, and the connections with them,
      // before this request landed.
      stored = after;
      connections = [];
      return fail(status, code, message);
    });
    renderEmailTab();

    // Routing is opened once so its connections are cached, then left again.
    fireEvent.click(await screen.findByRole("tab", { name: "Routing" }));
    expect(await screen.findByRole("button", DELETE_BACKUP)).toBeInTheDocument();
    expect(listConnectionsMock).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("tab", { name: "Provider" }));

    confirmRemoval(await openConfirmation());
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(await screen.findByRole("alert")).toHaveTextContent(message);

    // Routing now shows what the server holds. A cached list is fresh for a
    // minute, so only a refresh by the refusal can have replaced it.
    fireEvent.click(screen.getByRole("tab", { name: "Routing" }));
    expect(await screen.findByText(/No named connections yet/)).toBeInTheDocument();
    expect(screen.queryByRole("button", DELETE_BACKUP)).toBeNull();
    expect(listConnectionsMock).toHaveBeenCalledTimes(2);
  });
});
