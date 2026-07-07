import { describe, it, expect } from "vitest";

import {
  extractSiteLimitReached,
  SiteLimitReachedError,
  extractByoDestinationRequired,
  ByoDestinationRequiredError,
} from "./api";

// Named test: "the 402 interceptor mapping". `extractSiteLimitReached` is the
// shared handler every site-create mutation error path runs through (Add Site
// dialog, and any future site-create path) before falling back to a generic
// error — see use-site-connection.ts's useCreateSiteFirst.

describe("extractSiteLimitReached", () => {
  const validBody = {
    code: "site_limit_reached",
    message: "this workspace is at its site limit.",
    details: { limit: 3, usage: 3, plan: "free" },
  };

  it("recognizes the 402 site_limit_reached shape and returns a typed error", () => {
    const err = extractSiteLimitReached(402, validBody);
    expect(err).toBeInstanceOf(SiteLimitReachedError);
    expect(err?.limit).toBe(3);
    expect(err?.usage).toBe(3);
    expect(err?.plan).toBe("free");
  });

  it("returns null for a non-402 status even with a matching body (e.g. a 409 collision)", () => {
    expect(extractSiteLimitReached(409, validBody)).toBeNull();
  });

  it("returns null for a 402 with a different error code", () => {
    expect(
      extractSiteLimitReached(402, { ...validBody, code: "some_other_code" }),
    ).toBeNull();
  });

  it("returns null when details are missing entirely", () => {
    expect(
      extractSiteLimitReached(402, { code: "site_limit_reached", message: "x" }),
    ).toBeNull();
  });

  it("returns null when a details field has the wrong type", () => {
    expect(
      extractSiteLimitReached(402, {
        code: "site_limit_reached",
        message: "x",
        details: { limit: "3", usage: 3, plan: "free" },
      }),
    ).toBeNull();
  });

  it("returns null for a non-object error body", () => {
    expect(extractSiteLimitReached(402, "some string error")).toBeNull();
    expect(extractSiteLimitReached(402, null)).toBeNull();
    expect(extractSiteLimitReached(402, undefined)).toBeNull();
  });

  it("returns null when status is undefined (transport error, no response)", () => {
    expect(extractSiteLimitReached(undefined, validBody)).toBeNull();
  });
});

describe("SiteLimitReachedError", () => {
  it("is an instance of Error so it propagates through TanStack Query's onError", () => {
    const err = new SiteLimitReachedError(3, 3, "free");
    expect(err).toBeInstanceOf(Error);
  });

  it("has a predictable name and code for narrowing", () => {
    const err = new SiteLimitReachedError(10, 10, "starter");
    expect(err.name).toBe("SiteLimitReachedError");
    expect(err.code).toBe("site_limit_reached");
  });

  it("carries the limit/usage/plan the UpgradePrompt needs", () => {
    const err = new SiteLimitReachedError(50, 47, "agency");
    expect(err.limit).toBe(50);
    expect(err.usage).toBe(47);
    expect(err.plan).toBe("agency");
  });
});

// Named test: "the 402 byo_destination_required interceptor mapping"
// (backup-destinations Phase 2). `extractByoDestinationRequired` is the
// shared handler a manual backup-create mutation error path runs through
// before falling back to a generic error — see use-backups.ts's
// useCreateBackup.

describe("extractByoDestinationRequired", () => {
  const validBody = {
    code: "byo_destination_required",
    message: "Free plan backups must go to your own storage.",
    details: { plan: "free", has_byo_destination: false },
  };

  it("recognizes the 402 byo_destination_required shape and returns a typed error", () => {
    const err = extractByoDestinationRequired(402, validBody);
    expect(err).toBeInstanceOf(ByoDestinationRequiredError);
    expect(err?.plan).toBe("free");
    expect(err?.hasByoDestination).toBe(false);
  });

  it("returns null for a non-402 status even with a matching body (e.g. a 422)", () => {
    expect(extractByoDestinationRequired(422, validBody)).toBeNull();
  });

  it("returns null for a 402 with a different error code", () => {
    expect(
      extractByoDestinationRequired(402, {
        ...validBody,
        code: "some_other_code",
      }),
    ).toBeNull();
  });

  it("returns null when details are missing entirely", () => {
    expect(
      extractByoDestinationRequired(402, {
        code: "byo_destination_required",
        message: "x",
      }),
    ).toBeNull();
  });

  it("returns null when a details field has the wrong type", () => {
    expect(
      extractByoDestinationRequired(402, {
        code: "byo_destination_required",
        message: "x",
        details: { plan: "free", has_byo_destination: "false" },
      }),
    ).toBeNull();
  });

  it("returns null for a non-object error body", () => {
    expect(extractByoDestinationRequired(402, "some string error")).toBeNull();
    expect(extractByoDestinationRequired(402, null)).toBeNull();
    expect(extractByoDestinationRequired(402, undefined)).toBeNull();
  });

  it("returns null when status is undefined (transport error, no response)", () => {
    expect(extractByoDestinationRequired(undefined, validBody)).toBeNull();
  });
});

describe("ByoDestinationRequiredError", () => {
  it("is an instance of Error so it propagates through TanStack Query's onError", () => {
    const err = new ByoDestinationRequiredError("free", false);
    expect(err).toBeInstanceOf(Error);
  });

  it("has a predictable name and code for narrowing", () => {
    const err = new ByoDestinationRequiredError("free", false);
    expect(err.name).toBe("ByoDestinationRequiredError");
    expect(err.code).toBe("byo_destination_required");
  });

  it("carries the plan/has_byo_destination the prompt needs", () => {
    const err = new ByoDestinationRequiredError("free", true);
    expect(err.plan).toBe("free");
    expect(err.hasByoDestination).toBe(true);
  });
});
