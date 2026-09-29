import { describe, it, expect } from "vitest";

import { isLikelyIndian, isLikelyIndianTimezone } from "./likely-indian";

// S0.4 (stripe-design-v9.md 5.10): likelyIndian = currency hint INR, OR
// Intl timezone Asia/Kolkata / Asia/Calcutta (C25).

describe("isLikelyIndianTimezone", () => {
  it("is true for Asia/Kolkata", () => {
    expect(isLikelyIndianTimezone("Asia/Kolkata")).toBe(true);
  });

  it("is true for the legacy Asia/Calcutta spelling", () => {
    expect(isLikelyIndianTimezone("Asia/Calcutta")).toBe(true);
  });

  it("is false for an unrelated timezone", () => {
    expect(isLikelyIndianTimezone("America/New_York")).toBe(false);
  });

  it("is false for undefined", () => {
    expect(isLikelyIndianTimezone(undefined)).toBe(false);
  });
});

describe("isLikelyIndian", () => {
  it("is true when the currency hint is INR, regardless of timezone", () => {
    expect(
      isLikelyIndian({ currencyHint: "INR", timeZone: "America/New_York" }),
    ).toBe(true);
  });

  it("is true when the timezone is Indian, regardless of currency hint", () => {
    expect(
      isLikelyIndian({ currencyHint: "USD", timeZone: "Asia/Kolkata" }),
    ).toBe(true);
  });

  it("is false when neither signal is Indian", () => {
    expect(
      isLikelyIndian({ currencyHint: "USD", timeZone: "America/New_York" }),
    ).toBe(false);
  });

  it("is false when both signals are absent", () => {
    expect(isLikelyIndian({})).toBe(false);
  });
});
