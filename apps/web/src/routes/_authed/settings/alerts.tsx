import { createFileRoute } from "@tanstack/react-router";

import { PageHeader } from "@/components/shared/page-header";
import { useMe, canOperate } from "@/features/auth/use-auth";
import { AlertConfigForm } from "@/features/monitoring/alert-config-form";

// Future field: notify_security (boolean) -- suppress/surface security scanner
// alerts per-tenant. Needs an ogen regen once the backend route is wired.
// Do not add it here until the OpenAPI spec is updated.

export const Route = createFileRoute("/_authed/settings/alerts")({
  component: AlertSettingsPage,
});

function AlertSettingsPage() {
  const { data: me } = useMe();
  const operate = canOperate(me);

  return (
    <section aria-labelledby="alerts-heading" className="max-w-2xl space-y-6">
      <PageHeader
        title="Alert settings"
        subline="Configure how this tenant is notified when monitored sites go down."
      />

      {operate ? (
        <AlertConfigForm />
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
