import { createFileRoute, Outlet } from "@tanstack/react-router";

// `/sites/$siteId/backups` — LAYOUT for the site-scoped backups surface.
//
// GH #188: the snapshot detail used to live at the top-level, siteId-less
// `/backups/$snapshotId` route, so the breadcrumb (a pure pathname->label
// mapper, see components/layout/top-bar-helpers.ts) could only render
// `Backups › <snapshotId>` with "Backups" linking to the FLEET `/backups`
// list — a dead end that doesn't contain the snapshot. Nesting the snapshot
// detail under `/sites/$siteId/backups/$snapshotId` (mirroring how every
// other site tab works) makes the pathname itself carry siteId, so the
// breadcrumb mapper produces `Sites › <siteId> › Backups › <snapshotId>`
// with "Backups" correctly linking back to this site's Backups tab, with NO
// change to the breadcrumb mechanism itself.
//
// This file is now a pure layout (no content of its own): the list body
// lives in `$siteId.backups.index.tsx` and the snapshot detail lives in
// `$siteId.backups.$snapshotId.tsx` — same layout+index pattern the site
// shell already uses at `$siteId.tsx` / `$siteId.index.tsx`.

export const Route = createFileRoute("/_authed/sites/$siteId/backups")({
  component: BackupsLayout,
});

function BackupsLayout() {
  return <Outlet />;
}
