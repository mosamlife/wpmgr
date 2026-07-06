import type { AuditEntry } from "@wpmgr/api";

import { cn, formatBytes, relativeTime } from "@/lib/utils";
import { DefinitionList, type KvRowProps } from "@/components/shared/definition-list";

import {
  formatClockTime,
  formatDeliveryStatus,
  humanizeDeliveryReason,
  humanizeDeliveryStatus,
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
// email_reason/webhook_reason are folded into their paired email_status/
// webhook_status row (see DELIVERY_STATUS_REASON_KEYS below) instead of
// rendering as their own separate line.
const HANDLED_KEYS = new Set(["path", "reason", "email_reason", "webhook_reason"]);

// Maps a delivery-status metadata key to the reason key that, when present,
// explains a non-"sent" outcome.
const DELIVERY_STATUS_REASON_KEYS: Record<string, string> = {
  email_status: "email_reason",
  webhook_status: "webhook_reason",
};

export function AuditEntryDetail({ entry }: { entry: AuditEntry }) {
  const denied = entry.action.endsWith(".denied");
  const reason = metaString(entry.metadata, "reason");
  const path = metaString(entry.metadata, "path");

  const metaEntries = Object.entries(entry.metadata ?? {}).filter(
    ([key]) => !HANDLED_KEYS.has(key),
  );

  const metaRows: KvRowProps[] = metaEntries.map(([key, value]) => {
    const reasonKey = DELIVERY_STATUS_REASON_KEYS[key];
    if (reasonKey && typeof value === "string") {
      return {
        label: humanizeKey(key),
        value: (
          <DeliveryStatusValue
            status={value}
            reason={metaString(entry.metadata, reasonKey)}
          />
        ),
      };
    }
    return {
      label: humanizeKey(key),
      value: formatMetaValue(key, value),
      mono: key.endsWith("_id") || key === "ip" || key === "user_agent",
    };
  });

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

// Tone for the email_status/webhook_status pill: success for "sent",
// destructive for "failed", and the quiet muted default for "skipped" (and
// any future/unknown status code) — a skip isn't an error, so it shouldn't
// read as one.
const DELIVERY_STATUS_TONE: Record<string, string> = {
  sent: "bg-success-subtle text-success-subtle-fg",
  failed: "bg-destructive-subtle text-destructive-subtle-fg",
};
const DELIVERY_STATUS_DEFAULT_TONE = "bg-muted text-muted-foreground";

/** Renders an email_status/webhook_status value as a colored pill, with the
 * paired reason (when the outcome isn't "sent") as plain text alongside it —
 * e.g. a "Skipped" pill next to "(SMTP not configured)". Replaces the old
 * "Emailed: Yes" line, which asserted delivery even when it was skipped or
 * failed. */
function DeliveryStatusValue({
  status,
  reason,
}: {
  status: string;
  reason: string | null;
}) {
  const tone = DELIVERY_STATUS_TONE[status] ?? DELIVERY_STATUS_DEFAULT_TONE;
  return (
    <span
      className="inline-flex flex-wrap items-center gap-1.5"
      title={formatDeliveryStatus(status, reason)}
    >
      <span
        className={cn(
          "inline-flex w-fit items-center rounded px-1.5 py-0.5 text-[11px] font-medium",
          tone,
        )}
      >
        {humanizeDeliveryStatus(status)}
      </span>
      {status !== "sent" && reason ? (
        <span className="text-xs text-muted-foreground">
          ({humanizeDeliveryReason(reason)})
        </span>
      ) : null}
    </span>
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
