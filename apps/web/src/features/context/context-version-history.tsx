import type { UseQueryResult } from "@tanstack/react-query";
import { ChevronDown, ChevronRight, History } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import { CopyableMono } from "@/components/shared/copyable-mono";
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogDescription,
} from "@/components/ui/dialog";
import { relativeTime } from "@/lib/utils";
import type {
  GovContextDiff,
  GovContextFieldDiff,
  GovContextListDiff,
  GovContextVersionSummary,
} from "@wpmgr/api";

import {
  ContextSecretDetectedError,
  ContextVersionConflictError,
  ContextWidenForbiddenError,
} from "./use-context";

// ADR-064 S5 Stage B, Screen 4 — version history, diff and restore
// (Decision 5). Shared between org and site scopes; the two
// `*-context-history-section.tsx` wrappers own the hooks and pass the
// result down here.
//
// IMPORTANT: every version here is a STORED row — what was authored at
// write time. A diff is therefore a diff of two authored snapshots, never
// of what either one enforced at the time: an organisation's own
// restrictions can move between when a site version was written and when
// it's read here, and a stored site-layer row never restates the org's
// current state (use-context.ts's own module doc says the same). The
// "as enforced right now" answer lives on Screen 1, never on this screen.

export interface ContextVersionHistoryProps {
  scopeLabel: string;
  items: GovContextVersionSummary[];
  isPending: boolean;
  isError: boolean;
  error: Error | null;
  onRetry: () => void;
  hasNextPage: boolean;
  isFetchingNextPage: boolean;
  onLoadMore: () => void;
  expandedId: string | null;
  onToggleExpand: (id: string) => void;
  /** The diff query for the currently expanded row, or undefined when none
   *  is expanded. */
  diff?: UseQueryResult<GovContextDiff, Error>;
  canWrite: boolean;
  onRequestRestore: (version: GovContextVersionSummary) => void;
  /** True while ANY restore is in flight (CodeRabbit finding on #566: "the
   *  history still exposes restore buttons, so overlapping restores can
   *  create multiple new versions"). Disables every row's restore action,
   *  not just the one being confirmed — only one restore may be proposed
   *  at a time. */
  isRestoring: boolean;
}

export function ContextVersionHistory({
  scopeLabel,
  items,
  isPending,
  isError,
  error,
  onRetry,
  hasNextPage,
  isFetchingNextPage,
  onLoadMore,
  expandedId,
  onToggleExpand,
  diff,
  canWrite,
  onRequestRestore,
  isRestoring,
}: ContextVersionHistoryProps) {
  if (isPending) {
    return <HistorySkeleton />;
  }

  if (isError) {
    return (
      <PageError
        what={`Could not load this ${scopeLabel}'s version history.`}
        why={error instanceof Error ? error.message : "Unknown error."}
        onRetry={onRetry}
        retryLabel="Reload"
      />
    );
  }

  if (items.length === 0) {
    return (
      <div className="flex flex-col items-center justify-center gap-2 rounded-lg border border-border bg-card px-6 py-10 text-center">
        <History aria-hidden="true" className="size-5 text-muted-foreground" />
        <p className="text-sm font-medium text-foreground">No edits yet</p>
        <p className="max-w-sm text-xs text-muted-foreground">
          Every accepted write to this {scopeLabel}&apos;s context becomes a
          new version here — nothing is edited in place.
        </p>
      </div>
    );
  }

  const currentId = items[0]?.id;

  return (
    <div className="space-y-2">
      <ul className="divide-y divide-border overflow-hidden rounded-lg border border-border bg-card">
        {items.map((item) => (
          <li key={item.id}>
            <VersionRow
              item={item}
              isCurrent={item.id === currentId}
              isExpanded={expandedId === item.id}
              onToggleExpand={() => onToggleExpand(item.id)}
              canWrite={canWrite}
              onRequestRestore={() => onRequestRestore(item)}
              scopeLabel={scopeLabel}
              restoreDisabled={isRestoring}
            />
            {expandedId === item.id ? (
              <div className="border-t border-border bg-muted/30 px-4 py-3">
                <VersionDiffPanel diff={diff} scopeLabel={scopeLabel} />
              </div>
            ) : null}
          </li>
        ))}
      </ul>
      {hasNextPage ? (
        <div className="flex justify-center">
          <Button
            type="button"
            variant="ghost"
            size="sm"
            disabled={isFetchingNextPage}
            onClick={onLoadMore}
          >
            {isFetchingNextPage ? "Loading…" : "Load older versions"}
          </Button>
        </div>
      ) : null}
    </div>
  );
}

