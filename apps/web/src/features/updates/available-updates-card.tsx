import { useEffect, useRef, useState } from "react";
import { Link } from "@tanstack/react-router";
import {
  AlertTriangle,
  Check,
  CheckCircle2,
  ExternalLink,
  RotateCcw,
  X,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { FreshnessBadge } from "@/components/shared/freshness-badge";
import { LiveIndicator } from "@/components/shared/live-indicator";
import { VersionArrow } from "@/components/shared/version-arrow";
import { PageError } from "@/components/feedback/page-error";
import { UpdateChip } from "@/components/status/update-chip";
import { useCreateUpdateRun } from "@/features/updates/use-updates";
import { createBackup } from "@wpmgr/api";
import { toError } from "@/features/auth/use-auth";
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
import { AgentUpdateNotice } from "@/features/updates/agent-update-notice";
import {
  useSiteAgentUpdate,
  type SiteAgentUpdate,
} from "@/features/updates/use-site-agent-update";
import {
  isSiteDownRecovery,
  SITE_DOWN_RECOVERY_FALLBACK_DETAIL,
} from "@/features/updates/summarize";
import { toast } from "@/components/toast";
import { wpOrgSlug } from "@/features/updates/wp-org-slug";

// AvailableUpdatesCard — the per-site "what needs updating" panel mounted on
// the site detail page. Drives a row-per-target table with:
//   - WordPress core (if reported)
//   - Plugins / themes the agent flagged as having an update
//   - Multi-select + Update all / Update selected
//   - SSE-driven live state per row (idle | starting | pending | running |
//     succeeded | failed | rolled_back | skipped)
//   - Refresh button (POST /updates/refresh) + relative "as of" timestamp
//   - A separate, non-selectable WPMgr agent line (GH #314, AgentUpdateNotice)
//
// Wire-level behavior all lives in `useAvailableUpdates`, `useRefreshSiteUpdates`,
// and `useRowUpdate` — this file is pure presentation + selection state.
//
// SCOPE OF EVERY "up to date" CLAIM IN THIS FILE (GH #314). `data.items` and
// `data.core_update` are the components WPMgr MANAGES. The WPMgr agent is
// stripped from that projection on the control plane on purpose (shipped
// 0.61.97), so `total === 0` means "nothing WPMgr manages needs updating", and
// it has never meant "this site has no updates waiting". The card said "All up
// to date" for that state, which an operator reasonably read as including the
// agent, while the same site's wp-admin was offering an agent update. Every
// claim here is now scoped to the managed set in words, and the agent is
// stated separately by AgentUpdateNotice.

const WP_RELEASES = "https://wordpress.org/news/category/releases/";
const CORE_SELECTION_KEY = "core:core";

function changelogHref(item: AvailableUpdateItem): string {
  // Use the directory slug (before the first slash) so "suremails/suremails.php"
  // resolves to "suremails" not the 404-generating full file path.
  const slug = wpOrgSlug(item.slug);
  if (item.type === "theme") {
    return `https://wordpress.org/themes/${encodeURIComponent(slug)}/`;
  }
  return `https://wordpress.org/plugins/${encodeURIComponent(slug)}/#developers`;
}

export function AvailableUpdatesCard({ siteId }: { siteId: string }) {
  const { data, isPending, isError, error, refetch, isFetching } =
    useAvailableUpdates(siteId);
  const refresh = useRefreshSiteUpdates(siteId);
  // GH #314. Resolved once here and passed down, so the header badge and the
  // body cannot disagree about what this tab knows. `null` while the fleet
  // rollup is loading, refused (a site-scoped collaborator's 403) or missing
  // this site, in which case no agent line renders at all.
  const agent = useSiteAgentUpdate(siteId);

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
              // GH #314: "Up to date" here carried the same half truth as the
              // body's "All up to date" did, so it is scoped in words too.
              // Unconditional rather than conditional on the agent's state:
              // the agent classification arrives from a second, independently
              // loading query that a site-scoped collaborator never gets at
              // all, and a badge whose claim flips once that lands would be
              // both jumpy and, for the moment before it lands, the same half
              // truth again.
              <span className="inline-flex items-center gap-1 rounded px-2 py-0.5 text-xs font-medium text-[var(--color-muted-foreground)] bg-[var(--color-muted)]">
                No managed updates
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
            agent={agent}
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
  agent,
  selected,
  onToggle,
  onClearSelection,
}: {
  siteId: string;
  data: SiteAvailableUpdates;
  agent: SiteAgentUpdate | null;
  selected: Set<string>;
  onToggle: (key: string) => void;
  onClearSelection: () => void;
}) {
  const total = (data.core_update ? 1 : 0) + data.items.length;

  return (
    <div className="space-y-4">
      {total === 0 ? (
        <div
          role="status"
          className="flex flex-col items-center gap-2 py-6 text-center"
        >
          <CheckCircle2
            aria-hidden="true"
            className="size-8 text-[var(--color-success)]"
          />
          {/* GH #314. QUALIFIED rather than suppressed, and qualified
              unconditionally.
              Suppressing the up-to-date state whenever the agent is behind
              would have made the most common, entirely healthy case (nothing
              to update, agent current) render as an absence, and it would
              have made the sentence depend on a second query that a
              site-scoped collaborator is refused outright, so the same site
              would say different things to two operators.
              "Managed components" is the honest scope and it is the only
              scope this data has ever described: the control plane strips the
              agent from this projection before it is sent. The agent is then
              stated separately, below, in exactly the terms the
              classification supports. */}
          <p className="text-sm font-medium text-[var(--color-foreground)]">
            All managed components are up to date
          </p>
          <FreshnessBadge collectedAt={data.as_of ?? null} />
        </div>
      ) : (
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
      )}

      {/* Rendered in BOTH states, and always AFTER the selectable list and
          its bulk footer. The agent is a fact about this site whether or not
          anything else needs updating, and its position outside the <ul> the
          footer acts on is the structural half of "not selectable": there is
          no row for it to be a row of. */}
      <AgentUpdateNotice agent={agent} />
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
    case "rolled_back": {
      // GH #210 — the worst-case rollback failure (site-wide fatal, the
      // rollback command undeliverable, an agent watchdog attempting
      // automatic filesystem recovery) reads as its own severe, actionable
      // condition here too, not a generic "failed"/"rolled back" line.
      if (isSiteDownRecovery(state, progress, error)) {
        return (
          <span
            role="alert"
            className="inline-flex items-start gap-1.5 text-destructive-subtle-fg"
          >
            <AlertTriangle aria-hidden="true" className="mt-0.5 size-3.5 shrink-0" />
            {error ?? progress ?? SITE_DOWN_RECOVERY_FALLBACK_DETAIL}
          </span>
        );
      }
      if (state === "failed") {
        return (
          <span
            role="alert"
            className="inline-flex items-center gap-1 text-[var(--color-destructive)]"
          >
            <X aria-hidden="true" className="size-3.5" />
            {error ?? "Update failed"}
          </span>
        );
      }
      return (
        <span
          role="alert"
          className="inline-flex items-center gap-1 text-warning-subtle-fg"
        >
          <RotateCcw aria-hidden="true" className="size-3.5" />
          Rolled back{error ? `: ${error}` : ""}
        </span>
      );
    }
    case "skipped":
      return (
        <span role="status" className="text-[var(--color-muted-foreground)]">
          Skipped
        </span>
      );
    // GH #463: not yet eligible for dispatch (parent run hasn't reached its
    // scheduled_at). Nothing is imminent, so plain muted static text —
    // NOT the pulsing LiveIndicator used for pending/running.
    case "scheduled":
      return (
        <span role="status" className="text-[var(--color-muted-foreground)]">
          Scheduled
        </span>
      );
    // GH #255 Phase 2: never dispatched because its run halted first.
    case "cancelled":
      return (
        <span role="status" className="text-[var(--color-muted-foreground)]">
          Not attempted
        </span>
      );
    // GH #463: the parent run expired without dispatching this task.
    case "expired":
      return (
        <span role="status" className="text-[var(--color-muted-foreground)]">
          Expired
        </span>
      );
  }
}

