import { useEffect, useState } from "react";
import type { AbilityRequest } from "@wpmgr/api";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { setUpForLine } from "@/features/ai-requests/request-card-model";

import { isRestWrite } from "./rest-card-model";
import { StructuredAbilityCard } from "./structured-card";
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

export function AbilityRequestCard(props: AbilityRequestCardProps) {
  const canUndo = useUndoWindowOpen(props.request.undo_offered, props.request.undo_available_until);
  if (isRestWrite(props.request)) return <StructuredAbilityCard {...props} canUndo={canUndo} />;
  return <PageCreateCard {...props} canUndo={canUndo} />;
}

function PageCreateCard({
  request,
  siteUrl,
  onApprove,
  onDecline,
  onUndo,
  approvePending = false,
  declinePending = false,
  undoPending = false,
  notice,
  autoFocusDecline = false,
  className,
  canUndo,
}: AbilityRequestCardProps & { canUndo: boolean }) {
  const pending = isPending(request);
  const status = abilityStatus(request);
  const preview = parsePagePreview(request.input_json);
  const setUpFor = setUpForLine(request);
  const busy = approvePending || declinePending;
  const editHref = status.kind === "done" || status.draftMayExist === true ? editDraftHref(siteUrl, request.created_post_id) : null;
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

/**
 * True while the server offered Undo and the window end is still ahead of the
 * client clock. One timer to the expiry flips it off; no polling clock.
 */
function useUndoWindowOpen(offered: boolean, until: string | null | undefined): boolean {
  // No window end means a recovery undo, which has no expiry of its own.
  const end = until ? Date.parse(until) : null;
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!offered || end === null || !Number.isFinite(end) || end <= now) return;
    // One timer to the expiry. setTimeout caps at a signed 32-bit delay, so a
    // longer wait re-arms itself because `now` is a dependency.
    const t = setTimeout(() => setNow(Date.now()), Math.min(Math.max(end - Date.now(), 0), 2_147_483_647));
    return () => clearTimeout(t);
  }, [offered, end, now]);
  if (!offered) return false;
  if (end === null) return true;
  return Number.isFinite(end) && end > now;
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
