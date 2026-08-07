// Split out of social-buttons.tsx so that file exports only components, which
// is what keeps fast refresh working during development.

/**
 * Turns a `?social_error=` code from the callback into something a person can
 * act on. The ones that matter are not really errors: they are the policy
 * refusing to do something unsafe, and the reader needs to know what to do
 * next, not that a request failed.
 *
 * EVERY code the API can put in `social_error` needs a case here, including the
 * ones whose answer is the generic sentence. A code with no case is a code the
 * product never actually says: the API side of the work is invisible until this
 * switch turns it into words. The Go test
 * apps/api/internal/auth/social_error_vocabulary_test.go reads this file and
 * fails when the two sides drift.
 */
export function socialErrorMessage(code: string): string {
  switch (code) {
    case "social_link_requires_verification":
      return "An account already exists for that email address but has never been verified. Sign in with your password, or reset it, to confirm the account is yours. You can connect Google or GitHub afterwards.";
    case "social_email_unverified":
      return "That account has no verified email address. Verify your email with the provider, then try again.";
    case "social_provider_already_linked":
      return "This email address already belongs to an account here, and that account is connected to a different Google or GitHub account. Sign in with the one you connected first, or disconnect it in account settings before connecting this one.";
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
    case "social_rate_limited":
      return "Too many sign-in attempts from here. Wait a minute and try again.";
    case "social_start_failed":
      return "We could not reach that sign-in provider. Please try again in a moment.";

    // Deliberately generic, and listed anyway so the vocabulary is complete and
    // the choice is visible. Each of these is a failure whose detail would tell
    // whoever caused it which step it got to, and none of them has an action
    // beyond retrying.
    case "social_no_code":
    case "social_exchange_failed":
    case "social_sign_in_failed":
      return "Sign-in failed. Please try again.";

    default:
      return "Sign-in failed. Please try again.";
  }
}
