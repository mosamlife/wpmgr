import type { SiteDiagnosticsCard } from "@wpmgr/api";

import { DefinitionList } from "@/components/shared/definition-list";
import { formatBytes } from "@/lib/utils";

import { DiagnosticCard } from "./diagnostic-card";
import {
  asWpNative,
  fieldDebugNumber,
  fieldValue,
  section,
} from "./wp-native";

// Directory Sizes card — the richest payload, so it leads with a proportional
// stacked bar (WordPress / Themes / Plugins / Uploads / Database) using the
// chart-1..5 tokens, with the full size list below.
//
// Sourced from `wp_native["wp-paths-sizes"]`. WP populates each row's `debug`
// field with the raw byte count and `value` with a human label like "1.2 GB".
// We prefer the byte count so we can re-format with our tabular formatter, and
// also use it to weight the bar segments; we fall back to WP's label when the
// byte count isn't present.

interface SizeEntry {
  label: string;
  bytes: number | null;
  display: string;
  swatch: string;
}

// chart-1..5 in payload order so each segment reads distinctly without any one
// hue dominating (DESIGN.md chart palette).
const SEGMENTS: Array<{ key: string; label: string; swatch: string }> = [
  { key: "wordpress_size", label: "WordPress", swatch: "bg-chart-1" },
  { key: "themes_size", label: "Themes", swatch: "bg-chart-2" },
  { key: "plugins_size", label: "Plugins", swatch: "bg-chart-3" },
  { key: "uploads_size", label: "Uploads", swatch: "bg-chart-4" },
  { key: "database_size", label: "Database", swatch: "bg-chart-5" },
];

export function CardDirectorySizes({
  card,
}: {
  card: SiteDiagnosticsCard | undefined;
}) {
  const payload = asWpNative(card);
  const sizes = section(payload, "wp-paths-sizes");
  const status = sizes?.directory_size_status;
  // Any non-"ok" status means at least one size didn't fully resolve. v0.9.15
  // agents emit "partial" or "unavailable"; v0.9.14 agents emitted "timeout".
  const partial =
    status === "partial" || status === "timeout" || status === "unavailable";

  const segments: SizeEntry[] = SEGMENTS.map((s) => {
    const bytes = fieldDebugNumber(sizes, s.key);
    const fallback = fieldValue(sizes, s.key);
    return {
      label: s.label,
      bytes,
      display: bytes !== null && bytes >= 0 ? formatBytes(bytes) : fallback ?? "",
      swatch: s.swatch,
    };
  }).filter((s) => s.display !== "");

  const fonts = sizeRow(sizes, "fonts_size", "Fonts");
  const total = sizeRow(sizes, "total_size", "Total");

  const rows = [
    ...segments.map((s) => ({
      label: s.label,
      value: s.display,
      mono: true,
    })),
    ...(fonts ? [fonts] : []),
    ...(total ? [total] : []),
  ];

  const barTotal = segments.reduce(
    (acc, s) => acc + (s.bytes != null && s.bytes > 0 ? s.bytes : 0),
    0,
  );
  const showBar = barTotal > 0;

  return (
    <DiagnosticCard
      title="Directory Sizes"
      card={card}
      note={
        status === "unavailable"
          ? "Sizes unavailable on this WordPress version"
          : status === "pending"
            ? "Computing directory sizes in the background — check back shortly"
            : status === "stale"
              ? "Showing the last successful scan; refreshing in the background"
              : partial
                ? "Partial: a directory walk did not finish in time"
                : undefined
      }
    >
      {rows.length > 0 ? (
        <>
          {showBar ? (
            <div className="space-y-2">
              <div
                className="flex h-2 w-full overflow-hidden rounded-full bg-muted"
                role="img"
                aria-label={segments
                  .filter((s) => s.bytes != null && s.bytes > 0)
                  .map((s) => `${s.label} ${s.display}`)
                  .join(", ")}
              >
                {segments
                  .filter((s) => s.bytes != null && s.bytes > 0)
                  .map((s) => (
                    <div
                      key={s.label}
                      style={{
                        width: `${Math.max(
                          1,
                          Math.round(((s.bytes as number) / barTotal) * 100),
                        )}%`,
                      }}
                      className={s.swatch}
                      aria-hidden="true"
                    />
                  ))}
              </div>
              <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs">
                {segments
                  .filter((s) => s.bytes != null && s.bytes > 0)
                  .map((s) => (
                    <span
                      key={s.label}
                      className="inline-flex items-center gap-1.5 text-muted-foreground"
                    >
                      <span
                        aria-hidden="true"
                        className={`size-2 rounded-sm ${s.swatch}`}
                      />
                      {s.label}
                    </span>
                  ))}
              </div>
            </div>
          ) : null}
          <DefinitionList rows={rows} />
        </>
      ) : (
        <p className="text-sm text-muted-foreground">
          Awaiting first sync from the agent.
        </p>
      )}
    </DiagnosticCard>
  );
}

function sizeRow(
  sec: ReturnType<typeof section>,
  key: string,
  label: string,
): { label: string; value: string; mono?: boolean } | null {
  if (!sec?.fields?.[key]) return null;
  const bytes = fieldDebugNumber(sec, key);
  if (bytes !== null && bytes >= 0) {
    return { label, value: formatBytes(bytes), mono: true };
  }
  const fallback = fieldValue(sec, key);
  if (fallback === null) return null;
  return { label, value: fallback, mono: true };
}
