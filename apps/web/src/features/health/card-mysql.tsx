import type { SiteDiagnosticsCard } from "@wpmgr/api";

import { DefinitionList } from "@/components/shared/definition-list";

import { DiagnosticCard } from "./diagnostic-card";
import { pickString } from "./diagnostic-pick";

// MySQL card — version, charset, dbsig (privacy-safe fingerprint), and the
// max_allowed_packet variable which is the load-bearing tunable for
// mysqldump and large-attachment uploads.

export function CardMySQL({ card }: { card: SiteDiagnosticsCard | undefined }) {
  const payload = card?.payload as Record<string, unknown> | null | undefined;
  const variables = (payload?.variables ?? {}) as Record<string, unknown>;

  return (
    <DiagnosticCard title="MySQL" card={card}>
      <DefinitionList
        rows={[
          { label: "Version", value: pickString(payload, "version"), mono: true },
          { label: "Charset", value: pickString(payload, "charset"), mono: true },
          {
            label: "Collation",
            value: pickString(payload, "collation"),
            mono: true,
          },
          { label: "DB sig", value: pickString(payload, "dbsig"), mono: true },
          {
            label: "Max packet",
            value: pickString(variables, "max_allowed_packet"),
            mono: true,
          },
          {
            label: "InnoDB pool",
            value: pickString(variables, "innodb_buffer_pool_size"),
            mono: true,
          },
        ]}
      />
    </DiagnosticCard>
  );
}
