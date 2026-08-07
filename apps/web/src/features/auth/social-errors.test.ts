import { describe, expect, it } from "vitest";

import { socialErrorMessage } from "./social-errors";

describe("socialErrorMessage", () => {
  // The control plane refuses a GitHub account whose primary address is the
  // provider's outbound-only privacy address, because an account built on one
  // can never be sent a verification link, a password reset or an alert. That
  // refusal only helps if the sentence names the setting to change.
  it("tells a GitHub user with a private address what to change", () => {
    const msg = socialErrorMessage("social_email_unreachable");
    expect(msg).toContain("private");
    expect(msg).toContain("Keep my email addresses private");
  });

  // It must NOT collapse into the unverified-email advice: that address is
  // verified, so "verify your email" describes a problem that is not there.
  it("does not repeat the unverified-email advice", () => {
    expect(socialErrorMessage("social_email_unreachable")).not.toEqual(
      socialErrorMessage("social_email_unverified"),
    );
  });

  it("falls back to a generic sentence for an unknown code", () => {
    expect(socialErrorMessage("something_new")).toBe(
      "Sign-in failed. Please try again.",
    );
  });
});
