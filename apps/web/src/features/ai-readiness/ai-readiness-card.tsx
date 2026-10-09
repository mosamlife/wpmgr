import { useId } from "react";
import {
  AlertTriangle,
  CheckCircle2,
  MinusCircle,
  RefreshCw,
  XCircle,
} from "lucide-react";
import type { AiReadinessGroup, SiteAiReadiness } from "@wpmgr/api";

import { PageError } from "@/components/feedback/page-error";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { useNow } from "@/lib/use-now";
import { cn } from "@/lib/utils";

import {
  comingNote,
  describeCheck,
  freshnessLine,
  groupHeading,
  builderHeaderRight,
  isInactiveRow,
  LOAD_ERROR_NOT_FOUND,
  LOAD_ERROR_WHAT,
  notInstalledLine,
  refreshOtherCopy,
  REFRESH_ASKED,
  statusLine,
  warningCopy,
  type CopyContext,
  type RowTone,
  type RowView,
  type StatusTone,
} from "./readiness-copy";
import {
  AiReadinessLoadError,
  useBoundedPolling,
  useRefreshAiReadiness,
  useSiteAiReadiness,
} from "./use-ai-readiness";

// The "AI readiness" card on a site's Content tab, above "AI editing". It lists
// what an AI assistant connected to WPMgr needs on this site, each row green,
// red or grey, and lets anyone who can see the site ask it to report again.
//
// The card is advice to a person. Nothing it shows is read by the code that
// decides what the AI may do.

const TONE_ICON_CLASS: Record<RowTone, string> = {
  pass: "text-[var(--color-success)]",
  fail: "text-[var(--color-destructive)]",
  neutral: "text-[var(--color-muted-foreground)]",
};

function RowIcon({ tone, label }: { tone: RowTone; label: string }) {
  const Icon = tone === "pass" ? CheckCircle2 : tone === "fail" ? XCircle : MinusCircle;
  return (
    <span role="img" aria-label={label} className={cn("mt-0.5 inline-flex shrink-0", TONE_ICON_CLASS[tone])}>
      <Icon aria-hidden="true" className="size-4" />
    </span>
  );
}

function CheckRow({ row }: { row: RowView }) {
  return (
    <li data-check={row.id} className="flex items-start gap-2.5">
      <RowIcon tone={row.tone} label={row.iconLabel} />
      <div className="min-w-0 space-y-0.5">
        <p className="text-sm font-medium text-foreground">{row.label}</p>
        <p className="text-sm text-muted-foreground">{row.detail}</p>
      </div>
    </li>
  );
}

function GroupSection({ group, ctx }: { group: AiReadinessGroup; ctx: CopyContext }) {
  const titleId = useId();
  const { title, builder } = groupHeading(group.id);
  const isBuilder = builder !== null;
  const installed = group.installed === true;
  // The rows of an installed builder that is not switched on wait on it, and
  // say so, rather than asking for a newer version.
  const rowCtx: CopyContext = { ...ctx, builderInactive: group.checks.some(isInactiveRow) };
  const rows = group.checks.map((c) => describeCheck(c, rowCtx));

  return (
    <section aria-labelledby={titleId} className="space-y-3 border-t border-border pt-4">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h3 id={titleId} className="text-sm font-semibold text-foreground">
          {title}
        </h3>
        {isBuilder ? (
          <span className="text-xs text-muted-foreground">{builderHeaderRight(group)}</span>
        ) : null}
      </div>
      {isBuilder && !installed ? (
        <p className="text-sm text-muted-foreground">{notInstalledLine(builder)}</p>
      ) : (
        <>
          <ul className="space-y-3">
            {rows.map((row) => (
              <CheckRow key={row.id} row={row} />
            ))}
          </ul>
          {isBuilder && group.wpmgr_support === "coming" ? (
            <p className="text-xs text-muted-foreground">{comingNote(builder)}</p>
          ) : null}
        </>
      )}
    </section>
  );
}

