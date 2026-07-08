import { useEffect, useRef, useState } from "react";
import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from "@tanstack/react-query";
import { client } from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";

// M16 Phase B — tenant billing hooks.
//
// The billing endpoints are landing in the control plane in parallel with
// this web surface and are NOT yet in the generated @wpmgr/api SDK. Hand-
// rolled via the shared `client` (same credentials/baseUrl as every generated
// SDK call — see use-scan.ts / use-audit.ts for the identical pattern).
// Migrate to the generated `getBilling` / `createBillingCheckout` /
// `createBillingPortal` functions (re-exported from
// packages/openapi-client/src/index.ts) once the SDK regenerates against the
// landed contract.
//
//   GET  /api/v1/billing            (owner-only; 404 when not hosted)
//   POST /api/v1/billing/checkout        { tier, provider?, currency? } -> CheckoutResult
//   POST /api/v1/billing/checkout/verify { razorpay_payment_id, razorpay_subscription_id, razorpay_signature } -> { verified }
//   POST /api/v1/billing/portal          -> { url }
//   POST /api/v1/billing/cancel          (owner-only) -> { ok }

export type BillingPlanId = "free" | "starter" | "agency" | "scale";

/** Checkout only ever targets a paid tier; downgrading to free happens via the portal (cancel). */
export type CheckoutTierId = Exclude<BillingPlanId, "free">;

export function isCheckoutTier(id: BillingPlanId): id is CheckoutTierId {
  return id !== "free";
}

/** Payment provider a checkout can target — mirrors the CP's provider registry (internal/billing/provider.go). */
export type BillingProvider = "stripe" | "razorpay";

/**
 * Currency choice, meaningful ONLY for the Razorpay provider — Razorpay has
 * no single multi-currency price the way Stripe's own Price object does, so
 * the CP resolves a plan PER (tier, currency). Ignored by every other
 * provider.
 */
export type BillingCurrency = "USD" | "INR";

export type BillingPlanStatus =
  | "none"
  | "trialing"
  | "active"
  | "past_due"
  | "canceled"
  | "paused"
  | "comped";

export interface BillingMeter {
  used: number;
  limit: number;
}

/**
 * Data-driven usage meters. Phase C adds more dimensions (storage, seats,
 * ...) to this map without any web-side code change — every consumer reads
 * it via `Object.entries` (see usage-meter-list.tsx), never a hardcoded
 * field access.
 */
export type BillingMeters = Record<string, BillingMeter>;

export interface BillingInfo {
  plan: BillingPlanId;
  plan_status: BillingPlanStatus;
  current_period_end?: string;
  provider?: string;
  grace_until?: string;
  meters: BillingMeters;
  portal_available: boolean;
}

export const billingKeys = {
  all: ["billing"] as const,
  info: () => [...billingKeys.all, "info"] as const,
};

/**
 * GET /api/v1/billing. Returns `null` on 404 (instance not hosted) or 403
 * (caller not the tenant owner) instead of throwing, so callers can render a
 * calm "not available" state — mirrors `fetchMe`'s 401-returns-null
 * convention in use-auth.ts.
 */
async function fetchBilling(): Promise<BillingInfo | null> {
  const result = await client.get<{ 200: BillingInfo }>({
    url: "/api/v1/billing",
  });
  if (result.response?.status === 404) return null;
  if (result.response?.status === 403) return null;
  if (result.error) throw toError(result.error);
  return result.data ?? null;
}

export interface UseBillingOptions {
  /** Only owners on a hosted instance should ever call this. */
  enabled: boolean;
}

export function useBilling(
  options: UseBillingOptions,
): UseQueryResult<BillingInfo | null, Error> {
  return useQuery({
    queryKey: billingKeys.info(),
    queryFn: fetchBilling,
    enabled: options.enabled,
  });
}

/**
 * The data WPMgr's in-app Checkout.js modal needs to open a Razorpay
 * subscription checkout — mirrors the CP's `RazorpayCheckoutData` wire shape
 * exactly (apps/api/internal/billing/provider.go). `amount` is in the
 * currency's smallest unit (paise for INR, cents for USD), read
 * authoritatively off the Razorpay Plan — never computed client-side.
 */
export interface RazorpayCheckoutData {
  subscription_id: string;
  key_id: string;
  currency: string;
  amount: number;
}

/**
 * POST /api/v1/billing/checkout's response. Exactly one of `url`/`razorpay`
 * is populated depending on the chosen provider's checkout style:
 *   - Stripe: `url` is a hosted Checkout Session redirect target.
 *   - Razorpay: `razorpay` is handed straight to the in-app Checkout.js modal.
 */
