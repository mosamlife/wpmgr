// Shared tooltip for every WPMgr Recharts chart. Operator-grade: a single calm
// card with monospace numerics, a per-series dot + label + value row, and an
// optional previous-period comparison row. Built to be passed straight into
// Recharts via `<Tooltip content={<ChartTooltip ... />} />` so series colors,
// names, and values flow through `payload` without us having to know in advance
// whether the chart is uptime, latency, or something else.

import type { ReactNode } from "react";

// Recharts hands us payload entries with at minimum these fields populated.
// We deliberately keep this loose because per-chart series can carry extras
// (dataKey, stroke, fill, payload) that we don't need here.
interface TooltipPayloadEntry {
  name?: string;
  value?: number | string;
  color?: string;
  dataKey?: string | number;
  stroke?: string;
  payload?: Record<string, unknown>;
}

export interface ChartTooltipProps {
  active?: boolean;
  payload?: TooltipPayloadEntry[];
  label?: string;
  /** Formatter for numeric values (e.g. percent, ms, bytes). */
  formatter?: (value: number) => string;
  /** Force showing the "Previous period" caption even when only one series. */
  showPrevious?: boolean;
  /** Optional override for the top label (defaults to Recharts `label`). */
  labelFormatter?: (label: string) => ReactNode;
}

function defaultFormatter(value: number | string | undefined): string {
  if (value === undefined || value === null) return "—";
  if (typeof value === "number") {
    return Number.isFinite(value) ? value.toLocaleString() : "—";
  }
  return String(value);
}

export function ChartTooltip({
  active,
  payload,
  label,
  formatter,
  showPrevious,
  labelFormatter,
}: ChartTooltipProps) {
  if (!active || !payload || payload.length === 0) return null;

  const renderValue = (value: number | string | undefined): string => {
    if (typeof value === "number" && formatter) return formatter(value);
    return defaultFormatter(value);
  };

  const top = label
    ? labelFormatter
      ? labelFormatter(label)
      : label
    : null;

  return (
    <div
      role="tooltip"
      className="rounded-md border border-[var(--color-border)] bg-[var(--color-popover)] px-3 py-2 text-sm shadow-md"
    >
      {top ? (
        <div className="mb-1 text-xs text-[var(--color-muted-foreground)] tabular-nums">
          {top}
        </div>
      ) : null}
      <dl className="flex flex-col gap-1">
        {payload.map((entry, idx) => {
          const swatch = entry.color ?? entry.stroke ?? "var(--color-chart-1)";
          const isPrevious =
            (entry.dataKey === "previousUptime" ||
              entry.name?.toLowerCase().includes("previous")) ??
            false;
          return (
            <div
              key={`${entry.dataKey ?? entry.name ?? idx}`}
              className="flex items-center gap-2"
            >
              <span
                aria-hidden="true"
                className="inline-block h-2 w-2 rounded-full"
                style={{
                  backgroundColor: swatch,
                  opacity: isPrevious ? 0.5 : 1,
                }}
              />
              <dt className="text-[var(--color-muted-foreground)]">
                {entry.name ?? entry.dataKey ?? "Series"}
              </dt>
              <dd className="ml-auto font-mono tabular-nums text-[var(--color-foreground)]">
                {renderValue(entry.value)}
              </dd>
            </div>
          );
        })}
        {showPrevious &&
        !payload.some(
          (p) =>
            p.dataKey === "previousUptime" ||
            p.name?.toLowerCase().includes("previous"),
        ) ? (
          <div className="mt-1 text-xs text-[var(--color-muted-foreground)]">
            Previous period
          </div>
        ) : null}
      </dl>
    </div>
  );
}
