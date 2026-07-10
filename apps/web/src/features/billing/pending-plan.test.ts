import { describe, it, expect, beforeEach } from "vitest";

import { stashPendingPlan, readPendingPlan, clearPendingPlan } from "./pending-plan";

// M16 Phase C2 — the same-browser fast path for a plan chosen at
// /register?plan=... . Pure storage-boundary coverage: no React, no router.

function clearAllStorage() {
  window.localStorage.clear();
  document.cookie = "wpmgr_pending_plan=; max-age=0; path=/";
}

beforeEach(() => {
  clearAllStorage();
});

describe("stashPendingPlan / readPendingPlan / clearPendingPlan", () => {
  it("round-trips a stashed plan through localStorage", () => {
    stashPendingPlan({ plan: "agency", currency: "INR" });

    expect(readPendingPlan()).toEqual({ plan: "agency", currency: "INR" });
  });

  it("round-trips a stashed plan with no currency", () => {
    stashPendingPlan({ plan: "starter" });

    expect(readPendingPlan()).toEqual({ plan: "starter" });
  });

  it("also writes a cookie fallback (readable even if localStorage is cleared, matching a real private-mode fallback)", () => {
    stashPendingPlan({ plan: "scale", currency: "USD" });
    window.localStorage.clear();

    expect(readPendingPlan()).toEqual({ plan: "scale", currency: "USD" });
  });

  it("returns null when nothing was ever stashed", () => {
    expect(readPendingPlan()).toBeNull();
  });

  it("clearPendingPlan removes both the localStorage entry and the cookie", () => {
    stashPendingPlan({ plan: "agency" });
    expect(readPendingPlan()).not.toBeNull();

    clearPendingPlan();

    expect(readPendingPlan()).toBeNull();
    expect(window.localStorage.getItem("wpmgr_pending_plan")).toBeNull();
    expect(document.cookie).not.toContain("wpmgr_pending_plan=");
  });

  it("ignores a corrupt localStorage value rather than throwing", () => {
    window.localStorage.setItem("wpmgr_pending_plan", "{not json");

    expect(readPendingPlan()).toBeNull();
  });

  it("ignores a stashed value naming an unknown plan (defensive against a future/stale format)", () => {
    window.localStorage.setItem(
      "wpmgr_pending_plan",
      JSON.stringify({ plan: "enterprise" }),
    );

    expect(readPendingPlan()).toBeNull();
  });
});
