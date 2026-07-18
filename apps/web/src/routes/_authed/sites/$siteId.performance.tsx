import { createFileRoute, redirect } from "@tanstack/react-router";

// GH #243 — `/sites/$siteId/performance` → `/sites/$siteId/cache`.
//
// "Performance" was the old name for what is now the Cache tab (Performance
// Suite / Phase 7). Sites bookmarked or linked (fleet dashboards, saved
// operator notes, external docs) still point at the old path — redirect them
// forward instead of 404ing. Mirrors the `beforeLoad` redirect idiom in
// `$siteId.index.tsx` (bare `/sites/{id}` → `/sites/{id}/health`).

export const Route = createFileRoute("/_authed/sites/$siteId/performance")({
  beforeLoad: ({ params }) => {
    throw redirect({
      to: "/sites/$siteId/cache",
      params: { siteId: params.siteId },
      replace: true,
    });
  },
});