export interface CheckoutResult {
  url?: string;
  razorpay?: RazorpayCheckoutData;
}

export interface CreateCheckoutVariables {
  tier: CheckoutTierId;
  /** Omitted defers to this tenant's already-pinned provider, then this instance's configured default ("stripe"). */
  provider?: BillingProvider;
  /** Razorpay-only; ignored (and omitted from the request body) for every other provider. */
  currency?: BillingCurrency;
}

/**
 * POST /api/v1/billing/checkout { tier, provider?, currency? } -> CheckoutResult.
 * The caller branches on which field the result populates — see
 * `routes/_authed/settings/billing.tsx`'s `startCheckout`.
 */
export function useCreateBillingCheckout(): UseMutationResult<
  CheckoutResult,
  Error,
  CreateCheckoutVariables
> {
  return useMutation({
    mutationFn: async ({ tier, provider, currency }) => {
      const body: { tier: string; provider?: string; currency?: string } = {
        tier,
      };
      if (provider) body.provider = provider;
      if (provider === "razorpay" && currency) body.currency = currency;
      const result = await client.post<{ 200: CheckoutResult }>({
        url: "/api/v1/billing/checkout",
        body,
      });
      if (result.error) throw toError(result.error);
      if (!result.data) throw new Error("Empty response");
      return result.data;
    },
  });
}

/**
 * The EXACT field names Razorpay's Checkout.js `handler` option hands the
 * browser on a successful payment, passed straight through as this request's
 * body so the frontend never renames them.
 */
export interface RazorpayCheckoutSuccess {
  razorpay_payment_id: string;
  razorpay_subscription_id: string;
  razorpay_signature: string;
}

export interface VerifyCheckoutResult {
  verified: boolean;
}

/**
 * POST /api/v1/billing/checkout/verify. UX-CONFIRMATION ONLY — per the CP's
 * own doc comment (`Service.VerifyCheckoutCallback`), this never mutates the
 * tenant's plan itself; the provider webhook remains the sole source of
 * truth. Callers must poll GET /api/v1/billing afterward (see
 * `useBillingCheckoutReturn`) regardless of whether this call succeeds or
 * fails — a bad/late signature here must never block the poll that will
 * eventually observe the real plan flip.
 */
export function useVerifyRazorpayCheckout(): UseMutationResult<
  VerifyCheckoutResult,
  Error,
  RazorpayCheckoutSuccess
> {
  return useMutation({
    mutationFn: async (payload) => {
      const result = await client.post<{ 200: VerifyCheckoutResult }>({
        url: "/api/v1/billing/checkout/verify",
        body: payload,
      });
      if (result.error) throw toError(result.error);
      if (!result.data) throw new Error("Empty response");
      return result.data;
    },
  });
}

export interface CancelSubscriptionResult {
  ok: boolean;
}

/**
 * POST /api/v1/billing/cancel (owner-only). Cancels the tenant's subscription
 * AT THE END OF THE CURRENT BILLING PERIOD, never immediately — the
 * provider's webhook then drives the non-destructive plan=free downgrade,
 * exactly like a natural expiry. Only relevant when `portal_available` is
 * false (e.g. Razorpay, which has no hosted billing-management portal to
 * cancel through instead).
 */
export function useCancelBillingSubscription(): UseMutationResult<
  CancelSubscriptionResult,
  Error,
  void
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      const result = await client.post<{ 200: CancelSubscriptionResult }>({
        url: "/api/v1/billing/cancel",
      });
      if (result.error) throw toError(result.error);
      if (!result.data) throw new Error("Empty response");
      return result.data;
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: billingKeys.info() });
    },
  });
}

export interface PortalResult {
  url: string;
}

/** POST /api/v1/billing/portal -> { url }. Only call when portal_available. */
export function useCreateBillingPortal(): UseMutationResult<
  PortalResult,
  Error,
  void
> {
  return useMutation({
    mutationFn: async () => {
      const result = await client.post<{ 200: PortalResult }>({
        url: "/api/v1/billing/portal",
      });
      if (result.error) throw toError(result.error);
      if (!result.data) throw new Error("Empty response");
      return result.data;
    },
  });
}

// ---------------------------------------------------------------------------
// Checkout-return polling — never trust the URL param for state.
// ---------------------------------------------------------------------------

