import type { SiteDiagnosticsCard } from "@wpmgr/api";

import { cn } from "@/lib/utils";

import { DiagnosticCard } from "./diagnostic-card";
import { asWpNative, fieldRows, section } from "./wp-native";

// Filesystem Permissions card — wp-filesystem section of WP_Debug_Data.
//
// Per-directory writability: WordPress / wp-content / uploads / plugins /
// themes / mu-plugins / fonts. Each field's value is "Writable" / "Not
// writable"; we color-code with semantic tokens so a sea-of-success operator
// scan turns a single non-writable row into an instant warning signal.

export function CardPermissions({
  card,
}: {
  card: SiteDiagnosticsCard | undefined;
}) {
  const payload = asWpNative(card);
  const sec = section(payload, "wp-filesystem");
  const rows = fieldRows(sec);

  return (
    <DiagnosticCard title="Filesystem Permissions" card={card}>
      {rows.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          Awaiting first sync from the agent.
        </p>
      ) : (
        <dl className="grid grid-cols-[2fr_1fr] gap-y-2 text-sm">
          {rows.map((r) => {
            const writable =
              /^writable$/i.test(r.value) ||
              (/writable\b/i.test(r.value) && !/not writable/i.test(r.value));
            return (
              <div key={r.key} className="contents">
                <dt className="break-all font-mono text-muted-foreground">
                  {r.label}
                </dt>
                <dd
                  className={cn(
                    "tabular-nums",
                    writable
                      ? "text-success-subtle-fg"
                      : "text-warning-subtle-fg",
                  )}
                >
                  {r.value}
                </dd>
              </div>
            );
          })}
        </dl>
      )}
    </DiagnosticCard>
  );
}
