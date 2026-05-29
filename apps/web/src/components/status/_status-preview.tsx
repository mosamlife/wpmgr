/**
 * Visual QA preview for the status indicator primitives. Not registered as a
 * route; the leading underscore in the filename keeps it out of TanStack
 * Router's file-based scanner and out of the public barrel (`index.ts`).
 *
 * Import ad-hoc during design review:
 *   import StatusPreview from "@/components/status/_status-preview";
 */
import * as React from "react";

import { BackupChip } from "./backup-chip";
import { StatusChip } from "./status-chip";
import { StatusDot, type StatusTone } from "./status-dot";
import { UpdateChip } from "./update-chip";
import { VulnSeverityChip, type VulnSeverity } from "./vuln-severity-chip";

const tones: StatusTone[] = [
  "success",
  "warning",
  "destructive",
  "info",
  "muted",
];
const severities: VulnSeverity[] = ["critical", "high", "medium", "low"];

interface SectionProps {
  title: string;
  children: React.ReactNode;
}

function Section({ title, children }: SectionProps) {
  return (
    <section className="space-y-3">
      <h2 className="text-xs font-semibold uppercase tracking-[0.02em] text-muted-foreground">
        {title}
      </h2>
      <div className="flex flex-wrap items-center gap-3">{children}</div>
    </section>
  );
}

export default function StatusPreview() {
  return (
    <div className="space-y-8 p-6 text-foreground">
      <Section title="Status dot (standalone, with a11y label)">
        {tones.map((tone) => (
          <StatusDot key={tone} tone={tone} label={`${tone} state`} />
        ))}
        <StatusDot tone="success" pulse label="live" />
      </Section>

      <Section title="Status chip — label only">
        {tones.map((tone) => (
          <StatusChip
            key={tone}
            tone={tone}
            label={tone.charAt(0).toUpperCase() + tone.slice(1)}
          />
        ))}
      </Section>

      <Section title="Status chip — label + time">
        <StatusChip tone="success" label="Up" time="14d" pulse />
        <StatusChip tone="destructive" label="Down" time="4m" />
        <StatusChip tone="warning" label="Slow" time="2h" />
        <StatusChip tone="info" label="Pending" time="just now" />
        <StatusChip tone="muted" label="Pending check" />
      </Section>

      <Section title="Update chip">
        <UpdateChip count={1} severity="minor" />
        <UpdateChip count={6} severity="minor" description="Minor: plugin patches" />
        <UpdateChip count={3} severity="major" description="Major: WP 7.0" />
      </Section>

      <Section title="Backup chip">
        <BackupChip status="success" time="2h ago" />
        <BackupChip status="running" progressPercent={38} />
        <BackupChip status="running" />
        <BackupChip status="failed" onRetry={() => {}} />
        <BackupChip status="failed" />
      </Section>

      <Section title="Vuln severity chip">
        {severities.map((severity) => (
          <VulnSeverityChip key={severity} severity={severity} />
        ))}
        {severities.map((severity) => (
          <VulnSeverityChip
            key={`${severity}-count`}
            severity={severity}
            count={12}
          />
        ))}
      </Section>
    </div>
  );
}
