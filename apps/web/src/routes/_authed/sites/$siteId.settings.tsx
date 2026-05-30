import { createFileRoute } from "@tanstack/react-router";
import type { ReactNode } from "react";

import { SiteComponentsTable } from "@/features/sites/site-components-table";
import { countUpToDate } from "@/features/sites/site-components-helpers";
import { SiteTagsEditor } from "@/features/sites/site-tags-editor";
import { AutoLoginButton } from "@/features/sites/auto-login-button";
import { LoginBrandPanel } from "@/features/sites/login-brand-panel";
import { useSite } from "@/features/sites/use-sites";
import { useMe, canOperate } from "@/features/auth/use-auth";
import type { Site } from "@wpmgr/api";

// `/sites/$siteId/settings` — Identity + Tags + Components + Access.
//
// One card surface holding multiple stacked sections separated by horizontal
// rules — NOT four nested cards (DESIGN.md: "Don't nest cards.").

export const Route = createFileRoute("/_authed/sites/$siteId/settings")({
  component: SettingsTab,
});

function SettingsTab() {
  const { siteId } = Route.useParams();
  // Layout has already resolved the site query; this call hits the React Query
  // cache (no network). Rendering a thin loading state covers the edge case
  // where this child mounted from a direct deep link before layout settled.
  const { data: site, isPending } = useSite(siteId);
  const { data: me } = useMe();
  const operate = canOperate(me);

  if (isPending || !site) {
    return (
      <div className="px-4 pb-8 pt-6 sm:px-6">
        <p role="status" className="text-sm text-muted-foreground">
          Loading settings…
        </p>
      </div>
    );
  }

  return (
    <section
      aria-labelledby="settings-heading"
      className="px-4 pb-8 pt-6 sm:px-6"
    >
      <h2
        id="settings-heading"
        className="mb-3 text-xs font-medium uppercase tracking-wide text-muted-foreground"
      >
        Settings
      </h2>
      <SettingsCard site={site} canEditTags={operate} />
    </section>
  );
}

function SettingsCard({
  site,
  canEditTags,
}: {
  site: Site;
  canEditTags: boolean;
}) {
  return (
    <div className="rounded-lg border border-border bg-card">
      <div className="space-y-1 p-6">
        <h3 className="text-sm font-semibold text-foreground">Identity</h3>
        <dl className="grid grid-cols-1 gap-x-6 gap-y-2 text-sm sm:grid-cols-2">
          <Detail label="Name" value={site.name} />
          <Detail
            label="URL"
            value={<span className="font-mono break-all">{site.url}</span>}
          />
          <Detail
            label="Site ID"
            value={<span className="font-mono">{site.id}</span>}
          />
          <Detail label="Status" value={site.status} />
          <Detail
            label="WordPress"
            value={
              <span className="font-mono tabular-nums">
                {site.wp_version || "–"}
              </span>
            }
          />
          <Detail
            label="PHP"
            value={
              <span className="font-mono tabular-nums">
                {site.php_version || "–"}
              </span>
            }
          />
        </dl>
      </div>
      <hr className="border-border" aria-hidden="true" />
      <div className="space-y-3 p-6">
        <h3 className="text-sm font-semibold text-foreground">Tags</h3>
        {canEditTags ? (
          <SiteTagsEditor site={site} />
        ) : site.tags.length > 0 ? (
          <div className="flex flex-wrap gap-1">
            {site.tags.map((tag) => (
              <span
                key={tag}
                className="rounded border border-border px-2 py-0.5 text-xs"
              >
                {tag}
              </span>
            ))}
          </div>
        ) : (
          <p className="text-sm text-muted-foreground">No tags.</p>
        )}
      </div>
      <hr className="border-border" aria-hidden="true" />
      <div className="space-y-3 p-6">
        <h3 className="text-sm font-semibold text-foreground">
          Installed components (
          {countUpToDate(site.components?.plugins, site.components?.themes)})
        </h3>
        <p className="text-xs text-muted-foreground">
          Plugins and themes already up to date.
        </p>
        <SiteComponentsTable
          plugins={site.components?.plugins}
          themes={site.components?.themes}
        />
      </div>
      <hr className="border-border" aria-hidden="true" />
      <div className="space-y-3 p-6">
        <h3 className="text-sm font-semibold text-foreground">Access</h3>
        <p className="text-xs text-muted-foreground">
          One-click sign in into wp-admin as an existing administrator. Logs the
          action to the audit trail.
        </p>
        <AutoLoginButton siteId={site.id} siteName={site.name} />
      </div>
      <hr className="border-border" aria-hidden="true" />
      <div className="space-y-3 p-6">
        <h3 className="text-sm font-semibold text-foreground">
          Login page branding
        </h3>
        <p className="text-xs text-muted-foreground">
          Customise the logo, logo link, and message shown on wp-login.php.
        </p>
        <LoginBrandPanel siteId={site.id} />
      </div>
    </div>
  );
}

function Detail({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div className="min-w-0">
      <dt className="text-xs uppercase tracking-wide text-muted-foreground">
        {label}
      </dt>
      <dd className="break-words font-medium">{value}</dd>
    </div>
  );
}
