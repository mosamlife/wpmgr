import type { Site } from "@wpmgr/api";

import { Badge } from "@/components/ui/badge";
import { StatusChip, type StatusTone } from "@/components/status";

// Health status maps to a StatusTone so the chip reads correctly at a glance.
// "healthy" maps to success (green), "unreachable" to destructive (red), and
// "unknown" to muted (gray) — same tone mapping used in the sites table.
const healthMeta: Record<
  Site["health_status"],
  { label: string; tone: StatusTone; pulse: boolean }
> = {
  healthy: { label: "Healthy", tone: "success", pulse: true },
  unreachable: { label: "Unreachable", tone: "destructive", pulse: false },
  unknown: { label: "Unknown", tone: "muted", pulse: false },
};

/**
 * HealthBadge — StatusChip variant of the health indicator. Dot + label,
 * no bare colored dot, no purple, tokens only.
 */
export function HealthBadge({ status }: { status: Site["health_status"] }) {
  const meta = healthMeta[status];
  return (
    <StatusChip
      tone={meta.tone}
      label={meta.label}
      pulse={meta.pulse}
    />
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