function VersionRow({
  item,
  isCurrent,
  isExpanded,
  onToggleExpand,
  canWrite,
  onRequestRestore,
  scopeLabel,
  restoreDisabled,
}: {
  item: GovContextVersionSummary;
  isCurrent: boolean;
  isExpanded: boolean;
  onToggleExpand: () => void;
  canWrite: boolean;
  onRequestRestore: () => void;
  scopeLabel: string;
  restoreDisabled: boolean;
}) {
  return (
    <div className="flex flex-wrap items-center gap-3 px-4 py-3">
      <button
        type="button"
        onClick={onToggleExpand}
        aria-expanded={isExpanded}
        aria-label={`${isExpanded ? "Collapse" : "Expand"} version ${item.version}`}
        className="flex shrink-0 items-center justify-center rounded text-muted-foreground hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
      >
        {isExpanded ? (
          <ChevronDown aria-hidden="true" className="size-4" />
        ) : (
          <ChevronRight aria-hidden="true" className="size-4" />
        )}
      </button>

      <span className="w-12 shrink-0 text-sm font-medium text-foreground tabular-nums">
        v{item.version}
      </span>

      <span className="inline-flex items-center gap-1.5 text-xs text-muted-foreground">
        {authorLabel(item.author_type)}
        {item.author_id ? (
          <CopyableMono value={item.author_id} truncate label="Copy author id" />
        ) : null}
      </span>

      <Badge variant="outline" className="capitalize">
        {item.provenance}
      </Badge>

      <span title={item.created_at} className="text-xs text-muted-foreground">
        {relativeTime(item.created_at) ?? item.created_at}
      </span>

      <div className="ml-auto flex items-center gap-2">
        {isCurrent ? (
          <Badge variant="muted">Current</Badge>
        ) : canWrite ? (
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={onRequestRestore}
            disabled={restoreDisabled}
          >
            Restore this version
          </Button>
        ) : null}
      </div>

      <span className="sr-only">
        {scopeLabel} version {item.version}
      </span>
    </div>
  );
}

function authorLabel(authorType: GovContextVersionSummary["author_type"]): string {
  switch (authorType) {
    case "api_key":
      return "API key";
    case "system":
      return "System";
    default:
      return "User";
  }
}

// ── Diff panel ───────────────────────────────────────────────────────────

const DIFF_FIELD_LABELS: Record<string, string> = {
  forbidden_tools: "Forbidden tools",
  forbidden_domains: "Forbidden domains",
  forbidden_topics: "Forbidden topics",
  brand_voice: "Brand voice",
  audience: "Audience",
  terminology: "Terminology",
  style: "Style",
};

const LIST_DIFF_FIELDS = new Set(["forbidden_tools", "forbidden_domains", "forbidden_topics"]);

function VersionDiffPanel({
  diff,
  scopeLabel,
}: {
  diff?: UseQueryResult<GovContextDiff, Error>;
  scopeLabel: string;
}) {
  if (!diff || diff.isPending) {
    return (
      <div role="status" aria-busy="true" aria-label="Loading diff" className="space-y-2">
        <Skeleton className="h-3 w-32" />
        <Skeleton className="h-3 w-48" />
      </div>
    );
  }

  if (diff.isError) {
    return (
      <PageError
        what="Could not load this version's diff."
        why={diff.error instanceof Error ? diff.error.message : "Unknown error."}
        onRetry={() => void diff.refetch()}
        retryLabel="Reload diff"
      />
    );
  }

  const result = diff.data;

  if (result.baseline) {
    return (
      <p className="text-xs text-muted-foreground">
        No prior version to compare — this is the first version for this{" "}
        {scopeLabel}
        {result.prior ? "" : ", or the first after a transfer"}.
      </p>
    );
  }

  const entries = result.diff ? Object.entries(result.diff) : [];

  return (
    <div className="space-y-3">
      <p className="text-xs text-muted-foreground">
        Compared to version {result.prior?.version ?? "?"}. This compares what
        was <em>authored</em> in these two versions, not what either one
        enforced at the time — an organisation&apos;s own restrictions may
        have changed since either was written.
      </p>
      {entries.length === 0 ? (
        <p className="text-xs text-muted-foreground">
          No fields changed between these two versions.
        </p>
      ) : (
        <div className="space-y-3">
          {entries.map(([key, value]) => (
            <DiffField key={key} fieldKey={key} value={value} />
          ))}
        </div>
      )}
    </div>
  );
}

function DiffField({
  fieldKey,
  value,
}: {
  fieldKey: string;
  value: GovContextListDiff | GovContextFieldDiff;
}) {
  const label = DIFF_FIELD_LABELS[fieldKey] ?? fieldKey;

  if (LIST_DIFF_FIELDS.has(fieldKey)) {
    const listDiff = value as GovContextListDiff;
    return (
      <div className="space-y-0.5">
        <p className="text-xs font-medium text-foreground">{label}</p>
        {listDiff.added && listDiff.added.length > 0 ? (
          <p className="text-xs text-success-subtle-fg">+ {listDiff.added.join(", ")}</p>
        ) : null}
        {listDiff.removed && listDiff.removed.length > 0 ? (
          <p className="text-xs text-[var(--color-destructive)]">
            − {listDiff.removed.join(", ")}
          </p>
        ) : null}
      </div>
    );
  }

  const fieldDiff = value as GovContextFieldDiff;
  return (
    <div className="space-y-0.5">
      <p className="text-xs font-medium text-foreground">{label}</p>
      <p className="text-xs text-muted-foreground line-through">
        {fieldDiff.old || "(empty)"}
      </p>
      <p className="text-xs text-foreground">{fieldDiff.new || "(empty)"}</p>
    </div>
  );
}

