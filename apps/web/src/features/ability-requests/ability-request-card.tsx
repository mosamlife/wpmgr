import { Fragment, useEffect, useId, useState } from "react";
import type { AbilityRequest } from "@wpmgr/api";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { setUpForLine } from "@/features/ai-requests/request-card-model";

import { LayoutPreview } from "./layout-preview";
import { layoutSummary, parsePageBuilder, parsePagePreview } from "./outline-model";
import { isRestWrite } from "./rest-card-model";
import { StructuredAbilityCard } from "./structured-card";
import {
  NOT_SHOWABLE_COPY,
  NOTHING_PUBLISHED,
  abilityCardTitle,
  abilityStatus,
  clockTime,
  draftLinks,
  editorName,
  elementorCardRows,
  elementorEditorLine,
  isElementorRequest,
  isPending,
} from "./ability-card-model";

// The approval card for "AI creates a draft page" (engine slice E2, widened to
// page layouts and to pages Elementor builds). The AI's words (title, outline)
// sit in a "Chosen by the AI" slot and the site's words (site name, address,
// image file names, the Elementor version) in "From the site". All of it is
// rendered as React text nodes: no innerHTML, no markup from a model string, no
// href built from one.

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
  const elementor = isElementorRequest(request);
  // Null unless every node, every image fact and the page's builder can be
  // shown in full, and the builder is the editor the request recorded; a null
  // preview also keeps Approve off.
  const parsed = parsePagePreview(request.input_json, request.page_media, request.page_builder);
  const preview = parsed !== null && (parsed.builder !== null) === elementor ? parsed : null;
  const builder = elementor ? parsePageBuilder(request.page_builder) : null;
  const layout = preview ? layoutSummary(preview.outline) : null;
  const outlineLabelId = useId();
  const setUpFor = setUpForLine(request);
  const busy = approvePending || declinePending;
  const links = draftLinks(request, status, siteUrl);
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
        {builder ? (
          <dd
            data-testid="editor-line"
            className="flex min-w-0 flex-wrap items-baseline justify-between gap-x-3 text-foreground"
          >
            <span className="break-words">{elementorEditorLine(builder)}</span>
            <span className="text-xs font-medium text-muted-foreground">From the site</span>
          </dd>
        ) : (
          <dd data-testid="editor-line" className="min-w-0 break-words text-foreground">
            {editorName(request.editor)}
          </dd>
        )}
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

      {preview?.builder ? (
        <dl
          data-testid="builder-rows"
          className="grid grid-cols-[minmax(7rem,auto)_1fr] gap-x-4 gap-y-1 text-sm"
        >
          {elementorCardRows(preview.builder).map((row) => (
            <Fragment key={row.label}>
              <dt className="text-muted-foreground">{row.label}</dt>
              <dd className="min-w-0 space-y-0.5 break-words text-foreground">
                {row.lines.map((line) => (
                  <p key={line}>{line}</p>
                ))}
              </dd>
            </Fragment>
          ))}
        </dl>
      ) : (
        <p className="text-sm text-muted-foreground">{NOTHING_PUBLISHED}</p>
      )}

      {pending ? null : (
        <div className="space-y-1">
          <p className="text-sm text-foreground">{status.text}</p>
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
