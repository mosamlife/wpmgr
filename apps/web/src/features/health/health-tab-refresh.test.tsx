import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  act,
  fireEvent,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import type { SiteDiagnosticsCard } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";

import { HealthTab } from "./health-tab";

// GH #1005, dashboard half. The Diagnostics "Re-run all checks" button against
// the control plane's new answers for POST /api/v1/sites/{siteId}/diagnostics/refresh.
//
// HealthTab is rendered through the router and a QueryClient (renderWithProviders
// with withRouter). Only the generated operations and the toast module are
// mocked, so the real hook (use-diagnostics.ts), the real cache and the real
// component run, and the arguments checked below are exactly what the app sends.
//
// Every status, code and message below was found in the source, not assumed:
//   - 202: the handler's c.Status(http.StatusAccepted) after the report is stored
//     (apps/api/internal/diagnostics/handler.go, refresh).
//   - 501 diagnostics_refresh_unwired: handler.go:114 builds domain.Unavailable,
//     which domain.HTTPStatus maps to http.StatusNotImplemented
//     (apps/api/internal/domain/errors.go:193-194). Pinned by
//     TestRefreshWithoutACommanderAnswers501 in
//     apps/api/internal/diagnostics/refresh_site_failure_test.go. The message is
//     the one handler.go:114 sends.
//   - 502 and its seven codes: domain.BadGateway(f.Code, f.Message) in
//     apps/api/internal/diagnostics/refresh_enqueuer.go (siteFailure); the codes
//     are the constants in apps/api/internal/agentcmd/site_failure.go.
//   - The 502 messages are composed as the Go code composes them:
//     "<action> not started. " + forbiddenFirewallAdvice
//     (agentcmd/command_error.go:501-502, 539), "<action> failed. " +
//     DescribeAttemptError (command_error.go:432-476), agentFailedMessage
//     (site_failure.go) and RedirectError.Explanation (agentcmd/redirect.go),
//     with action "Diagnostics check" (diagnostics/refresh_enqueuer.go).
//   - 422 collected_at_missing and its message: agentCollectedAt in
//     apps/api/internal/diagnostics/service.go.
//   - The body is {code, message} (errorEnvelope in
//     apps/api/internal/server/httpx/respond.go), and the generated client hands
//     back the parsed body as `error`, or `{}` when the body is empty
//     (packages/openapi-client/src/generated/client/client.gen.ts, around 204-238).

const SITE_ID = "00000000-0000-0000-0000-0000000000a1";

const SITE_REFUSED_MESSAGE =
  "Diagnostics check not started. A firewall or security rule blocked the request (HTTP 403) before it reached the WPMgr agent. " +
  "Ask the site's host to allow requests to /wp-json/wpmgr/v1/ from WPMgr, and allow them in any firewall service or security plugin the site uses.";

const SITE_TIMEOUT_MESSAGE =
  "Diagnostics check failed. The site did not answer in time.";

// One realistic message per code the 502 can carry.
const SITE_FAILURES: Array<[code: string, message: string]> = [
  ["site_refused", SITE_REFUSED_MESSAGE],
  ["site_timeout", SITE_TIMEOUT_MESSAGE],
  [
    "site_unreachable",
    "Diagnostics check failed. Could not connect to the site.",
  ],
  [
    "site_url_redirects",
    "Diagnostics check not started. https://example.com answered with a redirect (HTTP 302) that names no usable address, so no command was sent. " +
      "Exempt /wp-json/wpmgr/ from the redirect on the site or its CDN.",
  ],
  [
    "agent_command_failed",
    "Diagnostics check failed: the WPMgr agent on this site stopped with an error while running it. " +
      "Check the site's PHP error log for the cause, fix it, then run the diagnostics check again.",
  ],
  [
    "site_error",
    "Diagnostics check failed. The site returned a server error (HTTP 500).",
  ],
  [
    "site_bad_reply",
    "Diagnostics check failed. The site answered, but the reply was not the WPMgr agent's response. " +
      "A PHP notice or another plugin's output may be in the way.",
  ],
];

// What handler.go:114 sends for an install without the command client.
const UNWIRED_SERVER_MESSAGE = "on-demand diagnostics refresh is not yet wired";

