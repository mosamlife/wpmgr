import { createFileRoute, redirect } from "@tanstack/react-router";

// `/sites/$siteId/` → `/sites/$siteId/health`.
//
// The bare site route has no content of its own; Health is the default tab.
// A `beforeLoad` redirect happens before render, so deep links to
// `/sites/{id}` land directly on the Health tab without a flicker.

export const Route = createFileRoute("/_authed/sites/$siteId/")({
  beforeLoad: ({ params }) => {
    throw redirect({
      to: "/sites/$siteId/health",
      params: { siteId: params.siteId },
      replace: true,
    });
  },
});
