import { Fragment, useId, useState, type ReactNode } from "react";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { cn } from "@/lib/utils";
import { setUpForLine } from "@/features/ai-requests/request-card-model";
import { useDeepLinkFocus } from "@/features/ai-requests/use-deep-link";
import { ranAutomatically } from "@/features/ai-trust/ai-trust-copy";

import {
  NOT_SHOWABLE_COPY,
  clockTime,
  draftPreviewHref,
  elementorEditHref,
  isPending,
} from "./ability-card-model";
import type { AbilityRequestCardProps } from "./ability-request-card";
import { AutoApprovalRows, RanAutomaticallyChip, WaitingBecauseRow } from "./auto-approval";
import {
  CHECKS_COPY,
  EFFECT_COPY,
  KEEPS_COPY,
  UNDO_EXPIRED_COPY,
  UNDO_LATER_FIRST_COPY,
  UNDO_NOT_AVAILABLE_COPY,
  changesCount,
  checkedLine,
  editUndoView,
  pageEditCardTitle,
  pageEditStatus,
  parsePageEdit,
  undoPromise,
  type EditTone,
  type PageEditView,
} from "./page-edit-model";
import { PageEditPreview } from "./page-edit-preview";
import { useWindowOpen } from "./use-window-open";

// The approval card for "AI changes a draft in Elementor" (wpmgr/page-edit,
// BF-E), and what it says afterwards: the change done, each change's undo
// (newest first, 14 days), and every way an edit can fail. The AI's words
// (new text, inserted outlines) sit under "Chosen by the AI" and the site's
// (site address, the draft's title, the text before, the page after) under
// "From the site". All of it is a React text node: no innerHTML, no <img> of
// site content, and links are built from the site's address and an integer id.
//
// A site's setting can approve a page edit with no person deciding (approval
// tiers), so the card also says "Ran automatically" and what allowed it, says
// why it waits when it does, and takes the focus an AI's link names.

