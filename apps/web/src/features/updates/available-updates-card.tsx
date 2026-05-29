import { useEffect, useRef, useState } from "react";
import { Link } from "@tanstack/react-router";
import { Check, CheckCircle2, ExternalLink, RotateCcw, X } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { FreshnessBadge } from "@/components/shared/freshness-badge";
import { LiveIndicator } from "@/components/shared/live-indicator";
import { VersionArrow } from "@/components/shared/version-arrow";
import { PageError } from "@/components/feedback/page-error";
import { UpdateChip } from "@/components/status/update-chip";
import { useCreateUpdateRun } from "@/features/updates/use-updates";
import {
  useAvailableUpdates,
  useRefreshSiteUpdates,
  RefreshConflictError,
} from "@/features/updates/use-available-updates";
import {
  buildBulkBody,
  useRowUpdate,
  useCoreRowUpdate,
  type RowUpdate,
  type RowUpdateState,
} from "@/features/updates/use-row-update";
import {
  itemKey,
  type AvailableUpdateItem,
  type CoreUpdate,
  type SiteAvailableUpdates,
} from "@/features/updates/types";
import { toast } from "@/components/toast";

// AvailableUpdatesCard — the per-site "what needs updating" panel mounted on
// the site detail page. Drives a row-per-target table with:
//   - WordPress core (if reported)
//   - Plugins / themes the agent flagged as having an update
//   - Multi-select + Update all / Update selected
//   - SSE-driven live state per row (idle | starting | pending | running |
//     succeeded | failed | rolled_back | skipped)
//   - Refresh button (POST /updates/refresh) + relative "as of" timestamp
//
// Wire-level behavior all lives in `useAvailableUpdates`, `useRefreshSiteUpdates`,
// and `useRowUpdate` — this file is pure presentation + selection state.

const WP_RELEASES = "https://wordpress.org/news/category/releases/";
const CORE_SELECTION_KEY = "core:core";

function changelogHref(item: AvailableUpdateItem): string {
  if (item.type === "theme") {
    return `https://wordpress.org/themes/${encodeURIComponent(item.slug)}/`;
  }
  return `https://wordpress.org/plugins/${encodeURIComponent(item.slug)}/#developers`;
}

export function AvailableUpdatesCard({ siteId }: { siteId: string }) {
  const { data, isPending, isError, error, refetch, isFetching } =
    useAvailableUpdates(siteId);
  const refresh = useRefreshSiteUpdates(siteId);

  // Multi-selection state. Keys: "core:core", "plugin:<slug>", "theme:<slug>".
  const [selected, setSelected] = useState<Set<string>>(() => new Set());

  function toggle(key: string) {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  }

  function clearSelection() {
    setSelected(new Set());
  }

  async function onRefresh() {
    try {
      await refresh.mutateAsync();
      toast.success("Refresh requested", {
        description: "Updates will reappear in a moment.",
      });
    } catch (err) {
      if (err instanceof RefreshConflictError) {
        // Not actually an error path — the agent is already doing the thing.
        toast.info("A refresh is already in progress for this site");
        return;
      }
      const message = err instanceof Error ? err.message : "Refresh failed";
      toast.error("Could not refresh updates", {
        description: message,
        action: {
          label: "Try again",
          onClick: () => void onRefresh(),
        },
      });
    }
  }

  // Determine header badge
  const total = data ? (data.core_update ? 1 : 0) + data.items.length : 0;
  const hasCoreUpdate = Boolean(data?.core_update);
  const severity: "minor" | "major" = hasCoreUpdate ? "major" : "minor";

  return (
    <div className="rounded-xl border border-[var(--color-border)] bg-[var(--color-card)] text-[var(--color-card-foreground)]">
      {/* Header */}
      <div className="flex flex-wrap items-start justify-between gap-3 border-b border-[var(--color-border)] px-5 py-4">
        <div className="space-y-1">
          <div className="flex items-center gap-2">
            <h3 className="text-sm font-semibold text-[var(--color-foreground)]">
              Available updates
            </h3>
            {data && total > 0 ? (
              <UpdateChip
                count={total}
                severity={severity}
                description={
                  hasCoreUpdate ? "Major: WordPress core update included" : undefined
                }
              />
            ) : data && total === 0 ? (
              <span className="inline-flex items-center gap-1 rounded px-2 py-0.5 text-xs font-medium text-[var(--color-muted-foreground)] bg-[var(--color-muted)]">
                Up to date
              </span>
            ) : null}
          </div>
          <div className="text-xs">
            <FreshnessBadge collectedAt={data?.as_of ?? null} />
          </div>
        </div>
        <Button
          variant="outline"
          size="sm"
          onClick={() => {
            void onRefresh();
            void refetch();
          }}
          disabled={refresh.isPending || isFetching}
          aria-label="Refresh available updates"
        >
          {refresh.isPending ? "Refreshing…" : "Refresh"}
        </Button>
      </div>

      {/* Body */}
      <div className="px-5 py-4">
        {isPending ? (
          <SkeletonRows />
        ) : isError ? (
          <PageError
            what="Could not load available updates"
            why={error instanceof Error ? error.message : undefined}
            onRetry={() => void refetch()}
            isRetrying={isFetching}
          />
        ) : (
          <UpdatesBody
            siteId={siteId}
            data={data}
            selected={selected}
            onToggle={toggle}
            onClearSelection={clearSelection}
          />
        )}
      </div>
    </div>
  );
}

