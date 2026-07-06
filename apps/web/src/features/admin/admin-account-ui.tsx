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
  isOverCap,
  meterBarPercent,
  meterPercent,
  meterTone,
} from "./admin-accounts-format";
import type { AdminAccountMeter, AdminAccountStorageUsage } from "./use-admin-accounts";

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
  account: { plan_status: string; suspended: boolean };
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

export function StorageMeterChip({ storage }: { storage: AdminAccountStorageUsage }) {
  const tone = meterTone(meterPercent(storage.used_bytes, storage.cap_bytes));
  const chip = (
    <span
      className={cn(
        "inline-flex items-center gap-0.5 rounded px-1.5 py-0.5 text-xs font-medium tabular-nums whitespace-nowrap",
        CHIP_TONE_CLASS[tone],
      )}
    >
      {storage.approximate ? <span aria-hidden="true">~</span> : null}
      {formatBytes(storage.used_bytes)} / {formatBytes(storage.cap_bytes)}
    </span>
  );

  if (!storage.approximate) return chip;

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

export function AdminMeterList({ meters }: { meters: AdminAccountMeter[] }) {
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
                {meter.used} / {meter.cap}
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
                Over cap by {(meter.used - meter.cap).toLocaleString()}.
              </p>
            ) : null}
          </div>
        );
      })}
    </div>
  );
}
