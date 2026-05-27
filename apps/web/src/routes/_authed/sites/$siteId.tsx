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

export const Route = createFileRoute("/_authed/sites/$siteId")({
  component: SiteDetailPage,
});

function SiteDetailPage() {
  const { siteId } = Route.useParams();
  const { data: site, isPending, isError, error, refetch } = useSite(siteId);

  return (
    <section aria-labelledby="site-heading" className="space-y-4">
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
        <Card>
          <CardHeader>
            <CardTitle id="site-heading">{site.name}</CardTitle>
            <CardDescription>{site.url}</CardDescription>
          </CardHeader>
          <CardContent>
            <dl className="grid grid-cols-2 gap-x-6 gap-y-3 text-sm">
              <Detail label="Status" value={site.status} />
              <Detail label="WordPress" value={site.wp_version} />
              <Detail label="PHP" value={site.php_version} />
              <Detail label="Site ID" value={site.id} />
              <Detail label="Created" value={site.created_at} />
              <Detail label="Updated" value={site.updated_at} />
            </dl>
          </CardContent>
        </Card>
      )}
    </section>
  );
}

function Detail({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <dt className="text-[var(--color-muted-foreground)]">{label}</dt>
      <dd className="font-medium break-all">{value}</dd>
    </div>
  );
}
