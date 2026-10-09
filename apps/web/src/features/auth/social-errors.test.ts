import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { describe, it, expect, vi } from "vitest";

import { socialRefusal, sameOriginPath } from "./social-errors";

// 2.14. A refusal that only a verification link can clear has to say so AND
// offer the link. The server sends mail on exactly one of these codes
// (social_link_requires_verification); the status gate that produces
// email_not_verified sends none, and no other page will send one either, since
// only opening a verification link writes email_verified_at. The `canResend`
// flag is what the sign-in page renders the control from, so these assertions
// are what stop the copy drifting back to "check your inbox" for mail that was
// never sent.

describe("socialRefusal", () => {
  it("offers a resend for the refusals a verification link can clear", () => {
    for (const code of ["email_not_verified", "social_link_requires_verification"]) {
      expect(socialRefusal(code).canResend, code).toBe(true);
    }
  });

  it("offers nothing for refusals a verification link cannot clear", () => {
    for (const code of [
      "account_disabled",
      "social_email_unverified",
      // Nothing can be delivered to this address at all, so a resend offer
      // would be a button that cannot work.
      "social_email_unreachable",
      "social_cancelled",
      "social_provider_disabled",
      "social_state_mismatch",
      "social_url_failed",
      "social_start_failed",
      "something_new_from_the_server",
    ]) {
      expect(socialRefusal(code).canResend, code).toBe(false);
    }
  });

  it("does not tell a pending account to open mail nobody sent", () => {
    const { message } = socialRefusal("email_not_verified");
    // The old copy was "Please verify your email address before signing in",
    // which reads as "we sent you something" when nothing was sent.
    expect(message).not.toMatch(/check your inbox/i);
    expect(message).toMatch(/send yourself a verification link/i);
  });

  it("tells the link refusal that the mail is already on its way", () => {
    // The server calls sendVerificationForSocialLink on this code, so the copy
    // says the link is sent rather than pointing at a password sign-in, which
    // cannot clear this refusal at all.
    const { message } = socialRefusal("social_link_requires_verification");
    expect(message).toMatch(/sent a verification link/i);
  });

  it("still tells the link refusal to take the account back", () => {
    // THE MITIGATION, and the reason this assertion is separate. This refusal
    // fires precisely when an account exists on that address that nobody has
    // proven they own, which is what an attacker parking a row on a stranger's
    // address looks like. Opening the link verifies THAT row, and the next
    // provider sign-in links onto it with whatever password it was created
    // with. Copy that walks someone through the merge without telling them to
    // reset the password hands the account over politely.
    const { message } = socialRefusal("social_link_requires_verification");
    expect(message).toMatch(/reset its password/i);
  });

  // The control plane refuses a GitHub account whose primary address is the
  // provider's outbound-only privacy address, because an account built on one
  // can never be sent a verification link, a password reset or an alert. That
  // refusal only helps if the sentence names the setting to change.
  it("tells a GitHub user with a private address what to change", () => {
    const { message } = socialRefusal("social_email_unreachable");
    expect(message).toContain("private");
    expect(message).toContain("Keep my email addresses private");
  });

  // It must NOT collapse into the unverified-email advice: that address is
  // verified, so "verify your email" describes a problem that is not there.
  it("does not repeat the unverified-email advice", () => {
    expect(socialRefusal("social_email_unreachable").message).not.toEqual(
      socialRefusal("social_email_unverified").message,
    );
  });
});

/**
 * THE CONTRACT, READ FROM THE SERVER RATHER THAN RESTATED HERE.
 *
 * This block used to hold a hand-written ACTIONABLE_SERVER_CODES list that
 * declared apps/api/internal/auth/social_handler.go as its source of truth and
 * then disagreed with it: it named social_rate_limited, social_start_failed and
 * social_provider_already_linked, none of which any server path emitted. The
 * tests passed, because both ends of the "contract" were lists that only had to
 * agree with themselves, and the invented social_rate_limited read as evidence
 * that the unauthenticated start endpoint was rate limited when nothing limited
 * it at all.
 *
 * So the list is no longer written down twice. It is parsed out of the server's
 * socialErrorCodes table, which a Go test
 * (TestSocialErrorCodesAreExactlyWhatTheHandlerEmits) holds to the handler's
 * actual socialFail call sites. A code that appears on one side and not the
 * other now fails here, in whichever direction it drifted.
 */
// Resolved from the vitest root (apps/web), which is where this suite runs.
const SOCIAL_HANDLER_GO = resolve(process.cwd(), "../api/internal/auth/social_handler.go");
const SOCIAL_ERRORS_TS = resolve(process.cwd(), "src/features/auth/social-errors.ts");

/** Every code the server can put in ?social_error=, and whether its sentence is
 * deliberately generic. */
