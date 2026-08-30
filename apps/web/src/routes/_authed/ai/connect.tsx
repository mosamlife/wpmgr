import { createFileRoute } from "@tanstack/react-router";

import { PageHeader } from "@/components/shared/page-header";
import { ConnectWizard } from "@/features/ai-connections/connect-wizard";
import { mcpEndpointUrl } from "@/features/ai-connections/endpoint";

// /ai/connect — the connection wizard (design §18).
//
// NO SIDEBAR ENTRY, DELIBERATELY. It is reached from the primary action on
// /ai, which is the house rule for an authenticated route that hangs off
// another page rather than being a destination of its own.
//
// NOT TO BE CONFUSED WITH /connect/ai, which is the OAuth consent screen an
// external client redirects into. This route is the operator-facing setup
// guide; that one is the approval. They are different halves of step 6 and
// step 7 and neither links into the middle of the other.

export const Route = createFileRoute("/_authed/ai/connect")({
  component: ConnectAiClientPage,
});

function ConnectAiClientPage() {
  return (
    <div className="space-y-6">
      <PageHeader
        title="Connect an AI client"
        subline="Pick your client first. Everything after that is computed from it, because the setup differs per client in ways that fail quietly."
        backTo={{ to: "/ai", label: "AI connections" }}
      />
      <ConnectWizard endpointUrl={mcpEndpointUrl()} />
    </div>
  );
}
