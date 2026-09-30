import { INDIAN_UPGRADE_COPY } from "./billing-copy";

// S0.4 (stripe-design-v9.md 5.14) — shown beside "Change plan" (the Manage
// billing button, which opens the WPMgr portal for Stripe — 3.5) for
// likely-Indian users on Stripe with a live subscription. The portal can't
// show per-customer text, so this renders on WPMgr's own page before the
// portal opens.

export function IndianUpgradeCopy() {
  return (
    <p className="max-w-prose text-xs text-muted-foreground">
      {INDIAN_UPGRADE_COPY}
    </p>
  );
}
