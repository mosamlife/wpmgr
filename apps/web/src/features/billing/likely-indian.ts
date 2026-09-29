// S0.4 (stripe-design-v9.md 5.10) — the "likely-Indian user" signal that
// gates the Razorpay payment option, the Indian-card guidance copy, the
// decline banner, and the Stripe upgrade-mandate copy.
//
// `likelyIndian` = currency hint INR (welcome.checkout.tsx's own `?currency=`
// / pending-plan stash), OR the browser's IANA timezone is Asia/Kolkata or
// Asia/Calcutta (the two spellings ICU has used for India over time).
//
// Kept dependency-free and pure so the decision is unit-testable without
// mocking `Intl` at all — `currentTimezone()` is the one function that reads
// the ambient environment, isolated so callers use it exactly once, in a
// lazy `useState` initializer (never read fresh on every render — see
// use-billing.ts's react-hooks/purity note for why that convention exists
// here).

const INDIAN_TIMEZONES = new Set(["Asia/Kolkata", "Asia/Calcutta"]);

export function isLikelyIndianTimezone(timeZone: string | undefined): boolean {
  return timeZone !== undefined && INDIAN_TIMEZONES.has(timeZone);
}

export function isLikelyIndian(params: {
  currencyHint?: string;
  timeZone?: string;
}): boolean {
  if (params.currencyHint === "INR") return true;
  return isLikelyIndianTimezone(params.timeZone);
}

/** Best-effort read of the browser's resolved IANA timezone. Never throws. */
export function currentTimezone(): string | undefined {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone;
  } catch {
    return undefined;
  }
}
