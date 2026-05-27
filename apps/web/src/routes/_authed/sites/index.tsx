import { createFileRoute } from "@tanstack/react-router";

import { Button } from "@/components/ui/button";
import { useSites } from "@/features/sites/use-sites";
import { SitesTable } from "@/features/sites/sites-table";

export const Route = createFileRoute("/_authed/sites/")({
  component: SitesPage,
});

function SitesPage() {
  const { data: sites, isPending, isError, error, refetch } = useSites();

  return (
    <section aria-labelledby="sites-heading" className="space-y-4">
      <h1 id="sites-heading" className="text-2xl font-semibold">
        Sites
      </h1>

      {isPending ? (
        <p role="status" className="text-[var(--color-muted-foreground)]">
          Loading sites…
        </p>
      ) : isError ? (
        <div role="alert" className="space-y-3">
          <p className="text-[var(--color-destructive)]">
            Failed to load sites: {error.message}
          </p>
          <Button variant="outline" size="sm" onClick={() => void refetch()}>
            Retry
          </Button>
        </div>
      ) : sites.length === 0 ? (
        <p className="text-[var(--color-muted-foreground)]">
          No sites yet. Sites added in Phase 5 will appear here.
        </p>
      ) : (
        <SitesTable sites={sites} />
      )}
    </section>
  );
}