function SkeletonRows() {
  return (
    <ul aria-busy="true" aria-live="polite" className="space-y-2">
      {[0, 1, 2].map((i) => (
        <li
          key={i}
          className="h-10 animate-pulse rounded-md bg-[var(--color-muted)]/40"
        />
      ))}
    </ul>
  );
}

function UpdatesBody({
  siteId,
  data,
  selected,
  onToggle,
  onClearSelection,
}: {
  siteId: string;
  data: SiteAvailableUpdates;
  selected: Set<string>;
  onToggle: (key: string) => void;
  onClearSelection: () => void;
}) {
  const total = (data.core_update ? 1 : 0) + data.items.length;

  if (total === 0) {
    return (
      <div
        role="status"
        className="flex flex-col items-center gap-2 py-6 text-center"
      >
        <CheckCircle2
          aria-hidden="true"
          className="size-8 text-[var(--color-success)]"
        />
        <p className="text-sm font-medium text-[var(--color-foreground)]">
          All up to date
        </p>
        <FreshnessBadge collectedAt={data.as_of ?? null} />
      </div>
    );
  }

  return (
    <div className="space-y-3">
      <ul
        aria-label="Updates available"
        className="divide-y divide-[var(--color-border)] rounded-md border border-[var(--color-border)]"
      >
        {data.core_update ? (
          <CoreRow
            siteId={siteId}
            coreUpdate={data.core_update}
            checked={selected.has(CORE_SELECTION_KEY)}
            onToggle={() => onToggle(CORE_SELECTION_KEY)}
          />
        ) : null}
        {data.items.map((item) => (
          <ComponentRow
            key={itemKey(item)}
            siteId={siteId}
            item={item}
            checked={selected.has(itemKey(item))}
            onToggle={() => onToggle(itemKey(item))}
          />
        ))}
      </ul>

      <BulkFooter
        siteId={siteId}
        data={data}
        selected={selected}
        onCleared={onClearSelection}
      />
    </div>
  );
}

interface RowFrameProps {
  checkbox?: React.ReactNode;
  label: React.ReactNode;
  rightLabel: React.ReactNode;
  actions: React.ReactNode;
  state: RowUpdateState;
}

function RowFrame({
  checkbox,
  label,
  rightLabel,
  actions,
  state,
}: RowFrameProps) {
  // Once succeeded, fade the row out so the eye knows the work is done. The
  // available-updates cache scrubs the item shortly after via useRowUpdate so
  // the row unmounts; the fade is the bridge.
  const faded = state === "succeeded";
  return (
    <li
      data-testid="available-update-row"
      data-state={state}
      className={
        "flex flex-wrap items-center gap-3 px-3 py-2 transition-opacity duration-700 " +
        (faded ? "opacity-40" : "opacity-100")
      }
    >
      <div className="flex min-w-0 flex-1 items-center gap-3">
        {checkbox}
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">{label}</div>
          <div className="text-xs text-[var(--color-muted-foreground)]">
            {rightLabel}
          </div>
        </div>
      </div>
      <div className="flex shrink-0 items-center gap-2">{actions}</div>
    </li>
  );
}

