import { createFileRoute } from "@tanstack/react-router";

import { PageHeader } from "@/components/shared/page-header";
import { useMe, canOperate } from "@/features/auth/use-auth";
import { AlertConfigForm } from "@/features/monitoring/alert-config-form";
import { AppHealthAlertUpgradePrompt } from "@/features/monitoring/app-health-alert-prompt";
import { AppHealthAlertingNotice } from "@/features/monitoring/app-health-alerting-notice";

export const Route = createFileRoute("/_authed/alerts")({
  component: AlertSettingsPage,
});

function AlertSettingsPage() {
  const { data: me } = useMe();
  const operate = canOperate(me);

  return (
    <section aria-labelledby="alerts-heading" className="max-w-2xl space-y-6">
      <PageHeader
        title="Alert settings"
        subline="Configure how this tenant is notified about downtime, security events, and new vulnerabilities."
      />

      {operate ? (
        <>
          {/* GH #291 Phase 3: the tenant's app-health alerting flag is now
              live (AlertConfig.app_alerts_enabled). This renders a one-time
              prompt only while it is off for the tenant and not yet
              dismissed. See use-app-health-alerting-availability.ts. */}
          <AppHealthAlertUpgradePrompt />
          <AlertConfigForm />
          <AppHealthAlertingNotice scope="tenant" />
        </>
      ) : (
        <p
          role="alert"
          className="rounded-xl border border-[var(--color-border)] p-4 text-sm text-[var(--color-muted-foreground)]"
        >
          You need the operator role or higher to manage alert settings. Ask an
          admin for access.
        </p>
      )}
    </section>
  );
}
