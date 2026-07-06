import { Progress } from "@/components/ui/progress";
import type { BillingMeters } from "./use-billing";

// Data-driven usage meters: renders one row per key in `meters`. Phase C adds
// more dimensions (storage, seats, ...) to that map with zero web-side code
// change — an unrecognized key still renders correctly via the title-cased
// fallback label.

const METER_LABELS: Record<string, string> = {
  sites: "Sites",
};

function titleCase(key: string): string {
  return key
    .split(/[_-]/)
    .filter(Boolean)
    .map((word) => word.charAt(0).toUpperCase() + word.slice(1))
    .join(" ");
}

function labelFor(key: string): string {
  return METER_LABELS[key] ?? titleCase(key);
}

export interface UsageMeterListProps {
  meters: BillingMeters;
}

export function UsageMeterList({ meters }: UsageMeterListProps) {
  const entries = Object.entries(meters);
  if (entries.length === 0) return null;

  return (
    <div className="space-y-4">
      {entries.map(([key, meter]) => {
        const pct =
          meter.limit > 0
            ? Math.min(100, Math.round((meter.used / meter.limit) * 100))
            : 0;
        const label = labelFor(key);
        return (
          <div key={key} className="space-y-1.5">
            <div className="flex items-baseline justify-between gap-2 text-sm">
              <span className="font-medium text-foreground">{label}</span>
              <span className="font-mono tabular-nums text-muted-foreground">
                {meter.used} / {meter.limit}
              </span>
            </div>
            <Progress value={pct} label={`${label} used`} />
          </div>
        );
      })}
    </div>
  );
}
