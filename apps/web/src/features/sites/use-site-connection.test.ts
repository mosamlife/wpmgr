import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, act } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement, type ReactNode } from "react";

// GH #755 round 2. `client.post` is mocked at the `@wpmgr/api` wire
// boundary (never the hook module itself, see below), so the real
// `useRecheckConnection` mutationFn, including its status/code branching,
// actually executes.
const { postMock } = vi.hoisted(() => ({ postMock: vi.fn() }));
vi.mock("@wpmgr/api", () => ({ client: { post: postMock } }));

import {
  AgentUnreachableError,
  SiteUrlExistsError,
  SiteUrlRedirectsError,
  useRecheckConnection,
} from "./use-site-connection";
import { sitesKeys } from "./use-sites";

// Unit coverage for the exported error classes and query-key shapes used by
// `useRecheckConnection`. The full mutation lifecycle (HTTP call, invalidate,
// toast) is integration-level and requires @testing-library/react +
// @tanstack/react-query wrappers, which the "real hook against a faked
// transport" describe block below now covers; this file pins the contracts
// that the mutation's call site branches on.

// ---------------------------------------------------------------------------
// AgentUnreachableError — the 502/agent_unreachable typed error
// ---------------------------------------------------------------------------

describe("AgentUnreachableError", () => {
  it("has code 'agent_unreachable' so callers can instanceof-branch without string comparison", () => {
    const err = new AgentUnreachableError();
    expect(err.code).toBe("agent_unreachable");
  });

  it("is an instance of Error so it propagates naturally through TanStack Query's onError", () => {
    const err = new AgentUnreachableError();
    expect(err).toBeInstanceOf(Error);
  });

  it("carries a non-alarming message (no 'disconnected'/'down' vocabulary) appropriate for a calm toast", () => {
    const err = new AgentUnreachableError();
    // The message must NOT use alarming terminology — the badge must NOT flip.
    expect(err.message).not.toMatch(/disconnected|down|failed|error/i);
    // It SHOULD describe the quiet-agent scenario so the operator understands.
    expect(err.message).toMatch(/quiet|monitor/i);
  });

  it("has a predictable name for log filtering", () => {
    const err = new AgentUnreachableError();
    expect(err.name).toBe("AgentUnreachableError");
  });

  it("is NOT an instance of SiteUrlExistsError — distinct error lineages", () => {
    const err = new AgentUnreachableError();
    expect(err).not.toBeInstanceOf(SiteUrlExistsError);
  });
});

// ---------------------------------------------------------------------------
// SiteUrlRedirectsError, the 502/site_url_redirects typed error (GH #755)
// ---------------------------------------------------------------------------

describe("SiteUrlRedirectsError", () => {
  it("carries the server's message VERBATIM, never a generic 'could not reach' substitute", () => {
    const err = new SiteUrlRedirectsError(
      "Couldn't reach the agent. https://example.com redirects to https://www.example.com, so no command was sent. If WordPress on the site reports https://www.example.com as its address, the saved address updates to https://www.example.com automatically at the site's next daily check-in.",
      { from: "https://example.com", to: "https://www.example.com/wp-json/wpmgr/v1/command/metadata", suggested_url: "https://www.example.com" },
    );
    expect(err.message).toContain("https://www.example.com");
    expect(err.message).not.toContain("Reconnect");
  });

  it("exposes the sanitised to/suggestedUrl/from fields off `details`", () => {
    const err = new SiteUrlRedirectsError("msg", {
      from: "https://example.com",
      to: "https://www.example.com/wp-json/wpmgr/v1/command/metadata",
      suggested_url: "https://www.example.com",
    });
    expect(err.from).toBe("https://example.com");
    expect(err.to).toBe("https://www.example.com/wp-json/wpmgr/v1/command/metadata");
    expect(err.suggestedUrl).toBe("https://www.example.com");
  });

  it("leaves suggestedUrl undefined when the server omitted it (downgrade/foreign-target rows never set one)", () => {
    const err = new SiteUrlRedirectsError("msg", {
      from: "https://example.com",
      to: "http://example.com/wp-json/wpmgr/v1/command/metadata",
    });
    expect(err.suggestedUrl).toBeUndefined();
  });

  it("has code 'site_url_redirects' so callers can instanceof-branch without string comparison", () => {
    const err = new SiteUrlRedirectsError("msg");
    expect(err.code).toBe("site_url_redirects");
  });

  it("is an instance of Error, and distinct from AgentUnreachableError/SiteUrlExistsError", () => {
    const err = new SiteUrlRedirectsError("msg");
    expect(err).toBeInstanceOf(Error);
    expect(err).not.toBeInstanceOf(AgentUnreachableError);
    expect(err).not.toBeInstanceOf(SiteUrlExistsError);
  });
});

