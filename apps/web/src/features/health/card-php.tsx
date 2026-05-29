import type { SiteDiagnosticsCard } from "@wpmgr/api";

import { DefinitionList } from "@/components/shared/definition-list";

import { DiagnosticCard } from "./diagnostic-card";
import { pickString } from "./diagnostic-pick";
import { EolChip } from "./eol-chip";

// PHP card — version, SAPI, memory limit, opcache state. Version is the
// load-bearing field for the PHP-EOL countdown, which we carry on the title as
// the same chip the header ribbon shows, so the card is self-contained.

export function CardPHP({ card }: { card: SiteDiagnosticsCard | undefined }) {
  const payload = card?.payload as Record<string, unknown> | null | undefined;
  const opcache = (payload?.opcache ?? {}) as Record<string, unknown>;
  const version =
    typeof payload?.version === "string" ? payload.version : null;

  return (
    <DiagnosticCard title="PHP" card={card} titleChip={<EolChip version={version} />}>
      <DefinitionList
        rows={[
          { label: "Version", value: pickString(payload, "version"), mono: true },
          { label: "SAPI", value: pickString(payload, "sapi"), mono: true },
          {
            label: "Memory limit",
            value: pickString(payload, "memory_limit"),
            mono: true,
          },
          {
            label: "Max execution",
            value: `${pickString(payload, "max_execution_time")} s`,
            mono: true,
          },
          {
            label: "Upload max",
            value: pickString(payload, "upload_max_filesize"),
            mono: true,
          },
          {
            label: "Opcache",
            value:
              pickString(opcache, "enabled") === "true"
                ? "Enabled"
                : "Disabled",
          },
        ]}
      />
    </DiagnosticCard>
  );
}