const {
  getSiteDiagnostics,
  refreshSiteDiagnostics,
  getSite,
  toastSuccess,
  toastError,
} = vi.hoisted(() => ({
  getSiteDiagnostics: vi.fn<(options: unknown) => Promise<unknown>>(),
  refreshSiteDiagnostics: vi.fn<(options: unknown) => Promise<unknown>>(),
  getSite: vi.fn<(options: unknown) => Promise<unknown>>(),
  toastSuccess: vi.fn(),
  toastError: vi.fn(),
}));

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return { ...actual, getSiteDiagnostics, refreshSiteDiagnostics, getSite };
});

vi.mock("@/components/toast", () => ({
  toast: {
    success: toastSuccess,
    error: toastError,
    warning: vi.fn(),
    info: vi.fn(),
    destructive: vi.fn(),
    dismiss: vi.fn(),
  },
}));

// The report the fake control plane holds. A 202 stores a new one first, the way
// the real route does before it answers.
let storedReport: SiteDiagnosticsCard[];

function reportWithWpVersion(wpVersion: string): SiteDiagnosticsCard[] {
  return [
    {
      category: "identity",
      payload: { wp_version: wpVersion },
      collected_at: "2026-10-30T10:00:00Z",
      fresh: true,
    },
  ];
}

/** What a generated operation resolves to for a 200. */
function ok(data: unknown) {
  return {
    data,
    error: undefined,
    response: new Response(null, { status: 200 }),
  };
}

/** What the refresh resolves to for a 202: no body. */
function accepted() {
  return {
    data: undefined,
    error: undefined,
    response: new Response(null, { status: 202 }),
  };
}

/** What a generated operation resolves to for an error status: the body is `error`. */
function failed(status: number, error: unknown) {
  return { data: undefined, error, response: new Response(null, { status }) };
}

function siteFailure(status: number, code: string, message: string) {
  return failed(status, { code, message });
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

function renderHealth() {
  return renderWithProviders(<HealthTab siteId={SITE_ID} />, {
    withRouter: true,
  });
}

/** The router resolves its first match on a microtask, so the button is awaited. */
async function rerunButton() {
  return screen.findByRole("button", { name: "Re-run all checks" });
}

beforeEach(() => {
  vi.resetAllMocks();
  storedReport = reportWithWpVersion("6.6.1");
  getSiteDiagnostics.mockImplementation(() =>
    Promise.resolve(ok({ items: storedReport })),
  );
  getSite.mockImplementation(() =>
    Promise.resolve(ok({ id: SITE_ID, url: "https://example.com" })),
  );
});

afterEach(() => {
  vi.useRealTimers();
});

describe("Re-run all checks, 202", () => {
  it("says the checks ran and shows the new report the site just stored", async () => {
    refreshSiteDiagnostics.mockImplementation(() => {
      storedReport = reportWithWpVersion("6.7.2");
      return Promise.resolve(accepted());
    });
    renderHealth();
    // Before the click the page shows the old report.
    expect(await screen.findByText("6.6.1")).toBeInTheDocument();

    fireEvent.click(await rerunButton());

    await waitFor(() => expect(toastSuccess).toHaveBeenCalledTimes(1));
    expect(toastSuccess).toHaveBeenCalledWith("All checks ran", {
      description: "The results below are up to date.",
    });
    // The report on screen is the new one, so the diagnostics were refetched. A
    // page that only toasted would still show 6.6.1.
    expect(screen.getByText("6.7.2")).toBeInTheDocument();
    expect(screen.queryByText("6.6.1")).not.toBeInTheDocument();
    expect(getSiteDiagnostics).toHaveBeenCalledTimes(2);
    expect(refreshSiteDiagnostics).toHaveBeenCalledTimes(1);
    expect(refreshSiteDiagnostics).toHaveBeenCalledWith({
      path: { siteId: SITE_ID },
    });
    // Nothing was reported as queued or failed.
    expect(toastError).not.toHaveBeenCalled();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("does not announce success before the new report has loaded", async () => {
    // The refetch is held open: the toast must wait for it, because it says the
    // results below are up to date.
    const refetch = deferred<unknown>();
    refreshSiteDiagnostics.mockImplementation(() => {
      storedReport = reportWithWpVersion("6.7.2");
      getSiteDiagnostics.mockImplementation(() => refetch.promise);
      return Promise.resolve(accepted());
    });
    renderHealth();
    expect(await screen.findByText("6.6.1")).toBeInTheDocument();

    fireEvent.click(await rerunButton());
    await waitFor(() => expect(getSiteDiagnostics).toHaveBeenCalledTimes(2));
    expect(toastSuccess).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: /Running checks/ })).toBeDisabled();

    await act(async () => {
      refetch.resolve(ok({ items: storedReport }));
    });
    await waitFor(() => expect(toastSuccess).toHaveBeenCalledTimes(1));
    expect(screen.getByText("6.7.2")).toBeInTheDocument();
  });
});

