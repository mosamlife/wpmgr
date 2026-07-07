import { describe, it, expect } from "vitest";

import { __isSiteStreamOpen, resetSiteStream } from "./use-site-events";

// ---------------------------------------------------------------------------
// resetSiteStream — GH #152 Part 1 (org-switch SSE lifecycle)
//
// The shared Sites SSE stream is a module-level `EventSource` singleton whose
// lifecycle is otherwise driven ONLY by subscriber count via `useSiteEvents`
// (a React hook, effect-bound). Exercising the "reopen while subscribers
// remain" branch would require mounting a real component tree (the project
// deliberately has no @testing-library/react / jsdom dependency for these
// singleton hook tests — see the sibling sites-filter.test.ts /
// use-site-connection.test.ts comments), so this file covers what's testable
// as pure module state: the no-subscriber no-op contract that
// `resetSiteStream()` must uphold when called with nothing mounted (e.g. if a
// tenant switch races a route where nothing is currently subscribed).
// ---------------------------------------------------------------------------

describe("resetSiteStream", () => {
  it("is a no-op and never throws when there are no active subscribers", () => {
    expect(__isSiteStreamOpen()).toBe(false);
    expect(() => resetSiteStream()).not.toThrow();
    expect(__isSiteStreamOpen()).toBe(false);
  });

  it("is safe to call repeatedly back-to-back with no subscribers", () => {
    expect(() => {
      resetSiteStream();
      resetSiteStream();
      resetSiteStream();
    }).not.toThrow();
    expect(__isSiteStreamOpen()).toBe(false);
  });
});
