import type { ReactNode } from "react";
import { createFileRoute, Link } from "@tanstack/react-router";

import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { useSite, NotFoundError } from "@/features/sites/use-sites";
import { HealthBadge, EnrollmentBadge } from "@/features/sites/site-badges";
import { SiteComponentsTable } from "@/features/sites/site-components-table";
import { SiteTagsEditor } from "@/features/sites/site-tags-editor";
import { BackupsSection } from "@/features/backups/backups-section";
import { UptimeSection } from "@/features/monitoring/uptime-section";
import { useMe, canOperate } from "@/features/auth/use-auth";
import { relativeTime } from "@/lib/utils";
import type { Site } from "@wpmgr/api";

export const Route = createFileRoute("/_authed/sites/$siteId")({
  component: SiteDetailPage,
});

function SiteDetailPage() {
  const { siteId } = Route.useParams();
  const { data: site, isPending, isError, error, refetch } = useSite(siteId);
  const { data: me } = useMe();
  const operate = canOperate(me);

  return (
    <section aria-labelledby="site-heading" className="space-y-6">
      <div className="flex items-center gap-3">
        <Button asChild variant="outline" size="sm">
          <Link to="/sites">Back to sites</Link>
        </Button>
      </div>

      {isPending ? (
        <p role="status" className="text-[var(--color-muted-foreground)]">
          Loading site…
        </p>
      ) : isError ? (
        error instanceof NotFoundError ? (
          <div role="alert" className="space-y-2">
            <h1 id="site-heading" className="text-2xl font-semibold">
              Site not found
            </h1>
            <p className="text-[var(--color-muted-foreground)]">
              No site exists with id <code>{siteId}</code>.
            </p>
          </div>
        ) : (
          <div role="alert" className="space-y-3">
            <h1 id="site-heading" className="text-2xl font-semibold">
              Could not load site
            </h1>
            <p className="text-[var(--color-destructive)]">{error.message}</p>
            <Button variant="outline" size="sm" onClick={() => void refetch()}>
              Retry
            </Button>
          </div>
        )
      ) : (
        <SiteDetail site={site} canOperate={operate} />
      )}
    </section>
  );
}

function SiteDetail({
  site,
  canOperate,
}: {
  site: Site;
  canOperate: boolean;
}) {
  const canEditTags = canOperate;
  const lastSeen = relativeTime(site.last_seen_at);
  const enrolledAt = relativeTime(site.enrolled_at);

  return (
    <div className="space-y-6">
      <div className="space-y-1">
        <h1 id="site-heading" className="text-2xl font-semibold">
          {site.name}
        </h1>
        <p className="text-[var(--color-muted-foreground)]">{site.url}</p>
        <div className="flex flex-wrap items-center gap-2 pt-1">
          <EnrollmentBadge site={site} />
          <HealthBadge status={site.health_status} />
        </div>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Metadata</CardTitle>
          <CardDescription>
            Environment details reported by the enrolled agent.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <dl className="grid grid-cols-1 gap-x-6 gap-y-3 text-sm sm:grid-cols-2">
            <Detail label="WordPress" value={site.wp_version || "—"} />
            <Detail label="PHP" value={site.php_version || "—"} />
            <Detail label="Server" value={site.server_info ?? "—"} />
            <Detail
              label="Multisite"
              value={site.multisite ? "Yes" : "No"}
            />
            <Detail label="Active theme" value={site.active_theme ?? "—"} />
            <Detail label="Status" value={site.status} />
            <Detail
              label="Enrolled"
              value={
                site.enrolled
                  ? enrolledAt
                    ? `Yes — ${enrolledAt}`
                    : "Yes"
                  : "Not yet"
              }
            />
            <Detail label="Last seen" value={lastSeen ?? "Never"} />
            <Detail label="Site ID" value={site.id} />
            <Detail label="Created" value={site.created_at} />
          </dl>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Tags</CardTitle>
          <CardDescription>
            {canEditTags
              ? "Organize sites for filtering. Changes save immediately."
              : "Tags applied to this site."}
          </CardDescription>
        </CardHeader>
        <CardContent>
          {canEditTags ? (
            <SiteTagsEditor site={site} />
          ) : site.tags.length > 0 ? (
            <div className="flex flex-wrap gap-1">
              {site.tags.map((tag) => (
                <span
                  key={tag}
                  className="rounded-full border border-[var(--color-border)] px-2 py-0.5 text-xs"
                >
                  {tag}
                </span>
              ))}
            </div>
          ) : (
            <p className="text-sm text-[var(--color-muted-foreground)]">
              No tags.
            </p>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Installed components</CardTitle>
          <CardDescription>Plugins and themes on this site.</CardDescription>
        </CardHeader>
        <CardContent>
          <SiteComponentsTable
            plugins={site.components?.plugins}
            themes={site.components?.themes}
          />
        </CardContent>
      </Card>

      <UptimeSection siteId={site.id} />

      <BackupsSection siteId={site.id} canOperate={canOperate} />
    </div>
  );
}

function Detail({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div>
      <dt className="text-[var(--color-muted-foreground)]">{label}</dt>
      <dd className="font-medium break-all">{value}</dd>
    </div>
  );
}
