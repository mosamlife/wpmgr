import { createFileRoute } from "@tanstack/react-router";

import { VulnFeedPanel } from "@/features/admin/vuln-feed-panel";

// Auth gate is enforced by the parent /admin layout route (route.tsx); no
// additional beforeLoad guard is needed here. The page itself lives in
// features/admin/vuln-feed-panel.tsx, shared with /settings/vuln-feed.
export const Route = createFileRoute("/_authed/admin/vuln-feed")({
  component: VulnFeedPanel,
});
