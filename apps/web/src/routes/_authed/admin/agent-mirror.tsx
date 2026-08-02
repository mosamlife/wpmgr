import { createFileRoute } from "@tanstack/react-router";
import { RefreshCw } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { PageHeader } from "@/components/shared/page-header";
import { useCheckAgentMirrorNow } from "@/features/admin/use-admin-agent-mirror";
import { cn } from "@/lib/utils";

// ---------------------------------------------------------------------------
// Route. Auth gate is enforced by the parent /admin layout route (route.tsx);
// no additional beforeLoad guard is needed here.
// ---------------------------------------------------------------------------

export const Route = createFileRoute("/_authed/admin/agent-mirror")({
  component: AgentMirrorAdminPage,
});

// ---------------------------------------------------------------------------
// Page
// ---------------------------------------------------------------------------
//
// GH #322. Triggering an immediate upstream agent-release mirror check is a
// superadmin, install-level operation (POST /api/v1/admin/agent-mirror/
// check): the mirror is ONE PER INSTALL, so this lives here rather than on
// the tenant-scoped Sites page, which a superadmin cannot even open (see
// routes/_authed.tsx's isSuperadminAllowedPath guard). The freshness of the
// mirror itself (last confirmed, last attempted, stale/misconfigured/
// standing down) is shown to every tenant that CAN see the Sites page, in
// the Agent column header's popover (features/sites/agent-column-header.tsx).
// This page only has the action, since there is no install-level read
// endpoint for that state.

function AgentMirrorAdminPage() {
  return (
    <section aria-labelledby="agent-mirror-heading" className="space-y-6">
      <PageHeader
        title="Agent release mirror"
        subline="Manually trigger the upstream agent-release check for this install."
      />
      <CheckNowCard />
      <HelpCard />
    </section>
  );
}

// ---------------------------------------------------------------------------
// CheckNowCard
// ---------------------------------------------------------------------------

function CheckNowCard() {
  const check = useCheckAgentMirrorNow();

  return (
    <Card>
      <CardHeader className="pb-3">
        <CardTitle className="text-sm font-medium">Check now</CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        <p className="text-sm text-muted-foreground">
          Queue one immediate check of the upstream agent release instead of
          waiting for the next scheduled run, up to six hours away. This only
          does something on a self-hosted install with the upstream mirror
          enabled (WPMGR_UPDATE_AGENT_MIRROR_ENABLED); the freshness of the
          mirror itself is shown on the Sites page, in the Agent column
          header.
        </p>
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={check.isPending}
          onClick={() => check.mutate()}
          aria-label="Check upstream for a newer agent release now"
        >
          <RefreshCw
            aria-hidden="true"
            className={cn("mr-1.5 size-3.5", check.isPending && "animate-spin")}
          />
          {check.isPending ? "Checking..." : "Check now"}
        </Button>
      </CardContent>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// HelpCard
// ---------------------------------------------------------------------------

function HelpCard() {
  return (
    <Card>
      <CardHeader className="pb-3">
        <CardTitle className="text-sm font-medium">About this check</CardTitle>
      </CardHeader>
      <CardContent className="space-y-3 text-sm text-muted-foreground">
        <p>
          Queuing a check does not mean anything was confirmed yet. The run
          may still end rate limited, refused, or unable to reach upstream;
          the real outcome appears on the Sites page the next time the fleet
          agent view refreshes.
        </p>
        <p>
          A request made too soon after the last one is refused honestly with
          a wait time, never a false success, and a check already queued or
          running is reported as such rather than starting a second one.
        </p>
        <p>
          This action is install-wide, not per organization: it spends this
          install&apos;s shared, unauthenticated request budget to GitHub and
          affects the reference every tenant on this install is compared
          against.
        </p>
      </CardContent>
    </Card>
  );
}
