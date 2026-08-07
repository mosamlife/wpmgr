// Split out of social-buttons.tsx so that file exports only components, which
// is what keeps fast refresh working during development.

/**
 * What the sign-in page should say and offer for a `?social_error=` code.
 *
 * `canResend` is the part that was missing. Two of these codes refuse an
 * account that has never been verified, and the server mails a link on exactly
 * one of them (social_link_requires_verification, via
 * Service.sendVerificationForSocialLink); the status gate that produces
 * email_not_verified mails nothing. Telling someone to check an inbox nothing
 * was sent to, with no way to send it, is a dead end: they cannot verify by
 * signing in with a password and they cannot verify by resetting one, because
 * only opening a verification link writes email_verified_at.
 *
 * So each refusal declares whether a verification email can unblock it, and the
 * page renders the control that sends one. Codes where mail cannot help
 * (a disabled account, a provider that will not vouch for its own user's
 * address) say so and offer nothing, because offering a button that changes
 * nothing is worse than offering none.
 */
export type SocialRefusal = {
  message: string;
  canResend: boolean;
};

export function socialRefusal(code: string): SocialRefusal {
  switch (code) {
    case "social_link_requires_verification":
      // The server has already sent the link at this point, so the copy says
      // that rather than sending the reader off to reset a password.
      return {
        message:
          "An account already exists for that email address and has never been verified. We have emailed a verification link to it. Open the link, then sign in with your provider again.",
        canResend: true,
      };
    case "email_not_verified":
      // No mail was sent: this is the status gate refusing a pending account,
      // and nothing on this path mails anything. Hence the offer below.
      return {
        message:
          "That account has not been verified yet, so it cannot sign in. Enter its email address below and send yourself a verification link, then open the link and try again.",
        canResend: true,
      };
    case "account_disabled":
      return {
        message:
          "This account is disabled, so it cannot sign in by any method. Contact your organisation's owner.",
        canResend: false,
      };
    case "social_email_unverified":
      return {
        message:
          "That account has no verified email address with the provider. Verify your email with the provider, then try again.",
        canResend: false,
      };
    case "user_exists":
      // A local account already holds this address. Verification is not the
      // blocker here, so no resend is offered.
      return {
        message:
          "An account already exists for that email address. Sign in with your password, then connect the provider from your account settings.",
        canResend: false,
      };
    case "social_cancelled":
      return { message: "Sign-in was cancelled.", canResend: false };
    case "social_provider_disabled":
      return {
        message: "That sign-in method is not enabled on this instance.",
        canResend: false,
      };
    case "social_state_mismatch":
      return { message: "That sign-in link expired. Please try again.", canResend: false };
    default:
      return { message: "Sign-in failed. Please try again.", canResend: false };
  }
}

/** The sentence alone, for callers that render no controls. */
export function socialErrorMessage(code: string): string {
  return socialRefusal(code).message;
}

/**
 * Narrows a `?redirect=` target to a path on this origin, or undefined.
 *
 * The server validates it again (auth.safeReturnPath) and is the authority;
 * this exists so the app never asks the server to honour something it should
 * not, and never puts an off-site URL in an outbound link. "//host" and
 * "/\host" are protocol-relative and read as another origin, which is exactly
 * what they do not look like.
 */
export function sameOriginPath(raw: string | undefined): string | undefined {
  if (!raw || raw.length > 512) return undefined;
  if (!raw.startsWith("/")) return undefined;
  if (raw.startsWith("//") || raw.startsWith("/\\")) return undefined;
  return raw;
}