// ── Loading state ────────────────────────────────────────────────────────

function HistorySkeleton() {
  return (
    <div
      role="status"
      aria-busy="true"
      aria-label="Loading version history"
      className="overflow-hidden rounded-lg border border-border"
    >
      {Array.from({ length: 3 }).map((_, i) => (
        <div
          key={i}
          className="flex items-center gap-4 border-b border-border px-4 py-3 last:border-0"
        >
          <Skeleton className="h-4 w-4" />
          <Skeleton className="h-3 w-10" />
          <Skeleton className="h-3 w-24" />
          <Skeleton className="h-5 w-16 rounded-full" />
          <Skeleton className="ml-auto h-3 w-12" />
        </div>
      ))}
    </div>
  );
}

// ── Restore dialog ───────────────────────────────────────────────────────

/**
 * The WHY differs deliberately from the edit-form's widen-refusal copy
 * (gov-context-editor.tsx): a restore's widen refusal is not a mistaken edit,
 * it is the system correctly refusing to reintroduce a rule that has since
 * been tightened above it (S4's security review confirmed this refusal is
 * correct, unlike the guidance-only over-fire bug being fixed elsewhere).
 */
function restoreErrorCopy(error: Error, scopeLabel: string): { what: string; why: string } {
  if (error instanceof ContextWidenForbiddenError) {
    return {
      what: "This version could not be restored.",
      why: `Restoring it would put back a rule that has since been tightened above this ${scopeLabel}. ${error.message}`,
    };
  }
  if (error instanceof ContextVersionConflictError) {
    return {
      what: "This version could not be restored.",
      why: `Another write landed first. ${error.message}`,
    };
  }
  if (error instanceof ContextSecretDetectedError) {
    return { what: "This version could not be restored.", why: error.message };
  }
  return { what: "This version could not be restored.", why: error.message };
}

export interface RestoreVersionDialogProps {
  open: boolean;
  onClose: () => void;
  version: GovContextVersionSummary | null;
  scopeLabel: string;
  onConfirm: () => void;
  isPending: boolean;
  error: Error | null;
}

export function RestoreVersionDialog({
  open,
  onClose,
  version,
  scopeLabel,
  onConfirm,
  isPending,
  error,
}: RestoreVersionDialogProps) {
  const errCopy = error ? restoreErrorCopy(error, scopeLabel) : null;
  // CodeRabbit finding on #566: `restore.reset()` clears the mutation's
  // observed state but does not cancel the in-flight `mutateAsync` call
  // (TanStack Query does not abort mutations — the request keeps running
  // server-side regardless). Dialog routes Escape, an overlay click, AND a
  // programmatic close all through `onClose` (dialog.tsx's own doc
  // comment), so every one of those needs blocking while a restore is
  // pending, not just the Cancel button — closing here would let the
  // operator open a second restore confirmation on another row while the
  // first is still running.
  const handleClose = () => {
    if (isPending) return;
    onClose();
  };
  return (
    <Dialog open={open} onClose={handleClose}>
      <DialogContent
        ariaLabelledBy="restore-context-version-title"
        ariaDescribedBy="restore-context-version-desc"
      >
        {version ? (
          <>
            <DialogHeader>
              <DialogTitle id="restore-context-version-title">
                Restore version {version.version}?
              </DialogTitle>
              <DialogDescription id="restore-context-version-desc">
                Creates a new version with version {version.version}&apos;s
                content. Nothing is deleted — the current version stays in
                history and can itself be restored later.
              </DialogDescription>
            </DialogHeader>
            <DialogBody>
              {errCopy ? (
                <p
                  role="alert"
                  className="rounded-lg border border-[var(--color-destructive)]/30 bg-[var(--color-card)] p-3 text-sm"
                >
                  <span className="block font-medium text-foreground">{errCopy.what}</span>
                  <span className="block text-muted-foreground">{errCopy.why}</span>
                </p>
              ) : null}
            </DialogBody>
            <DialogFooter>
              <Button type="button" variant="ghost" onClick={onClose} disabled={isPending}>
                Cancel
              </Button>
              <Button type="button" onClick={onConfirm} disabled={isPending}>
                {isPending ? "Restoring…" : "Restore this version"}
              </Button>
            </DialogFooter>
          </>
        ) : null}
      </DialogContent>
    </Dialog>
  );
}
