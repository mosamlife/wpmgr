import { createFileRoute } from "@tanstack/react-router";
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
import { PageError } from "@/components/feedback";
import { PageHeader } from "@/components/shared/page-header";
import { useMe, activeRole } from "@/features/auth/use-auth";
import {
  useBilling,
  useCreateBillingCheckout,
  useCreateBillingPortal,
  useBillingCheckoutReturn,
  isCheckoutTier,
  type BillingInfo,
} from "@/features/billing/use-billing";
import {
  billingBannerFor,
  formatBillingDate,
  planStatusLabel,
} from "@/features/billing/billing-status";
import {
  PLAN_CATALOG,
  isDowngrade,
  exceedsPlanLimit,
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

  const billing = useBilling({ enabled: hosted && isOwner });
  const checkoutReturn = useBillingCheckoutReturn(
    search.checkout,
    billing.data,
    () => void billing.refetch(),
  );

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
}: {
  billing: BillingInfo;
  checkoutStatus: "success" | "cancel" | undefined;
  finalizing: boolean;
  timedOut: boolean;
}) {
  const banner = billingBannerFor(billing);
  const portal = useCreateBillingPortal();

  const openPortal = () => {
    portal.mutate(undefined, {
      onSuccess: (result) => {
        window.location.href = result.url;
      },
    });
  };

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
          {billing.portal_available ? (
            <Button
              type="button"
              variant="outline"
              disabled={portal.isPending}
              onClick={openPortal}
            >
              {portal.isPending ? "Opening…" : "Manage billing"}
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
// Plan tiers grid
// ---------------------------------------------------------------------------

function PlanTiersGrid({
  billing,
  portalAvailable,
  onManageBilling,
  portalPending,
}: {
  billing: BillingInfo;
  portalAvailable: boolean;
  onManageBilling: () => void;
  portalPending: boolean;
}) {
  const checkout = useCreateBillingCheckout();
  const usedSites = billing.meters.sites?.used ?? 0;

  return (
    <div className="space-y-3">
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
                      Contact support to move to Free.
                    </p>
                  )
                ) : (
                  <Button
                    type="button"
                    className="w-full"
                    disabled={blocked || checkout.isPending}
                    onClick={() => {
                      if (!isCheckoutTier(tier.id)) return;
                      checkout.mutate(
                        { tier: tier.id },
                        {
                          onSuccess: (result) => {
                            window.location.href = result.url;
                          },
                        },
                      );
                    }}
                  >
                    {checkout.isPending
                      ? "Redirecting…"
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