const STATUS_ICON: Record<StatusTone, typeof CheckCircle2> = {
  ready: CheckCircle2,
  attention: XCircle,
  neutral: MinusCircle,
};

const STATUS_ICON_CLASS: Record<StatusTone, string> = {
  ready: "text-[var(--color-success)]",
  attention: "text-[var(--color-destructive)]",
  neutral: "text-[var(--color-muted-foreground)]",
};

function Warnings({ warnings }: { warnings: SiteAiReadiness["warnings"] }) {
  if (warnings.length === 0) return null;
  return (
    <div className="space-y-2">
      {warnings.map((w) => (
        <div
          key={w.code}
          role="note"
          data-warning={w.code}
          className="flex items-start gap-2.5 rounded-md border border-[var(--color-warning)]/40 bg-warning-subtle p-3 text-sm text-warning-subtle-fg"
        >
          <AlertTriangle aria-hidden="true" className="mt-0.5 size-4 shrink-0" />
          <p>{warningCopy(w.code)}</p>
        </div>
      ))}
    </div>
  );
}

export interface AiReadinessCardProps {
  siteId: string;
  /** operator+ on this site: the "Turn on" button in AI editing is present. */
  canOperate: boolean;
}

export function AiReadinessCard({ siteId, canOperate }: AiReadinessCardProps) {
  const query = useSiteAiReadiness(siteId);
  const refresh = useRefreshAiReadiness(siteId);
  const polling = useBoundedPolling(query.refetch);
  const headingId = useId();
  const now = useNow(60_000);

  if (query.isPending) {
    return (
      <div role="status" aria-label="Loading AI readiness" className="space-y-2">
        <Skeleton className="h-14 w-full" />
        <Skeleton className="h-24 w-full" />
      </div>
    );
  }

  if (query.isError) {
    const notFound = query.error instanceof AiReadinessLoadError && query.error.status === 404;
    return (
      <PageError
        what={LOAD_ERROR_WHAT}
        why={notFound ? LOAD_ERROR_NOT_FOUND : query.error.message}
        onRetry={() => void query.refetch()}
        retryLabel="Retry"
        isRetrying={query.isFetching}
      />
    );
  }

  const data = query.data;
  const ctx: CopyContext = { floors: data.floors, canOperate };
  const status = statusLine(data.status, data.fix_count);
  const StatusIcon = STATUS_ICON[status.tone];

  const onCheckAgain = () =>
    refresh.mutate(undefined, { onSuccess: () => polling.start() });

  return (
    <section aria-labelledby={headingId} className="space-y-4 rounded-lg border border-border bg-card p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="space-y-0.5">
          <h2 id={headingId} className="text-sm font-semibold text-foreground">
            AI readiness
          </h2>
          <p className="text-sm text-muted-foreground">
            Whether an AI assistant connected to WPMgr can work on this site.
          </p>
          <p className="text-xs text-muted-foreground">
            {freshnessLine(data.metadata_as_of, data.abilities_as_of, now)}
          </p>
        </div>
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={refresh.isPending}
          onClick={onCheckAgain}
        >
          <RefreshCw aria-hidden="true" className="size-4" />
          {refresh.isPending ? "Checking…" : "Check again"}
        </Button>
      </div>

      {refresh.isSuccess ? (
        <p role="status" className="text-sm text-muted-foreground">
          {REFRESH_ASKED}
        </p>
      ) : null}
      {refresh.isError ? (
        <p role="alert" className="text-sm text-destructive">
          {refresh.error.kind === "other"
            ? refreshOtherCopy(refresh.error.message)
            : refresh.error.message}
        </p>
      ) : null}

      <p className="flex items-center gap-2 text-sm font-semibold text-foreground">
        <StatusIcon
          aria-hidden="true"
          className={cn("size-4 shrink-0", STATUS_ICON_CLASS[status.tone])}
        />
        <span data-testid="ai-readiness-status">{status.text}</span>
      </p>

      <Warnings warnings={data.warnings} />

      {data.groups.map((group) => (
        <GroupSection key={group.id} group={group} ctx={ctx} />
      ))}
    </section>
  );
}
