import { describe, it, expect } from "vitest";

import {
  PLAN_CATALOG,
  isDowngrade,
  exceedsPlanLimit,
  planLabel,
  planCatalogEntry,
} from "./plan-catalog";

describe("PLAN_CATALOG", () => {
  it("has exactly the four locked tiers in ascending order", () => {
    expect(PLAN_CATALOG.map((p) => p.id)).toEqual([
      "free",
      "starter",
      "agency",
      "scale",
    ]);
  });

  it("site limits match the locked entitlement ladder (apps/api/internal/billing/entitlements.go)", () => {
    expect(planCatalogEntry("free")?.sitesLimit).toBe(3);
    expect(planCatalogEntry("starter")?.sitesLimit).toBe(10);
    expect(planCatalogEntry("agency")?.sitesLimit).toBe(50);
    expect(planCatalogEntry("scale")?.sitesLimit).toBe(200);
  });
});

describe("isDowngrade", () => {
  it("is false for the same tier", () => {
    expect(isDowngrade("agency", "agency")).toBe(false);
  });

  it("is false moving up the ladder", () => {
    expect(isDowngrade("free", "starter")).toBe(false);
    expect(isDowngrade("starter", "scale")).toBe(false);
  });

  it("is true moving down the ladder", () => {
    expect(isDowngrade("scale", "agency")).toBe(true);
    expect(isDowngrade("agency", "free")).toBe(true);
  });
});

describe("exceedsPlanLimit — the server-guarded downgrade note", () => {
  const starter = planCatalogEntry("starter")!;

  it("is false when usage is within the target tier's limit", () => {
    expect(exceedsPlanLimit(5, starter)).toBe(false);
    expect(exceedsPlanLimit(10, starter)).toBe(false);
  });

  it("is true when usage exceeds the target tier's limit — disables the downgrade button", () => {
    expect(exceedsPlanLimit(11, starter)).toBe(true);
  });
});

describe("planLabel", () => {
  it("resolves a known plan id to its display name", () => {
    expect(planLabel("agency")).toBe("Agency");
  });

  it("title-cases an unrecognized plan string rather than throwing", () => {
    expect(planLabel("enterprise")).toBe("Enterprise");
  });
});