describe("Re-run all checks, 502 from the site", () => {
  it("site_refused shows the server's message exactly as given, in an alert, and keeps it", async () => {
    refreshSiteDiagnostics.mockResolvedValue(
      siteFailure(502, "site_refused", SITE_REFUSED_MESSAGE),
    );
    renderHealth();

    const button = await rerunButton();
    fireEvent.click(button);

    const alert = await screen.findByRole("alert");
    // The whole sentence, in one element, word for word.
    expect(within(alert).getByText(SITE_REFUSED_MESSAGE)).toBeInTheDocument();
    expect(alert).not.toHaveTextContent(/something went wrong|request failed/i);
    // Not a toast: a toast would be gone before the remedy was read.
    expect(toastError).not.toHaveBeenCalled();
    expect(toastSuccess).not.toHaveBeenCalled();

    // The attempt is over and the button is usable again, yet the alert stays.
    await waitFor(() => expect(button).toBeEnabled());
    expect(screen.getByRole("alert")).toBeInTheDocument();
    expect(screen.getByText(SITE_REFUSED_MESSAGE)).toBeInTheDocument();
    // The old report is still shown; nothing was stored.
    expect(screen.getByText("6.6.1")).toBeInTheDocument();
  });

  it("keeps the alert on screen long after a four second toast would have gone", async () => {
    // Fake timers go in before the click, so a timer started when the failure
    // arrives is one this test can run out.
    vi.useFakeTimers({ shouldAdvanceTime: true });
    refreshSiteDiagnostics.mockResolvedValue(
      siteFailure(502, "site_refused", SITE_REFUSED_MESSAGE),
    );
    renderHealth();

    fireEvent.click(await rerunButton());
    expect(await screen.findByText(SITE_REFUSED_MESSAGE)).toBeInTheDocument();

    await act(async () => {
      vi.advanceTimersByTime(60_000);
    });

    expect(screen.getByRole("alert")).toBeInTheDocument();
    expect(screen.getByText(SITE_REFUSED_MESSAGE)).toBeInTheDocument();
  });

  it("site_timeout shows its own message, and none of another code's", async () => {
    refreshSiteDiagnostics.mockResolvedValue(
      siteFailure(502, "site_timeout", SITE_TIMEOUT_MESSAGE),
    );
    renderHealth();

    fireEvent.click(await rerunButton());

    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText(SITE_TIMEOUT_MESSAGE)).toBeInTheDocument();
    expect(alert).not.toHaveTextContent("firewall");
    expect(toastError).not.toHaveBeenCalled();
  });

  it.each(SITE_FAILURES)(
    "%s shows the server's message as given",
    async (code, message) => {
      refreshSiteDiagnostics.mockResolvedValue(siteFailure(502, code, message));
      renderHealth();

      fireEvent.click(await rerunButton());

      const alert = await screen.findByRole("alert");
      expect(within(alert).getByText(message)).toBeInTheDocument();
      expect(toastError).not.toHaveBeenCalled();
    },
  );

  it("is dismissed by the operator, without asking the site again", async () => {
    refreshSiteDiagnostics.mockResolvedValue(
      siteFailure(502, "site_refused", SITE_REFUSED_MESSAGE),
    );
    renderHealth();
    fireEvent.click(await rerunButton());
    expect(await screen.findByRole("alert")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Dismiss this message" }));

    // The mutation resets on the query client's next notification batch.
    await waitFor(() =>
      expect(screen.queryByRole("alert")).not.toBeInTheDocument(),
    );
    expect(screen.queryByText(SITE_REFUSED_MESSAGE)).not.toBeInTheDocument();
    expect(refreshSiteDiagnostics).toHaveBeenCalledTimes(1);
  });

  it("is cleared when the next attempt starts, then shows that attempt's own answer", async () => {
    refreshSiteDiagnostics.mockResolvedValueOnce(
      siteFailure(502, "site_refused", SITE_REFUSED_MESSAGE),
    );
    const second = deferred<unknown>();
    refreshSiteDiagnostics.mockReturnValueOnce(second.promise);
    renderHealth();

    const button = await rerunButton();
    fireEvent.click(button);
    expect(await screen.findByText(SITE_REFUSED_MESSAGE)).toBeInTheDocument();
    await waitFor(() => expect(button).toBeEnabled());

    fireEvent.click(button);

    // While the second attempt runs the first answer is gone.
    expect(
      await screen.findByRole("button", { name: /Running checks/ }),
    ).toBeDisabled();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.queryByText(SITE_REFUSED_MESSAGE)).not.toBeInTheDocument();

    await act(async () => {
      second.resolve(siteFailure(502, "site_timeout", SITE_TIMEOUT_MESSAGE));
    });

    expect(await screen.findByText(SITE_TIMEOUT_MESSAGE)).toBeInTheDocument();
    expect(screen.queryByText(SITE_REFUSED_MESSAGE)).not.toBeInTheDocument();
    expect(refreshSiteDiagnostics).toHaveBeenCalledTimes(2);
  });

  it("does not show a proxy's own 502 page as if it were the control plane's message", async () => {
    // A gateway in front of the control plane answers 502 with HTML. The client
    // hands that text back as `error`; it has no `message` and is not for an
    // operator.
    refreshSiteDiagnostics.mockResolvedValue(
      failed(502, "<html><body>502 Bad Gateway</body></html>"),
    );
    renderHealth();

    fireEvent.click(await rerunButton());

    await waitFor(() => expect(toastError).toHaveBeenCalledTimes(1));
    expect(toastError).toHaveBeenCalledWith("Could not re-run the checks", {
      description: "Request failed",
    });
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.queryByText(/Bad Gateway/)).not.toBeInTheDocument();
  });
});

