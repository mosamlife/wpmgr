import { PauseCircle } from "lucide-react";
import type { Site } from "@wpmgr/api";

import { Badge } from "@/components/ui/badge";
import { StatusChip } from "@/components/status";
import { useNow } from "@/lib/use-now";
import { cn } from "@/lib/utils";
import { uptimeBadgeFor, pausedBadgeFor } from "@/features/sites/monitoring-pause";

/**
 * UptimeBadge — StatusChip variant of the uptime indicator. Dot + label,
 * no bare colored dot, no purple, tokens only.
 *
 * GH #414. It takes the whole `site` rather than the bare `up` flag because a
 * paused site's uptime result is FROZEN: phase 2 filters paused sites out of
 * the uptime prober's enumeration, and the prober is what refreshes `up` /
 * `uptime_pct`. `uptimeBadgeFor` greys the chip and stamps it with the age of
 * `health_checked_at` (the prober's own `last_probed_at`); see that function
 * for the decision and why.
 *
 * `site.health_status` is a different, unrelated field with two other writers
 * that never stop under pause — it is not read here, and nothing in this
 * component mutes it. See monitoring-pause.ts's file header.
 */
export function UptimeBadge({ site }: { site: Site }) {
  // 30 s cadence: coarse enough not to thrash, fine enough that "as of 4m ago"
  // does not sit stale on a long-open dashboard.
  const now = useNow(30_000);
  const view = uptimeBadgeFor(site, now);
  return (
    <StatusChip
      tone={view.tone}
      label={view.label}
      time={view.time ?? undefined}
      pulse={view.pulse}
      className={view.tone === "muted" ? "text-muted-foreground" : undefined}
    />
  );
}

/**
 * PausedBadge — GH #414. A pause you cannot see is a pause you forget, so this
 * rides the site card and the site row whenever `monitoring_paused_at` is set.
 * Who paused it, since when, why, and when it comes back are all in the hover
 * text; the chip itself stays short enough not to crowd the row.
 */
export function PausedBadge({
  site,
  resolveActor,
  className,
}: {
  site: Site;
  resolveActor?: (userId: string) => string | null;
  className?: string;
}) {
  const now = useNow(30_000);
  const view = pausedBadgeFor(site, resolveActor, now);
  if (!view) return null;
  return (
    <Badge
      variant="outline"
      title={view.title}
      aria-label={view.title}
      className={cn(
        // warning-SUBTLE-FG, not warning-foreground (GH #414, a repeat of
        // GH #322 — see agent-column-header.tsx:186-199 for the full
        // reasoning). This badge has no warning fill, only a border, so its
        // text sits on the ordinary card/row surface. --warning-foreground is
        // the text colour for content sitting ON a --warning background: it
        // is near-black in both themes and darker still in dark mode, which
        // is why the pill read as empty. --warning-subtle-fg is the token for
        // warning-tinted text on an ordinary surface and inverts properly.
        "gap-1 border-warning/40 text-warning-subtle-fg",
        className,
      )}
    >
      <PauseCircle aria-hidden="true" className="size-3" />
      {view.label}
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
