import { Badge } from "@/components/ui/badge";
import {
  statusMeta,
  type UpDown,
} from "@/features/monitoring/uptime-badges-helpers";

/** Color-coded current up/down status badge (green / red / gray). */
export function UptimeStatusBadge({ status }: { status: UpDown }) {
  const meta = statusMeta[status];
  return (
    <Badge variant={meta.variant} aria-label={`Status: ${meta.label}`}>
      <span aria-hidden="true">●</span>
      {meta.label}
    </Badge>
  );
}
