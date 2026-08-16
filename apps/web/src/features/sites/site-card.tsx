/**
 * SiteCard — rich info-dense card for the Sites grid view.
 *
 * Single `rounded-lg border bg-card` surface. No nested cards, no side-stripe
 * borders. Selection uses a ring (ring-2 ring-primary) + bg-primary/5, NOT a
 * left stripe.
 *
 * Card anatomy (top to bottom):
 *   1. Hero band          — SiteCardThumbnail (16:10 comfortable / 16:9 compact)
 *   2. Header row         — site name link + SiteRowActions menu
 *   3. Status rail        — ConnectionStateBadge + hostname (mono, reserved slot)
 *   4. Capability group   — labeled 2-col grid: Page Cache / Object Cache /
 *                           HTTPS / Multisite — always visible
 *   5. Chip flow          — UpdateChip / calm "Up to date" | BackupChip / calm
 *                           "No backups yet" | SslChip (when tls_expires_at)
 *   6. Uptime row         — UptimeBadge (dot + label, GH #414 pause-aware) +
 *                           pct + latency, reserved slot
 *   7. Meta footer        — labeled DefinitionList: Versions / Host / Client /
 *                           Tags / Screenshot; comfortable=always, compact=hover
 *
 * Alignment discipline:
 *   Every section renders with a calm empty/default state when data is absent so
 *   all cards in a grid row align baseline-to-baseline regardless of which
 *   optional fields are populated.
 *
 * Density:
 *   comfortable — all sections inline, caption in footer
 *   compact     — hero 16:9, footer hover-reveal
 */
import { Link, useNavigate } from "@tanstack/react-router";
import {
  CheckCircle2,
  Database,
  Globe,
  HardDrive,
  Lock,
  Plus,
} from "lucide-react";
import type { ReactNode } from "react";
import type { Site } from "@wpmgr/api";

import { Checkbox } from "@/components/ui/checkbox";
import { TagChip, TagOverflowChip } from "@/features/sites/tag-chip";
import { SiteTagPickerPopover } from "@/features/sites/tag-picker";
import { useTagColorMap } from "@/features/tags/use-tag-color-map";
import {
  BackupChip,
  ConnectionStateBadge,
  SslChip,
  UpdateChip,
  type BackupChipStatus,
} from "@/components/status";
import { DefinitionList } from "@/components/shared/definition-list";
import {
  connectionStateOf,
  asConnectedSite,
} from "@/features/sites/connection-state";
import { SiteRowActions } from "@/features/sites/site-row-actions";
import { PausedBadge, UptimeBadge } from "@/features/sites/site-badges";
import { SiteCardThumbnail } from "@/features/sites/site-card-thumbnail";
import { isMonitoringPaused } from "@/features/sites/monitoring-pause";
import {
  CapabilityGroup,
  type CapabilityItem,
} from "@/features/sites/capability-strip";
import { useSitesSelection } from "@/features/sites/use-sites-selection";
import type { CardSize } from "@/features/sites/use-sites-view";
import { cn, relativeTime } from "@/lib/utils";

// ─── Helpers ──────────────────────────────────────────────────────────────────

