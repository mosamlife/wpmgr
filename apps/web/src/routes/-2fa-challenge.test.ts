import { describe, it, expect } from "vitest";
import { getErrorMessage } from "./2fa-challenge";

// GH #215: the 2FA challenge screen's error banner must distinguish a real
// server configuration problem (the operator's TOTP secret can no longer be
// decrypted because a self-host's secrets-at-rest key changed) from an
// ordinary wrong code, so the operator doesn't waste time re-typing a code
// that will never work. Pins the new `totp_decrypt_failed` copy and locks
// the existing 401/410 copy against regression.
describe("getErrorMessage — 2fa-challenge banner copy", () => {
  it("surfaces a distinct, actionable message for `totp_decrypt_failed`, pointing at recovery codes", () => {
    const message = getErrorMessage("totp_decrypt_failed");
    expect(message).toContain("encryption key changed");
    expect(message).toContain("not a wrong code");
    expect(message).toContain("recovery code");
    expect(message).toContain("WPMGR_SITE_DEST_AGE_SECRET");
    // Must NOT fall through to the generic banner text.
    expect(message).not.toBe("Verification failed. Please try again.");
  });

  it("still maps a plain invalid code to \"Incorrect code\" (no regression)", () => {
    expect(getErrorMessage("invalid_code")).toBe(
      "Incorrect code. Please check and try again.",
    );
  });

  it("still maps challenge_expired to the session-expired copy (no regression)", () => {
    expect(getErrorMessage("challenge_expired")).toBe(
      "This login session has expired. Please sign in again.",
    );
  });

  it("still maps too_many_attempts to the locked-session copy (no regression)", () => {
    expect(getErrorMessage("too_many_attempts")).toBe(
      "Too many failed attempts. This session is locked. Please sign in again.",
    );
  });

  it("falls back to the generic message for an unrecognized code", () => {
    expect(getErrorMessage("some_future_code")).toBe(
      "Verification failed. Please try again.",
    );
  });
});
