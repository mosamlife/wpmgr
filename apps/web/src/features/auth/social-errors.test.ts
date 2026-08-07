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
      "social_provider_already_linked",
      "social_rate_limited",
      "social_start_failed",
      "social_cancelled",
      "social_provider_disabled",
      "social_state_mismatch",
      "something_new_from_the_server",
    ]) {
      expect(socialRefusal(code).canResend, code).toBe(false);
    }
  });

  // The server picks these codes; this list is the contract. Source of truth is
  // apps/api/internal/auth/social_handler.go: actionableSocialCodes plus the
  // socialFail call sites that name a code directly. A code that reaches the
  // URL with no case here renders "Sign-in failed. Please try again.", which
  // for a refusal a person could act on is the same dead end this whole file
  // exists to remove.
  const ACTIONABLE_SERVER_CODES = [
    "social_link_requires_verification",
    "social_email_unverified",
    "social_email_unreachable",
    "social_provider_already_linked",
    "account_disabled",
    "email_not_verified",
    "social_provider_disabled",
    "social_rate_limited",
    "social_start_failed",
    "social_state_mismatch",
    "social_cancelled",
  ];

  it("has its own sentence for every code the server chooses deliberately", () => {
    const generic = socialRefusal("a_code_that_will_never_exist").message;
    for (const code of ACTIONABLE_SERVER_CODES) {
      expect(socialRefusal(code).message, code).not.toBe(generic);
    }
  });

  it("keeps the coarse failures generic", () => {
    // Deliberately vague on the server: naming the failed verification step is
    // a hint for whoever caused it. Nothing here should un-blur them.
    const generic = socialRefusal("a_code_that_will_never_exist").message;
    for (const code of ["social_exchange_failed", "social_no_code", "social_sign_in_failed"]) {
      expect(socialRefusal(code).message, code).toBe(generic);
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
