import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { setUpForLine } from "@/features/ai-requests/request-card-model";

import { clockTime, isPending } from "./ability-card-model";
import type { AbilityRequestCardProps } from "./ability-request-card";
import {
  REST_NOT_SHOWABLE_COPY,
  isPublishedImmediately,
  parseRestCardFacts,
  postStatusWord,
  restCardTitle,
  restWriteStatus,
} from "./rest-card-model";

// The generic structured approval card for a reviewed site change (engine
// slice E3), driven by the row's card_facts. Everything is a React text node.

export function StructuredAbilityCard(props: AbilityRequestCardProps & { canUndo: boolean }) {
  const {
    request,
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
  } = props;
  const facts = parseRestCardFacts(request);
  const pending = isPending(request);
  const status = restWriteStatus(request);
  const setUpFor = setUpForLine(request);
  const busy = approvePending || declinePending;
  const title = restCardTitle(facts, request.site_label);
  const live = facts !== null && isPublishedImmediately(facts);
  return (
    <article
      aria-label={title}
      className={cn("space-y-3 rounded-lg border border-border bg-card p-4", className)}
    >
      <div className="space-y-1">
        <h3 className="text-sm font-semibold text-foreground">
          {facts ? facts.route_title : "Change a site field"}
          {facts ? (
            <span className="ml-2 font-mono text-xs font-normal text-muted-foreground">{facts.method}</span>
          ) : null}
        </h3>
        <div>
          <p className="text-xs font-medium text-muted-foreground">From the site</p>
          <p className="text-sm text-muted-foreground">{request.site_host}</p>
        </div>
      </div>

      <dl className="grid grid-cols-[minmax(7rem,auto)_1fr] gap-x-4 gap-y-1 text-sm">
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

      {facts ? (
        <>
          <p
            data-testid="effect-line"
            className={cn(
              "text-sm",
              live
                ? "rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 font-semibold text-destructive"
                : "text-muted-foreground",
            )}
          >
            {facts.effect_label}
          </p>

          <div className="space-y-1" data-testid="rest-target">
            <p className="text-xs font-medium text-muted-foreground">Target</p>
            <p className="text-sm text-foreground">
              {facts.target.post_type} #{facts.target.id}
            </p>
            <p className="text-xs font-medium text-muted-foreground">From the site</p>
            <p className="break-words text-sm text-muted-foreground">Status: {postStatusWord(facts.target.from_the_site.status)}</p>
            <p className="break-words text-sm text-muted-foreground">
              Current title: {facts.target.from_the_site.title_before}
            </p>
          </div>

          <div className="space-y-2" data-testid="rest-changes">
            <p className="text-xs font-medium text-muted-foreground">Changes</p>
            {facts.changes.map((c) => (
              <div key={c.key} className="space-y-1 rounded-md bg-muted/30 p-3 text-sm">
                <p className="font-medium text-foreground">{c.label}</p>
                <p className="text-xs font-medium text-muted-foreground">From the site (before)</p>
                <p className="whitespace-pre-wrap break-words text-muted-foreground">{c.from_the_site.before}</p>
                <p className="text-xs font-medium text-muted-foreground">Chosen by the AI (after)</p>
                <p className="whitespace-pre-wrap break-words text-foreground">{c.after}</p>
              </div>
            ))}
          </div>

          {facts.undo === "post_fields" ? (
            <p className="text-sm text-muted-foreground">
              WPMgr keeps the old text and can put it back for a limited time.
              {facts.undo_note ? ` ${facts.undo_note}` : ""}
            </p>
          ) : null}
        </>
      ) : (
        <p className="text-sm text-muted-foreground">{REST_NOT_SHOWABLE_COPY}</p>
      )}

      {pending ? null : <p className="text-sm text-foreground">{status.text}</p>}

      {notice ? (
        <p role="alert" className="text-sm text-destructive">
          {notice}
        </p>
      ) : null}

      {pending ? (
        <div className="flex flex-wrap justify-end gap-2">
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
            disabled={busy || facts === null || request.presented_digest === undefined}
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
