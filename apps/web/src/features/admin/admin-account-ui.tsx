import type { CSSProperties } from "react";

import { Progress } from "@/components/ui/progress";
import { Tooltip } from "@/components/ui/tooltip";
import { cn, formatBytes } from "@/lib/utils";
import { planLabel } from "@/features/billing/plan-catalog";
import {
  ACCOUNT_STATUS_BADGE_CLASS,
  ACCOUNT_STATUS_LABEL,
  METER_TONE_TEXT_CLASS,
  accountDisplayStatus,
  formatMeterValue,
  isOverCap,
  meterBarPercent,
  meterPercent,
  meterTone,
  type AccountMeterRow,
} from "./admin-accounts-format";

// Shared, presentation-only building blocks for the Accounts table, the
// account detail page, and the Revenue plan-distribution table. Kept
// React-only (no data fetching) so every piece is a pure function of props.

// ---------------------------------------------------------------------------
// AccountStatusBadge — the single derived display status (suspended overrides
// everything else; see accountDisplayStatus in admin-accounts-format.ts).
// ---------------------------------------------------------------------------

export function AccountStatusBadge({
  account,
}: {
  account: { plan_status: string; suspended_at?: string | null };
}) {
  const status = accountDisplayStatus(account);
  return (
    <span
      className={cn(
        "inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium whitespace-nowrap",
        ACCOUNT_STATUS_BADGE_CLASS[status],
      )}
    >
      {ACCOUNT_STATUS_LABEL[status]}
    </span>
  );
}

// ---------------------------------------------------------------------------
// PlanBadge — tier badge; a dashed border marks a comped account; a dot marks
// manual overrides. Both cues are independent of each other and of status.
// ---------------------------------------------------------------------------

export function PlanBadge({
  plan,
  comped,
  hasOverrides,
}: {
  plan: string;
  comped: boolean;
  hasOverrides: boolean;
}) {
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-xs font-medium capitalize whitespace-nowrap",
        comped
          ? "border-dashed border-violet-300 text-violet-800 dark:border-violet-700 dark:text-violet-300"
          : "border-border text-foreground",
      )}
    >
      {planLabel(plan)}
      {hasOverrides ? (
        <span
          role="img"
          aria-label="Has manual overrides"
          title="Has manual overrides"
          className="size-1.5 shrink-0 rounded-full bg-[var(--color-primary)]"
        />
      ) : null}
    </span>
  );
}

// ---------------------------------------------------------------------------
// Compact meter chips — used in the Accounts table's Sites/Storage columns.
// ---------------------------------------------------------------------------

const CHIP_TONE_CLASS: Record<"ok" | "warning" | "critical", string> = {
  ok: "bg-muted text-muted-foreground",
  warning: "bg-warning-subtle text-warning-subtle-fg",
  critical: "bg-destructive-subtle text-destructive-subtle-fg",
};

export function SitesMeterChip({ used, cap }: { used: number; cap: number }) {
  const tone = meterTone(meterPercent(used, cap));
  return (
    <span
      className={cn(
        "inline-flex items-center rounded px-1.5 py-0.5 text-xs font-medium tabular-nums whitespace-nowrap",
        CHIP_TONE_CLASS[tone],
      )}
    >
      {used} / {cap}
    </span>
  );
}

/**
 * `usedBytes`/`capBytes` map directly to the wire's flat
 * `storage_used_bytes_approx`/`storage_cap_bytes` fields (there is no nested
 * `storage` object, and no per-account `approximate` flag — the metric is
 * ALWAYS an approximation, so the "~" prefix and tooltip are unconditional).
 * `capBytes <= 0` means "no CP-managed cap to approach" (free tier,
 * BYO-storage only), not zero capacity.
 */
export function StorageMeterChip({
  usedBytes,
  capBytes,
}: {
  usedBytes: number;
  capBytes: number;
}) {
  const tone = meterTone(meterPercent(usedBytes, capBytes));
  const chip = (
    <span
      className={cn(
        "inline-flex items-center gap-0.5 rounded px-1.5 py-0.5 text-xs font-medium tabular-nums whitespace-nowrap",
        CHIP_TONE_CLASS[tone],
      )}
    >
      <span aria-hidden="true">~</span>
      {formatBytes(usedBytes)} / {capBytes > 0 ? formatBytes(capBytes) : "no cap"}
    </span>
  );

  return (
    <Tooltip content="Storage usage is computed periodically, not live.">
      {chip}
    </Tooltip>
  );
}

// ---------------------------------------------------------------------------
// AdminMeterList — the account detail "Usage vs limits" card. Reuses the
// Progress primitive from features/billing/usage-meter-list.tsx's pattern,
// extended with amber/red tone coloring (via a scoped --color-primary
// override, same technique as PortalShell's brand-color scoping) and an
// "over cap" note that Phase B's UsageMeterList doesn't need.
// ---------------------------------------------------------------------------

export function AdminMeterList({ meters }: { meters: AccountMeterRow[] }) {
  if (meters.length === 0) {
    return <p className="text-sm text-muted-foreground">No usage data.</p>;
  }

  return (
    <div className="space-y-4">
      {meters.map((meter) => {
        const rawPercent = meterPercent(meter.used, meter.cap);
        const tone = meterTone(rawPercent);
        const over = isOverCap(meter.used, meter.cap);
        const toneVar =
          tone === "critical"
            ? "var(--color-destructive)"
            : tone === "warning"
              ? "var(--color-warning)"
              : "var(--color-primary)";
        const barStyle = { "--color-primary": toneVar } as CSSProperties;

        return (
          <div key={meter.key} className="space-y-1.5">
            <div className="flex items-baseline justify-between gap-2 text-sm">
              <span className="font-medium text-foreground">{meter.label}</span>
              <span
                className={cn(
                  "font-mono tabular-nums",
                  METER_TONE_TEXT_CLASS[tone],
                )}
              >
                {meter.approximate ? "~" : ""}
                {formatMeterValue(meter.used, meter.unit)} /{" "}
                {meter.cap > 0 ? formatMeterValue(meter.cap, meter.unit) : "no cap"}
              </span>
            </div>
            <div style={barStyle}>
              <Progress
                value={meterBarPercent(rawPercent)}
                label={`${meter.label} used`}
              />
            </div>
            {over ? (
              <p className="text-xs text-destructive">
                Over cap by {formatMeterValue(meter.used - meter.cap, meter.unit)}.
              </p>
            ) : null}
          </div>
        );
      })}
    </div>
  );
}