export interface CheckoutPollSnapshot {
  plan: BillingPlanId;
  plan_status: BillingPlanStatus;
}

/**
 * Pure decision function for the checkout-return poll: should we keep
 * refetching billing? Kept dependency-free so it is fully unit-testable
 * without React or a QueryClient (see use-billing.test.ts).
 *
 * Keeps polling while the budget (default 30s) remains AND the current
 * snapshot still matches the pre-checkout baseline (the provider webhook
 * has not landed yet, or we have not fetched anything at all yet). Stops
 * once the plan/status changed from baseline, or the budget is spent.
 */
export function shouldPollCheckoutReturn(params: {
  elapsedMs: number;
  baseline: CheckoutPollSnapshot | null;
  current: CheckoutPollSnapshot | null;
  budgetMs?: number;
}): boolean {
  const budget = params.budgetMs ?? 30_000;
  if (params.elapsedMs >= budget) return false;
  if (!params.current) return true;
  if (!params.baseline) return false;
  return (
    params.current.plan === params.baseline.plan &&
    params.current.plan_status === params.baseline.plan_status
  );
}

export interface CheckoutReturnState {
  /** True while actively polling — renders the "finalizing" banner. */
  finalizing: boolean;
  /** True once the poll budget elapsed with no observed change. */
  timedOut: boolean;
}

const POLL_INTERVAL_MS = 2000;
const POLL_BUDGET_MS = 30_000;

function snapshotOf(billing: BillingInfo | null | undefined): CheckoutPollSnapshot | null {
  return billing ? { plan: billing.plan, plan_status: billing.plan_status } : null;
}

/**
 * Drives the "finalizing your subscription" state after a checkout
 * redirect. `checkout` is the raw `?checkout=` search param: a hint that a
 * recheck is worthwhile, never trusted for the actual plan state. The
 * billing query's own data (via `refetch`) is always the source of truth.
 *
 * All derived state (`finalizing`/`timedOut`) is computed inside effects and
 * stored via `useState` — never read from a ref or `Date.now()` during
 * render — so the render body stays pure (react-hooks/refs, react-hooks/purity).
 */
function initialCheckoutReturnState(
  checkout: "success" | "cancel" | undefined,
): CheckoutReturnState {
  return checkout === "success"
    ? { finalizing: true, timedOut: false }
    : { finalizing: false, timedOut: false };
}

export function useBillingCheckoutReturn(
  checkout: "success" | "cancel" | undefined,
  billing: BillingInfo | null | undefined,
  refetch: () => void,
): CheckoutReturnState {
  const [state, setState] = useState<CheckoutReturnState>(() =>
    initialCheckoutReturnState(checkout),
  );

  // Adjust state during render when `checkout` itself changes (e.g. a client
  // navigation swaps the search param without remounting this hook) — the
  // sanctioned "adjust state during render" pattern already used elsewhere in
  // this codebase (ShowOnceDialog in settings/api-keys.tsx, DestructiveConfirm's
  // prevOpen comparison), not an effect, so it never trips
  // react-hooks/set-state-in-effect.
  const [prevCheckout, setPrevCheckout] = useState(checkout);
  if (checkout !== prevCheckout) {
    setPrevCheckout(checkout);
    setState(initialCheckoutReturnState(checkout));
  }

  // "Latest ref" pattern: the poll effect below only re-arms when `checkout`
  // changes, so its interval closure would otherwise see a stale `billing`
  // from the render that started it. This sync effect keeps a ref current so
  // the interval tick (an effect callback, not render) can always read the
  // freshest snapshot.
  const billingRef = useRef(billing);
  useEffect(() => {
    billingRef.current = billing;
  }, [billing]);

  useEffect(() => {
    if (checkout !== "success") return;

    const start = Date.now();
    const baseline = snapshotOf(billingRef.current);
    refetch();

    const id = window.setInterval(() => {
      const elapsedMs = Date.now() - start;
      const current = snapshotOf(billingRef.current);
      const stillPolling = shouldPollCheckoutReturn({ elapsedMs, baseline, current });
      setState({
        finalizing: stillPolling,
        timedOut: !stillPolling && elapsedMs >= POLL_BUDGET_MS,
      });
      if (stillPolling) refetch();
    }, POLL_INTERVAL_MS);

    return () => window.clearInterval(id);
    // Intentionally only re-arms when `checkout` itself changes — `refetch`
    // is a stable TanStack Query function and `billingRef` is a ref (reads
    // through it need no dependency entry).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [checkout]);

  return state;
}
