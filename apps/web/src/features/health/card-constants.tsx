import type { SiteDiagnosticsCard } from "@wpmgr/api";

import { DiagnosticCard } from "./diagnostic-card";
import { asWpNative, fieldRows, section } from "./wp-native";

// WordPress Constants card — scrollable dump of every define() WP surfaces
// under wp-constants (WP_DEBUG, WP_HOME, WP_SITEURL, AUTH_KEY redacted to
// "REDACTED" by the agent, WP_POST_REVISIONS, EMPTY_TRASH_DAYS, WP_MEMORY_LIMIT,
// WP_DEBUG_LOG, DB_CHARSET, …).
//
// Heights are capped so the card sits comfortably in the grid even when the
// constant list has 60+ entries; the inner list is its own scrollable region so
// the operator can spelunk without page-scrolling.

export function CardConstants({
  card,
}: {
  card: SiteDiagnosticsCard | undefined;
}) {
  const payload = asWpNative(card);
  const sec = section(payload, "wp-constants");
  const rows = fieldRows(sec);

  return (
    <DiagnosticCard title="WordPress Constants" card={card}>
      {rows.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          Awaiting first sync from the agent.
        </p>
      ) : (
        <div className="max-h-72 overflow-y-auto pr-1">
          <dl className="grid grid-cols-[1fr_1fr] gap-y-1.5 text-xs">
            {rows.map((r) => (
              <div key={r.key} className="contents">
                <dt className="break-all font-mono text-muted-foreground">
                  {r.key}
                </dt>
                <dd className="break-all font-mono tabular-nums text-foreground">
                  {r.value}
                </dd>
              </div>
            ))}
          </dl>
        </div>
      )}
    </DiagnosticCard>
  );
}
