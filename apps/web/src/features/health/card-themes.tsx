import type { SiteDiagnosticsCard } from "@wpmgr/api";

import { DefinitionList } from "@/components/shared/definition-list";

import { DiagnosticCard } from "./diagnostic-card";
import { pickNumber, pickString } from "./diagnostic-pick";

// Themes card — active stylesheet + parent template + installed count.

export function CardThemes({ card }: { card: SiteDiagnosticsCard | undefined }) {
  const payload = card?.payload as Record<string, unknown> | null | undefined;
  const active = (payload?.active ?? {}) as Record<string, unknown>;

  return (
    <DiagnosticCard title="Themes" card={card}>
      <DefinitionList
        rows={[
          { label: "Active", value: pickString(active, "name") },
          {
            label: "Stylesheet",
            value: pickString(active, "stylesheet"),
            mono: true,
          },
          {
            label: "Template",
            value: pickString(active, "template"),
            mono: true,
          },
          { label: "Version", value: pickString(active, "version"), mono: true },
          {
            label: "Installed",
            value: pickNumber(payload, "installed"),
            tabular: true,
          },
        ]}
      />
    </DiagnosticCard>
  );
}