export function PageEditCard({
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
  currentUserId,
}: AbilityRequestCardProps) {
  const view = parsePageEdit(request);
  const pending = isPending(request);
  const { articleRef, declineRef } = useDeepLinkFocus(deepLinked, pending);
  const status = pageEditStatus(request);
  const auto = ranAutomatically(request.approval);
  const windowOpen = useWindowOpen(
    request.undo_available_until,
    request.state === "done" && request.undo_state === "available",
  );
  const undo = editUndoView(request, windowOpen);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const changesLabelId = useId();
  const setUpFor = setUpForLine(request);
  const busy = approvePending || declinePending;
  const title = pageEditCardTitle(request.site_label);
  const links = status.links && view ? editLinks(siteUrl, view.postId) : [];

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
        {view ? (
          <>
            <dt className="text-muted-foreground">Page</dt>
            <dd
              data-testid="page-line"
              className="flex min-w-0 flex-wrap items-baseline justify-between gap-x-3 text-foreground"
            >
              <span className="break-words">
                {`#${view.postId} "`}
                <bdi>{view.title}</bdi>
                {`" · Draft · Elementor ${view.builderVersion}`}
              </span>
              <span className="text-xs font-medium text-muted-foreground">From the site</span>
            </dd>
            <dt className="text-muted-foreground">Changes</dt>
            <dd className="flex min-w-0 flex-wrap items-baseline justify-between gap-x-3 text-foreground">
              <span id={changesLabelId}>{changesCount(view.lines.length)}</span>
              <span className="text-xs font-medium text-muted-foreground">Chosen by the AI</span>
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

      {view ? (
        <>
          <PageEditPreview view={view} siteHost={request.site_host} siteUrl={siteUrl} labelledBy={changesLabelId} />
          <dl data-testid="edit-rows" className="grid grid-cols-[minmax(7rem,auto)_1fr] gap-x-4 gap-y-1 text-sm">
            {editRows(view, request.created_at, pending).map((row) => (
              <Fragment key={row.label}>
                <dt className="text-muted-foreground">{row.label}</dt>
                <dd className="min-w-0 break-words text-foreground">{row.text}</dd>
              </Fragment>
            ))}
          </dl>
        </>
      ) : (
        <p className="text-sm text-muted-foreground">{NOT_SHOWABLE_COPY}</p>
      )}

      {pending ? null : (
        <div className="space-y-1">
          <StatusLine tone={status.tone} text={status.text} />
          {links.length > 0 ? (
            <div className="flex flex-wrap gap-x-4 gap-y-1">
              {links.map((link) => (
                <a
                  key={link.label}
                  href={link.href}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="text-sm font-medium text-primary underline underline-offset-2 hover:opacity-80"
                >
                  {link.label}
                </a>
              ))}
            </div>
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
            disabled={busy || view === null || request.presented_digest === undefined}
            onClick={() => onApprove(request)}
          >
            {approvePending ? "Approving…" : "Approve"}
          </Button>
        </div>
      ) : (
        <UndoArea
          undo={undo}
          undoPending={undoPending}
          onAsk={() => setConfirmOpen(true)}
        />
      )}

      {/* The offer to undo is the row's word and does not depend on the card, so
          the confirmation opens on a card that cannot be shown too. */}
      <UndoConfirm
        open={confirmOpen}
        page={view === null ? null : view.title === "" ? `#${view.postId}` : view.title}
        onCancel={() => setConfirmOpen(false)}
        onConfirm={() => {
          setConfirmOpen(false);
          onUndo(request);
        }}
      />
    </article>
  );
}

function editLinks(siteUrl: string | null | undefined, postId: number): Array<{ label: string; href: string }> {
  const out: Array<{ label: string; href: string }> = [];
  const open = elementorEditHref(siteUrl, postId);
  if (open !== null) out.push({ label: "Open in Elementor", href: open });
  const preview = draftPreviewHref(siteUrl, postId, false);
  if (preview !== null) out.push({ label: "Preview", href: preview });
  return out;
}

interface EditRow {
  readonly label: string;
  readonly text: string;
}

/** The rows under the changes: what is kept, what WPMgr checks, the effect and the undo. */
function editRows(view: PageEditView, createdAt: string, pending: boolean): EditRow[] {
  const rows: EditRow[] = [];
  if (view.hasTextChange) rows.push({ label: "Keeps", text: KEEPS_COPY });
  rows.push({ label: "Checks", text: CHECKS_COPY });
  rows.push({ label: "Effect", text: EFFECT_COPY });
  if (pending) rows.push({ label: "Undo", text: undoPromise(createdAt) });
  rows.push({ label: "Checked", text: checkedLine(view.checkedAt) });
  return rows;
}

const TONE_CLASS: Record<EditTone, string> = {
  neutral: "text-sm text-foreground",
  amber: "rounded-md bg-warning-subtle px-3 py-2 text-sm font-medium text-warning-subtle-fg",
  red: "rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm font-semibold text-destructive",
};

/** The card's status line. A state that needs a person's attention is an alert. */
function StatusLine({ tone, text }: { tone: EditTone; text: string }): ReactNode {
  return (
    <p data-testid="status-line" data-tone={tone} role={tone === "red" ? "alert" : undefined} className={TONE_CLASS[tone]}>
      {text}
    </p>
  );
}

function UndoArea({
  undo,
  undoPending,
  onAsk,
}: {
  undo: ReturnType<typeof editUndoView>;
  undoPending: boolean;
  onAsk: () => void;
}) {
  switch (undo) {
    case "offer":
      return (
        <div className="flex flex-wrap items-center justify-end gap-2">
          <Button type="button" variant="outline" disabled={undoPending} onClick={onAsk}>
            {undoPending ? "Undoing…" : "Undo this change"}
          </Button>
        </div>
      );
    case "later_first":
      return (
        <p data-testid="undo-later-first" className="text-sm text-muted-foreground">
          {UNDO_LATER_FIRST_COPY}
        </p>
      );
    case "expired":
      return (
        <p data-testid="undo-expired" className="text-sm text-muted-foreground">
          {UNDO_EXPIRED_COPY}
        </p>
      );
    case "none":
      return (
        <p data-testid="undo-none" className="text-sm text-muted-foreground">
          {UNDO_NOT_AVAILABLE_COPY}
        </p>
      );
    case "silent":
      return null;
  }
}

/**
 * Cancel is first in the order and focused: the safe choice is the default.
 * page is the draft's title (else its number) from a card the screen could
 * show in full; null for a card it could not, which names no page.
 */
function UndoConfirm({
  open,
  page,
  onCancel,
  onConfirm,
}: {
  open: boolean;
  page: string | null;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const titleId = useId();
  const bodyId = useId();
  return (
    <Dialog open={open} onClose={onCancel}>
      <DialogContent ariaLabelledBy={titleId} ariaDescribedBy={bodyId}>
        <DialogHeader>
          <DialogTitle id={titleId}>Undo this change?</DialogTitle>
        </DialogHeader>
        <DialogBody>
          <p id={bodyId} className="text-sm text-foreground">
            {page === null ? (
              "WPMgr puts back what this change wrote on the page."
            ) : (
              <>
                {'WPMgr puts back what this change wrote on "'}
                <bdi>{page}</bdi>
                {'".'}
              </>
            )}
            {" Changes approved after it that touched other parts of the page, like a featured image, stay."}
          </p>
        </DialogBody>
        <DialogFooter className="pt-2">
          <Button type="button" variant="outline" autoFocus onClick={onCancel}>
            Cancel
          </Button>
          <Button type="button" onClick={onConfirm}>
            Undo
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
