import { describe, it, expect } from "vitest";

import { socialRefusal, socialErrorMessage, sameOriginPath } from "./social-errors";

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
      "user_exists",
      "social_cancelled",
      "social_provider_disabled",
      "social_state_mismatch",
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
    // has to match what actually happened rather than sending the reader off
    // to reset a password, which cannot clear this refusal at all.
    const { message } = socialRefusal("social_link_requires_verification");
    expect(message).toMatch(/emailed a verification link/i);
    expect(message).not.toMatch(/reset/i);
  });

  it("has a sentence for the duplicate-account refusal", () => {
    expect(socialRefusal("user_exists").message).toMatch(/already exists/i);
    expect(socialRefusal("user_exists").message).not.toBe(
      socialRefusal("anything_else").message,
    );
  });

  it("keeps socialErrorMessage answering with the sentence alone", () => {
    expect(socialErrorMessage("account_disabled")).toBe(
      socialRefusal("account_disabled").message,
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
