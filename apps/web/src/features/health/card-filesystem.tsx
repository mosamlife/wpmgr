import type { SiteDiagnosticsCard } from "@wpmgr/api";

import { DefinitionList } from "@/components/shared/definition-list";
import { formatBytes } from "@/lib/utils";

import { DiagnosticCard } from "./diagnostic-card";
import { pickBool, pickNumber, pickString } from "./diagnostic-pick";

// Filesystem card — the single Storage "where do the bytes live + can we write
// them" picture. It carries the disk-usage bar that used to live in the
// route-level HostingCard (wp-content / uploads vs free headroom), so there is
// ONE storage visual rather than two competing surfaces.

export interface FilesystemDisk {
  wpContentBytes: number;
  uploadsBytes: number;
  freeBytes: number;
}

export function CardFilesystem({
  card,
  disk,
}: {
  card: SiteDiagnosticsCard | undefined;
  disk?: FilesystemDisk;
}) {
  const payload = card?.payload as Record<string, unknown> | null | undefined;
  const freeBytes = pickNumber(payload, "free_bytes");

  const hasDisk =
    disk != null &&
    (disk.wpContentBytes > 0 || disk.uploadsBytes > 0 || disk.freeBytes > 0);

  return (
    <DiagnosticCard title="Filesystem" card={card}>
      {hasDisk ? <DiskBar disk={disk} /> : null}
      <DefinitionList
        rows={[
          {
            label: "wp-content",
            value: pickString(payload, "wp_content_dir"),
            mono: true,
          },
          {
            label: "wp-content writable",
            value: pickBool(payload, "wp_content_writable") ? "Yes" : "No",
          },
          {
            label: "Uploads",
            value: pickString(payload, "uploads_dir"),
            mono: true,
          },
          {
            label: "Uploads writable",
            value: pickBool(payload, "uploads_writable") ? "Yes" : "No",
          },
          {
            label: "Free space",
            value: freeBytes > 0 ? formatBytes(freeBytes) : undefined,
            mono: true,
            tabular: true,
          },
          {
            label: "open_basedir",
            value: pickString(payload, "open_basedir", "Not set"),
            mono: true,
          },
        ]}
      />
    </DiagnosticCard>
  );
}

// Disk usage bar: wp-content + uploads relative to the larger of (used, free).
// free_bytes is "free on the filesystem" and wp-content/uploads are "used by
// WP" — they are not strictly co-domain (the disk also holds the OS, MySQL,
// other apps), so we do not sum-and-call-it-total; the bar visualizes the
// proportion WP itself takes vs the headroom we would have to grow into.
function DiskBar({ disk }: { disk: FilesystemDisk }) {
  const { wpContentBytes, uploadsBytes, freeBytes } = disk;
  const total = Math.max(wpContentBytes + uploadsBytes, freeBytes) || 1;
  const wpPct = Math.min(100, Math.round((wpContentBytes / total) * 100));
  const upPct = Math.min(100, Math.round((uploadsBytes / total) * 100));

  return (
    <div className="space-y-2" data-testid="host-disk-usage">
      <div
        className="flex h-2 w-full overflow-hidden rounded-full bg-muted"
        role="img"
        aria-label={`wp-content ${formatBytes(wpContentBytes)}, uploads ${formatBytes(uploadsBytes)}, free ${formatBytes(freeBytes)}`}
      >
        <div
          style={{ width: `${wpPct}%` }}
          className="bg-chart-1"
          aria-hidden="true"
        />
        <div
          style={{ width: `${upPct}%` }}
          className="bg-chart-3"
          aria-hidden="true"
        />
      </div>
      <dl className="flex flex-wrap gap-x-4 gap-y-1 text-xs">
        <LegendItem swatch="bg-chart-1" label="wp-content" bytes={wpContentBytes} />
        <LegendItem swatch="bg-chart-3" label="uploads" bytes={uploadsBytes} />
        <LegendItem swatch="bg-muted" label="free" bytes={freeBytes} />
      </dl>
    </div>
  );
}

function LegendItem({
  swatch,
  label,
  bytes,
}: {
  swatch: string;
  label: string;
  bytes: number;
}) {
  return (
    <div className="inline-flex items-center gap-1.5">
      <span aria-hidden="true" className={`size-2 rounded-sm ${swatch}`} />
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="font-mono tabular-nums text-foreground">
        {formatBytes(bytes)}
      </dd>
    </div>
  );
}
