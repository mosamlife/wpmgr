import { describe, it, expect } from "vitest";

import { billingBannerFor, formatBillingDate, planStatusLabel } from "./billing-status";
import type { BillingPlanStatus } from "./use-billing";

// Named test: "billing-page state rendering (each plan_status)". Every
// plan_status branch the page's banner logic can hit is independently
// asserted here — pure functions, no rendering (see MEMORY: only pure-logic
// tests exist in this stack; no @testing-library/react).

const ALL_STATUSES: BillingPlanStatus[] = [
  "none",
  "trialing",
  "active",
  "past_due",
  "canceled",
  "paused",
  "comped",
];

describe("billingBannerFor", () => {
  it("returns null for none / trialing / active / comped — no banner needed", () => {
    for (const status of ["none", "trialing", "active", "comped"] as const) {
      expect(billingBannerFor({ plan_status: status })).toBeNull();
    }
  });

  it("returns a warning banner for past_due with a grace_until date", () => {
    const banner = billingBannerFor({
      plan_status: "past_due",
      grace_until: "2027-01-12T00:00:00Z",
    });
    expect(banner).not.toBeNull();
    expect(banner?.tone).toBe("warning");
    expect(banner?.message).toContain("Payment issue");
    expect(banner?.message).toContain("Jan 12, 2027");
    expect(banner?.message).toContain("Update your card");
  });

  it("returns a warning banner for past_due without a grace_until (defensive fallback)", () => {
    const banner = billingBannerFor({ plan_status: "past_due" });
    expect(banner?.tone).toBe("warning");
    expect(banner?.message).toContain("Payment issue");
  });

  it("returns a muted banner for canceled", () => {
    const banner = billingBannerFor({ plan_status: "canceled" });
    expect(banner?.tone).toBe("muted");
    expect(banner?.message).toContain("canceled");
  });

  it("returns a muted banner for paused", () => {
    const banner = billingBannerFor({ plan_status: "paused" });
    expect(banner?.tone).toBe("muted");
    expect(banner?.message).toContain("paused");
  });

  it("never emits an em dash or en dash in any banner message (house style)", () => {
    for (const status of ALL_STATUSES) {
      const banner = billingBannerFor({
        plan_status: status,
        grace_until: "2027-01-12T00:00:00Z",
      });
      if (banner) {
        expect(banner.message).not.toMatch(/[–—]/);
      }
    }
  });
});

describe("planStatusLabel", () => {
  it("maps every plan_status to a distinct, human-readable label", () => {
    const labels = ALL_STATUSES.map(planStatusLabel);
    expect(new Set(labels).size).toBe(ALL_STATUSES.length);
    expect(planStatusLabel("past_due")).toBe("Payment issue");
    expect(planStatusLabel("active")).toBe("Active");
  });
});

describe("formatBillingDate", () => {
  it("formats an ISO timestamp as a short human date", () => {
    expect(formatBillingDate("2027-01-12T00:00:00Z")).toBe("Jan 12, 2027");
  });

  it("falls back to the raw string for an invalid date", () => {
    expect(formatBillingDate("not-a-date")).toBe("not-a-date");
  });
});
