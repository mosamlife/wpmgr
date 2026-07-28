import { describe, it, expect } from "vitest";

import { agentStatusFilterHint } from "./agent-status-hint";

// GH #255: "the agent-status filter doesn't seem to trigger or activate
// properly" turned out to be a knock-on of every site classifying Unknown,
// not an independent bug in the filter's toggle/apply logic (see
// sites-filter.test.ts for that logic's own coverage), a one-option
// dropdown whose only value matches every row looks like a no-op from the
// outside. These pin when the explanatory hint does, and does not, appear.

describe("agentStatusFilterHint", () => {
  it("is undefined when there is more than one option (an ordinary, working axis)", () => {
    expect(agentStatusFilterHint(["Current", "Unknown"], "published")).toBeUndefined();
  });

  it("is undefined when there are no options yet (rollup still loading)", () => {
    expect(agentStatusFilterHint([], undefined)).toBeUndefined();
  });

  it("is undefined when the single option is not Unknown (e.g. every site is Current)", () => {
    expect(agentStatusFilterHint(["Current"], "published")).toBeUndefined();
  });

  it("names the no-reference-version root cause when reference_source is none", () => {
    const hint = agentStatusFilterHint(["Unknown"], "none");
    expect(hint).toMatch(/no reference agent version/);
    expect(hint).toMatch(/cannot narrow the list/);
  });

  it("still explains a single Unknown bucket even when a reference version exists", () => {
    // e.g. every site simply has not reported agent_version yet.
    const hint = agentStatusFilterHint(["Unknown"], "published");
    expect(hint).toBe("Every site here is Unknown, so this filter cannot narrow the list.");
  });

  it("handles an undefined reference_source (rollup not yet loaded) the same as a generic explanation", () => {
    const hint = agentStatusFilterHint(["Unknown"], undefined);
    expect(hint).toBe("Every site here is Unknown, so this filter cannot narrow the list.");
  });
});
