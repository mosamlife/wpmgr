import { useEffect, useId, useState } from "react";
import type { AbilityRequest } from "@wpmgr/api";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { setUpForLine } from "@/features/ai-requests/request-card-model";
import { useDeepLinkFocus } from "@/features/ai-requests/use-deep-link";
import { UNDO_WINDOW_OVER_LINE, ranAutomatically } from "@/features/ai-trust/ai-trust-copy";

import { AutoApprovalRows, RanAutomaticallyChip, WaitingBecauseRow } from "./auto-approval";
import { LayoutPreview } from "./layout-preview";
import { layoutSummary, parsePagePreview } from "./outline-model";
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
  undoWindowOver,
} from "./ability-card-model";

// The approval card for "AI creates a draft page" (engine slice E2, widened to
// page layouts). The AI's words (title, outline) sit in a "Chosen by the AI"
// slot and the site's words (site name, address, image file names) in "From the
// site". All of it is rendered as React text nodes: no innerHTML, no markup
// from a model string, no href built from one.

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
  /**
   * The address named this request (`?request=<id>`): scroll to the card and
   * focus Decline on it, or the card itself when nothing is left to decide.
   */
  deepLinked?: boolean;
  className?: string;
  /** The signed-in person's id, so a setting they chose reads "set by you". */
  currentUserId?: string | null;
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
  deepLinked = false,
  className,
  canUndo,
  currentUserId,
}: AbilityRequestCardProps & { canUndo: boolean }) {
  const pending = isPending(request);
  const { articleRef, declineRef } = useDeepLinkFocus(deepLinked, pending);
  const status = abilityStatus(request);
  const auto = ranAutomatically(request.approval);
  // Null unless every node and every image fact can be shown in full; a null
  // preview also keeps Approve off.
  const preview = parsePagePreview(request.input_json, request.page_media);
  const layout = preview ? layoutSummary(preview.outline) : null;
  const outlineLabelId = useId();
  const setUpFor = setUpForLine(request);
  const busy = approvePending || declinePending;
  const editHref = status.kind === "done" || status.draftMayExist === true ? editDraftHref(siteUrl, request.created_post_id) : null;
  const title = abilityCardTitle(request);

  return (
    <article
      ref={articleRef}
      aria-label={title}
      tabIndex={deepLinked ? -1 : undefined}
      className={cn(
        "space-y-3 rounded-lg border border-border bg-card p-4 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]",
        auto && status.kind === "failed" && "border-destructive",
        className,
      )}
    >
      <div className="space-y-1">
        <div className="flex flex-wrap items-start justify-between gap-2">
          <h3 className="text-sm font-semibold text-foreground">{title}</h3>
          {auto ? <RanAutomaticallyChip /> : null}
        </div>
        <div>
          <p className="text-xs font-medium text-muted-foreground">From the site</p>
          <p className="text-sm text-muted-foreground">{request.site_host}</p>
        </div>
      </div>

      <dl className="grid grid-cols-[minmax(7rem,auto)_1fr] gap-x-4 gap-y-1 text-sm">
        <dt className="text-muted-foreground">Editor</dt>
        <dd className="min-w-0 break-words text-foreground">{editorName(request.editor)}</dd>
        {layout ? (
          <>
            <dt className="text-muted-foreground">Layout</dt>
            <dd data-testid="layout-summary" className="min-w-0 break-words text-foreground">
              {layout}
            </dd>
          </>
        ) : null}
        <dt className="text-muted-foreground">Asked by</dt>
        <dd className="min-w-0 break-words text-foreground">{request.grant_label}</dd>
        <dt className="text-muted-foreground">Set up for</dt>
        <dd className="min-w-0 break-words text-foreground">{setUpFor.primary}</dd>
        <WaitingBecauseRow request={request} />
        <AutoApprovalRows request={request} currentUserId={currentUserId} />
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
        <p id={outlineLabelId} className="text-xs font-medium text-muted-foreground">
          Chosen by the AI
        </p>
        {preview ? (
          <LayoutPreview
            preview={preview}
            siteHost={request.site_host}
            siteUrl={siteUrl}
            labelledBy={outlineLabelId}
          />
        ) : (
          <p className="text-sm text-muted-foreground">{NOT_SHOWABLE_COPY}</p>
        )}
      </div>

      <p className="text-sm text-muted-foreground">{NOTHING_PUBLISHED}</p>

      {pending ? null : (
        <div className="space-y-1">
          <p className="text-sm text-foreground">{status.text}</p>
          {undoWindowOver(request, status.kind, canUndo) ? (
            <p className="text-sm text-muted-foreground">{UNDO_WINDOW_OVER_LINE}</p>
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
            ref={declineRef}
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