describe("Re-run all checks, 501 from an install without the command client", () => {
  it("shows the unwired state in plain words, keyed on the code", async () => {
    refreshSiteDiagnostics.mockResolvedValue(
      siteFailure(501, "diagnostics_refresh_unwired", UNWIRED_SERVER_MESSAGE),
    );
    renderHealth();

    fireEvent.click(await rerunButton());

    await waitFor(() => expect(toastError).toHaveBeenCalledTimes(1));
    expect(toastError).toHaveBeenCalledWith("Could not re-run the checks", {
      description:
        "This control plane is not set up to send commands to sites, so checks cannot be run on demand. " +
        "The diagnostics on this page still update when the site's agent sends its daily report.",
    });
    // The server's wording is for whoever deploys it, not for this reader.
    expect(JSON.stringify(toastError.mock.calls)).not.toContain(
      UNWIRED_SERVER_MESSAGE,
    );
    expect(toastSuccess).not.toHaveBeenCalled();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("does not take a 503 for the unwired state", async () => {
    // A gateway's own 503 has no body. The old code read any 503 as "not wired".
    refreshSiteDiagnostics.mockResolvedValue(failed(503, {}));
    renderHealth();

    fireEvent.click(await rerunButton());

    await waitFor(() => expect(toastError).toHaveBeenCalledTimes(1));
    expect(toastError).toHaveBeenCalledWith("Could not re-run the checks", {
      description: "Request failed",
    });
  });
});

describe("Re-run all checks, other failures keep the generic handling", () => {
  it("a 422 shows the server's message in a toast, not in the alert", async () => {
    const message =
      "The diagnostics report has no collection time (collected_at), so it was not saved.";
    refreshSiteDiagnostics.mockResolvedValue(
      siteFailure(422, "collected_at_missing", message),
    );
    renderHealth();

    fireEvent.click(await rerunButton());

    await waitFor(() => expect(toastError).toHaveBeenCalledTimes(1));
    expect(toastError).toHaveBeenCalledWith("Could not re-run the checks", {
      description: message,
    });
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });
});
