import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { useDeepLinkFocus } from "@/features/ai-requests/use-deep-link";

import type { AbilityRequestCardProps } from "./ability-request-card";
import { abilityStatus, draftLinks, isPostRequest } from "./ability-card-model";
import { AutoApprovalRows } from "./auto-approval";
import { isPageEdit, pageEditStatus } from "./page-edit-model";
import { isRestWrite, restWriteStatus } from "./rest-card-model";

export function ClearedRequestCard({
  request,
  siteUrl,
  onUndo,
  undoPending = false,
  deepLinked = false,
  className,
  currentUserId,
  notice,
  laterEdits = 0,
  canUndo,
}: AbilityRequestCardProps & { canUndo: boolean }) {
  const { articleRef } = useDeepLinkFocus(deepLinked, false);
  const kind = isPageEdit(request)
    ? "Edit a page"
    : isRestWrite(request)
      ? "Change the site"
      : `Create a draft ${isPostRequest(request) ? "post" : "page"}`;
  const status = isPageEdit(request)
    ? pageEditStatus(request)
    : isRestWrite(request)
      ? restWriteStatus(request)
      : abilityStatus(request, laterEdits);
  const title = `${kind} · ${request.site_label}`;
  const links =
    isPageEdit(request) || isRestWrite(request)
      ? []
      : draftLinks(request, abilityStatus(request, laterEdits), siteUrl);
  const asked = new Date(request.created_at);
  const when = Number.isNaN(asked.getTime())
    ? "Time not recorded"
    : asked.toLocaleString([], {
        year: "numeric",
        month: "short",
        day: "numeric",
        hour: "2-digit",
        minute: "2-digit",
      });
  return (
    <article
      ref={articleRef}
      aria-label={title}
      tabIndex={deepLinked ? -1 : undefined}
      className={cn(
        "space-y-3 rounded-lg border border-border bg-card p-4 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]",
        className,
      )}
    >
      <h3 className="text-sm font-semibold text-foreground">{title}</h3>
      <p className="text-sm text-muted-foreground">{request.site_host}</p>
      <dl className="grid grid-cols-[minmax(7rem,auto)_1fr] gap-x-4 gap-y-1 text-sm">
        <dt className="text-muted-foreground">Asked by</dt>
        <dd className="min-w-0 break-words text-foreground">
          {request.grant_label}
        </dd>
        <dt className="text-muted-foreground">Asked at</dt>
        <dd className="text-foreground">
          <time dateTime={request.created_at}>{when}</time>
        </dd>
        <dt className="text-muted-foreground">Kind</dt>
        <dd className="text-foreground">{kind}</dd>
        <AutoApprovalRows request={request} currentUserId={currentUserId} />
        <dt className="text-muted-foreground">Result</dt>
        <dd data-testid="cleared-result" className="text-foreground">
          {status.text}
        </dd>
      </dl>
      <p className="text-sm text-muted-foreground">
        Details cleared 15 days after this request finished.
      </p>
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
      {notice ? (
        <p role="alert" className="text-sm text-destructive">
          {notice}
        </p>
      ) : null}
      {canUndo ? (
        <div className="flex justify-end">
          <Button
            type="button"
            variant="outline"
            disabled={undoPending}
            onClick={() => onUndo(request)}
          >
            {undoPending ? "Undoing…" : "Undo"}
          </Button>
        </div>
      ) : null}
    </article>
  );
}
