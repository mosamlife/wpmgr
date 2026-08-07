import { describe, it, expect } from "vitest";

import { socialErrorMessage } from "./social-errors";

// The API can only ever put a code in the address bar. If this switch has no
// case for it, the refusal reads as "Sign-in failed. Please try again.", which
// is the generic sentence the code existed to replace: the whole server-side
// feature is then invisible to the person it was written for.
//
// social_provider_already_linked shipped exactly like that. The Go test
// apps/api/internal/auth/social_error_vocabulary_test.go enforces that every
// code the API emits appears here at all; this one pins that the codes with an
// ACTION behind them actually say something different, since a case that
// returns the generic string would satisfy the other test alone.

const generic = "Sign-in failed. Please try again.";

describe("socialErrorMessage", () => {
  it("gives an unknown code the generic sentence", () => {
    expect(socialErrorMessage("something_new")).toBe(generic);
  });

  // Each of these tells the reader which account to use, what to fix, or how
  // long to wait. None of them is answered by "try again".
  it.each([
    "social_link_requires_verification",
    "social_email_unverified",
    "social_provider_already_linked",
    "account_disabled",
    "email_not_verified",
    "social_cancelled",
    "social_provider_disabled",
    "social_state_mismatch",
    "social_rate_limited",
    "social_start_failed",
  ])("says something specific for %s", (code) => {
    expect(socialErrorMessage(code)).not.toBe(generic);
  });

  // The refusal a second account at the same provider produces has to name the
  // way out, because retrying reproduces it forever.
  it("tells a second account at the same provider what to do", () => {
    const msg = socialErrorMessage("social_provider_already_linked");
    expect(msg).toMatch(/connected/i);
    expect(msg).toMatch(/disconnect|sign in with/i);
  });

  // Deliberately coarse: naming the step that failed would tell whoever caused
  // it how far they got.
  it.each(["social_no_code", "social_exchange_failed", "social_sign_in_failed"])(
    "keeps %s generic on purpose",
    (code) => {
      expect(socialErrorMessage(code)).toBe(generic);
    },
  );
});
