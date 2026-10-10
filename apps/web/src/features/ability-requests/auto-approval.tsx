import type { AbilityRequest } from "@wpmgr/api";

import {
  RAN_AUTOMATICALLY,
  allowedByLine,
  ranAutomatically,
  waitingBecauseLine,
} from "@/features/ai-trust/ai-trust-copy";

import { clockTime } from "./ability-card-model";

// The rows a request card gains from approval tiers (design §8.4 and §8.6):
// "Ran automatically" with what allowed it, for a change a site's setting
// approved, and "Waiting because" for one that waits for a person. Every value
// comes from the request row the server returned; nothing is re-read from the
// site, so the line stays true after the setting changes.

export function RanAutomaticallyChip() {
  return (
    <span
      data-testid="ran-automatically"
      className="inline-flex items-center rounded-full border border-info/40 bg-info-subtle px-2 py-0.5 text-xs font-medium text-info-subtle-fg"
    >
      {RAN_AUTOMATICALLY}
    </span>
  );
}

/** States in which a change a setting approved has been sent to the site. */
function hasRun(r: AbilityRequest): boolean {
  return r.state === "dispatched" || r.state === "done" || r.state === "failed" || r.state === "outcome_unknown";
}

/** "Ran" and "Allowed by", as <dt>/<dd> pairs for the card's definition list. Nothing for a person's approval. */
export function AutoApprovalRows({
  request,
  currentUserId,
}: {
  request: AbilityRequest;
  currentUserId: string | null | undefined;
}) {
  if (!ranAutomatically(request.approval)) return null;
  const allowedBy = allowedByLine(request.approval, currentUserId);
  return (
    <>
      {hasRun(request) ? (
        <>
          <dt className="text-muted-foreground">Ran</dt>
          <dd className="text-foreground">{clockTime(request.decided_at)}</dd>
        </>
      ) : null}
      {allowedBy ? (
        <>
          <dt className="text-muted-foreground">Allowed by</dt>
          <dd data-testid="allowed-by" className="min-w-0 break-words text-foreground">
            {allowedBy}
          </dd>
        </>
      ) : null}
    </>
  );
}

/** "Waiting because", for a card that waits for a person. Nothing when the server recorded no reason. */
export function WaitingBecauseRow({ request }: { request: AbilityRequest }) {
  if (request.state !== "pending") return null;
  const line = waitingBecauseLine(request.ask_reason, request.change_kind_name);
  if (!line) return null;
  return (
    <>
      <dt className="text-muted-foreground">Waiting because</dt>
      <dd data-testid="waiting-because" className="min-w-0 break-words text-foreground">
        {line}
      </dd>
    </>
  );
}
