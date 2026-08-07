// Split out of social-buttons.tsx so that file exports only components, which
// is what keeps fast refresh working during development.

/**
 * Turns a `?social_error=` code from the callback into something a person can
 * act on. The two that matter are not really errors: they are the policy
 * refusing to do something unsafe, and the reader needs to know what to do
 * next, not that a request failed.
 */
export function socialErrorMessage(code: string): string {
  switch (code) {
    case "social_link_requires_verification":
      return "An account already exists for that email address but has never been verified. Sign in with your password, or reset it, to confirm the account is yours. You can connect Google or GitHub afterwards.";
    case "social_email_unverified":
      return "That account has no verified email address. Verify your email with the provider, then try again.";
    case "account_disabled":
      return "This account is disabled. Contact your organisation's owner.";
    case "email_not_verified":
      return "Please verify your email address before signing in.";
    case "social_cancelled":
      return "Sign-in was cancelled.";
    case "social_provider_disabled":
      return "That sign-in method is not enabled on this instance.";
    case "social_state_mismatch":
      return "That sign-in link expired. Please try again.";
    default:
      return "Sign-in failed. Please try again.";
  }
}