function hostnameFromUrl(url: string): string {
  try {
    return new URL(url).hostname || url;
  } catch {
    return url.replace(/^https?:\/\//i, "").replace(/\/$/, "");
  }
}

/**
 * Derive the capability item list for a site. Items are always in the same
 * order so the grid is stable across re-renders. On/off encoded by the
 * `enabled` flag — never hue alone.
 */
function buildCapabilityItems(site: Site): CapabilityItem[] {
  // GH #243 — read the real drop-in state directly off the site record.
  // Page Cache / Object Cache both ship as drop-ins, never as a plugin, so
  // an active-plugin-slug inference can never be true; it was a permanent
  // false positive since v0.49.0. `site.page_cache_enabled` /
  // `site.object_cache_enabled` are the authoritative booleans.
  const hasPageCache = site.page_cache_enabled;
  const hasObjectCache = site.object_cache_enabled;

  const isHttps = site.url.startsWith("https://");
  const isMultisite = site.multisite;

  return [
    {
      icon: HardDrive,
      label: "Page Cache",
      enabled: hasPageCache,
    },
    {
      icon: Database,
      label: "Object Cache",
      enabled: hasObjectCache,
    },
    {
      icon: Lock,
      label: "HTTPS",
      enabled: isHttps,
    },
    {
      icon: Globe,
      label: "Multisite",
      enabled: isMultisite,
    },
  ];
}

// ─── Props ────────────────────────────────────────────────────────────────────

export interface SiteCardProps {
  site: Site;
  cardSize: CardSize;
  selectionCount: number;
  onOpenAutoLogin?: (site: Site) => void;
  /** Opens the site's detail page — wired to the "Open site" menu action. */
  onOpenDetail?: (site: Site) => void;
  onDisconnect?: (site: Site) => void;
  onReconnect?: (site: Site) => void;
  /** GH #414 — opens the pause-confirmation dialog for this one site. */
  onPauseMonitoring?: (site: Site) => void;
  /** GH #414 — resumes this one site directly, no confirmation. */
  onResumeMonitoring?: (site: Site) => void;
}

// ─── Component ────────────────────────────────────────────────────────────────

export function SiteCard({
  site,
  cardSize,
  selectionCount,
  onOpenAutoLogin,
  onOpenDetail,
  onDisconnect,
  onReconnect,
  onPauseMonitoring,
  onResumeMonitoring,
}: SiteCardProps) {
  const selection = useSitesSelection();
  const navigate = useNavigate();
  const colorMap = useTagColorMap();
  const isSelected = selection.selected.has(site.id);
  const anySelected = selectionCount > 0;

  const hostname = hostnameFromUrl(site.url);
  const connectionState = connectionStateOf(site);
  const connectedSite = asConnectedSite(site);
  const disconnectedReason = connectedSite.disconnected_reason ?? null;

  const updatesCount = site.updates_available ?? 0;
  const backupStatus = (site.last_backup_status as BackupChipStatus | null) ?? null;
  const backupTime = site.last_backup_at ?? null;
  // GH #231 — show a relative label ("2h ago") in the chip, with the exact
  // absolute timestamp on hover, matching the /backups page convention.
  const backupTimeRelative = relativeTime(backupTime) ?? undefined;
  const backupTimeTitle = backupTime
    ? new Date(backupTime).toLocaleString()
    : undefined;
  // GH #414 — a paused site's uptime result is frozen; the row below shows
  // the stale/"as of" treatment for a paused site even with no prior probe.
  const showUptimeRow = site.uptime_pct != null || isMonitoringPaused(site);

  const capabilityItems = buildCapabilityItems(site);
  const isCompact = cardSize === "compact";

  // Space toggles selection when the card wrapper itself has focus (not a
  // child element). We check currentTarget === target to distinguish card-level
  // focus from bubbled events.
  const handleKeyDown = (e: React.KeyboardEvent<HTMLDivElement>) => {
    if (e.target !== e.currentTarget) return;
    if (e.key === " ") {
      e.preventDefault();
      selection.toggle(site.id);
    }
  };

  // Versions string — only the parts that are present, joined by " · "
  const versionParts: string[] = [];
  if (site.wp_version) versionParts.push(`WP ${site.wp_version}`);
  if (site.php_version) versionParts.push(`PHP ${site.php_version}`);
  if (site.agent_version) versionParts.push(`agent ${site.agent_version}`);
  const versionString = versionParts.join(" · ");

  // Screenshot capture label for the metadata footer row.
  const screenshotValue = site.screenshot_captured_at
    ? relativeTime(site.screenshot_captured_at)
    : "never captured";

  function filterByTag(name: string) {
    void navigate({
      to: "/sites",
      search: (prev: Record<string, unknown>) => ({
        ...prev,
        tags: [name],
        tagMode: undefined,
      }),
      replace: true,
    });
  }

  // Tags: max 2 shown + overflow chip (opens the tag picker), or a ghost "+"
  // affordance (hover/focus-within only, since an empty card has nothing to
  // anchor an always-visible control to) when the site has no tags at all.
  const tagVisible = site.tags.slice(0, 2);
  const tagOverflow = site.tags.length - tagVisible.length;
  const tagsValue: ReactNode =
    site.tags.length > 0 ? (
      <span className="flex flex-wrap items-center gap-1">
        {tagVisible.map((name) => (
          <TagChip
            key={name}
            tag={{ name, color: colorMap.get(name) }}
            onClick={() => filterByTag(name)}
          />
        ))}
        {tagOverflow > 0 ? (
          <SiteTagPickerPopover
            site={site}
            align="start"
            trigger={<TagOverflowChip count={tagOverflow} />}
          />
        ) : null}
        <SiteTagPickerPopover
          site={site}
          align="start"
          trigger={
            <button
              type="button"
              aria-label={`Edit tags on ${site.name || hostname}`}
              onClick={(e) => e.stopPropagation()}
              className="inline-flex size-5 shrink-0 items-center justify-center rounded-sm text-muted-foreground opacity-0 transition-opacity hover:bg-accent hover:text-accent-foreground group-hover:opacity-100 group-focus-within:opacity-100 focus-visible:opacity-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1"
            >
              <Plus aria-hidden="true" className="size-3.5" />
            </button>
          }
        />
      </span>
    ) : (
      // No always-visible "+" on empty cards — hover/focus-within only.
      <SiteTagPickerPopover
        site={site}
        align="start"
        trigger={
          <button
            type="button"
            aria-label={`Add tags to ${site.name || hostname}`}
            onClick={(e) => e.stopPropagation()}
            className="inline-flex items-center gap-1 rounded-sm text-xs text-muted-foreground opacity-0 transition-opacity hover:text-foreground group-hover:opacity-100 group-focus-within:opacity-100 focus-visible:opacity-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1"
          >
            <Plus aria-hidden="true" className="size-3.5" />
            Add tag
          </button>
        }
      />
    );

  // Client value: small dot indicator when present.
  const clientValue: ReactNode = site.client_name
    ? (
      <span className="flex min-w-0 items-center gap-1.5">
        <span
          aria-hidden="true"
          className="inline-block size-1.5 shrink-0 rounded-full border border-border bg-muted"
        />
        <span className="truncate">{site.client_name}</span>
      </span>
    )
    : undefined;

  return (
    <div
      role="article"
      aria-label={site.name || hostname}
      className={cn(
        "group relative flex flex-col rounded-lg border bg-card text-card-foreground transition-colors",
        "outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
        isSelected
          ? "border-primary/50 bg-primary/5 ring-2 ring-primary"
          : "border-border hover:border-border/80 hover:bg-card",
      )}
      tabIndex={0}
      onKeyDown={handleKeyDown}
    >
      {/* ── 1. Hero band ─────────────────────────────────────────────────── */}
      {/* Checkbox overlay top-left — shown on hover/selection */}
      <div
        className={cn(
          "absolute left-2 top-2 z-10 transition-opacity",
          isSelected || anySelected
            ? "opacity-100"
            : "opacity-0 group-hover:opacity-100 group-focus-within:opacity-100",
        )}
      >
        <Checkbox
          aria-label={`Select ${site.name || hostname}`}
          checked={isSelected}
          onChange={() => selection.toggle(site.id)}
          onClick={(e) => e.stopPropagation()}
          className="rounded border-border bg-background shadow-sm"
        />
      </div>

      <SiteCardThumbnail
        site={site}
        hideCaption={isCompact}
        className={isCompact ? "aspect-[16/9]" : undefined}
      />

      {/* ── Card body ────────────────────────────────────────────────────── */}
      <div className="flex flex-1 flex-col gap-2 p-3">

        {/* ── 2. Header row ────────────────────────────────────────────── */}
        <div className="flex min-w-0 items-start justify-between gap-2">
          <Link
            to="/sites/$siteId"
            params={{ siteId: site.id }}
            className="min-w-0 flex-1 truncate text-sm font-medium text-foreground underline-offset-4 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
            onClick={(e) => e.stopPropagation()}
          >
            {site.name || hostname}
          </Link>
          <div className="shrink-0" onClick={(e) => e.stopPropagation()}>
            <SiteRowActions
              site={site}
              connectionState={connectionState}
              onOpenAutoLogin={onOpenAutoLogin}
              onOpenDetail={onOpenDetail}
              onDisconnect={onDisconnect}
              onReconnect={onReconnect}
              onPauseMonitoring={onPauseMonitoring}
              onResumeMonitoring={onResumeMonitoring}
            />
          </div>
        </div>

        {/* ── 3. Status rail ───────────────────────────────────────────── */}
        {/* Reserved height: hostname slot is always rendered (min-h-5) so
            cards with/without a distinct hostname stay aligned. */}
        <div className="flex min-w-0 flex-col gap-1">
          <div className="flex min-w-0 flex-wrap items-center gap-1.5">
            <ConnectionStateBadge
              state={connectionState}
              lastSeenAt={site.last_seen_at ?? null}
              disconnectedReason={disconnectedReason}
            />
            {/* GH #414 — a pause you cannot see is a pause you forget. */}
            <PausedBadge site={site} />
          </div>
          {/* Hostname (mono) — always reserve the slot height */}
          <div className="min-h-4">
            {site.name && site.name !== hostname ? (
              <span className="truncate font-mono text-xs text-muted-foreground">
                {hostname}
              </span>
            ) : null}
          </div>
        </div>

        {/* ── 4. Capability group ──────────────────────────────────────── */}
        {/* Always rendered (fixed height via grid) — never conditionally
            omitted — so cards align row-for-row regardless of data. */}
        <CapabilityGroup items={capabilityItems} />

        {/* ── 5. Chip flow ─────────────────────────────────────────────── */}
        {/* Always renders all three slots (updates / backup / ssl) with calm
            empty text so chip-row height is consistent across cards. */}
        <div className="flex flex-wrap items-center gap-1.5">
          {/* Updates: chip when >0, calm check when 0 */}
          {updatesCount > 0 ? (
            <UpdateChip count={updatesCount} severity="minor" />
          ) : (
            <span className="inline-flex items-center gap-1 text-xs text-muted-foreground">
              <CheckCircle2 aria-hidden="true" className="size-3 text-muted-foreground/60" />
              <span>Up to date</span>
            </span>
          )}

          {/* Backup: chip when status present, calm text when absent */}
          {backupStatus ? (
            <BackupChip
              status={backupStatus}
              time={backupTimeRelative}
              title={backupTimeTitle}
            />
          ) : (
            <span className="text-xs text-muted-foreground">No backups yet</span>
          )}

          {/* SSL: only when tls_expires_at is present */}
          {site.tls_expires_at ? (
            <SslChip expiresAt={site.tls_expires_at} />
          ) : null}
        </div>

        {/* ── 6. Uptime row ────────────────────────────────────────────── */}
        {/* Reserved-height slot (min-h-5) prevents layout shift when
            monitoring data arrives later. Text-only (no sparkline; deferred). */}
        <div className="flex min-h-5 items-center">
          {showUptimeRow ? (
            <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <UptimeBadge site={site} />
              {site.uptime_pct != null ? (
                <span className="tabular-nums">
                  {" · Uptime "}
                  <span className="font-medium text-foreground tabular-nums">
                    {site.uptime_pct.toFixed(2)}%
                  </span>
                  {site.avg_latency_ms != null ? (
                    <>
                      {" · "}
                      <span className="tabular-nums">{site.avg_latency_ms}ms</span>
                    </>
                  ) : null}
                </span>
              ) : null}
            </div>
          ) : (
            <span className="text-xs text-muted-foreground/60">
              Uptime not monitored
            </span>
          )}
        </div>

        {/* ── 7. Meta footer ───────────────────────────────────────────── */}
        {/* comfortable: always visible; compact: hover-reveal via group-hover.
            Uses DefinitionList so every value has a visible label prefix.
            Always renders all rows with calm empty states (en-dash) for
            absent values — this is the primary alignment anchor. */}
        <div
          className={cn(
            "border-t border-border/50 pt-2",
            isCompact &&
              "opacity-0 transition-opacity duration-200 group-hover:opacity-100",
          )}
        >
          <DefinitionList
            className="gap-y-1 text-xs"
            rows={[
              {
                label: "Versions",
                value: versionString || undefined,
                mono: true,
                tabular: true,
              },
              {
                label: "Host",
                value: site.host_provider || undefined,
              },
              {
                label: "Client",
                value: clientValue,
              },
              {
                label: "Tags",
                value: tagsValue,
              },
              {
                label: "Screenshot",
                value: screenshotValue,
              },
            ]}
          />
        </div>

      </div>
    </div>
  );
}
