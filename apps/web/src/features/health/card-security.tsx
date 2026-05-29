import type { SiteDiagnosticsCard } from "@wpmgr/api";

import { DefinitionList } from "@/components/shared/definition-list";

import { DiagnosticCard } from "./diagnostic-card";
import { pickBool } from "./diagnostic-pick";

// Security card — the load-bearing wp-config defines. We render Yes/No so a
// scrim review reads at a glance; the full numeric/string values are in the
// raw payload for power users to inspect via the API.

const DEFINES_OF_INTEREST = [
  "WP_DEBUG",
  "WP_DEBUG_LOG",
  "WP_DEBUG_DISPLAY",
  "DISALLOW_FILE_EDIT",
  "DISALLOW_FILE_MODS",
  "FORCE_SSL_ADMIN",
  "AUTOMATIC_UPDATER_DISABLED",
] as const;

export function CardSecurity({
  card,
}: {
  card: SiteDiagnosticsCard | undefined;
}) {
  const payload = card?.payload as Record<string, unknown> | null | undefined;
  const defines = (payload?.defines ?? {}) as Record<string, unknown>;

  return (
    <DiagnosticCard title="Security" card={card}>
      <DefinitionList
        rows={[
          ...DEFINES_OF_INTEREST.map((d) => ({
            label: d,
            value: renderDefine(defines[d]),
            mono: true,
          })),
          {
            label: "Salts configured",
            value: pickBool(payload, "salts_configured") ? "Yes" : "No",
          },
        ]}
      />
    </DiagnosticCard>
  );
}

function renderDefine(v: unknown): string {
  if (v === null || v === undefined) return "not set";
  if (typeof v === "boolean") return v ? "true" : "false";
  if (typeof v === "number") return String(v);
  if (typeof v === "string") return v;
  return "set";
}
