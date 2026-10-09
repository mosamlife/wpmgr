import { createFileRoute } from "@tanstack/react-router";

import { PageHeader } from "@/components/shared/page-header";
import { useMe, canManageInstanceEmail } from "@/features/auth/use-auth";
import { VulnFeedPanel } from "@/features/admin/vuln-feed-panel";

// The vulnerability feed key is instance-wide, and whoever may manage the
// instance email settings may manage it: a superadmin, or the owner of the
// install's only organisation. `me.can_manage_instance_email` reports that same
// decision, so this page and the "Vulnerability feed" settings entry follow it
// and nothing else. The server applies the decision to every feed route, so a
// principal it does not admit has no read-only view either.
export const Route = createFileRoute("/_authed/settings/vuln-feed")({
  component: VulnFeedSettingsPage,
});

function VulnFeedSettingsPage() {
  const { data: me } = useMe();

  // Say so plainly instead of mounting the panel, which would fire a request
  // that can only be refused and then show a load failure with a Retry that can
  // never succeed.
  if (!canManageInstanceEmail(me)) {
    return (
      <section aria-labelledby="vuln-feed-heading" className="max-w-2xl space-y-6">
        <PageHeader
          title="Vulnerability feed"
          subline="Wordfence Intelligence feed configuration for this instance."
        />
        <p
          role="alert"
          className="rounded-xl border border-[var(--color-border)] p-4 text-sm text-[var(--color-muted-foreground)]"
        >
          Only the instance administrator can change these settings. Ask your
          instance administrator to make changes.
        </p>
      </section>
    );
  }

  return (
    <div className="max-w-2xl">
      <VulnFeedPanel />
    </div>
  );
}
