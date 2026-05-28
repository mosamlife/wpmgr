import { createFileRoute } from "@tanstack/react-router";

import { useMe, canOperate } from "@/features/auth/use-auth";
import { AlertConfigForm } from "@/features/monitoring/alert-config-form";

export const Route = createFileRoute("/_authed/settings/alerts")({
  component: AlertSettingsPage,
});

function AlertSettingsPage() {
  const { data: me } = useMe();
  const operate = canOperate(me);

  return (
    <section aria-labelledby="alerts-heading" className="max-w-2xl space-y-6">
      <div>
        <h1 id="alerts-heading" className="text-2xl font-semibold">
          Alert settings
        </h1>
        <p className="text-sm text-[var(--color-muted-foreground)]">
          Configure how this tenant is notified about site downtime.
        </p>
      </div>

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
