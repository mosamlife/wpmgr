import type { SiteDiagnosticsCard } from "@wpmgr/api";

import { DefinitionList } from "@/components/shared/definition-list";

import { DiagnosticCard } from "./diagnostic-card";
import { pickNumber } from "./diagnostic-pick";

// Users card — total + admin count + a one-row-per-role breakdown.
// We deliberately do NOT enumerate email addresses; the agent does not ship
// them and the CP redacts SENSITIVE fields server-side.

export function CardUsers({ card }: { card: SiteDiagnosticsCard | undefined }) {
  const payload = card?.payload as Record<string, unknown> | null | undefined;
  const byRole = (payload?.by_role ?? {}) as Record<string, unknown>;
  const roles = Object.entries(byRole)
    .filter(([, v]) => typeof v === "number")
    .sort(([a], [b]) => a.localeCompare(b));

  return (
    <DiagnosticCard title="Users" card={card}>
      <DefinitionList
        rows={[
          { label: "Total", value: pickNumber(payload, "total"), tabular: true },
          { label: "Admins", value: pickNumber(payload, "admins"), tabular: true },
          ...roles.map(([role, count]) => ({
            label: role,
            value: String(count),
            tabular: true,
          })),
        ]}
      />
    </DiagnosticCard>
  );
}
