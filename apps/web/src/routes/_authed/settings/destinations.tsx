// Backup destinations settings route (ADR-036 P1). Destinations are scoped
// per-site in the API, so this settings page lets the operator pick a site
// then renders the per-site list below. The "where do my backups go?" picture
// is genuinely a tenant-level concern — but the destination ROWS belong to a
// site (different sites can target different buckets), hence the picker.

import { useMemo, useState } from "react";
import { createFileRoute } from "@tanstack/react-router";

import { useMe, canOperate } from "@/features/auth/use-auth";
import { useSites } from "@/features/sites/use-sites";
import { DestinationsList } from "@/features/destinations/destinations-list";

export const Route = createFileRoute("/_authed/settings/destinations")({
  component: DestinationsSettingsPage,
});

function DestinationsSettingsPage() {
  const { data: me } = useMe();
  const operate = canOperate(me);
  const { data: sites, isPending: sitesLoading, isError: sitesError } = useSites();
  const [siteId, setSiteId] = useState<string>("");

  // Auto-pick the first site when the list arrives so the page isn't empty
  // on first load.
  const effectiveSiteId = useMemo(() => {
    if (siteId) return siteId;
    return sites?.[0]?.id ?? "";
  }, [siteId, sites]);

  return (
    <section
      aria-labelledby="destinations-heading"
      className="max-w-4xl space-y-6"
    >
      <div>
        <h1 id="destinations-heading" className="text-2xl font-semibold">
          Backup destinations
        </h1>
        <p className="text-sm text-[var(--color-muted-foreground)]">
          Pick a site, then choose where its backup chunks should land — our
          managed storage (default), a folder on the same webserver, or your
          own S3-compatible bucket.
        </p>
      </div>

      {!operate ? (
        <p
          role="alert"
          className="rounded-xl border border-[var(--color-border)] p-4 text-sm text-[var(--color-muted-foreground)]"
        >
          You need at least the operator role to manage destinations.
        </p>
      ) : sitesLoading ? (
        <p role="status">Loading sites…</p>
      ) : sitesError ? (
        <p role="alert" className="text-[var(--color-destructive)]">
          Failed to load sites.
        </p>
      ) : !sites || sites.length === 0 ? (
        <div className="rounded-xl border border-dashed border-[var(--color-border)] p-8 text-center">
          <p className="text-sm">
            No destinations yet. WPMgr ships backups to our managed storage by
            default. Add a destination here if you want to send them elsewhere
            too.
          </p>
          <p className="mt-2 text-xs text-[var(--color-muted-foreground)]">
            Connect a site first to configure its destinations.
          </p>
        </div>
      ) : (
        <>
          <div className="rounded-xl border border-[var(--color-border)] p-4">
            <label
              htmlFor="site-picker"
              className="block text-sm font-medium"
            >
              Site
            </label>
            <select
              id="site-picker"
              value={effectiveSiteId}
              onChange={(e) => setSiteId(e.target.value)}
              className="mt-1 h-9 w-full rounded-md border border-[var(--color-border)] bg-transparent px-3 text-sm"
            >
              {sites.map((s) => (
                <option key={s.id} value={s.id}>
                  {s.url}
                </option>
              ))}
            </select>
          </div>

          {effectiveSiteId ? (
            <DestinationsList siteId={effectiveSiteId} />
          ) : null}
        </>
      )}
    </section>
  );
}
