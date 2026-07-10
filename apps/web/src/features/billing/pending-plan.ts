import { CHECKOUT_TIER_IDS, BILLING_CURRENCIES } from "./plan-catalog";
import type { BillingCurrency, CheckoutTierId } from "./use-billing";

// M16 Phase C2 — signup-to-premium (WPMgr, not to be confused with any
// third-party "premium" naming): a same-browser fast path for the plan
// chosen at `/register?plan=...`.
//
// The CANONICAL carrier of the chosen plan is always the backend:
// `RegisterRequest.plan` is persisted against the email-verification token
// and surfaced back as `Me.desired_plan` once verified (or immediately on
// the first-run bootstrap path) — see use-auth.ts's `useRegister` /
// `useVerifyEmail`. This stash is ONLY a convenience layered on top of that:
//   - `/register`'s "check your email" branch stashes the plan so a same
//     browser opening the verification link in a new tab can still recover a
//     `?currency=` hint the backend response doesn't carry.
//   - `/welcome/checkout` falls back to it when its own `?plan=` URL param is
//     missing (e.g. a bookmarked/retried link).
//
// Never treated as authoritative for WHETHER to start a checkout — only
// `Me.desired_plan` + `Me.hosted` decide that (see verify-email.tsx /
// register.tsx). Both localStorage and a short-lived first-party cookie are
// written so the read side has a fallback if either storage mechanism is
// blocked (private browsing, cookie-only, etc).

const STORAGE_KEY = "wpmgr_pending_plan";
const COOKIE_NAME = "wpmgr_pending_plan";
// Long enough to open a verification email and click through, short enough
// that a stale, abandoned signup intent does not linger indefinitely.
const COOKIE_MAX_AGE_SECONDS = 60 * 60;

export interface PendingPlan {
  plan: CheckoutTierId;
  currency?: BillingCurrency;
}

function isPendingPlan(value: unknown): value is PendingPlan {
  if (typeof value !== "object" || value === null) return false;
  const v = value as Record<string, unknown>;
  const planOk =
    typeof v.plan === "string" &&
    (CHECKOUT_TIER_IDS as readonly string[]).includes(v.plan);
  const currencyOk =
    v.currency === undefined ||
    (typeof v.currency === "string" &&
      (BILLING_CURRENCIES as readonly string[]).includes(v.currency));
  return planOk && currencyOk;
}

/** Stash the chosen plan for the same-browser fast path. Best-effort; never throws. */
export function stashPendingPlan(pending: PendingPlan): void {
  try {
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(pending));
  } catch {
    // localStorage blocked (e.g. private mode with storage denied) — the
    // cookie below is the fallback read path.
  }
  try {
    const value = encodeURIComponent(JSON.stringify(pending));
    document.cookie = `${COOKIE_NAME}=${value}; max-age=${COOKIE_MAX_AGE_SECONDS}; path=/; SameSite=Lax`;
  } catch {
    // document.cookie can throw under some privacy settings — best effort only.
  }
}

/** Read the stashed plan, if any. localStorage first, cookie fallback. */
export function readPendingPlan(): PendingPlan | null {
  return readFromLocalStorage() ?? readFromCookie();
}

/** Clear the stash — call once a checkout has started, or the user skips. */
export function clearPendingPlan(): void {
  try {
    window.localStorage.removeItem(STORAGE_KEY);
  } catch {
    // ignore — nothing to clean up if it was never written.
  }
  try {
    document.cookie = `${COOKIE_NAME}=; max-age=0; path=/; SameSite=Lax`;
  } catch {
    // ignore
  }
}

function readFromLocalStorage(): PendingPlan | null {
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (!raw) return null;
    const parsed: unknown = JSON.parse(raw);
    return isPendingPlan(parsed) ? parsed : null;
  } catch {
    return null;
  }
}

function readFromCookie(): PendingPlan | null {
  if (typeof document === "undefined") return null;
  const match = document.cookie
    .split("; ")
    .find((row) => row.startsWith(`${COOKIE_NAME}=`));
  if (!match) return null;
  try {
    const raw = decodeURIComponent(match.slice(COOKIE_NAME.length + 1));
    const parsed: unknown = JSON.parse(raw);
    return isPendingPlan(parsed) ? parsed : null;
  } catch {
    return null;
  }
}
