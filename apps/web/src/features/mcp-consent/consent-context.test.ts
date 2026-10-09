import { describe, it, expect } from "vitest";

import {
  allScopesRecognised,
  asSelfAsserted,
  buildApprovalCapabilities,
  consentWireSchema,
  describeScope,
  offeredReads,
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

describe("parseConsentContext — the consent ticket is carried, never interpreted", () => {
  // The whitespace is load-bearing: it makes any trim, re-encode or round trip
  // through another type show up as a failed equality rather than as a
  // plausible-looking string that fails a signature check in production.
  const TICKET = "  v1.eyJhIjoxfQ.c1gN-_~+/=AbC  ";

  it("carries a ticket through byte for byte", () => {
    expect(parseConsentContext({ ...VALID, consent_ticket: TICKET }).consentTicket).toBe(TICKET);
  });

  it("parses fine with no ticket, because today's server sends none", () => {
    // Required here would fail this screen closed against every currently
    // deployed server -- the fail-closed direction aimed at our own users
    // rather than at an attacker. Absent and Go's "" zero value are the same
    // fact and reach the same null.
    expect(parseConsentContext(VALID).consentTicket).toBeNull();
    expect(parseConsentContext({ ...VALID, consent_ticket: "" }).consentTicket).toBeNull();
  });

  it("refuses a non-string ticket rather than coercing one", () => {
    expect(() => parseConsentContext({ ...VALID, consent_ticket: 12345 })).toThrow();
    expect(() => parseConsentContext({ ...VALID, consent_ticket: { t: "x" } })).toThrow();
    expect(() => parseConsentContext({ ...VALID, consent_ticket: null })).toThrow();
  });

  it("did NOT make the wire schema permissive", () => {
    // Adding one KNOWN optional key is the change. Turning the object
    // permissive is not, and it would be the easy accidental version of this
    // commit. The known key survives the parse and an unknown one still does
    // not, which is exactly the behaviour this file had yesterday.
    const parsed = consentWireSchema.parse({
      ...VALID,
      consent_ticket: "t",
      surprise: "payload",
    });
    expect(parsed.consent_ticket).toBe("t");
    expect(parsed).not.toHaveProperty("surprise");
  });
});

describe("parseConsentContext, the scopes the server withheld", () => {
  // The wire shape for a registration that holds mcp:read alone and asked for
  // the advertised list: `scopes` is the overlap and `unregistered_scopes` is
  // the rest, in the order the client asked. Pinned on the Go side by
  // TestAuthorizeHandler_NamesTheWithheldScopes
  // (apps/api/internal/mcp/advertised_scopes_test.go), which reads
  // `"scopes":["mcp:read"]` and `"unregistered_scopes":["mcp:site","mcp:cache"]`
  // off the handler's own output.
  const WITHHELD = { ...VALID, unregistered_scopes: ["mcp:site", "mcp:cache"] };

  it("carries the withheld scopes through, in the order the client asked", () => {
    // Without the key in the wire schema, zod's default object parse strips it
    // silently and the screen never hears about it.
    expect(parseConsentContext(WITHHELD).unregisteredScopes).toEqual(["mcp:site", "mcp:cache"]);
  });

  it("keeps them out of the scopes that can be approved", () => {
    // The server sealed `scopes` into the ticket. Folding the withheld ones in
    // here would put scopes the registration does not hold on the approval.
    const ctx = parseConsentContext(WITHHELD);
    expect(ctx.scopes).toEqual([SCOPE_READ]);
    expect(ctx.scopes).not.toContain("mcp:site");
    expect(ctx.scopes).not.toContain("mcp:cache");
  });

  it("reads [] as nothing withheld", () => {
    // dto.go sends [] rather than null when the request fit the registration.
    expect(parseConsentContext({ ...VALID, unregistered_scopes: [] }).unregisteredScopes).toEqual(
      [],
    );
  });

  it("reads an absent key as nothing withheld, because a server that predates the field sends none", () => {
    // Required here would fail the whole consent screen closed against every
    // server that has not shipped the field, and the field is copy only.
    expect(parseConsentContext(VALID).unregisteredScopes).toEqual([]);
  });

  it("refuses a value that is not an array of scope strings rather than guessing", () => {
    // Present means held to the server's promise: an array of non-empty
    // strings. dto.go builds the slice with make(), so it is never null.
    expect(() => parseConsentContext({ ...VALID, unregistered_scopes: "mcp:site" })).toThrow();
    expect(() => parseConsentContext({ ...VALID, unregistered_scopes: null })).toThrow();
    expect(() => parseConsentContext({ ...VALID, unregistered_scopes: [1] })).toThrow();
    expect(() => parseConsentContext({ ...VALID, unregistered_scopes: [""] })).toThrow();
    expect(() => parseConsentContext({ ...VALID, unregistered_scopes: { 0: "mcp:site" } })).toThrow();
  });

  it("did NOT make the wire schema permissive", () => {
    // One known optional key is the change. An unknown key is still dropped.
    const parsed = consentWireSchema.parse({ ...WITHHELD, surprise: "payload" });
    expect(parsed.unregistered_scopes).toEqual(["mcp:site", "mcp:cache"]);
    expect(parsed).not.toHaveProperty("surprise");
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

// ---------------------------------------------------------------------------
// The capability list an approval sends
// ---------------------------------------------------------------------------

// What the server lists for a request holding mcp:read, mcp:cache and mcp:site:
// scopeCapabilities in apps/api/internal/mcp/policy.go, written out rather than
// derived from the dashboard's vocabulary. Every read has the "read" effect,
// the cache-clear and ability.request have "request", and ability.read is a
// read.
const READS = [
  "mcp.activity.read",
  "mcp.backups.read",
  "mcp.diagnostics.read",
  "mcp.performance.read",
  "mcp.security.read",
  "mcp.sites.read",
  "mcp.uptime.read",
];
const asReads = (names: readonly string[]) => names.map((name) => ({ name, effect: "read" }));
const CACHE = { name: "mcp.cache.purge", effect: "request" };
const ABILITY_READ = { name: "mcp.ability.read", effect: "read" };
const ABILITY_REQUEST = { name: "mcp.ability.request", effect: "request" };
const EVERYTHING = [...asReads(READS), CACHE, ABILITY_READ, ABILITY_REQUEST];

describe("offeredReads", () => {
  it("is the reads the server listed, once each, in vocabulary order", () => {
    expect(
      offeredReads([
        ...asReads(["mcp.uptime.read", "mcp.sites.read", "mcp.sites.read"]),
        CACHE,
        ABILITY_READ,
      ]),
    ).toEqual(["mcp.sites.read", "mcp.uptime.read"]);
  });

  it("leaves out the write, the site-tools names and anything this build does not know", () => {
    expect(offeredReads([CACHE, ABILITY_READ, ABILITY_REQUEST])).toEqual([]);
    expect(offeredReads(asReads(["mcp.future-thing.read"]))).toEqual([]);
  });

  it("leaves out mcp.content.read even when the server lists it as a read", () => {
    expect(offeredReads(asReads(["mcp.content.read", "mcp.sites.read"]))).toEqual([
      "mcp.sites.read",
    ]);
  });

  it("does not take a conferrable name offered with the wrong effect for a read", () => {
    expect(offeredReads([{ name: "mcp.sites.read", effect: "request" }])).toEqual([]);
  });

  it("is empty for a server that listed nothing", () => {
    expect(offeredReads([])).toEqual([]);
  });
});

describe("buildApprovalCapabilities, exactly the ticks limited to what was offered", () => {
  it("sends the ticked reads and no read that is not ticked", () => {
    expect([...buildApprovalCapabilities(EVERYTHING, ["mcp.backups.read", "mcp.security.read"])].sort()).toEqual([
      "mcp.backups.read",
      "mcp.security.read",
    ]);
    // The reads on offer are seven; one is ticked; one is sent.
    expect(buildApprovalCapabilities(EVERYTHING, ["mcp.sites.read"])).toEqual(["mcp.sites.read"]);
  });

  it("sends every offered read when every read is ticked", () => {
    expect([...buildApprovalCapabilities(EVERYTHING, READS)].sort()).toEqual([...READS].sort());
  });

  it("sends nothing for an empty tick list, and the caller omits the key", () => {
    expect(buildApprovalCapabilities(EVERYTHING, [])).toEqual([]);
  });

  it("drops a ticked name the server did not offer", () => {
    // A tick left over from a different request, or one the screen should not
    // have been able to make, cannot widen the approval.
    expect(
      buildApprovalCapabilities(asReads(["mcp.sites.read"]), ["mcp.sites.read", "mcp.uptime.read"]),
    ).toEqual(["mcp.sites.read"]);
    expect(buildApprovalCapabilities([], ["mcp.sites.read", "mcp.cache.purge"])).toEqual([]);
  });

  it("never sends mcp.content.read, ticked or not, offered or not", () => {
    expect(
      buildApprovalCapabilities(asReads(["mcp.content.read", "mcp.sites.read"]), [
        "mcp.content.read",
        "mcp.sites.read",
      ]),
    ).toEqual(["mcp.sites.read"]);
  });

  it("sends mcp.cache.purge only when it is ticked and offered as a request", () => {
    expect(buildApprovalCapabilities(EVERYTHING, ["mcp.sites.read"])).not.toContain("mcp.cache.purge");
    expect(buildApprovalCapabilities(EVERYTHING, ["mcp.sites.read", "mcp.cache.purge"])).toEqual([
      "mcp.sites.read",
      "mcp.cache.purge",
    ]);
    // Offered with the wrong effect, or not offered at all: not sent.
    expect(
      buildApprovalCapabilities([{ name: "mcp.cache.purge", effect: "read" }], ["mcp.cache.purge"]),
    ).toEqual([]);
    expect(buildApprovalCapabilities(asReads(READS), ["mcp.cache.purge"])).toEqual([]);
  });

  it("sends the site-tools names only when ticked and offered with their own effect", () => {
    expect(
      buildApprovalCapabilities(EVERYTHING, [
        "mcp.sites.read",
        "mcp.ability.read",
        "mcp.ability.request",
      ]),
    ).toEqual(["mcp.sites.read", "mcp.ability.read", "mcp.ability.request"]);
    expect(buildApprovalCapabilities(EVERYTHING, ["mcp.sites.read"])).toEqual(["mcp.sites.read"]);
    expect(
      buildApprovalCapabilities([ABILITY_READ, { name: "mcp.ability.request", effect: "read" }], [
        "mcp.ability.read",
        "mcp.ability.request",
      ]),
    ).toEqual(["mcp.ability.read"]);
  });

  it("lists the reads first, in vocabulary order, then the write and the site tools", () => {
    expect(
      buildApprovalCapabilities(EVERYTHING, [
        "mcp.ability.request",
        "mcp.uptime.read",
        "mcp.cache.purge",
        "mcp.sites.read",
        "mcp.ability.read",
      ]),
    ).toEqual([
      "mcp.sites.read",
      "mcp.uptime.read",
      "mcp.cache.purge",
      "mcp.ability.read",
      "mcp.ability.request",
    ]);
  });

  it("does not mutate the tick list it was given", () => {
    const ticks = Object.freeze(["mcp.sites.read", "mcp.cache.purge"]);
    expect(() => buildApprovalCapabilities(EVERYTHING, ticks)).not.toThrow();
    expect(ticks).toEqual(["mcp.sites.read", "mcp.cache.purge"]);
  });
});