function serverCodes(): Map<string, "coarse" | "named"> {
  const source = readFileSync(SOCIAL_HANDLER_GO, "utf8");
  const table = /var socialErrorCodes = map\[string\]bool\{([\s\S]*?)\n\}/.exec(source);
  if (!table) {
    throw new Error(
      `socialErrorCodes not found in ${SOCIAL_HANDLER_GO}. It is the source of truth for ?social_error=; if it moved, point this test at it rather than reinstating a hand-written list.`,
    );
  }
  const codes = new Map<string, "coarse" | "named">();
  for (const match of (table[1] ?? "").matchAll(
    /"([a-z_]+)":\s*(coarseSentence|namedSentence),/g,
  )) {
    const [, code, kind] = match;
    if (!code) continue;
    codes.set(code, kind === "coarseSentence" ? "coarse" : "named");
  }
  return codes;
}

/** Every code this file's copy answers deliberately, parsed from its switch. */
function uiCodes(): Set<string> {
  const source = readFileSync(SOCIAL_ERRORS_TS, "utf8");
  const codes = new Set<string>();
  for (const [, code] of source.matchAll(/^\s*case "([a-z_]+)":/gm)) {
    if (code) codes.add(code);
  }
  return codes;
}

describe("the ?social_error= contract", () => {
  const server = serverCodes();
  const ui = uiCodes();
  const generic = socialRefusal("a_code_that_will_never_exist").message;

  it("reads a non-empty table from the server", () => {
    // A regex that silently matched nothing would make every assertion below
    // vacuous, which is the failure mode this whole rewrite is about.
    expect(server.size).toBeGreaterThan(5);
    expect(ui.size).toBeGreaterThan(5);
  });

  it("has its own sentence for every code the server chooses deliberately", () => {
    for (const [code, kind] of server) {
      if (kind !== "named") continue;
      expect(socialRefusal(code).message, code).not.toBe(generic);
    }
  });

  it("keeps the deliberately coarse failures generic", () => {
    // Vague on the server on purpose: naming the failed verification step is a
    // hint for whoever caused it, and it travels in browser history and proxy
    // logs. Nothing here should un-blur them.
    for (const [code, kind] of server) {
      if (kind !== "coarse") continue;
      expect(socialRefusal(code).message, code).toBe(generic);
    }
  });

  it("has no copy for a code the server cannot emit", () => {
    const orphans = [...ui].filter((code) => !server.has(code));
    expect(
      orphans,
      "copy for codes no server path emits: either the server stopped sending them or they were never sent",
    ).toEqual([]);
  });
});

