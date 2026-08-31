import { describe, it, expect } from "vitest";

import {
  allScopesRecognised,
  asSelfAsserted,
  describeScope,
  parseConsentContext,
  SCOPE_READ,
} from "./consent-context";

// m124 obligation 7: the consent screen must present registration-supplied
// identity as UNVERIFIED. RFC 7591 registration is unauthenticated, so
// client_name and client_uri are attacker-controlled strings and two clients
// may both call themselves "Claude Desktop".

const VALID = {
  client_id: "c_01HZ",
  client_name_unverified: "Claude Desktop",
  client_uri_unverified: "https://claude.ai",
  identity_verified: false,
  redirect_uri: "https://attacker.example/oauth/callback",
  redirect_host: "attacker.example",
  scopes: [SCOPE_READ],
  grant_lifetime_days: 90,
  state: "xyz",
  code_challenge: "abc",
  code_challenge_method: "S256",
};

describe("parseConsentContext — unverified identity", () => {
  it("keeps the self-declared name on a field whose name says it is unverified", () => {
    const ctx = parseConsentContext(VALID);
    // The domain object has NO plain `clientName`. A template cannot reach the
    // string without going through a field marked unverified and a union that
    // forces the absent branch to be handled.
    expect(ctx).not.toHaveProperty("clientName");
    expect(ctx.clientNameUnverified).toEqual({ stated: true, value: "Claude Desktop" });
    expect(ctx.identityVerified).toBe(false);
  });

  it("REFUSES a payload that claims the identity is verified", () => {
    // There is no verified path. dto.go writes `IdentityVerified: false` as a
    // literal precisely so no code path can set it true. A payload asserting
    // otherwise is not a more-trusted client, it is a payload we do not
    // understand, and an unreadable payload is a failed load.
    expect(() => parseConsentContext({ ...VALID, identity_verified: true })).toThrow();
  });

  it("REFUSES a payload with no verified redirect host to show", () => {
    // The redirect host is the only identity claim on the screen we stand
    // behind. Without it the screen would have nothing true to lead with and
    // the attacker-controlled name would become the answer to "who is asking".
    expect(() => parseConsentContext({ ...VALID, redirect_host: "" })).toThrow();
    const { redirect_host: _dropped, ...missing } = VALID;
    expect(() => parseConsentContext(missing)).toThrow();
  });

  it("REFUSES a payload carrying no scopes rather than showing an empty permission list", () => {
    // ParseRequestedScopes refuses a request naming no recognised scope, so an
    // empty array here is incoherent. Rendering it as "this client may read
    // nothing" would be a confident sentence about a payload we could not read.
    expect(() => parseConsentContext({ ...VALID, scopes: [] })).toThrow();
  });

  it("REFUSES a payload that is not an object at all", () => {
    expect(() => parseConsentContext(null)).toThrow();
    expect(() => parseConsentContext("ok")).toThrow();
    expect(() => parseConsentContext(undefined)).toThrow();
  });

  it("distinguishes a client that gave no name from one that gave a blank", () => {
    // Both are an ABSENCE and both must reach the same explicit branch. A
    // whitespace name is chosen by an attacker for exactly the effect a blank
    // has: it looks like a rendering glitch rather than a warning.
    expect(parseConsentContext({ ...VALID, client_name_unverified: "" }).clientNameUnverified)
      .toEqual({ stated: false });
    expect(parseConsentContext({ ...VALID, client_name_unverified: "   " }).clientNameUnverified)
      .toEqual({ stated: false });
    const { client_name_unverified: _omitted, ...noName } = VALID;
    expect(parseConsentContext(noName).clientNameUnverified).toEqual({ stated: false });
  });

  it("refuses a payload with no grant lifetime rather than assuming one", () => {
    // The screen has to say how long the authorisation lasts. A missing term
    // is an unrenderable screen, not a screen that quietly falls back to the
    // term this file last knew about. Same fail-closed direction as
    // redirect_host above.
    const { grant_lifetime_days: _omitted, ...noTerm } = VALID;
    expect(() => parseConsentContext(noTerm)).toThrow();
  });

  it("refuses a grant lifetime that is not a positive whole number of days", () => {
    expect(() => parseConsentContext({ ...VALID, grant_lifetime_days: 0 })).toThrow();
    expect(() => parseConsentContext({ ...VALID, grant_lifetime_days: -30 })).toThrow();
    expect(() => parseConsentContext({ ...VALID, grant_lifetime_days: 90.5 })).toThrow();
    expect(() => parseConsentContext({ ...VALID, grant_lifetime_days: "90" })).toThrow();
  });

  it("carries the server's term through unchanged", () => {
    expect(parseConsentContext({ ...VALID, grant_lifetime_days: 365 }).grantLifetimeDays).toBe(
      365,
    );
  });

  it("never coerces an absent optional into a plausible value", () => {
    const { state: _s, code_challenge: _c, code_challenge_method: _m, ...bare } = VALID;
    const ctx = parseConsentContext(bare);
    expect(ctx.state).toBeNull();
    expect(ctx.codeChallenge).toBeNull();
    expect(ctx.codeChallengeMethod).toBeNull();
  });
});

describe("asSelfAsserted", () => {
  it("reports presence and absence as distinct facts", () => {
    expect(asSelfAsserted("Fleet")).toEqual({ stated: true, value: "Fleet" });
    expect(asSelfAsserted("  Fleet  ")).toEqual({ stated: true, value: "Fleet" });
    expect(asSelfAsserted("")).toEqual({ stated: false });
    expect(asSelfAsserted(null)).toEqual({ stated: false });
    expect(asSelfAsserted(undefined)).toEqual({ stated: false });
  });
});

describe("describeScope", () => {
  it("describes the one recognised scope in blunt, specific terms", () => {
    const copy = describeScope(SCOPE_READ);
    expect(copy.title).toContain("Read");
    expect(copy.detail).toContain("plugins");
  });

  it("says an unrecognised scope is unrecognised rather than prettifying it", () => {
    const copy = describeScope("mcp:write");
    expect(copy.title.toLowerCase()).toContain("unrecognised");
    expect(copy.detail).toContain("Do not approve");
  });

  it("treats any scope other than the one in the registry as unrecognised", () => {
    expect(allScopesRecognised([SCOPE_READ])).toBe(true);
    expect(allScopesRecognised([SCOPE_READ, "mcp:write"])).toBe(false);
    // Matching is exact and case-sensitive server-side (RFC 6749 s3.3), so a
    // normalised near-miss must not be described as the real scope.
    expect(allScopesRecognised(["MCP:READ"])).toBe(false);
  });
});