// ---------------------------------------------------------------------------
// useRecheckConnection, real hook against a faked transport (GH #755)
//
// `client.post` is mocked at the @wpmgr/api wire boundary (see the
// `vi.mock("@wpmgr/api", ...)` above), never the hook module itself, so the
// mutationFn's status/code branching actually executes end to end: this is
// what proves the redirect target reaches the operator, rather than only
// proving the error CLASS's constructor works in isolation.
// ---------------------------------------------------------------------------

function makeQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
}

function wrapperFor(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return createElement(QueryClientProvider, { client: qc }, children);
  };
}

beforeEach(() => {
  postMock.mockReset();
});

describe("useRecheckConnection, real hook against a faked transport (GH #755)", () => {
  it("502 site_url_redirects rejects with a SiteUrlRedirectsError carrying the full server message and target (regression: revert the `error.code === \"site_url_redirects\"` branch, or its guard, and this goes red, because the message would collapse to toError's generic \"Could not reach the agent\" fallback instead of naming https://www.example.com)", async () => {
    postMock.mockResolvedValue({
      data: undefined,
      error: {
        code: "site_url_redirects",
        message:
          "Couldn't reach the agent. https://example.com redirects to https://www.example.com, so no command was sent. If WordPress on the site reports https://www.example.com as its address, the saved address updates to https://www.example.com automatically at the site's next daily check-in.",
        details: {
          from: "https://example.com",
          to: "https://www.example.com/wp-json/wpmgr/v1/command/metadata",
          suggested_url: "https://www.example.com",
        },
      },
      response: { status: 502 },
    });
    const { result } = renderHook(() => useRecheckConnection(), {
      wrapper: wrapperFor(makeQueryClient()),
    });

    let caught: unknown;
    await act(async () => {
      try {
        await result.current.mutateAsync({ siteId: "site-1" });
      } catch (err) {
        caught = err;
      }
    });

    expect(caught).toBeInstanceOf(SiteUrlRedirectsError);
    const err = caught as SiteUrlRedirectsError;
    // The redirect target, named IN FULL: this is the exact text an
    // operator's toast renders.
    expect(err.message).toContain("https://www.example.com");
    expect(err.message).not.toContain("Reconnect");
    expect(err.message).not.toBe("Could not reach the agent");
    expect(err.to).toBe("https://www.example.com/wp-json/wpmgr/v1/command/metadata");
    expect(err.suggestedUrl).toBe("https://www.example.com");
  });

  it("502 agent_unreachable still rejects with AgentUnreachableError (over-fire guard: the new branch must not swallow the existing quiet-agent path)", async () => {
    postMock.mockResolvedValue({
      data: undefined,
      error: { code: "agent_unreachable", message: "could not reach the site agent" },
      response: { status: 502 },
    });
    const { result } = renderHook(() => useRecheckConnection(), {
      wrapper: wrapperFor(makeQueryClient()),
    });

    await expect(result.current.mutateAsync({ siteId: "site-1" })).rejects.toBeInstanceOf(
      AgentUnreachableError,
    );
  });

  it("200 resolves with the server body and invalidates the detail + list keys", async () => {
    postMock.mockResolvedValue({
      data: { connection_state: "connected", last_seen_at: "2026-09-28T00:00:00Z", recovered: true },
      error: undefined,
      response: { status: 200 },
    });
    const qc = makeQueryClient();
    const invalidateSpy = vi.spyOn(qc, "invalidateQueries");
    const { result } = renderHook(() => useRecheckConnection(), { wrapper: wrapperFor(qc) });

    let resolved;
    await act(async () => {
      resolved = await result.current.mutateAsync({ siteId: "site-1" });
    });

    expect(resolved).toMatchObject({ connection_state: "connected", recovered: true });
    expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: sitesKeys.detail("site-1") });
    expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: sitesKeys.lists() });
  });
});

// ---------------------------------------------------------------------------
// sitesKeys — the exact cache keys useRecheckConnection invalidates
// ---------------------------------------------------------------------------

describe("sitesKeys — keys invalidated by useRecheckConnection on 200", () => {
  const SITE_ID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee";

  it("detail key is specific to the site so only the affected badge invalidates", () => {
    const key = sitesKeys.detail(SITE_ID);
    expect(key).toContain(SITE_ID);
    expect(key).toContain("detail");
  });

  it("lists key is under the 'sites' namespace so the table row reconciles", () => {
    const key = sitesKeys.lists();
    expect(key[0]).toBe("sites");
    expect(key).toContain("list");
  });

  it("detail key is distinct from the lists key so a recheck only refetches what it needs", () => {
    const detail = sitesKeys.detail(SITE_ID);
    const lists = sitesKeys.lists();
    // They share the root "sites" namespace but diverge immediately after.
    expect(detail).not.toEqual(lists);
  });
});
