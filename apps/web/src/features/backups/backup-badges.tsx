import type { BackupSnapshot } from "@wpmgr/api";

import { Badge } from "@/components/ui/badge";

type Status = BackupSnapshot["status"];
type Kind = BackupSnapshot["kind"];

const statusMeta: Record<
  Status,
  { label: string; variant: "success" | "destructive" | "muted" | "secondary" }
> = {
  pending: { label: "Pending", variant: "muted" },
  running: { label: "Running", variant: "secondary" },
  completed: { label: "Completed", variant: "success" },
  failed: { label: "Failed", variant: "destructive" },
};

/** Color-coded snapshot/restore status badge. */
export function StatusBadge({ status }: { status: Status }) {
  const meta = statusMeta[status];
  return (
    <Badge variant={meta.variant} aria-label={`Status: ${meta.label}`}>
      {meta.label}
    </Badge>
  );
}

const kindLabel: Record<Kind, string> = {
  files: "Files",
  db: "Database",
  full: "Full",
};

/** Neutral badge describing what a snapshot captured (files / db / full). */
export function KindBadge({ kind }: { kind: Kind }) {
  return <Badge variant="outline">{kindLabel[kind]}</Badge>;
}
