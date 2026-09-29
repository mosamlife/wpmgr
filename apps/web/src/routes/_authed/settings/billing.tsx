import { useEffect, useRef, useState } from "react";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { z } from "zod";
import { AlertTriangle } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { PageError } from "@/components/feedback";
import { PageHeader } from "@/components/shared/page-header";
import { DestructiveConfirm } from "@/components/dialogs/destructive-confirm";
import { toast } from "@/components/toast";
import { useMe, activeRole } from "@/features/auth/use-auth";
import {
  useBilling,
  useCreateBillingPortal,
  useCancelBillingSubscription,
  useBillingCheckoutReturn,
  useConfirmBillingCheckout,
  isCheckoutTier,
  type BillingInfo,
} from "@/features/billing/use-billing";
import {
  useCheckoutFlow,
  type UseCheckoutFlowResult,
} from "@/features/billing/use-checkout-flow";
import { mapBillingCheckoutError } from "@/features/billing/billing-checkout-error";
import {
  billingBannerFor,
  formatBillingDate,
  planStatusLabel,
} from "@/features/billing/billing-status";
import {
  PLAN_CATALOG,
  isDowngrade,
  exceedsPlanLimit,
  planLabel,
} from "@/features/billing/plan-catalog";
import { UsageMeterList } from "@/features/billing/usage-meter-list";
import { PaymentMethodPicker } from "@/features/billing/payment-method-picker";
import { CheckoutReturnBanner } from "@/features/billing/checkout-return-banner";
import { IndianCardGuidance } from "@/features/billing/indian-card-guidance";
import { IndianUpgradeCopy } from "@/features/billing/indian-upgrade-copy";
import { BillingTaxCopy } from "@/features/billing/billing-tax-copy";
import { isLikelyIndian, currentTimezone } from "@/features/billing/likely-indian";
import { cn } from "@/lib/utils";

// M16 Phase B, S0.4 — the tenant Billing settings page. Owner-only,
// hosted-only (the nav entry and this route both gate on the same me.hosted
// + activeRole==='owner' check — see routes/_authed/settings/route.tsx).

const billingSearchSchema = z.object({
  checkout: z.enum(["success", "cancel"]).optional(),
  // Stripe's checkout success URL carries this back (stripe-design-v9.md
  // 5.10) — kept across the redirect so confirm can be called with it.
  session_id: z.string().optional(),
});

export const Route = createFileRoute("/_authed/settings/billing")({
  validateSearch: billingSearchSchema,
  component: BillingPage,
});