function CoreRow({
  siteId,
  coreUpdate,
  checked,
  onToggle,
}: {
  siteId: string;
  coreUpdate: CoreUpdate;
  checked: boolean;
  onToggle: () => void;
}) {
  const row = useCoreRowUpdate(siteId, coreUpdate);

  return (
    <RowFrame
      checkbox={
        <Checkbox
          checked={checked}
          onChange={onToggle}
          aria-label="Select WordPress core"
        />
      }
      label={
        <>
          <span className="font-medium">WordPress core</span>
          <Badge variant="outline">Core</Badge>
          <VersionArrow
            from={coreUpdate.current_version}
            to={coreUpdate.new_version}
          />
        </>
      }
      rightLabel={
        <RowStateLine
          state={row.state}
          progress={row.progress}
          error={row.error}
        />
      }
      actions={
        <>
          <RowActionButton row={row} label="Update" />
          <Button asChild variant="ghost" size="sm">
            <a
              href={WP_RELEASES}
              target="_blank"
              rel="noopener noreferrer"
              aria-label="WordPress release notes (opens in new tab)"
            >
              <ExternalLink aria-hidden="true" className="size-3.5" />
              Notes
            </a>
          </Button>
          {row.runId ? <ViewLogsLink runId={row.runId} /> : null}
        </>
      }
      state={row.state}
    />
  );
}

function ComponentRow({
  siteId,
  item,
  checked,
  onToggle,
}: {
  siteId: string;
  item: AvailableUpdateItem;
  checked: boolean;
  onToggle: () => void;
}) {
  const row = useRowUpdate(siteId, item);
  return (
    <RowFrame
      checkbox={
        <Checkbox
          checked={checked}
          onChange={onToggle}
          aria-label={`Select ${item.name}`}
        />
      }
      label={
        <>
          <span className="font-medium">{item.name}</span>
          {item.active ? (
            <Badge variant="success">Active</Badge>
          ) : (
            <Badge variant="muted">Inactive</Badge>
          )}
          <Badge variant="outline" className="capitalize">
            {item.type}
          </Badge>
          <VersionArrow from={item.version} to={item.new_version} />
        </>
      }
      rightLabel={
        <RowStateLine
          state={row.state}
          progress={row.progress}
          error={row.error}
        />
      }
      actions={
        <>
          <RowActionButton row={row} label="Update" />
          <Button asChild variant="ghost" size="sm">
            <a
              href={changelogHref(item)}
              target="_blank"
              rel="noopener noreferrer"
              aria-label={`${item.name} changelog (opens in new tab)`}
            >
              <ExternalLink aria-hidden="true" className="size-3.5" />
              Changelog
            </a>
          </Button>
          {row.runId ? <ViewLogsLink runId={row.runId} /> : null}
        </>
      }
      state={row.state}
    />
  );
}

function ViewLogsLink({ runId }: { runId: string }) {
  return (
    <Button asChild variant="ghost" size="sm">
      <Link to="/updates/$runId" params={{ runId }} aria-label="View update logs">
        View logs
      </Link>
    </Button>
  );
}

function RowStateLine({
  state,
  progress,
  error,
}: {
  state: RowUpdateState;
  progress?: string;
  error?: string;
}) {
  switch (state) {
    case "idle":
      return null;
    case "starting":
      return (
        <span role="status" aria-live="polite" className="inline-flex items-center gap-1.5">
          <LiveIndicator state="connecting" label="Starting" />
        </span>
      );
    case "pending":
      return (
        <span role="status" aria-live="polite" className="inline-flex items-center gap-1.5">
          <LiveIndicator state="connecting" label="Queued" />
        </span>
      );
    case "running":
      return (
        <span role="status" aria-live="polite" className="inline-flex items-center gap-1.5">
          <LiveIndicator state="live" label={progress ?? "Updating"} />
        </span>
      );
    case "succeeded":
      return (
        <span
          role="status"
          aria-live="polite"
          className="inline-flex items-center gap-1 text-[var(--color-success)]"
        >
          <Check aria-hidden="true" className="size-3.5" />
          Updated
        </span>
      );
    case "failed":
      return (
        <span
          role="alert"
          className="inline-flex items-center gap-1 text-[var(--color-destructive)]"
        >
          <X aria-hidden="true" className="size-3.5" />
          {error ?? "Update failed"}
        </span>
      );
    case "rolled_back":
      return (
        <span
          role="alert"
          className="inline-flex items-center gap-1 text-warning-subtle-fg"
        >
          <RotateCcw aria-hidden="true" className="size-3.5" />
          Rolled back{error ? `: ${error}` : ""}
        </span>
      );
    case "skipped":
      return (
        <span role="status" className="text-[var(--color-muted-foreground)]">
          Skipped
        </span>
      );
  }
}

