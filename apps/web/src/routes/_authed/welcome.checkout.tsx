import { createFileRoute, redirect, useNavigate } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";
import { z } from "zod";
import { Sparkles } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { PageHeader } from "@/components/shared/page-header";
import { ensureMe } from "@/features/auth/use-auth";
import {
  useBilling,
  useBillingCheckoutReturn,
  type CheckoutTierId,
  type BillingCurrency,
} from "@/features/billing/use-billing";
import { useCheckoutFlow } from "@/features/billing/use-checkout-flow";
import { PaymentMethodPicker } from "@/features/billing/payment-method-picker";
import { CheckoutReturnBanner } from "@/features/billing/checkout-return-banner";
import { planCatalogEntry } from "@/features/billing/plan-catalog";
import { readPendingPlan, clearPendingPlan } from "@/features/billing/pending-plan";

// M16 Phase C2 — the post-verify "finish signing up for a plan" screen.
// Reached from either:
//   - /register's first-account bootstrap branch (session established
//     immediately, Me.desired_plan present) — see routes/register.tsx.
//   - /verify-email's 200 branch (Me.desired_plan present) — see
//     routes/verify-email.tsx.
// Never a normal nav destination (no sidebar entry) — always arrived at via
// one of those two redirects, always carrying `?plan=`.

const checkoutSearchSchema = z.object({
  plan: z.enum(["starter", "agency", "scale"]).optional().catch(undefined),
  currency: z.enum(["USD", "INR"]).optional().catch(undefined),
});

/**
 * Resolve the target tier: the URL's own `?plan=` wins, falling back to the
 * same-browser localStorage/cookie stash (see pending-plan.ts). Exported as a
 * pure function so the "URL then stash" precedence is directly unit-testable
 * without a router/React harness.
 */
export function resolveCheckoutTier(
  searchPlan: CheckoutTierId | undefined,
  stashPlan: CheckoutTierId | undefined,
): CheckoutTierId | undefined {
  return searchPlan ?? stashPlan;
}

/**
 * Whether the Razorpay in-page success path should drop the operator into
 * Sites: only once a checkout actually completed AND the checkout-return
 * poll has concluded (found the plan flip, or timed out — either way there
 * is nothing more productive to show here). Exported for a direct,
 * timer-free unit test of the edge condition (mirrors
 * `shouldPollCheckoutReturn` in use-billing.ts).
 */
export function shouldDropIntoSitesAfterCheckout(params: {
  status: "success" | undefined;
  wasFinalizing: boolean;
  isFinalizing: boolean;
}): boolean {
  return params.status === "success" && params.wasFinalizing && !params.isFinalizing;
}

export const Route = createFileRoute("/_authed/welcome/checkout")({
  validateSearch: checkoutSearchSchema,
  beforeLoad: async ({ context, search }) => {
    const me = await ensureMe(context.queryClient);
    // Self-host safety: paid checkout only ever exists on a hosted instance.
    if (!me?.hosted) {
      throw redirect({ to: "/sites" });
    }
    const tier = resolveCheckoutTier(search.plan, readPendingPlan()?.plan);
    if (!tier) {
      throw redirect({ to: "/sites" });
    }
  },
  component: WelcomeCheckoutPage,
});

function WelcomeCheckoutPage() {
  const search = Route.useSearch();
  // Resolved ONCE at mount (lazy initializer) rather than re-derived every
  // render: the mount effect below clears the pending-plan stash right after
  // starting checkout, and re-reading the stash on a later render would
  // otherwise make an already-in-flight checkout's tier disappear.
  const [resolved] = useState(() => {
    const stash = readPendingPlan();
    return {
      tier: resolveCheckoutTier(search.plan, stash?.plan),
      currency: search.currency ?? stash?.currency,
    };
  });

  // beforeLoad already guarantees a resolvable tier + a hosted session before
  // this component ever mounts; this defensive check only covers a same-tick
  // race and keeps the child component's tier prop non-optional.
  if (!resolved.tier) return null;

  return (
    <WelcomeCheckoutContent tier={resolved.tier} currencyHint={resolved.currency} />
  );
}

