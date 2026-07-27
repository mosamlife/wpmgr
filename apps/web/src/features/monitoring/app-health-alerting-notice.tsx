import { Construction } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { useAlertConfig } from "./use-uptime";

// GH #291 Phase 3, Task 2's "surface the app-health alerting toggle, the
// per-site custom health path, and the per-site alert mute" requirement.
//
// The tenant-wide switch now has a real home: the "Application health"
// section of `alert-config-form.tsx`, backed by `AlertConfig
// .app_alerts_enabled`. This panel no longer claims that switch is planned.
// The other two Phase 3 controls, a per-site custom health-check path and a
// per-site mute, still have no consumer in `apps/web` (see
// `use-app-health-alerting-availability.ts` for exactly what is missing and
// why), so at site scope this keeps following this codebase's convention for
// "designed, not yet backed" (`components/feedback/planned-feature.tsx`,
// ADR-037): an honest, calm, real-text panel that names what is coming, at
// section scope rather than that component's whole-page scope.
//
// The badge reads the tenant's real `AlertConfig.app_alerts_enabled` (via
// `useAlertConfig`, the same query the form and the upgrade prompt already
// share) so it always tells the truth about whether alerting is currently on
// or off for this tenant, never a hard-coded or stale label.
//
// It DOES ship real value today, independent of the two remaining gaps: the
// explanation of what `unknown` means and which two conditions are
// conclusive enough to ever trigger an alert, so an operator who sees a site
// come back "Unknown" on the Fleet page understands why that is not the same
// as "broken", the same conservative classifier already live in Phase 2.

export interface AppHealthAlertingNoticeProps {
  /** Where this panel is mounted, so the "coming" bullet list matches the
   * controls that actually belong at that scope (tenant vs per-site). */
  scope: "tenant" | "site";
}

export function AppHealthAlertingNotice({ scope }: AppHealthAlertingNoticeProps) {
  const { data: config, isPending } = useAlertConfig();
  const appAlertsEnabled = config?.app_alerts_enabled ?? false;

  return (
    <div className="space-y-3 rounded-lg border border-[var(--color-border)] p-4">
      <div className="flex items-start gap-3">
        <Construction
          aria-hidden="true"
          strokeWidth={1.5}
          className="mt-0.5 size-4 shrink-0 text-[var(--color-muted-foreground)]"
        />
        <div className="min-w-0 flex-1 space-y-3">
          <div className="flex flex-wrap items-center gap-2">
            <h3 className="text-sm font-semibold text-[var(--color-foreground)]">
              Application health alerts
            </h3>
            {isPending ? null : (
              <Badge variant={appAlertsEnabled ? "success" : "muted"}>
                {appAlertsEnabled ? "On" : "Off"}
              </Badge>
            )}
          </div>

          <p className="text-sm text-[var(--color-muted-foreground)]">
            The dashboard already detects when a site's WordPress backend has
            stopped responding, even while a page cache keeps serving
            visitors, and shows it on the Fleet page as Degraded with the
            reason "WordPress not responding." Sending an alert when that
            happens is available. It is off by default on any deployment
            that already had sites when this shipped, so an upgrade cannot
            wake anyone with a wave of alerts.
          </p>

          <p className="text-sm text-[var(--color-muted-foreground)]">
            Most inconclusive responses (a cache hit, a maintenance page, a
            timeout, a 502 or 504) are reported as unknown, not broken. Only
            two things ever count as conclusively down: an HTTP 500, or a
            page that carries WordPress's own fatal-error signature. An
            alert only fires on one of those, so an unknown result does not
            mean something is wrong, it means the probe could not tell
            either way.
          </p>

          {scope === "tenant" ? (
            <p className="text-sm text-[var(--color-muted-foreground)]">
              The tenant-wide switch to turn application health alerts on or
              off is in the Application health section above, alongside the
              rest of this tenant's alert channel.
            </p>
          ) : (
            <div className="space-y-1">
              <p className="text-sm font-medium text-[var(--color-foreground)]">
                Planned for this section:
              </p>
              <ul className="list-disc space-y-1 pl-5 text-sm text-[var(--color-muted-foreground)]">
                <li>
                  A custom health-check path for this site, for
                  installations where the default check is not reachable.
                </li>
                <li>
                  A mute switch to stop this specific site from sending
                  application health alerts, without affecting the rest of
                  the tenant.
                </li>
              </ul>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
