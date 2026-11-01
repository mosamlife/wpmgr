import { createElement, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { QueryClientProvider, type QueryClient } from "@tanstack/react-query";
import * as api from "@wpmgr/api";
import { client, type RemoveSiteEmailConfigResult } from "@wpmgr/api";

import { createTestQueryClient } from "@/test/render";

import {
  EmailConfigStaleError,
  emailKeys,
  removalNeedsSync,
  useRemoveSiteEmailConfig,
} from "./use-email";

// GH #968: the hook that removes a site's own email provider.
//
// Unlike the route test, nothing in @wpmgr/api is replaced here. The real
// facade, the real generated operation and the real client run, and only
// fetch is faked, so what is proven is the request that would leave the
// browser (method and path) and how a real error body arrives in the hook.
// The error body is {code, message}: apps/api/internal/server/httpx/respond.go.

const { toastSuccess, toastWarning, toastError } = vi.hoisted(() => ({
  toastSuccess: vi.fn(),
  toastWarning: vi.fn(),
  toastError: vi.fn(),
}));
vi.mock("@/components/toast", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/components/toast")>();
  return {
    ...actual,
    toast: { ...actual.toast, success: toastSuccess, warning: toastWarning, error: toastError },
  };
});

const SITE_ID = "11111111-1111-1111-1111-111111111111";
const OTHER_SITE_ID = "22222222-2222-2222-2222-222222222222";

const REMOVED: RemoveSiteEmailConfigResult = {
  removed_provider: "smtp",
  fallback: "org_default",
  org_default_synced: true,
  detail: "This site now uses the organisation's default email settings.",
};

let seen: Request[];
let answer: (request: Request) => Promise<Response>;

function replyJson(status: number, body: unknown) {
  answer = () =>
    Promise.resolve(
      new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } }),
    );
}

beforeEach(() => {
  seen = [];
  toastSuccess.mockReset();
  toastWarning.mockReset();
  toastError.mockReset();
  // The app runs same-origin with an empty baseUrl; a relative URL cannot be
  // built into a Request outside a browser, so the test gives it an origin.
  client.setConfig({
    baseUrl: "http://wpmgr.test",
    fetch: (input: RequestInfo | URL, init?: RequestInit) => {
      // The generated client always hands fetch a Request.
      const request = input instanceof Request ? input : new Request(input, init);
      seen.push(request);
      return answer(request);
    },
  });
});

afterEach(() => {
  client.setConfig({ baseUrl: "", fetch: undefined });
});

function setup() {
  const queryClient = createTestQueryClient();
  const wrapper = ({ children }: { children: ReactNode }) =>
    createElement(QueryClientProvider, { client: queryClient }, children);
  const hook = renderHook(() => useRemoveSiteEmailConfig(SITE_ID), { wrapper });
  return { queryClient, hook };
}

function isInvalidated(queryClient: QueryClient, key: readonly unknown[]): boolean {
  return queryClient.getQueryState(key)?.isInvalidated === true;
}

/** Gives the caches the removal may touch something to invalidate. */
function seed(queryClient: QueryClient) {
  for (const key of [
    emailKeys.siteConfig(SITE_ID),
    emailKeys.connections(SITE_ID),
    emailKeys.siteConfig(OTHER_SITE_ID),
    emailKeys.orgConfig(),
  ]) {
    queryClient.setQueryData(key, { seeded: true });
  }
}

describe("the facade", () => {
  it("exposes the operation that removes a site's email provider", () => {
    expect(typeof api.deleteSiteEmailConfig).toBe("function");
  });
});

