import { Link } from "@tanstack/react-router";
import { ArrowUpRight } from "lucide-react";
import type { SiteDiagnosticsCard } from "@wpmgr/api";

import { DefinitionList } from "@/components/shared/definition-list";

import { DiagnosticCard } from "./diagnostic-card";
import { pickNumber } from "./diagnostic-pick";

// Plugins card — installed / active / available-updates counts. When updates
// are available we promote the count to a warning chip on the title that links
// straight to the Updates tab, so the operator can act from the scan. The
// licensing summary counts how many known paid plugins probed "present".

interface LicensingRow {
  slug?: string;
  plugin?: string;
  status?: string;
  has_key?: boolean;
}

export function CardPlugins({
  card,
  siteId,
}: {
  card: SiteDiagnosticsCard | undefined;
  siteId: string;
}) {
  const payload = card?.payload as Record<string, unknown> | null | undefined;
  const licensing = Array.isArray(payload?.licensing)
    ? (payload.licensing as LicensingRow[])
    : [];
  const presentCount = licensing.filter(
    (l) => l.status === "present" || l.has_key === true,
  ).length;
  const updates = pickNumber(payload, "available_updates");

  return (
    <DiagnosticCard
      title="Plugins"
      card={card}
      titleChip={
        updates > 0 ? (
          <Link
            to="/sites/$siteId/updates"
            params={{ siteId }}
            className="inline-flex items-center gap-1 rounded bg-warning-subtle px-2 py-0.5 text-xs font-medium text-warning-subtle-fg transition-colors hover:opacity-90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1"
          >
            <span
              aria-hidden="true"
              className="size-1.5 rounded-full bg-warning"
            />
            <span className="tabular-nums">{updates}</span> updates
            <ArrowUpRight aria-hidden="true" className="size-3" />
          </Link>
        ) : undefined
      }
    >
      <DefinitionList
        rows={[
          {
            label: "Installed",
            value: pickNumber(payload, "installed_count"),
            tabular: true,
          },
          {
            label: "Active",
            value: pickNumber(payload, "active_count"),
            tabular: true,
          },
          { label: "Updates available", value: updates, tabular: true },
          {
            label: "Paid plugins seen",
            value: `${presentCount} / ${licensing.length}`,
            tabular: true,
          },
        ]}
      />
    </DiagnosticCard>
  );
}
