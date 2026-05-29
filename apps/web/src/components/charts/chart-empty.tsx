// Calm empty state for charts. Matches the operator-grade pattern used in the
// site detail page's `SecurityStub` (muted prose, optional verb CTA). The
// illustration is a single lucide `LineChart` glyph at 40% opacity — enough
// to read as "this is a chart space" without competing with the message.

import { LineChart as LineChartIcon } from "lucide-react";
import { Button } from "@/components/ui/button";

export interface ChartEmptyProps {
  message?: string;
  actionLabel?: string;
  onAction?: () => void;
}

export function ChartEmpty({
  message = "No data yet",
  actionLabel = "Refresh",
  onAction,
}: ChartEmptyProps) {
  return (
    <div
      role="status"
      className="flex flex-col items-center justify-center gap-2 py-12 text-[var(--color-muted-foreground)]"
    >
      <LineChartIcon
        aria-hidden="true"
        size={32}
        className="text-[var(--color-muted-foreground)]/40"
      />
      <p className="text-sm">{message}</p>
      {onAction ? (
        <Button size="sm" variant="outline" onClick={onAction}>
          {actionLabel}
        </Button>
      ) : null}
    </div>
  );
}