describe("useRemoveSiteEmailConfig", () => {
  it("sends DELETE /api/v1/sites/{siteId}/email/config and resolves with the outcome", async () => {
    replyJson(200, REMOVED);
    const { hook } = setup();

    let outcome: RemoveSiteEmailConfigResult | undefined;
    await act(async () => {
      outcome = await hook.result.current.mutateAsync();
    });

    expect(outcome).toEqual(REMOVED);
    expect(seen).toHaveLength(1);
    expect(seen[0]?.method).toBe("DELETE");
    expect(new URL(seen[0]?.url ?? "").pathname).toBe(`/api/v1/sites/${SITE_ID}/email/config`);
  });

  it("reports the removal in the server's own sentence, as a success when nothing is left to do", async () => {
    replyJson(200, REMOVED);
    const { hook } = setup();

    await act(async () => {
      await hook.result.current.mutateAsync();
    });

    expect(toastSuccess).toHaveBeenCalledWith("Email provider removed", { description: REMOVED.detail });
    expect(toastWarning).not.toHaveBeenCalled();
  });

  it("reports the removal as a warning when the organisation default did not reach the site", async () => {
    const unsynced = { ...REMOVED, org_default_synced: false, detail: "Not sent yet." };
    replyJson(200, unsynced);
    const { hook } = setup();

    await act(async () => {
      await hook.result.current.mutateAsync();
    });

    expect(toastWarning).toHaveBeenCalledWith("Email provider removed", { description: "Not sent yet." });
    expect(toastSuccess).not.toHaveBeenCalled();
  });

  it("invalidates this site's email config and connections, and nothing else", async () => {
    replyJson(200, REMOVED);
    const { queryClient, hook } = setup();
    seed(queryClient);

    await act(async () => {
      await hook.result.current.mutateAsync();
    });

    expect(isInvalidated(queryClient, emailKeys.siteConfig(SITE_ID))).toBe(true);
    expect(isInvalidated(queryClient, emailKeys.connections(SITE_ID))).toBe(true);
    // The organisation default is never touched by a removal, and neither is another site.
    expect(isInvalidated(queryClient, emailKeys.orgConfig())).toBe(false);
    expect(isInvalidated(queryClient, emailKeys.siteConfig(OTHER_SITE_ID))).toBe(false);
  });

  // Statuses, codes and sentences: RemoveSiteConfig in apps/api/internal/email/service.go.
  const STALE: Array<[number, string, string]> = [
    [404, "email_site_config_not_found", "This site has no email provider of its own to remove."],
    [409, "email_config_changed", "The email settings changed while removing them; nothing was removed."],
  ];
  const REFUSED: Array<[number, string, string]> = [
    [409, "email_agent_outdated", "Update the WPMgr agent on this site to remove its email provider."],
    [503, "email_agent_clear_failed", "The site did not confirm it stopped using its email provider, so nothing was removed: The site did not answer in time. Try again."],
    [503, "email_agent_unwired", "The agent command client is not configured, so the site cannot be told to stop using its email provider. Nothing was removed."],
    [503, "email_delete_site_config_failed", "The site stopped using its email provider, but its stored settings could not be removed. Remove the email provider again to finish."],
    [403, "email_site_config_delete_forbidden", "The site stopped using its email provider, but you cannot remove its stored settings. Ask an organisation member to remove them, or use Sync to site to restore them."],
  ];

  it.each([...STALE, ...REFUSED])("%i %s rejects with the server's message and announces no success", async (status, code, message) => {
    replyJson(status, { code, message });
    const { hook } = setup();

    await act(async () => {
      await expect(hook.result.current.mutateAsync()).rejects.toThrow(message);
    });

    expect(toastSuccess).not.toHaveBeenCalled();
    expect(toastWarning).not.toHaveBeenCalled();
  });

  it.each(STALE)("%i %s is told apart as a page that is out of date, and its cache is invalidated", async (status, code, message) => {
    replyJson(status, { code, message });
    const { queryClient, hook } = setup();
    seed(queryClient);

    await act(async () => {
      await expect(hook.result.current.mutateAsync()).rejects.toBeInstanceOf(EmailConfigStaleError);
    });

    expect(isInvalidated(queryClient, emailKeys.siteConfig(SITE_ID))).toBe(true);
    // The connections are rows of the config and went with it
    // (ON DELETE CASCADE, apps/api/migrations/20260625000000_m62_email_v1_completion.sql
    // line 26), so a list cached before the refusal is out of date as well.
    expect(isInvalidated(queryClient, emailKeys.connections(SITE_ID))).toBe(true);
    // Nothing of the organisation or of another site is touched.
    expect(isInvalidated(queryClient, emailKeys.orgConfig())).toBe(false);
    expect(isInvalidated(queryClient, emailKeys.siteConfig(OTHER_SITE_ID))).toBe(false);
  });

  it.each(REFUSED)("%i %s changes nothing on the server, so it is not an out-of-date page and nothing is refetched", async (status, code, message) => {
    replyJson(status, { code, message });
    const { queryClient, hook } = setup();
    seed(queryClient);

    await act(async () => {
      const failure: unknown = await hook.result.current.mutateAsync().catch((e: unknown) => e);
      expect(failure).toBeInstanceOf(Error);
      expect(failure).not.toBeInstanceOf(EmailConfigStaleError);
    });

    expect(isInvalidated(queryClient, emailKeys.siteConfig(SITE_ID))).toBe(false);
    expect(isInvalidated(queryClient, emailKeys.connections(SITE_ID))).toBe(false);
  });

  it("does not mistake another 409 for a changed config", async () => {
    replyJson(409, { code: "something_else", message: "Another conflict." });
    const { hook } = setup();

    await act(async () => {
      const failure: unknown = await hook.result.current.mutateAsync().catch((e: unknown) => e);
      expect(failure).not.toBeInstanceOf(EmailConfigStaleError);
      expect((failure as Error).message).toBe("Another conflict.");
    });
  });

  it("fails, rather than succeeding, when a proxy answers with a page that is not the API's", async () => {
    answer = () =>
      Promise.resolve(new Response("<html>Bad Gateway</html>", { status: 502, headers: { "Content-Type": "text/html" } }));
    const { hook } = setup();

    await act(async () => {
      await expect(hook.result.current.mutateAsync()).rejects.toThrow("Request failed");
    });

    expect(toastSuccess).not.toHaveBeenCalled();
    expect(toastWarning).not.toHaveBeenCalled();
  });

  it("fails, rather than succeeding, when the request never reaches the server", async () => {
    answer = () => Promise.reject(new TypeError("Failed to fetch"));
    const { hook } = setup();

    await act(async () => {
      await expect(hook.result.current.mutateAsync()).rejects.toThrow("Failed to fetch");
    });

    expect(toastSuccess).not.toHaveBeenCalled();
  });
});

describe("removalNeedsSync", () => {
  const base = { removed_provider: "smtp", detail: "x" };

  it.each([
    ["org_default, delivered", { ...base, fallback: "org_default", org_default_synced: true }, false],
    ["org_default, not delivered", { ...base, fallback: "org_default", org_default_synced: false }, true],
    // org_default_synced is false here as well, but there is no default to send.
    ["wordpress", { ...base, fallback: "wordpress", org_default_synced: false }, false],
    ["unknown", { ...base, fallback: "unknown", org_default_synced: false }, true],
  ] as const)("%s", (_name, result, expected) => {
    expect(removalNeedsSync(result as RemoveSiteEmailConfigResult)).toBe(expected);
  });
});
