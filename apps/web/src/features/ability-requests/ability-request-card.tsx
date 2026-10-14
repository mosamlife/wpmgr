import { Fragment, useId } from "react";
import type { AbilityRequest } from "@wpmgr/api";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { setUpForLine } from "@/features/ai-requests/request-card-model";
import { useDeepLinkFocus } from "@/features/ai-requests/use-deep-link";
import { UNDO_WINDOW_OVER_LINE, ranAutomatically } from "@/features/ai-trust/ai-trust-copy";

import { AutoApprovalRows, RanAutomaticallyChip, WaitingBecauseRow } from "./auto-approval";
import { ClearedRequestCard } from "./cleared-request-card";
import { DesignedSections } from "./designed-sections";
import { DesignSummary } from "./design-summary";
import { LayoutPreview } from "./layout-preview";
import { layoutSummary, parseDesignedPagePreview, parsePageBuilder, parsePagePreview } from "./outline-model";
import { PageEditCard } from "./page-edit-card";
import { alsoCoversLine, isPageEdit } from "./page-edit-model";
import { isRestWrite } from "./rest-card-model";
import { StructuredAbilityCard } from "./structured-card";
import { useWindowOpen } from "./use-window-open";
import {
  TEMPLATE_CHECKS, TEMPLATE_EFFECT, liveTemplateLine, readCreateTemplate, sentence,
  showsOnWords, templatePreviewInput, templatePublished, templateStatus,
} from "./template-card-model";
import {
  NOT_SHOWABLE_COPY,
  NOTHING_PUBLISHED,
  abilityCardTitle,
  abilityStatus,
  abilityTiming,
  draftLinks,
  editorName,
  elementorCardRows,
  elementorEditorLine,
  isElementorRequest,
  isPending,
  undoWindowOver,
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
  /**
   * A page creation's applied page edits made since: how many AI changes its
   * undo also covers. Zero or absent for every other request.
   */
  laterEdits?: number;
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
  if (props.request.content_cleared_at) return <ClearedRequestCard {...props} canUndo={canUndo} />;
  if (isRestWrite(props.request)) return <StructuredAbilityCard {...props} canUndo={canUndo} />;
  if (isPageEdit(props.request)) return <PageEditCard {...props} />;
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
  laterEdits = 0,
  notice,
  autoFocusDecline = false,
  deepLinked = false,
  className,
  canUndo,
  currentUserId,
}: AbilityRequestCardProps & { canUndo: boolean }) {
  const pending = isPending(request);
  const { articleRef, declineRef } = useDeepLinkFocus(deepLinked, pending);
  const template = readCreateTemplate(request);
  const status = template ? templateStatus(request, template, abilityStatus(request, laterEdits)) : abilityStatus(request, laterEdits);
  const undoOpen = canUndo && (template === null || !templatePublished(request) && request.undo_state !== "refused_conflict");
  const auto = ranAutomatically(request.approval);
  const elementor = isElementorRequest(request);
  // Null unless every node, every image fact and the page's builder can be
  // shown in full, and the builder is the editor the request recorded.
  const input = template ? templatePreviewInput(request, template) : request.page_template != null ? null : request.input_json;
  const parsed = input === null ? null : parsePagePreview(input, request.page_media, request.page_builder);
  const preview = parsed !== null && (parsed.builder !== null) === elementor ? parsed : null;
  // A designed page (outline version 3) has its own reader, and the two refuse
  // each other's input. The card shows its design and its sections in full.
  const designed =
    preview === null && elementor && input !== null
      ? parseDesignedPagePreview(input, request.page_media, request.page_builder)
      : null;
  // What the card can show in full, and the only thing Approve keys on: a
  // request neither reader accepts shows nothing and cannot be approved.
  const shown = preview ?? designed;
  const builder = elementor ? parsePageBuilder(request.page_builder) : null;
  const layout = preview ? layoutSummary(preview.outline) : null;
  const outlineLabelId = useId();
  const setUpFor = setUpForLine(request);
  const busy = approvePending || declinePending;
  const links = template && request.created_post_id != null && status.kind !== "undone"
    ? draftLinks(request, { ...status, kind: "done" }, siteUrl).filter((l) => l.label === "Open in Elementor")
    : draftLinks(request, status, siteUrl);
  const title = template ? `New ${template.type} draft in Elementor` : abilityCardTitle(request);

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
        {template ? (
          <>
            <dt className="text-muted-foreground">{template.type === "header" ? "Header" : "Footer"}</dt>
            <dd className="flex min-w-0 flex-wrap items-baseline justify-between gap-x-3 text-foreground">
              <span className="break-words">
                {request.created_post_id != null ? `#${request.created_post_id} ` : ""}“<bdi>{shown?.title ?? request.title_excerpt}</bdi>”
                {` · ${templatePublished(request) ? "Published" : "Draft"} · Elementor ${builder?.version ?? "(version not recorded)"} with Pro`}
              </span>
              <span className="text-xs font-medium text-muted-foreground">From the site</span>
            </dd>
            <dt className="text-muted-foreground">Shows on</dt>
            <dd className="flex min-w-0 flex-wrap items-baseline justify-between gap-x-3 text-foreground">
              <span className="break-words"><bdi>{sentence(showsOnWords(template.shows_on, template.rule_titles))}</bdi> Only after a person publishes it.</span>
              <span className="text-xs font-medium text-muted-foreground">Chosen by the AI</span>
            </dd>
            {template.also_live && template.also_live.length > 0 ? (
              <>
                <dt className="text-muted-foreground">Also live</dt>
                <dd className="min-w-0 space-y-1 break-words rounded-md bg-warning-subtle px-3 py-2 text-warning-subtle-fg">
                  {template.also_live.map((live) => <p key={live.id}><bdi>{liveTemplateLine(live, template)}</bdi></p>)}
                  <p>{`When you publish this one, set ${template.also_live.length === 1 ? "that one" : "those"} to draft so the two don't compete.`}</p>
                </dd>
              </>
            ) : null}
          </>
        ) : (
          <>
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
          </>
        )}
        <dt className="text-muted-foreground">Asked by</dt>
        <dd className="min-w-0 break-words text-foreground">{request.grant_label}</dd>
        <dt className="text-muted-foreground">Set up for</dt>
        <dd className="min-w-0 break-words text-foreground">{setUpFor.primary}</dd>
        <WaitingBecauseRow request={request} />
        <AutoApprovalRows request={request} currentUserId={currentUserId} />
        {pending ? (
          <>
            <dt className="text-muted-foreground">Timing</dt>
            <dd className="text-foreground">{abilityTiming(request)}</dd>
          </>
        ) : null}
      </dl>
      {setUpFor.caption ? <p className="text-xs text-muted-foreground">{setUpFor.caption}</p> : null}

      {template && !pending ? null : (<div className="space-y-1">
        <p id={outlineLabelId} className="text-xs font-medium text-muted-foreground">
          Chosen by the AI
        </p>
        {preview ? (
          <LayoutPreview
            preview={preview}
            siteHost={request.site_host}
            siteUrl={siteUrl}
            labelledBy={outlineLabelId}
            altFromLibrary={preview.builder !== null}
          />
        ) : designed ? (
          <div className="space-y-3">
            <DesignSummary title={designed.title} design={designed.design} pageFrame={designed.pageFrame} />
            <DesignedSections
              preview={designed}
              siteHost={request.site_host}
              siteUrl={siteUrl}
              labelledBy={outlineLabelId}
            />
          </div>
        ) : (
          <p className="text-sm text-muted-foreground">{NOT_SHOWABLE_COPY}</p>
        )}
      </div>)}

      {!template && shown?.builder ? (
        <dl
          data-testid="builder-rows"
          className="grid grid-cols-[minmax(7rem,auto)_1fr] gap-x-4 gap-y-1 text-sm"
        >
          {elementorCardRows(shown.builder, { designed: designed !== null }).map((row) => (
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
      ) : template ? null : (
        <p className="text-sm text-muted-foreground">{NOTHING_PUBLISHED}</p>
      )}

      {pending ? null : (
        <div className="space-y-1">
          <p className="text-sm text-foreground">{status.text}</p>
          {undoWindowOver(request, status.kind, undoOpen) ? (
            <p className="text-sm text-muted-foreground">{UNDO_WINDOW_OVER_LINE}</p>
          ) : null}
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

      {template && !templatePublished(request) && request.undo_state !== "refused_conflict" ? (
        <dl data-testid="builder-rows" className="grid grid-cols-[minmax(7rem,auto)_1fr] gap-x-4 gap-y-1 text-sm">
          <dt className="text-muted-foreground">Checks</dt>
          <dd className="min-w-0 break-words text-foreground">{TEMPLATE_CHECKS}</dd>
          <dt className="text-muted-foreground">Effect</dt>
          <dd className="min-w-0 break-words text-foreground">{TEMPLATE_EFFECT}</dd>
          <dt className="text-muted-foreground">Undo</dt>
          <dd className="min-w-0 break-words text-foreground">Undo moves the draft to the trash.</dd>
        </dl>
      ) : null}

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
            disabled={busy || shown === null || request.presented_digest === undefined}
            onClick={() => onApprove(request)}
          >
            {approvePending ? "Approving…" : "Approve"}
          </Button>
        </div>
      ) : undoOpen ? (
        <div className="space-y-1">
          {laterEdits > 0 ? (
            <p data-testid="also-covers" className="text-right text-xs text-muted-foreground">
              {alsoCoversLine(laterEdits)}
            </p>
          ) : null}
          <div className="flex flex-wrap items-center justify-end gap-2">
            <Button type="button" variant="outline" disabled={undoPending} onClick={() => onUndo(request)}>
              {undoPending ? "Undoing…" : template ? "Undo this change" : laterEdits > 0 ? "Move draft to trash" : "Undo"}
            </Button>
          </div>
        </div>
      ) : null}
    </article>
  );
}

/**
 * True while the server offered Undo and the window end is still ahead of the
 * client clock. No window end means a recovery undo, which has no expiry of
 * its own.
 */
function useUndoWindowOpen(offered: boolean, until: string | null | undefined): boolean {
  const open = useWindowOpen(until, offered);
  return offered && open;
}
