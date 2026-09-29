import type { AssistantRequest } from "@wpmgr/api";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

import { PageAddress } from "./page-address";
import {
  cardTitle,
  formatTime,
  ifApproveCopy,
  isActionable,
  requestStatusLine,
  setUpForLine,
} from "./request-card-model";

// The AI request card (tracka-cache-purge-design-v7 §2.6). Every field this
// component renders is named in "Where each fact comes from" and is shown
// as plain text: `site_label`, `site_host`, `grant_label`, `url` and
// `site_reported_text` (folded into requestStatusLine's failure lines) are
// all site- or connection-supplied and none of them is ever markup, a link,
// or interpolated into a sentence this component wrote.

export interface RequestCardProps {
  request: AssistantRequest;
  currentUserId: string | null | undefined;
  onApprove: (request: AssistantRequest) => void;
  onDecline: (request: AssistantRequest) => void;
  approvePending?: boolean;
  declinePending?: boolean;
  /** True for exactly one card in a list, so Decline is the default focus
   *  target without every card in a page stealing it from the last. */
  autoFocusDecline?: boolean;
  className?: string;
}

export function RequestCard({
  request,
  currentUserId,
  onApprove,
  onDecline,
  approvePending = false,
  declinePending = false,
  autoFocusDecline = false,
  className,
}: RequestCardProps) {
  const status = requestStatusLine(request, currentUserId);
  const actionable = isActionable(request);
  const setUpFor = setUpForLine(request);
  const busy = approvePending || declinePending;

  return (
    <article
      aria-label={cardTitle(request)}
      className={cn("space-y-3 rounded-lg border border-border bg-card p-4", className)}
    >
      <div className="space-y-1">
        <h3 className="text-sm font-semibold text-foreground">{cardTitle(request)}</h3>
        {/* site_host — siteaddr.HostKey output, ASCII/Punycode already, for
            BOTH scopes (F1, G2). A text node, same as everything else here. */}
        <p className="text-sm text-muted-foreground">{request.site_host}</p>
      </div>

      {request.scope === "url" && request.url !== null ? (
        <div className="space-y-1">
          <p className="text-xs font-medium text-muted-foreground">Page address</p>
          <PageAddress url={request.url} />
          <p className="text-xs text-muted-foreground">
            Chosen by the AI. WPMgr checked only that it is on this site.
          </p>
        </div>
      ) : null}

      <dl className="grid grid-cols-[minmax(7rem,auto)_1fr] gap-x-4 gap-y-1 text-sm">
        <dt className="text-muted-foreground">Connection name</dt>
        <dd className="min-w-0 break-words text-foreground">{request.grant_label}</dd>
        <dt className="text-muted-foreground">Set up for</dt>
        <dd className="min-w-0 break-words text-foreground">
          {setUpFor.primary}
          {actionable ? (
            <span className="text-muted-foreground">
              {" "}
              · asked {formatTime(request.created_at)} · closes {formatTime(request.expires_at)}
            </span>
          ) : null}
        </dd>
      </dl>
      {setUpFor.caption ? (
        <p className="text-xs text-muted-foreground">{setUpFor.caption}</p>
      ) : null}

      {actionable ? (
        <div className="space-y-1 rounded-md bg-muted/30 p-3">
          <p className="text-xs font-medium text-foreground">If you approve</p>
          <p className="text-sm text-muted-foreground">{ifApproveCopy(request.scope)}</p>
        </div>
      ) : (
        <div className="space-y-1">
          <p className="text-sm text-foreground">{status.text}</p>
          {status.detail ? (
            <p className="text-sm text-muted-foreground">{status.detail}</p>
          ) : null}
        </div>
      )}

      {actionable ? (
        <div className="flex flex-wrap justify-end gap-2">
          {/* Decline comes first in the DOM (and gets the default focus on
              the first card), never Approve -- ADR-061 :554-557. There is no
              select-all or bulk approve anywhere on this page: each request
              is decided on its own. */}
          <Button
            type="button"
            variant="outline"
            autoFocus={autoFocusDecline}
            disabled={busy}
            onClick={() => onDecline(request)}
          >
            {declinePending ? "Declining…" : "Decline"}
          </Button>
          <Button
            type="button"
            disabled={busy || request.presented_digest === undefined}
            onClick={() => onApprove(request)}
          >
            {approvePending ? "Approving…" : "Approve"}
          </Button>
        </div>
      ) : null}
    </article>
  );
}
