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
 *
 * Where the copy still names a password reset, it is not being offered as a way
 * to verify (it is not one) but as the way to take back an account somebody
 * else may have created on that address. See the link refusal below.
 *
 * THE CODE LIST IS THE SERVER'S. Every value that can reach `?social_error=`
 * comes from a socialFail call site or actionableSocialCodes in
 * apps/api/internal/auth/social_handler.go. A code with no case here still
 * renders, as the generic sentence, which is the right answer for the
 * deliberately coarse failures (exchange, no code) and the wrong one for a
 * refusal a person could act on. social-errors.test.ts pins the actionable set
 * so a code added on the server cannot quietly land in the generic bucket.
 */
export type SocialRefusal = {
  message: string;
  canResend: boolean;
};

export function socialRefusal(code: string): SocialRefusal {
  switch (code) {
    case "social_link_requires_verification":
      // Two facts, and the copy has to carry both.
      //
      // The server really has sent the link by this point
      // (Service.sendVerificationForSocialLink), so "check your inbox" is
      // accurate here, unlike on the status-gate code below.
      //
      // But the account this link verifies is one that SOMEBODY ELSE may have
      // created: the refusal exists because registration does not require
      // proving control of an address, so an attacker can park a row on a
      // stranger's address with a password only they know. Opening the link
      // verifies that row and the next provider sign-in links onto it, with
      // that password still on it. Resetting the password is what takes it
      // back, so the sentence that says so is a mitigation and not filler.
      return {
        message:
          "An account already exists for that email address and has never been verified. We have sent a verification link to it. Open the link, then sign in with your provider again. If you did not create that account yourself, reset its password too, so that whoever set it cannot sign in.",
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
    case "social_email_unreachable":
      // NOT the same answer as the case above, even though both are about an
      // email address. This address IS verified. It is the provider's own
      // outbound-only placeholder, so telling this person to verify their email
      // sends them looking for a problem they do not have. In GitHub the fix is
      // one setting.
      //
      // canResend is false and cannot be anything else: the whole refusal is
      // that nothing can be delivered to this address, so a button offering to
      // send a link there would be a button that does nothing.
      return {
        message:
          'That account keeps its email address private, so there is no address we can reach you at. In GitHub, open Settings then Emails and turn off "Keep my email addresses private", then try again.',
        canResend: false,
      };
    case "social_provider_already_linked":
      // A different account at the same provider is already connected to the
      // local account for this address. Retrying reproduces it exactly, so the
      // only useful answer names the other way in. Deliberately does NOT offer
      // "disconnect it in account settings": there is no such screen in this
      // app, and pointing at one is worse than saying nothing.
      return {
        message:
          "A different account at that provider is already connected to the account for this email address. Sign in with the provider account you connected first, or use your email and password.",
        canResend: false,
      };
    case "social_rate_limited":
      return {
        message:
          "Too many sign-in attempts from this network. Wait a minute, then try again.",
        canResend: false,
      };
    case "social_start_failed":
      return {
        message:
          "That sign-in provider could not be reached. Try again in a moment, or use your email and password.",
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
