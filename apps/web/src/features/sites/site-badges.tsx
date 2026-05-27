import type { Site } from "@wpmgr/api";

import { Badge } from "@/components/ui/badge";

const healthMeta: Record<
  Site["health_status"],
  { label: string; variant: "success" | "destructive" | "muted" }
> = {
  healthy: { label: "Healthy", variant: "success" },
  unreachable: { label: "Unreachable", variant: "destructive" },
  unknown: { label: "Unknown", variant: "muted" },
};

/** Color-coded site health badge (green / red / gray). */
export function HealthBadge({ status }: { status: Site["health_status"] }) {
  const meta = healthMeta[status];
  return (
    <Badge variant={meta.variant} aria-label={`Health: ${meta.label}`}>
      <span aria-hidden="true">●</span>
      {meta.label}
    </Badge>
  );
}

/** Enrollment badge: distinguishes an enrolled agent from a pending pairing. */
export function EnrollmentBadge({ site }: { site: Site }) {
  const enrolled = site.enrolled ?? false;
  return enrolled ? (
    <Badge variant="secondary">Enrolled</Badge>
  ) : (
    <Badge variant="outline">Pending</Badge>
  );
}
