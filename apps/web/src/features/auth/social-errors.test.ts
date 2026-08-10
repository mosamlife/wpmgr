import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { describe, it, expect } from "vitest";

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
// in an outbound sign-in link either.
describe("sameOriginPath", () => {
  it("keeps a path on this origin", () => {
    expect(sameOriginPath("/sites/abc")).toBe("/sites/abc");
    expect(sameOriginPath("/sites?tab=backups")).toBe("/sites?tab=backups");
  });

  it("drops anything that could leave this origin", () => {
    for (const raw of [
      undefined,
      "",
      "sites/abc",
      "https://evil.example/steal",
      "//evil.example/steal",
      "/\\evil.example/steal",
      "javascript:alert(1)",
      "/" + "a".repeat(600),
    ]) {
      expect(sameOriginPath(raw), String(raw)).toBeUndefined();
    }
  });
});