// 2.31. The redirect target is validated on both sides. The server is the
// authority (auth.safeReturnPath), but the app must never put an off-site URL
// in an outbound sign-in link, or navigate to one, either.
//
// Each case is its own test so a failure names the value. The clauses of the
// helper's contract (see its doc comment) are held one group at a time, and the
// names say which clause a case isolates: a value the URL parser would send to
// another host is refused by the lexical rules AND by the parser's verdict, so
// the cases that only one of them can refuse are the ones that show each is
// wired.
describe("sameOriginPath", () => {
  // Read once: the cases below that name this very origin must follow it.
  const origin = window.location.origin;
  const host = window.location.host;

  // The address an AI app's browser sign-in opens for someone who is signed
  // out, in the order a standard MCP client sends it. The query has to come
  // back byte for byte: its %20 and its parameter order are what the consent
  // screen reads.
  const CONSENT_LINK =
    "/connect/ai?response_type=code&client_id=c1-abc" +
    "&redirect_uri=http%3A%2F%2Flocalhost%3A61695%2Fcallback" +
    "&state=a~b-_c&code_challenge=E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" +
    "&code_challenge_method=S256&scope=mcp%3Aread%20mcp%3Asite";

  // Same-origin addresses that must come back exactly as given. The last four
  // are the over-fire arm: an encoded slash, backslash or tab is plain text to
  // the URL parser (it does not decode them), and `//` inside a query or hash
  // is not a host.
  const HONEST: readonly (readonly [why: string, value: string])[] = [
    ["the root", "/"],
    ["a path", "/sites/abc"],
    ["a path with a query", "/sites?tab=backups"],
    ["a path with a query and a hash", "/sites?x=1#y"],
    ["the consent link", CONSENT_LINK],
    ["an encoded slash after the slash", "/%2F/evil.example"],
    ["an encoded backslash after the slash", "/%5Cevil.example"],
    ["an encoded tab between the slashes", "/%09/evil.example"],
    ["slashes inside a query value", "/sites?next=//evil.example"],
    ["slashes inside a hash", "/sites#//evil.example"],
  ];

  it.each(HONEST)("keeps %s unchanged: %j", (_why, value) => {
    expect(sameOriginPath(value)).toBe(value);
  });

  // Values that must give nothing. The comments say what the URL parser makes
  // of each group, because that is why the group is here.
  const HOSTILE: readonly (readonly [why: string, value: string])[] = [
    ["an empty value", ""],

    // Not rooted. The parser resolves these two against this origin, so only
    // the rooted-path rule refuses them.
    ["a relative path with no leading slash", "sites/abc"],
    ["a query with no path", "?x=1"],
    // Not rooted either, and the parser trims a leading space or control
    // character before it reads the slashes, so these two read as a host.
    ["a space before a protocol-relative reference", " //evil.example"],
    ["a NUL before a protocol-relative reference", "\u0000//evil.example"],

    // The parser deletes tab, CR and LF before it reads the slashes, so the
    // first three read as "//evil.example/x". The last two stay on this origin
    // once the tab is gone and are refused anyway: the parser reads a different
    // string than the one shown.
    ["a tab between the slashes", "/\t/evil.example/x"],
    ["a line feed between the slashes", "/\n/evil.example/x"],
    ["a carriage return and line feed between the slashes", "/\r\n/evil.example/x"],
    ["a tab at the end", "/sites\t"],
    ["a tab inside a segment", "/si\ttes"],

    // The parser reads "\" as "/". The last one lands on this origin, at
    // /sites/x, and is refused anyway.
    ["a backslash after the slash", "/\\evil.example"],
    ["a backslash first", "\\/evil.example"],
    ["two backslashes after the slash", "/\\\\evil.example"],
    ["a backslash inside a path", "/sites\\x"],

    ["a protocol-relative reference", "//evil.example"],
    ["a protocol-relative reference with a path", "//evil.example/steal"],
    ["an absolute URL", "https://evil.example/x"],
    ["a javascript URL", "javascript:alert(1)"],
    ["a data URL", "data:text/html,x"],

    // Control characters. The parser keeps each of these on this origin (it
    // percent-encodes them in the middle of a path and trims them from the end
    // of a value), so only the control-character rule refuses them.
    ["a NUL after the slash", "/\u0000/evil.example"],
    ["a NUL at the end", "/sites\u0000"],
    ["a start-of-heading control", "/sites\u0001"],
    ["a unit separator", "/\u001f/x"],
    ["DEL", "/sites\u007f"],
    ["a C1 control (next line)", "/sites\u0085"],

    // On this origin, and still not a rooted path.
    ["an absolute URL that names this origin", `${origin}/sites`],
    ["a protocol-relative reference that names this host", `//${host}/sites`],
  ];

  it.each(HOSTILE)("refuses %s: %j", (_why, value) => {
    expect(sameOriginPath(value)).toBeUndefined();
  });

  it("refuses a missing value", () => {
    expect(sameOriginPath(undefined)).toBeUndefined();
  });

  it("caps the length at 512", () => {
    expect(sameOriginPath("/" + "a".repeat(511))).toBe("/" + "a".repeat(511));
    expect(sameOriginPath("/" + "a".repeat(512))).toBeUndefined();
  });

  // The parse-and-compare step. Nothing a lexical rule lets through can be put
  // on another origin by a real parser, so these three cases are the ones that
  // reach it: a different page origin, an origin the parser cannot use as a
  // base, and a parser that disagrees.
  describe("against the page's own origin", () => {
    function withLocation(location: object, run: () => void) {
      const original = window.location;
      Object.defineProperty(window, "location", {
        configurable: true,
        writable: true,
        value: location,
      });
      try {
        run();
      } finally {
        Object.defineProperty(window, "location", {
          configurable: true,
          writable: true,
          value: original,
        });
      }
    }

    it("follows the page's origin rather than a fixed one", () => {
      withLocation({ origin: "https://app.example.test" }, () => {
        expect(sameOriginPath("/sites?x=1")).toBe("/sites?x=1");
        expect(sameOriginPath("https://app.example.test/sites")).toBeUndefined();
      });
    });

    it("refuses everything when the page has no usable origin", () => {
      for (const location of [{ origin: "null" }, {}]) {
        withLocation(location, () => {
          expect(sameOriginPath("/sites"), JSON.stringify(location)).toBeUndefined();
        });
      }
    });

    it("takes the URL parser's verdict as final", () => {
      // A value the lexical rules accept, put on another origin by the parser.
      let result: string | undefined = "unset";
      vi.stubGlobal(
        "URL",
        class {
          origin = "https://evil.example";
        },
      );
      try {
        result = sameOriginPath("/sites");
      } finally {
        vi.unstubAllGlobals();
      }
      expect(result).toBeUndefined();
    });
  });
});
