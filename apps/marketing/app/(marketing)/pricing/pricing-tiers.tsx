"use client";

// Interactive tier cards + USD/INR currency toggle. Split out from
// page.tsx (a Server Component doing the build-time pricing fetch) because
// the toggle is client-only display state -- both currencies are already
// resolved server-side (see resolveTierPrices); switching currency here
// never triggers a network request.

import { useState } from "react";
import { Icon } from "@/components/ui/icon";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/primitives";
import { Stagger, StaggerItem } from "@/components/motion/stagger";
import { cn } from "@/lib/utils";
import { newTabRel } from "@/lib/site";
import {
  PRICING_TIERS,
  ctaHrefWithCurrency,
  type BillingCurrency,
  type PricingTier,
  type TierDisplayPrice,
} from "@/lib/content/pricing";

const CURRENCY_OPTIONS: BillingCurrency[] = ["USD", "INR"];

export function PricingTierCards({
  prices,
}: {
  prices: Record<PricingTier["id"], TierDisplayPrice>;
}) {
  const [currency, setCurrency] = useState<BillingCurrency>("USD");

  return (
    <>
      <div className="mb-8 flex flex-col items-center gap-2">
        <span className="text-xs font-medium uppercase tracking-wide text-[var(--muted-foreground)]">
          Show prices in
        </span>
        <div
          role="radiogroup"
          aria-label="Currency"
          className="inline-flex rounded-[var(--radius)] border border-[var(--border)] bg-card p-1 shadow-sm"
        >
          {CURRENCY_OPTIONS.map((option) => (
            <button
              key={option}
              type="button"
              role="radio"
              aria-checked={currency === option}
              onClick={() => setCurrency(option)}
              className={cn(
                "min-w-[4.5rem] rounded-[calc(var(--radius)-2px)] px-4 py-1.5 text-sm font-medium",
                "transition-colors duration-[var(--duration-fast)]",
                "focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]",
                currency === option
                  ? "bg-primary text-[var(--primary-foreground)] shadow-sm"
                  : "text-[var(--muted-foreground)] hover:text-foreground",
              )}
            >
              {option}
            </button>
          ))}
        </div>
      </div>

      <Stagger className="grid gap-5 sm:grid-cols-2 lg:grid-cols-4">
        {PRICING_TIERS.map((tier) => {
          const trailing = tier.cta.icon === "ArrowRight";
          const ctaHref = ctaHrefWithCurrency(tier.cta, currency);
          const tierPrice = prices[tier.id];
          const showInr = currency === "INR" && tierPrice.inr !== null;
          const priceLabel = showInr && tierPrice.inr ? tierPrice.inr.label : tierPrice.usd.label;
          const usdOnlyNote = tier.id !== "free" && currency === "INR" && tierPrice.inr === null;

          return (
            <StaggerItem key={tier.id} className="h-full">
              <Card
                className={cn(
                  "flex h-full flex-col gap-5",
                  tier.mostPopular && "border-[var(--primary)]/50 ring-1 ring-[var(--primary)]/20",
                )}
              >
                {tier.mostPopular ? (
                  <span className="inline-flex self-start rounded-full bg-[var(--primary-subtle)] px-2.5 py-0.5 text-xs font-semibold text-[var(--primary-pressed)]">
                    Most popular
                  </span>
                ) : (
                  <span className="h-[22px]" aria-hidden />
                )}
                <div>
                  <h2 className="text-lg font-semibold text-foreground">{tier.name}</h2>
                  <p className="mt-0.5 text-xs font-medium text-[var(--primary-pressed)] uppercase tracking-wide">
                    {tier.audience}
                  </p>
                </div>
                <div>
                  <div className="flex items-baseline gap-1">
                    <span
                      className="text-3xl font-semibold text-foreground"
                      style={{ fontVariantNumeric: "tabular-nums" }}
                    >
                      {priceLabel}
                    </span>
                    <span className="text-sm text-[var(--muted-foreground)]">/mo</span>
                  </div>
                  {usdOnlyNote ? (
                    <p className="mt-1 text-xs text-[var(--muted-foreground)]">Billed in USD</p>
                  ) : null}
                </div>
                <ul className="flex-1 space-y-2.5">
                  {tier.features.map((f) => (
                    <li key={f} className="flex items-start gap-2 text-sm text-foreground">
                      <Icon name="Check" size={16} className="mt-0.5 shrink-0 text-[var(--primary)]" />
                      <span>{f}</span>
                    </li>
                  ))}
                </ul>
                <Button
                  href={ctaHref}
                  variant={tier.cta.variant}
                  size="md"
                  target="_blank"
                  rel={newTabRel(ctaHref)}
                  className="w-full"
                >
                  {tier.cta.label}
                  {trailing ? <Icon name="ArrowRight" size={16} /> : null}
                </Button>
              </Card>
            </StaggerItem>
          );
        })}
      </Stagger>
    </>
  );
}