function BillingPage() {
  const { data: me } = useMe();
  const hosted = me?.hosted === true;
  const isOwner = activeRole(me) === "owner";
  const search = Route.useSearch();
  const navigate = useNavigate({ from: Route.fullPath });

  const billing = useBilling({ enabled: hosted && isOwner });
  const checkoutReturn = useBillingCheckoutReturn(
    search.checkout,
    billing.data,
    () => void billing.refetch(),
  );

  // Resolved once on mount, not re-read every render (Intl reads belong in
  // a lazy initializer — see use-billing.ts's react-hooks/purity note).
  // This route carries no `?currency=` hint, so the signal is timezone only.
  const [likelyIndian] = useState(() =>
    isLikelyIndian({ timeZone: currentTimezone() }),
  );

  // 5.10: on a Stripe return, keep `session_id` and call confirm once — a
  // UX-confirmation speedup only, fire-and-forget. The poll above
  // (`useBillingCheckoutReturn`) remains the sole source of truth and
  // already renders "Activating…" while it runs, whether confirm answered
  // 200 or 202.
  const confirm = useConfirmBillingCheckout();
  const confirmFiredRef = useRef(false);
  useEffect(() => {
    if (confirmFiredRef.current) return;
    if (search.checkout !== "success" || !search.session_id) return;
    confirmFiredRef.current = true;
    confirm.mutate({ session_id: search.session_id });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [search.checkout, search.session_id]);

  // Razorpay's Checkout.js modal completes IN-PAGE (no browser redirect), so
  // there is no natural `?checkout=success` navigation the way Stripe's
  // hosted-redirect success URL provides one. Setting the same search param
  // here reuses `useBillingCheckoutReturn`'s existing poll/"finalizing your
  // subscription" UX verbatim, instead of inventing a second mechanism.
  function markCheckoutSuccess() {
    void navigate({
      search: (prev) => ({ ...prev, checkout: "success" }),
      replace: true,
    });
  }

  if (!hosted) {
    return (
      <BillingUnavailable message="Billing is not available on this installation." />
    );
  }

  if (!isOwner) {
    return (
      <BillingUnavailable message="Billing is managed by your organisation's owner." />
    );
  }

  if (billing.isPending) {
    return (
      <section className="max-w-3xl space-y-6">
        <PageHeader title="Billing" subline="Plan, usage, and payment details." />
        <div
          role="status"
          aria-label="Loading billing"
          className="h-64 animate-pulse rounded-xl bg-muted/50"
        />
      </section>
    );
  }

  if (billing.isError) {
    return (
      <section className="max-w-3xl space-y-6">
        <PageHeader title="Billing" subline="Plan, usage, and payment details." />
        <PageError
          what="Could not load billing details."
          why={billing.error.message}
          onRetry={() => void billing.refetch()}
          retryLabel="Reload"
        />
      </section>
    );
  }

  if (!billing.data) {
    return (
      <BillingUnavailable message="Billing is not available on this installation." />
    );
  }

  return (
    <BillingContent
      billing={billing.data}
      checkoutStatus={search.checkout}
      finalizing={checkoutReturn.finalizing}
      timedOut={checkoutReturn.timedOut}
      likelyIndian={likelyIndian}
      onCheckoutSuccess={markCheckoutSuccess}
    />
  );
}

function BillingUnavailable({ message }: { message: string }) {
  return (
    <section className="max-w-3xl space-y-6">
      <PageHeader title="Billing" />
      <div className="rounded-lg border border-border p-6 text-sm text-muted-foreground">
        {message}
      </div>
    </section>
  );
}

// ---------------------------------------------------------------------------
// Loaded state
// ---------------------------------------------------------------------------

function BillingContent({
  billing,
  checkoutStatus,
  finalizing,
  timedOut,
  likelyIndian,
  onCheckoutSuccess,
}: {
  billing: BillingInfo;
  checkoutStatus: "success" | "cancel" | undefined;
  finalizing: boolean;
  timedOut: boolean;
  likelyIndian: boolean;
  onCheckoutSuccess: () => void;
}) {
  const banner = billingBannerFor(billing);
  const portal = useCreateBillingPortal();
  const cancel = useCancelBillingSubscription();
  const [cancelOpen, setCancelOpen] = useState(false);

  // Lifted here (rather than owned by PlanTiersGrid alone) so the decline
  // banner's "Pay with Razorpay" action (3.3) can select the provider the
  // tier buttons below then use.
  const checkoutFlow = useCheckoutFlow({ onCheckoutSuccess });

  const openPortal = () => {
    portal.mutate(undefined, {
      onSuccess: (result) => {
        window.location.href = result.url;
      },
    });
  };

  async function performCancel() {
    try {
      await cancel.mutateAsync(undefined);
      setCancelOpen(false);
      toast.success(
        "Your subscription will be cancelled at the end of the current billing period",
      );
    } catch {
      // Error surfaces inside the confirm dialog via the mutation state.
    }
  }

  // "Do NOT show both": a tenant either has a hosted portal (Stripe) or
  // doesn't (Razorpay), never both. A free-plan tenant with no portal has no
  // subscription to cancel either, so neither action renders for it.
  const showManageBilling = billing.portal_available;
  const showCancelSubscription = !billing.portal_available && billing.plan !== "free";

  return (
    <section className="max-w-3xl space-y-6">
      <PageHeader title="Billing" subline="Plan, usage, and payment details." />

      <CheckoutReturnBanner
        status={checkoutStatus}
        finalizing={finalizing}
        timedOut={timedOut}
        likelyIndian={likelyIndian}
        razorpayAvailable={billing.available_providers.includes("razorpay")}
        onPayWithRazorpay={() => checkoutFlow.setProvider("razorpay")}
      />

      {banner ? (
        <div
          role="status"
          className={cn(
            "flex items-start gap-2 rounded-lg px-4 py-3 text-sm",
            banner.tone === "warning"
              ? "bg-[var(--color-warning-subtle)] text-[var(--color-warning-subtle-fg)]"
              : "bg-muted/40 text-muted-foreground",
          )}
        >
          <AlertTriangle aria-hidden="true" className="mt-px size-4 shrink-0" />
          <span>{banner.message}</span>
        </div>
      ) : null}

      <Card>
        <CardHeader className="flex flex-row flex-wrap items-start justify-between gap-3">
          <div className="space-y-1">
            <CardTitle className="flex items-center gap-2">
              <span className="capitalize">{billing.plan}</span>
              <Badge variant={billing.plan_status === "active" ? "default" : "muted"}>
                {planStatusLabel(billing.plan_status)}
              </Badge>
            </CardTitle>
            <CardDescription>
              {billing.current_period_end ? (
                <>Renews {formatBillingDate(billing.current_period_end)}</>
              ) : (
                "No active renewal date"
              )}
              {billing.provider ? (
                <>
                  {" "}
                  <span aria-hidden="true">&middot;</span> via{" "}
                  <span className="capitalize">{billing.provider}</span>
                </>
              ) : null}
            </CardDescription>
          </div>
          {showManageBilling ? (
            <div className="flex flex-col items-end gap-2">
              <Button
                type="button"
                variant="outline"
                disabled={portal.isPending}
                onClick={openPortal}
              >
                {portal.isPending ? "Opening…" : "Manage billing"}
              </Button>
            </div>
          ) : showCancelSubscription ? (
            <Button
              type="button"
              variant="outline"
              onClick={() => setCancelOpen(true)}
            >
              Cancel subscription
            </Button>
          ) : null}
        </CardHeader>
        <CardContent className="space-y-4">
          <UsageMeterList meters={billing.meters} />
          {showManageBilling &&
          likelyIndian &&
          billing.provider === "stripe" &&
          billing.plan !== "free" ? (
            <IndianUpgradeCopy />
          ) : null}
          {portal.isError ? (
            <p role="alert" className="text-sm text-destructive">
              {portal.error.message}
            </p>
          ) : null}
        </CardContent>
      </Card>

      <PlanTiersGrid
        billing={billing}
        portalAvailable={billing.portal_available}
        onManageBilling={openPortal}
        portalPending={portal.isPending}
        likelyIndian={likelyIndian}
        checkoutFlow={checkoutFlow}
      />

      <DestructiveConfirm
        open={cancelOpen}
        onClose={() => setCancelOpen(false)}
        onConfirm={performCancel}
        title="Cancel subscription"
        consequencesBody={
          <>
            Your {planLabel(billing.plan)} plan stays active through the end
            of the current billing period. After that, this workspace moves
            to the Free plan automatically. Nothing you have already backed
            up or configured is deleted.
          </>
        }
        resourceName={planLabel(billing.plan)}
        confirmLabel="Cancel subscription"
        cancelLabel="Keep subscription"
        isPending={cancel.isPending}
        errorMessage={cancel.isError ? cancel.error.message : null}
      />
    </section>
  );
}

// ---------------------------------------------------------------------------
// Plan tiers grid
// ---------------------------------------------------------------------------

function PlanTiersGrid({
  billing,
  portalAvailable,
  onManageBilling,
  portalPending,
  likelyIndian,
  checkoutFlow,
}: {
  billing: BillingInfo;
  portalAvailable: boolean;
  onManageBilling: () => void;
  portalPending: boolean;
  likelyIndian: boolean;
  checkoutFlow: UseCheckoutFlowResult;
}) {
  const { provider, setProvider, startCheckout, isStarting, error: checkoutError } =
    checkoutFlow;
  const usedSites = billing.meters.sites?.used ?? 0;
  const errorCopy = checkoutError
    ? mapBillingCheckoutError(
        checkoutError.code,
        checkoutError.reason,
        planLabel(billing.plan),
        checkoutError.message,
      )
    : null;

  return (
    <div className="space-y-3">
      {likelyIndian ? <IndianCardGuidance showScaleLine /> : null}

      <PaymentMethodPicker
        provider={provider}
        onProviderChange={setProvider}
        availableProviders={billing.available_providers}
        likelyIndian={likelyIndian}
      />

      <BillingTaxCopy />

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        {PLAN_CATALOG.map((tier) => {
          const isCurrent = tier.id === billing.plan;
          const downgrade = isDowngrade(billing.plan, tier.id);
          const blocked = downgrade && exceedsPlanLimit(usedSites, tier);

          return (
            <Card
              key={tier.id}
              className={cn(isCurrent && "border-[var(--color-primary)]")}
            >
              <CardHeader className="space-y-1">
                <CardTitle className="flex items-center justify-between gap-2 text-base">
                  {tier.name}
                  {isCurrent ? <Badge variant="default">Current plan</Badge> : null}
                </CardTitle>
                <CardDescription>{tier.priceLabel}</CardDescription>
              </CardHeader>
              <CardContent className="space-y-3">
                <ul className="space-y-1 text-sm text-muted-foreground">
                  <li>{tier.sitesLimit} sites</li>
                  <li>{tier.storageLabel}</li>
                  <li>{tier.cadenceLabel}</li>
                </ul>

                {blocked ? (
                  <p className="text-xs text-[var(--color-warning-subtle-fg)]">
                    You have {usedSites} sites; {tier.name} allows{" "}
                    {tier.sitesLimit}. Archive sites first.
                  </p>
                ) : null}

                {isCurrent ? (
                  <Button type="button" variant="outline" disabled className="w-full">
                    Current plan
                  </Button>
                ) : tier.id === "free" ? (
                  portalAvailable ? (
                    <Button
                      type="button"
                      variant="outline"
                      className="w-full"
                      disabled={portalPending}
                      onClick={onManageBilling}
                    >
                      {portalPending ? "Opening…" : "Manage in billing portal"}
                    </Button>
                  ) : (
                    <p className="text-xs text-muted-foreground">
                      Cancel your subscription above to move to Free at the
                      end of the billing period.
                    </p>
                  )
                ) : (
                  <Button
                    type="button"
                    className="w-full"
                    disabled={blocked || isStarting}
                    onClick={() => {
                      if (!isCheckoutTier(tier.id)) return;
                      startCheckout(tier.id);
                    }}
                  >
                    {isStarting
                      ? provider === "razorpay"
                        ? "Opening…"
                        : "Redirecting…"
                      : downgrade
                        ? `Switch to ${tier.name}`
                        : `Upgrade to ${tier.name}`}
                  </Button>
                )}
              </CardContent>
            </Card>
          );
        })}
      </div>
      {errorCopy ? (
        <div className="flex flex-wrap items-center gap-2">
          <p role="alert" className="text-sm text-destructive">
            {errorCopy.message}
          </p>
          {errorCopy.action === "manage_billing" && portalAvailable ? (
            <Button type="button" size="sm" variant="outline" onClick={onManageBilling}>
              Manage billing
            </Button>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}
