// Backup destinations settings route (ADR-036 P1). Destinations are scoped
// per-site in the API, so this settings page lets the operator pick a site
// then renders the per-site list below. The "where do my backups go?" picture
// is genuinely a tenant-level concern -- but the destination ROWS belong to a
// site (different sites can target different buckets), hence the picker.

import { useMemo, useState } from "react";
import { createFileRoute } from "@tanstack/react-router";

import { PageError } from "@/components/feedback";
import { PageHeader } from "@/components/shared/page-header";
import { Label } from "@/components/ui/label";
import { useMe, canOperate } from "@/features/auth/use-auth";
import { useSites } from "@/features/sites/use-sites";
import { DestinationsList } from "@/features/destinations/destinations-list";

export const Route = createFileRoute("/_authed/settings/destinations")({
  component: DestinationsSettingsPage,
});

function DestinationsSettingsPage() {
  const { data: me } = useMe();
  const operate = canOperate(me);
  const {
    data: sites,
    isPending: sitesLoading,
    isError: sitesError,
    error: sitesErrorObj,
    refetch: sitesRefetch,
    isRefetching: sitesRefetching,
  } = useSites();
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
      <PageHeader
        title="Backup destinations"
        subline="Pick a site, then choose where its backup chunks should land — managed storage (default), a folder on the same server, or your own S3-compatible bucket."
      />

      {!operate ? (
        <p
          role="alert"
          className="rounded-xl border border-[var(--color-border)] p-4 text-sm text-[var(--color-muted-foreground)]"
        >
          You need the operator role or higher to manage backup destinations.
          Ask an admin for access.
        </p>
      ) : sitesLoading ? (
        <p role="status" className="text-[var(--color-muted-foreground)]">
          Loading sites…
        </p>
      ) : sitesError ? (
        <PageError
          what="Could not load sites."
          why={sitesErrorObj.message}
          onRetry={() => void sitesRefetch()}
          retryLabel="Reload sites"
          isRetrying={sitesRefetching}
        />
      ) : !sites || sites.length === 0 ? (
        <div className="rounded-xl border border-dashed border-[var(--color-border)] p-8 text-center">
          <p className="text-sm font-medium text-[var(--color-foreground)]">
            No sites connected yet.
          </p>
          <p className="mt-1 text-sm text-[var(--color-muted-foreground)]">
            Connect a site first, then configure its backup destinations here.
          </p>
        </div>
      ) : (
        <>
          <div className="space-y-1.5">
            <Label htmlFor="site-picker">Site</Label>
            <select
              id="site-picker"
              value={effectiveSiteId}
              onChange={(e) => setSiteId(e.target.value)}
              className="h-9 w-full max-w-sm rounded-md border border-[var(--color-border)] bg-transparent px-3 text-sm font-mono"
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
