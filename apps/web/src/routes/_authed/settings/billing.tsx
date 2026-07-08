import { useState } from "react";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { z } from "zod";
import { AlertTriangle, CircleCheck, Loader2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { SegmentedControl } from "@/components/ui/segmented-control";
import { PageError } from "@/components/feedback";
import { PageHeader } from "@/components/shared/page-header";
import { DestructiveConfirm } from "@/components/dialogs/destructive-confirm";
import { toast } from "@/components/toast";
import { useMe, activeRole } from "@/features/auth/use-auth";
import {
  useBilling,
  useCreateBillingCheckout,
  useCreateBillingPortal,
  useCancelBillingSubscription,
  useVerifyRazorpayCheckout,
  useBillingCheckoutReturn,
  isCheckoutTier,
  type BillingInfo,
  type BillingProvider,
  type BillingCurrency,
  type RazorpayCheckoutData,
  type CheckoutTierId,
} from "@/features/billing/use-billing";
import {
  loadRazorpayCheckout,
  type RazorpayHandlerResponse,
} from "@/features/billing/razorpay-checkout";
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
import { cn } from "@/lib/utils";

// M16 Phase B — the tenant Billing settings page. Owner-only, hosted-only
// (the nav entry and this route both gate on the same me.hosted +
// activeRole==='owner' check — see routes/_authed/settings/route.tsx).

const billingSearchSchema = z.object({
  checkout: z.enum(["success", "cancel"]).optional(),
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
  onCheckoutSuccess,
}: {
  billing: BillingInfo;
  checkoutStatus: "success" | "cancel" | undefined;
  finalizing: boolean;
  timedOut: boolean;
  onCheckoutSuccess: () => void;
}) {
  const banner = billingBannerFor(billing);
  const portal = useCreateBillingPortal();
  const cancel = useCancelBillingSubscription();
  const [cancelOpen, setCancelOpen] = useState(false);

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
            <Button
              type="button"
              variant="outline"
              disabled={portal.isPending}
              onClick={openPortal}
            >
              {portal.isPending ? "Opening…" : "Manage billing"}
            </Button>
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
        onCheckoutSuccess={onCheckoutSuccess}
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
// Checkout-return banner — success | cancel | finalizing | timed out
// ---------------------------------------------------------------------------

function CheckoutReturnBanner({
  status,
  finalizing,
  timedOut,
}: {
  status: "success" | "cancel" | undefined;
  finalizing: boolean;
  timedOut: boolean;
}) {
  if (!status) return null;

  if (status === "cancel") {
    return (
      <div
        role="status"
        className="rounded-lg border border-border bg-muted/40 px-4 py-3 text-sm text-muted-foreground"
      >
        Checkout was canceled. No changes were made to your plan.
      </div>
    );
  }

  if (finalizing) {
    return (
      <div
        role="status"
        aria-live="polite"
        className="flex items-center gap-2 rounded-lg border border-border bg-muted/40 px-4 py-3 text-sm text-foreground"
      >
        <Loader2 aria-hidden="true" className="size-4 animate-spin" />
        Finalizing your subscription…
      </div>
    );
  }

  if (timedOut) {
    return (
      <div
        role="status"
        className="rounded-lg border border-border bg-muted/40 px-4 py-3 text-sm text-muted-foreground"
      >
        Payment received. It can take a few minutes for your plan to update
        here.
      </div>
    );
  }

  return (
    <div
      role="status"
      className="flex items-center gap-2 rounded-lg bg-[var(--color-success-subtle)] px-4 py-3 text-sm text-[var(--color-success-subtle-fg)]"
    >
      <CircleCheck aria-hidden="true" className="size-4" />
      Subscription updated.
    </div>
  );
}

// ---------------------------------------------------------------------------
// Payment method picker — provider (Stripe/Razorpay) + Razorpay-only currency
// ---------------------------------------------------------------------------

function PaymentMethodPicker({
  provider,
  onProviderChange,
  currency,
  onCurrencyChange,
}: {
  provider: BillingProvider;
  onProviderChange: (provider: BillingProvider) => void;
  currency: BillingCurrency;
  onCurrencyChange: (currency: BillingCurrency) => void;
}) {
  return (
    <div className="flex flex-wrap items-center gap-4 rounded-lg border border-border bg-muted/20 px-4 py-3 text-sm">
      <div className="flex items-center gap-2">
        <span className="font-medium text-foreground">Pay via</span>
        <SegmentedControl
          aria-label="Payment provider"
          value={provider}
          onChange={onProviderChange}
          options={[
            { value: "stripe", label: "Stripe" },
            { value: "razorpay", label: "Razorpay" },
          ]}
        />
      </div>
      {provider === "razorpay" ? (
        <div className="flex items-center gap-2">
          <span className="font-medium text-foreground">Currency</span>
          <SegmentedControl
            aria-label="Currency"
            value={currency}
            onChange={onCurrencyChange}
            options={[
              { value: "USD", label: "USD ($)" },
              { value: "INR", label: "INR (₹)" },
            ]}
          />
          <span className="text-xs text-muted-foreground">
            Prices below are shown in USD; the exact INR amount is confirmed
            in the payment window before you pay.
          </span>
        </div>
      ) : null}
    </div>
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
  onCheckoutSuccess,
}: {
  billing: BillingInfo;
  portalAvailable: boolean;
  onManageBilling: () => void;
  portalPending: boolean;
  onCheckoutSuccess: () => void;
}) {
  const checkout = useCreateBillingCheckout();
  const verify = useVerifyRazorpayCheckout();
  const [provider, setProvider] = useState<BillingProvider>("stripe");
  const [currency, setCurrency] = useState<BillingCurrency>("USD");
  const usedSites = billing.meters.sites?.used ?? 0;

  function openRazorpayCheckout(data: RazorpayCheckoutData) {
    loadRazorpayCheckout()
      .then((Razorpay) => {
        const instance = new Razorpay({
          key: data.key_id,
          subscription_id: data.subscription_id,
          amount: data.amount,
          currency: data.currency,
          name: "WPMgr",
          description: "WPMgr subscription",
          handler: (response: RazorpayHandlerResponse) => {
            // Verify is a UX confirmation only — the poll below (via
            // onCheckoutSuccess -> the shared Stripe success-poll mechanism)
            // is what actually observes the plan flip, so it must start
            // regardless of whether verify itself succeeds or fails.
            verify.mutate(response, {
              onSettled: () => onCheckoutSuccess(),
            });
          },
          modal: {
            // The operator closed the modal without paying — a normal
            // cancel, never an error. No toast, no navigation.
            ondismiss: () => {},
          },
        });
        instance.open();
      })
      .catch((err: unknown) => {
        toast.error("Could not open the Razorpay payment window", {
          description:
            err instanceof Error
              ? err.message
              : "Please try again, or choose Stripe instead.",
        });
      });
  }

  function startCheckout(tier: CheckoutTierId) {
    checkout.mutate(
      {
        tier,
        provider,
        currency: provider === "razorpay" ? currency : undefined,
      },
      {
        onSuccess: (result) => {
          if (result.razorpay) {
            openRazorpayCheckout(result.razorpay);
          } else if (result.url) {
            window.location.href = result.url;
          }
        },
      },
    );
  }

  return (
    <div className="space-y-3">
      <PaymentMethodPicker
        provider={provider}
        onProviderChange={setProvider}
        currency={currency}
        onCurrencyChange={setCurrency}
      />

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
                    disabled={blocked || checkout.isPending}
                    onClick={() => {
                      if (!isCheckoutTier(tier.id)) return;
                      startCheckout(tier.id);
                    }}
                  >
                    {checkout.isPending
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
      {checkout.isError ? (
        <p role="alert" className="text-sm text-destructive">
          {checkout.error.message}
        </p>
      ) : null}
    </div>
  );
}