function RowActionButton({ row, label }: { row: RowUpdate; label: string }) {
  // GH #463: expired/cancelled are terminal with nothing ever sent — give
  // the operator a way to actually start it. Folded into the same Retry
  // bucket as failed/rolled_back (row.retry() just calls trigger() again, a
  // fresh immediate run), but the button itself is already the neutral
  // `variant="outline"` treatment below, not destructive/warning styling, so
  // nothing extra is needed to keep this muted since nothing actually failed.
  const isTerminalError =
    row.state === "failed" ||
    row.state === "rolled_back" ||
    row.state === "expired" ||
    row.state === "cancelled";
  const isBusy =
    row.state === "starting" ||
    row.state === "pending" ||
    row.state === "running";
  // GH #463: `scheduled` means a run already exists for this row and will
  // fire later on its own — hide the action button entirely rather than show
  // a disabled "Working…" button, since nothing is actively in progress.
  const isDone =
    row.state === "succeeded" || row.state === "skipped" || row.state === "scheduled";

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

    // "Take backup first" — gate on takeBackup ALONE (not && includeCore) so
    // plugin-only updates with the box checked still enqueue a backup.
    if (takeBackup) {
      try {
        const { error, response } = await createBackup({
          path: { siteId },
          body: { kind: "full" },
        });
        if (response?.status === 422) {
          // backup_already_in_flight — a backup is already in progress.
          // Treat this as satisfying "take backup first" and proceed.
          toast.info("A backup is already in progress", {
            description: "The existing backup satisfies the pre-update backup requirement. Continuing with updates.",
          });
        } else if (error) {
          // Real enqueue failure — abort the update and surface a retry.
          throw toError(error);
        } else {
          toast.success("Backup queued", {
            description: "The backup will run before the update completes.",
          });
        }
      } catch (err) {
        const message = err instanceof Error ? err.message : "Backup failed";
        toast.error("Could not queue backup", {
          description: message,
          action: {
            label: "Try again",
            onClick: () => void submit(includeAll),
          },
        });
        return;
      }
    }

    const body = buildBulkBody(siteId, items, includeCore);
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
