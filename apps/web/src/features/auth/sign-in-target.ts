import { sameOriginPath } from "./social-errors";

/**
 * Where the browser goes once a person is signed in, or already was: the
 * same-origin address they arrived with, else the sites list. Passed straight to
 * `navigate(...)` or `redirect(...)`.
 */
export type SignInTarget = { readonly href: string } | { readonly to: "/sites" };

/**
 * The single place a `?redirect=` value becomes a navigation target, used by the
 * sign-in page and the 2FA challenge page at every point they hand a person
 * onward (after a password, after a second factor, and for a visitor who already
 * has a session).
 *
 * THE VALUE IS NARROWED FIRST, ALWAYS. `?redirect=` is the one search parameter
 * on those pages that a link gets to choose, and the target is a full address
 * string: the router hands an absolute URL to the browser as a document
 * navigation, so anything that is not a path on this origin has to be dropped
 * here, before it can reach `navigate` or `redirect`. A value that does not
 * survive `sameOriginPath` becomes the sites list.
 *
 * AN ADDRESS, NOT A PATH PLUS PARTS. The deep link keeps its query string, and a
 * consent request depends on all of it. `href` is the form that separates the
 * path from the search for the router; a `to` string with a `?` in it leaves the
 * query inside the path and only comes out right by way of the history round
 * trip.
 */
export function signInTarget(redirect: string | undefined): SignInTarget {
  const href = sameOriginPath(redirect);
  return href === undefined ? { to: "/sites" } : { href };
}
