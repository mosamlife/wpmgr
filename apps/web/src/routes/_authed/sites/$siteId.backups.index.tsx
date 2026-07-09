import { createFileRoute } from "@tanstack/react-router";

import { BackupsSection } from "@/features/backups/backups-section";
import { useMe, canOperate } from "@/features/auth/use-auth";

// `/sites/$siteId/backups` (index) — recent snapshots + schedule + run-now
// (operator+). Moved here from `$siteId.backups.tsx` (GH #188), which is now
// the layout that renders this index alongside the sibling
// `$siteId.backups.$snapshotId.tsx` detail route.

export const Route = createFileRoute("/_authed/sites/$siteId/backups/")({
  component: BackupsTab,
});

function BackupsTab() {
  const { siteId } = Route.useParams();
  const { data: me } = useMe();
  const operate = canOperate(me);

  return (
    <section aria-label="Backups" className="px-4 pb-8 pt-6 sm:px-6">
      <BackupsSection siteId={siteId} canOperate={operate} />
    </section>
  );
}
