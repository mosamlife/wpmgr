// Build-time (SSG) fetch of the control plane's public pricing endpoint
// (GET /api/v1/pricing). This is SERVER-ONLY: it runs once, at `next build`,
// when the /pricing page is statically rendered -- never in the browser, so
// there is no runtime network dependency for a visitor and no CSP concern.
//
// Every failure mode (network error, non-2xx status, unexpected JSON shape,
// or a slow/unreachable host) resolves to `null` so the caller
// (lib/content/pricing.ts's resolveTierPrices) can always fall back to the
// static PRICING_TIERS amounts -- the marketing build must never fail or
// hang because the control plane is unreachable.

const DEFAULT_API_URL = "https://manage.wpmgr.app";
const FETCH_TIMEOUT_MS = 5000;

/** One concrete, live price point in the currency's smallest unit (cents/paise). */
export type LivePriceQuote = {
  amount: number;
  currency: string;
  interval: string;
};

/**
 * One tier entry as served by GET /api/v1/pricing. The free tier uses the
 * flat `amount`/`currency`/`interval` fields; a paid tier uses the nested
 * `usd`/`inr` sub-objects (only the currencies this instance actually
 * resolved a live price for -- see apps/api/internal/pricing/service.go's
 * TierPricing.MarshalJSON).
 */
export type LiveTier = {
  id: string;
  amount?: number;
  currency?: string;
  interval?: string;
  usd?: LivePriceQuote;
  inr?: LivePriceQuote;
};

export type LivePricingResponse = {
  currency_default: string;
  tiers: LiveTier[];
};

function isQuote(value: unknown): value is LivePriceQuote {
  if (typeof value !== "object" || value === null) return false;
  const v = value as Record<string, unknown>;
  return (
    typeof v.amount === "number" &&
    typeof v.currency === "string" &&
    typeof v.interval === "string"
  );
}

function isTier(value: unknown): value is LiveTier {
  if (typeof value !== "object" || value === null) return false;
  const v = value as Record<string, unknown>;
  if (typeof v.id !== "string") return false;
  if (v.amount !== undefined && typeof v.amount !== "number") return false;
  if (v.currency !== undefined && typeof v.currency !== "string") return false;
  if (v.interval !== undefined && typeof v.interval !== "string") return false;
  if (v.usd !== undefined && !isQuote(v.usd)) return false;
  if (v.inr !== undefined && !isQuote(v.inr)) return false;
  return true;
}

function isPricingResponse(value: unknown): value is LivePricingResponse {
  if (typeof value !== "object" || value === null) return false;
  const v = value as Record<string, unknown>;
  return (
    typeof v.currency_default === "string" &&
    Array.isArray(v.tiers) &&
    v.tiers.every(isTier)
  );
}

/**
 * Fetches GET /api/v1/pricing from WPMGR_API_URL (default
 * https://manage.wpmgr.app) at build time. Returns `null` on any failure --
 * network error, non-2xx status, unexpected JSON shape, or a host that does
 * not answer within FETCH_TIMEOUT_MS -- so the caller can fall back to
 * static pricing without ever failing or hanging the build.
 */
export async function fetchLivePricing(): Promise<LivePricingResponse | null> {
  const apiUrl = process.env.WPMGR_API_URL || DEFAULT_API_URL;
  try {
    // No `cache: "no-store"` here on purpose: this fetch must stay
    // cacheable so Next treats /pricing as a fully static route rendered
    // once at build time (SSG), not a per-request dynamic route. A
    // no-store/uncached fetch during static generation makes Next bail the
    // whole route to server-rendered-on-demand, which would turn every
    // production page view into a live call to the CP instead of a
    // baked-at-build-time price.
    const res = await fetch(`${apiUrl}/api/v1/pricing`, {
      signal: AbortSignal.timeout(FETCH_TIMEOUT_MS),
    });
    if (!res.ok) {
      console.warn(
        `pricing: GET ${apiUrl}/api/v1/pricing returned ${res.status}, using static fallback prices`,
      );
      return null;
    }
    const json: unknown = await res.json();
    if (!isPricingResponse(json)) {
      console.warn(
        "pricing: unexpected /api/v1/pricing response shape, using static fallback prices",
      );
      return null;
    }
    return json;
  } catch (err) {
    console.warn(
      "pricing: /api/v1/pricing fetch failed, using static fallback prices",
      err,
    );
    return null;
  }
}
