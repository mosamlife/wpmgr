import type { AuditEntry } from "@wpmgr/api";

import { formatBytes, relativeTime } from "@/lib/utils";
import { DefinitionList, type KvRowProps } from "@/components/shared/definition-list";

import {
  formatClockTime,
  humanizeKey,
  humanizeReason,
  humanizeTargetType,
  metaString,
} from "./metadata";

// AuditEntryDetail — the "detail on demand" panel (redesign point 10). Every
// row is expandable; this surfaces everything the control plane stores but
// the collapsed row never shows: the raw action key, a denial's WHY, and any
// remaining metadata (size, src/dst, version_id, transfer_id, ip, user
// agent, hash / prev_hash) without hand-listing every domain's exact field
// set — unknown metadata keys still render, just humanized and generic.

// Fields already surfaced elsewhere in the row (path in the target slot,
// reason in the "why" callout) or too internal to repeat verbatim here.
const HANDLED_KEYS = new Set(["path", "reason"]);

export function AuditEntryDetail({ entry }: { entry: AuditEntry }) {
  const denied = entry.action.endsWith(".denied");
  const reason = metaString(entry.metadata, "reason");
  const path = metaString(entry.metadata, "path");

  const metaEntries = Object.entries(entry.metadata ?? {}).filter(
    ([key]) => !HANDLED_KEYS.has(key),
  );

  const metaRows: KvRowProps[] = metaEntries.map(([key, value]) => ({
    label: humanizeKey(key),
    value: formatMetaValue(key, value),
    mono: key.endsWith("_id") || key === "ip" || key === "user_agent",
  }));

  return (
    <div className="space-y-3 border-t border-border bg-muted/20 px-4 py-3 text-sm">
      {denied && reason ? (
        <div className="rounded-md border border-destructive-subtle bg-destructive-subtle px-3 py-2 text-xs text-destructive-subtle-fg">
          <span className="font-medium">Why: </span>
          {humanizeReason(reason)}
        </div>
      ) : null}

      {path ? (
        <DefinitionList rows={[{ label: "Path", copyable: path }]} />
      ) : null}

      {metaRows.length > 0 ? <DefinitionList rows={metaRows} /> : null}

      <DefinitionList
        rows={[
          { label: "Event key", value: entry.action, mono: true },
          {
            label: "Target",
            value:
              entry.target_id.length > 0
                ? `${humanizeTargetType(entry.target_type)} · ${entry.target_id}`
                : undefined,
            mono: entry.target_id.length > 0,
          },
          { label: "Recorded", value: new Date(entry.created_at).toLocaleString() },
          { label: "Entry hash", copyable: entry.hash },
          { label: "Previous hash", copyable: entry.prev_hash },
        ]}
      />
    </div>
  );
}

function formatMetaValue(key: string, value: unknown): string {
  if (value == null) return "";
  if (key === "size" && typeof value === "number") return formatBytes(value);
  if (typeof value === "boolean") return value ? "Yes" : "No";
  if (typeof value === "number") return value.toLocaleString();
  if (typeof value === "string") return value;
  try {
    return JSON.stringify(value) ?? "(unavailable)";
  } catch {
    return "(unavailable)";
  }
}

/** Path + timestamp list inside an expanded collapsed run (redesign point 6). */
export function RunDetail({
  entries,
  isToday,
}: {
  entries: AuditEntry[];
  isToday: boolean;
}) {
  return (
    <ul className="space-y-1 border-t border-border bg-muted/20 px-4 py-3">
      {entries.map((entry) => {
        const path = metaString(entry.metadata, "path");
        return (
          <li
            key={entry.id}
            className="flex items-center justify-between gap-3 border-l border-border pl-3 text-xs text-muted-foreground"
          >
            <span className="min-w-0 truncate font-mono" title={path ?? undefined}>
              {path ?? "—"}
            </span>
            <span className="shrink-0 tabular-nums" title={entry.created_at}>
              {isToday
                ? (relativeTime(entry.created_at) ?? "just now")
                : formatClockTime(entry.created_at)}
            </span>
          </li>
        );
      })}
    </ul>
  );
}
