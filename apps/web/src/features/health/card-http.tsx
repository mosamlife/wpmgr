import type { SiteDiagnosticsCard } from "@wpmgr/api";

import { DefinitionList } from "@/components/shared/definition-list";

import { DiagnosticCard } from "./diagnostic-card";
import { pickBool, pickNumber, pickString } from "./diagnostic-pick";

// HTTP card — outbound loopback probe + WP_HTTP_BLOCK_EXTERNAL state. A
// failing loopback is a leading indicator that wp-cron + plugin update checks
// are silently broken; we surface the probe's status code prominently.

export function CardHTTP({ card }: { card: SiteDiagnosticsCard | undefined }) {
  const payload = card?.payload as Record<string, unknown> | null | undefined;
  const loopback = (payload?.loopback ?? {}) as Record<string, unknown>;
  const ok = pickBool(loopback, "ok");
  const status = pickNumber(loopback, "status");

  return (
    <DiagnosticCard title="HTTP" card={card}>
      <DefinitionList
        rows={[
          {
            label: "Home URL",
            value: pickString(payload, "home_url"),
            mono: true,
          },
          {
            label: "Loopback",
            value: ok
              ? `OK (${status})`
              : `Failed (${status || "no response"})`,
            mono: true,
          },
          {
            label: "Block external",
            value: pickBool(payload, "block_external") ? "Yes" : "No",
          },
          {
            label: "Accessible hosts",
            value: pickString(payload, "accessible_hosts", "All allowed"),
            mono: true,
          },
        ]}
      />
    </DiagnosticCard>
  );
}
