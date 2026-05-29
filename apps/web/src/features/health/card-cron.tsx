import type { SiteDiagnosticsCard } from "@wpmgr/api";

import { DefinitionList } from "@/components/shared/definition-list";

import { DiagnosticCard } from "./diagnostic-card";
import { pickBool, pickNumber } from "./diagnostic-pick";

// Cron card — DISABLE_WP_CRON state, registered event count, and the leapfrog
// `overdue_max_seconds` field (the longest-overdue WP-Cron event). A growing
// overdue_max is the canonical "wp-cron is silently broken" signal.

export function CardCron({ card }: { card: SiteDiagnosticsCard | undefined }) {
  const payload = card?.payload as Record<string, unknown> | null | undefined;
  const overdue = pickNumber(payload, "overdue_max_seconds");

  return (
    <DiagnosticCard title="Cron" card={card}>
      <DefinitionList
        rows={[
          {
            label: "WP-Cron disabled",
            value: pickBool(payload, "disabled") ? "Yes" : "No",
          },
          {
            label: "Alternate WP-Cron",
            value: pickBool(payload, "alternate") ? "Yes" : "No",
          },
          {
            label: "Event count",
            value: pickNumber(payload, "event_count"),
            tabular: true,
          },
          {
            label: "Longest overdue",
            value: overdue > 0 ? formatDuration(overdue) : "No overdue events",
            mono: overdue > 0,
          },
        ]}
      />
    </DiagnosticCard>
  );
}

function formatDuration(seconds: number): string {
  if (seconds < 60) return `${seconds}s`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h`;
  return `${Math.floor(seconds / 86400)}d`;
}