function WelcomeCheckoutContent({
  tier,
  currencyHint,
}: {
  tier: CheckoutTierId;
  currencyHint: BillingCurrency | undefined;
}) {
  const navigate = useNavigate();
  const entry = planCatalogEntry(tier);
  // Guards the auto-start effect so React StrictMode's dev double-invoke
  // never fires two checkout-creation calls (same pattern as
  // verify-email.tsx's own `firedRef`).
  const firedRef = useRef(false);
  const [checkoutStatus, setCheckoutStatus] = useState<"success" | undefined>(
    undefined,
  );
  const wasFinalizingRef = useRef(false);

  const billing = useBilling({ enabled: true });
  const checkoutReturn = useBillingCheckoutReturn(
    checkoutStatus,
    billing.data,
    () => void billing.refetch(),
  );

  const {
    provider,
    currency,
    setProvider,
    setCurrency,
    startCheckout,
    isStarting,
    error,
  } = useCheckoutFlow({
    initialCurrency: currencyHint,
    // Only ever fires on the Razorpay in-page path — Stripe's own hosted
    // redirect leaves this page entirely (see checkout-return-banner.tsx's
    // module doc and the /billing compatibility redirect route).
    onCheckoutSuccess: () => setCheckoutStatus("success"),
  });

  useEffect(() => {
    if (firedRef.current) return;
    firedRef.current = true;
    startCheckout(tier);
    // The same-browser stash has done its job (resolved above, if it was
    // ever needed) — clear it now that a checkout has actually started.
    clearPendingPlan();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tier]);

  useEffect(() => {
    const wasFinalizing = wasFinalizingRef.current;
    wasFinalizingRef.current = checkoutReturn.finalizing;
    if (
      shouldDropIntoSitesAfterCheckout({
        status: checkoutStatus,
        wasFinalizing,
        isFinalizing: checkoutReturn.finalizing,
      })
    ) {
      void navigate({ to: "/sites" });
    }
  }, [checkoutStatus, checkoutReturn.finalizing, navigate]);

  function handleSkip() {
    clearPendingPlan();
    void navigate({ to: "/sites" });
  }

  return (
    <section className="mx-auto max-w-lg space-y-6 py-8">
      <PageHeader
        title={`Complete your ${entry?.name ?? "plan"} subscription`}
        subline="One more step and your paid features are live."
      />

      <CheckoutReturnBanner
        status={checkoutStatus}
        finalizing={checkoutReturn.finalizing}
        timedOut={checkoutReturn.timedOut}
      />

      <Card>
        <CardHeader className="space-y-1">
          <CardTitle className="flex items-center gap-2">
            <Sparkles aria-hidden="true" className="size-4 text-[var(--color-primary)]" />
            {entry?.name ?? "Plan"}
          </CardTitle>
          {entry ? <CardDescription>{entry.priceLabel}</CardDescription> : null}
        </CardHeader>
        <CardContent className="space-y-4">
          {entry ? (
            <ul className="space-y-1 text-sm text-muted-foreground">
              <li>{entry.sitesLimit} sites</li>
              <li>{entry.storageLabel}</li>
              <li>{entry.cadenceLabel}</li>
            </ul>
          ) : null}

          <PaymentMethodPicker
            provider={provider}
            onProviderChange={setProvider}
            currency={currency}
            onCurrencyChange={setCurrency}
          />

          {error ? (
            <p role="alert" className="text-sm text-destructive">
              {error.message}
            </p>
          ) : null}

          <Button
            type="button"
            className="w-full"
            disabled={isStarting}
            onClick={() => startCheckout(tier)}
          >
            {isStarting
              ? provider === "razorpay"
                ? "Opening…"
                : "Redirecting…"
              : "Continue to payment"}
          </Button>
        </CardContent>
      </Card>

      <div className="text-center">
        <Button type="button" variant="ghost" onClick={handleSkip}>
          Skip for now
        </Button>
      </div>
    </section>
  );
}