function RowActionButton({ row, label }: { row: RowUpdate; label: string }) {
  const isTerminalError = row.state === "failed" || row.state === "rolled_back";
  const isBusy =
    row.state === "starting" ||
    row.state === "pending" ||
    row.state === "running";
  const isDone = row.state === "succeeded" || row.state === "skipped";

  if (isDone) return null;
  if (isTerminalError) {
    return (
      <Button size="sm" variant="outline" onClick={() => void row.retry()}>
        Retry
      </Button>
    );
  }
  return (
    <Button
      size="sm"
      onClick={() => void row.trigger()}
      disabled={isBusy}
      aria-label={label}
    >
      {isBusy ? "Working…" : label}
    </Button>
  );
}

function BulkFooter({
  siteId,
  data,
  selected,
  onCleared,
}: {
  siteId: string;
  data: SiteAvailableUpdates;
  selected: Set<string>;
  onCleared: () => void;
}) {
  const create = useCreateUpdateRun();
  // Default the "Take backup first" toggle to ON when a core update is in the
  // list (matches the brief). Re-arm whenever the presence of a core update
  // changes so operators don't accidentally update core without it.
  const [takeBackup, setTakeBackup] = useState(() => Boolean(data.core_update));
  const lastHadCore = useRef<boolean>(Boolean(data.core_update));
  useEffect(() => {
    const hasCore = Boolean(data.core_update);
    if (hasCore !== lastHadCore.current) {
      lastHadCore.current = hasCore;
      setTakeBackup(hasCore);
    }
  }, [data.core_update]);

  const total = (data.core_update ? 1 : 0) + data.items.length;
  const selectedCount = selected.size;

  async function submit(includeAll: boolean) {
    const includeCore = includeAll
      ? data.core_update
      : selected.has(CORE_SELECTION_KEY)
        ? data.core_update
        : null;

    const wantedKeys = includeAll
      ? new Set(data.items.map((i) => itemKey(i)))
      : selected;
    const items = data.items.filter((i) => wantedKeys.has(itemKey(i)));

    if (!includeCore && items.length === 0) return;

    const body = buildBulkBody(siteId, items, includeCore);
    if (takeBackup && includeCore) {
      // The "backup first" hint is not yet in the wire schema. Log it so the
      // intent is visible in the browser console; Track B will lift this into
      // the schema and we can promote it from a hint to a body field.
      console.info("[updates] backup-first requested for core update");
    }
    try {
      await create.mutateAsync(body);
      const count = items.length + (includeCore ? 1 : 0);
      toast.success(`Started ${count} update${count === 1 ? "" : "s"}`, {
        description: "The agent will report back as each one finishes.",
      });
      onCleared();
    } catch (err) {
      const message = err instanceof Error ? err.message : "Could not start";
      toast.error("Update did not start", {
        description: message,
        action: {
          label: "Try again",
          onClick: () => void submit(includeAll),
        },
      });
    }
  }

  return (
    <div className="flex flex-wrap items-center justify-between gap-3 rounded-md border border-[var(--color-border)] bg-[var(--color-muted)]/30 p-3">
      <div className="flex flex-wrap items-center gap-2">
        <Button
          size="sm"
          onClick={() => void submit(true)}
          disabled={create.isPending || total === 0}
        >
          Update all ({total})
        </Button>
        <Button
          size="sm"
          variant="outline"
          onClick={() => void submit(false)}
          disabled={create.isPending || selectedCount === 0}
        >
          Update selected ({selectedCount})
        </Button>
      </div>
      <label className="flex items-center gap-2 text-sm">
        <Checkbox
          checked={takeBackup}
          onChange={(e) => setTakeBackup(e.target.checked)}
        />
        Take backup first
      </label>
    </div>
  );
}
