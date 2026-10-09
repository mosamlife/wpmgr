import { createFileRoute } from "@tanstack/react-router";

import { ContentFleetReport } from "@/features/content/ContentFleetReport";

// Superadmin gate is enforced by the parent /admin layout route.
export const Route = createFileRoute("/_authed/admin/content-report")({
  component: ContentReportPage,
});

function ContentReportPage() {
  return (
    <section aria-label="Page editors" className="space-y-6">
      <ContentFleetReport />
    </section>
  );
}
