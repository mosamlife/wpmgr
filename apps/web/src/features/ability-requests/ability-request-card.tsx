import type { AbilityRequest } from "@wpmgr/api";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { setUpForLine } from "@/features/ai-requests/request-card-model";

import {
  NOT_SHOWABLE_COPY,
  NOTHING_PUBLISHED,
  abilityCardTitle,
  abilityStatus,
  clockTime,
  editDraftHref,
  editorName,
  isPending,
  parsePagePreview,
  undoOpen,
  type PagePreview,
} from "./ability-card-model";

// The approval card for "AI creates a draft page" (engine slice E2). The AI's
// words (title, outline) sit in a "Chosen by the AI" slot and the site's words
// (site name, address) in "From the site". All of it is rendered as React text
// nodes: no innerHTML, no markup from a model string, no href built from one.

export interface AbilityRequestCardProps {
  request: AbilityRequest;
  /** The site's own address, for the "edit the draft" link. Unknown is fine. */
  siteUrl?: string | null;
  now: Date;
  onApprove: (request: AbilityRequest) => void;
  onDecline: (request: AbilityRequest) => void;
  onUndo: (request: AbilityRequest) => void;
  approvePending?: boolean;
  declinePending?: boolean;
  undoPending?: boolean;
  /** A refusal from the last action on this card, shown as an alert. */
  notice?: string | null;
  autoFocusDecline?: boolean;
  className?: string;
}

export function AbilityRequestCard({
  request,
  siteUrl,
  now,
  onApprove,
  onDecline,
  onUndo,
  approvePending = false,
  declinePending = false,
  undoPending = false,
  notice,
  autoFocusDecline = false,
  className,
}: AbilityRequestCardProps) {
  const pending = isPending(request);
  const status = abilityStatus(request);
  const preview = parsePagePreview(request.input_json);
  const setUpFor = setUpForLine(request);
  const busy = approvePending || declinePending;
  const canUndo = undoOpen(request, now);
  const editHref = status.kind === "done" ? editDraftHref(siteUrl, request.created_post_id) : null;
  const title = abilityCardTitle(request);

  return (
    <article
      aria-label={title}
      className={cn("space-y-3 rounded-lg border border-border bg-card p-4", className)}
    >
      <div className="space-y-1">
        <h3 className="text-sm font-semibold text-foreground">{title}</h3>
        <div>
          <p className="text-xs font-medium text-muted-foreground">From the site</p>
          <p className="text-sm text-muted-foreground">{request.site_host}</p>
        </div>
      </div>

      <dl className="grid grid-cols-[minmax(7rem,auto)_1fr] gap-x-4 gap-y-1 text-sm">
        <dt className="text-muted-foreground">Editor</dt>
        <dd className="min-w-0 break-words text-foreground">{editorName(request.editor)}</dd>
        <dt className="text-muted-foreground">Asked by</dt>
        <dd className="min-w-0 break-words text-foreground">{request.grant_label}</dd>
        <dt className="text-muted-foreground">Set up for</dt>
        <dd className="min-w-0 break-words text-foreground">{setUpFor.primary}</dd>
        {pending ? (
          <>
            <dt className="text-muted-foreground">Timing</dt>
            <dd className="text-foreground">
              Asked {clockTime(request.created_at)} · closes {clockTime(request.expires_at)}
            </dd>
          </>
        ) : null}
      </dl>
      {setUpFor.caption ? <p className="text-xs text-muted-foreground">{setUpFor.caption}</p> : null}

      <div className="space-y-1">
        <p className="text-xs font-medium text-muted-foreground">Chosen by the AI</p>
        {preview ? (
          <OutlinePreview preview={preview} />
        ) : (
          <p className="text-sm text-muted-foreground">{NOT_SHOWABLE_COPY}</p>
        )}
      </div>

      <p className="text-sm text-muted-foreground">{NOTHING_PUBLISHED}</p>

      {pending ? null : (
        <div className="space-y-1">
          <p className="text-sm text-foreground">{status.text}</p>
          {status.siteSaid ? (
            <p className="text-sm text-muted-foreground">The site said: {status.siteSaid}</p>
          ) : null}
          {editHref ? (
            <a
              href={editHref}
              target="_blank"
              rel="noopener noreferrer"
              className="text-sm font-medium text-primary underline underline-offset-2 hover:opacity-80"
            >
              Edit the draft in WordPress
            </a>
          ) : null}
        </div>
      )}

      {notice ? (
        <p role="alert" className="text-sm text-destructive">
          {notice}
        </p>
      ) : null}

      {pending ? (
        <div className="flex flex-wrap justify-end gap-2">
          {/* Decline first in the DOM and focused on the first card: the safe
              choice is the default. Each request is decided on its own. */}
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
            disabled={busy || preview === null || request.presented_digest === undefined}
            onClick={() => onApprove(request)}
          >
            {approvePending ? "Approving…" : "Approve"}
          </Button>
        </div>
      ) : canUndo ? (
        <div className="flex flex-wrap items-center justify-end gap-2">
          <Button type="button" variant="outline" disabled={undoPending} onClick={() => onUndo(request)}>
            {undoPending ? "Undoing…" : "Undo"}
          </Button>
        </div>
      ) : null}
    </article>
  );
}

function OutlinePreview({ preview }: { preview: PagePreview }) {
  return (
    <div
      data-testid="ability-outline"
      className="max-h-72 space-y-2 overflow-y-auto rounded-md bg-muted/30 p-3 text-sm text-foreground"
    >
      <p className="break-words font-semibold">{preview.title}</p>
      {preview.outline.map((node, i) => {
        if (node.type === "heading") {
          return (
            <p key={i} className="break-words font-medium">
              {node.text}
            </p>
          );
        }
        if (node.type === "paragraph") {
          return (
            <p key={i} className="break-words text-muted-foreground">
              {node.text}
            </p>
          );
        }
        const Tag = node.ordered ? "ol" : "ul";
        return (
          <Tag
            key={i}
            className={cn("space-y-0.5 pl-5 text-muted-foreground", node.ordered ? "list-decimal" : "list-disc")}
          >
            {node.items.map((item, j) => (
              <li key={j} className="break-words">
                {item}
              </li>
            ))}
          </Tag>
        );
      })}
    </div>
  );
}
