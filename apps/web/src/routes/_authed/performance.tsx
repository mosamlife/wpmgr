import { createFileRoute } from "@tanstack/react-router";

import { PlannedFeature } from "@/components/feedback/planned-feature";

export const Route = createFileRoute("/_authed/performance")({
  component: PerformancePage,
});

function PerformancePage() {
  return (
    <PlannedFeature
      title="Performance"
      summary="Fleet-wide performance monitoring is planned and not yet available. The feature will surface Core Web Vitals, time-to-first-byte, and load trends across all connected sites."
    />
  );
}
