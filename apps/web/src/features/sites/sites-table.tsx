import {
  createContext,
  forwardRef,
  useContext,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type HTMLAttributes,
  type RefObject,
  type TableHTMLAttributes,
} from "react";
import {
  flexRender,
  getCoreRowModel,
  getSortedRowModel,
  useReactTable,
  type ColumnDef,
  type Row,
  type SortingState,
} from "@tanstack/react-table";
import { Link, useNavigate } from "@tanstack/react-router";
import {
  TableVirtuoso,
  type TableComponents,
} from "react-virtuoso";
import {
  ChevronDown,
  ChevronUp,
  ChevronsUpDown,
  Plus,
} from "lucide-react";
import { motion } from "motion/react";
import type { AgentMirrorStatus, FleetAgentVersions, Site } from "@wpmgr/api";

import { Checkbox } from "@/components/ui/checkbox";
import { TagChip, TagOverflowChip } from "@/features/sites/tag-chip";
import { SiteTagPickerPopover } from "@/features/sites/tag-picker";
import { useTagColorMap } from "@/features/tags/use-tag-color-map";
import { fadeUp } from "@/lib/motion-presets";
import {
  AgentStatusChip,
  BackupChip,
  ConnectionStateBadge,
  UpdateChip,
  type AgentStatus,
  type BackupChipStatus,
} from "@/components/status";
import {
  asConnectedSite,
  connectionStateOf,
  type ConnectionState,
} from "@/features/sites/connection-state";
import { cn, relativeTime } from "@/lib/utils";
import {
  rowHeightFor,
  useSitesDensity,
  type SitesDensity,
} from "@/features/sites/use-sites-density";
import {
  useSitesSelection,
  type SitesSelection,
} from "@/features/sites/use-sites-selection";
import { SiteRowActions } from "@/features/sites/site-row-actions";
import { PausedBadge } from "@/features/sites/site-badges";
import { siteUptimeBadge, siteUptimeTextClass } from "@/features/sites/uptime-badge";
import { AgentColumnFleetNote } from "@/features/sites/agent-column-header";
import {
  computeSitesColumnWidths,
  SITES_COLUMN_TRACKS,
  SITES_TABLE_MIN_WIDTH_PX,
} from "@/features/sites/sites-table-geometry";

// Surface 4.5 — the Sites table.
//
// The single most important surface in the app. Operators stare at this for
// hours. Built per DESIGN.md (calm, dense, borders over shadows, mono for
// versions/hostnames, no striping) and PRODUCT.md (operator-grade, information
// density over decoration).
//
// Architecture
// ------------
//   • TanStack v8 owns column defs, sort state, and the row model.
//   • react-virtuoso's <TableVirtuoso> owns the scroll container, sticky
//     header, and row virtualization. Below ~100 rows it renders everything.
//   • Selection is lifted into a hook (use-sites-selection) keyed by site_id
//     so it survives pagination / filter / sort changes.
//   • Density is lifted into a hook (use-sites-density) and persisted to
//     localStorage["wpmgr.sites.density"].
//
// Animation: Phase 5.
//
// We deliberately do NOT stagger individual rows. TableVirtuoso recycles row
// DOM nodes as the viewport scrolls (that's the whole point of
// virtualization), so any per-row enter animation would re-fire every time a
// row scrolls in from offscreen — a perpetual choreography that's actively
// hostile to an operator scanning a list. Instead, the entire table
// container gets a single, gentle `fadeUp` on its first mount per dataset
// identity. Re-fetches (same `sites` array reference shape but new contents)
// are tracked via a ref guard and explicitly do NOT re-trigger the
// animation — that would feel like flicker, not feedback.
//
// The "skeleton → real table" crossfade still happens at the surrounding
// useCrossfade layer (500ms opacity); the fadeUp here is what makes the
// rows feel like they "settled in" after the skeleton dissolves.

export interface SitesTableProps {
  sites: Site[];
  isLoading?: boolean;
  /** Override the density (defaults to localStorage, falls back to "compact"). */
  density?: SitesDensity;
  /**
   * Optional pre-lifted selection state. Pass when the surrounding page needs
   * to read the selection (e.g. to drive a bulk-update toolbar). When omitted,
   * the table owns its own selection internally.
   */
  selection?: SitesSelection;
  /** Optional pre-lifted density tuple. Same rationale as `selection`. */
  densityState?: [SitesDensity, (next: SitesDensity) => void];
  /**
   * Per-site agent-freshness classification from the fleet-wide agent
   * rollup (GET /api/v1/fleet/agents), keyed by site id. Omit while the
   * rollup is loading or unavailable (e.g. a site-scoped collaborator with
   * no org-level access), the Agent column falls back to the raw version
   * text instead of guessing a classification.
   */
  agentStatusById?: ReadonlyMap<string, AgentStatus>;
  /**
   * Where the `agentStatusById` classification's reference version came
   * from (see FleetAgentVersions.reference_source). Threaded through to the
   * Agent column's chip so a fleet-derived "current" never renders as an
   * unqualified "Current" (GH #255 follow-up: a site can be "current"
   * against the newest agent this fleet has seen while dozens of releases
   * behind a real published build). Omit while the rollup is loading.
   */
  agentReferenceSource?: FleetAgentVersions["reference_source"];
  /**
   * Freshness of the upstream agent-release mirror (GH #322;
   * FleetAgentVersions.agent_mirror), threaded only to the Agent column
   * HEADER's popover (AgentColumnFleetNote): it is a property of the
   * install-wide comparison, not of any row, so it is never passed to the
   * per-row AgentStatusChip. Omit while the rollup is loading.
   *
   * It also carries `can_check_now`, the control plane's answer for THIS
   * viewer, which is what reveals the popover's "Check now" action. That is a
   * property of the caller rather than of a row too, so it rides the same
   * single object rather than being resolved separately.
   */
  agentReferenceCheck?: AgentMirrorStatus;
  /** Optional click handler for the inline "Log in" (Zap) action. */
  onOpenAutoLogin?: (site: Site) => void;
  /** Optional click handler for the three-dot "More" item entries. */
  onOpenDetail?: (site: Site) => void;
  /** Phase 5 — open the Disconnect (revoke) confirm for a connected site. */
  onDisconnect?: (site: Site) => void;
  /** Phase 5 — start the Reconnect flow for a revoked/disconnected/archived site. */
  onReconnect?: (site: Site) => void;
  /** Hard-remove an archived/disconnected site from WPMgr (operator-only). */
  onRemove?: (site: Site) => void;
}

interface SiteRow {
  readonly site: Site;
  readonly hostname: string;
  /** Phase 5 connection lifecycle state — drives the ConnectionStateBadge. */
  readonly connectionState: ConnectionState;
  /** ISO-8601 string for the <time datetime> attribute; null when unknown. */
  readonly lastSeenAt: string | null;
  /**
   * CP-written disconnect reason; distinguishes "agent_unreachable" (active
   * verify failed) from "heartbeat_timeout" / absent (passive gap).
   */
  readonly disconnectedReason: string | null;
  readonly updatesCount: number;
  readonly updatesSeverity: "minor" | "major";
  readonly backupStatus: BackupChipStatus | null;
  readonly backupTime: string | null;
  readonly wpVersionEol: boolean;
  readonly phpVersionEol: boolean;
  /** Current WPMgr agent plugin version (M27); "" until the site re-syncs. */
  readonly agentVersion: string;
  /**
   * Agent-freshness classification from the fleet agent rollup; `undefined`
   * while the rollup is loading/unavailable, in which case the Agent column
   * renders the raw version text instead of a chip.
   */
  readonly agentStatus?: AgentStatus;
}

// ---------------------------------------------------------------------------
// Site-domain adapters
// ---------------------------------------------------------------------------

function hostnameFromUrl(url: string): string {
  // Tolerant of agent-side strings that may omit the scheme — keep the visible
  // value stable even when URL parsing fails.
  try {
    return new URL(url).hostname || url;
  } catch {
    return url.replace(/^https?:\/\//i, "").replace(/\/$/, "");
  }
}

function rowOf(
  site: Site,
  agentStatusById?: ReadonlyMap<string, AgentStatus>,
): SiteRow {
  return {
    site,
    hostname: hostnameFromUrl(site.url),
    connectionState: connectionStateOf(site),
    lastSeenAt: site.last_seen_at ?? null,
    disconnectedReason: asConnectedSite(site).disconnected_reason ?? null,
    // M27 — real data from the sites list DTO. updates_available, last_backup_*
    // and agent_version are summarized/joined CP-side.
    updatesCount: site.updates_available ?? 0,
    updatesSeverity: "minor",
    backupStatus: site.last_backup_status ?? null,
    backupTime: site.last_backup_at ?? null,
    wpVersionEol: false,
    phpVersionEol: false,
    agentVersion: site.agent_version ?? "",
    // A missing map entry (the site is not in the rollup at all, e.g. an
    // archived site, which the rollup excludes) still resolves to "unknown"
    // once the rollup has loaded; a wholly absent map (still loading, or the
    // caller never wired one) leaves this undefined so the cell falls back
    // to plain version text.
    agentStatus: agentStatusById?.get(site.id) ?? (agentStatusById ? "unknown" : undefined),
  };
}

// ---------------------------------------------------------------------------
// Tags cell — GH #230 "rich tags"
// ---------------------------------------------------------------------------

const TAGS_CELL_VISIBLE = 3;

function TagsCell({ site }: { site: Site }) {
  const navigate = useNavigate();
  const colorMap = useTagColorMap();
  const tags = site.tags;
  const visible = tags.slice(0, TAGS_CELL_VISIBLE);
  const overflow = tags.length - visible.length;

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

  return (
    <div className="flex flex-wrap items-center gap-1">
      {visible.map((name) => (
        <TagChip
          key={name}
          tag={{ name, color: colorMap.get(name) }}
          onClick={() => filterByTag(name)}
        />
      ))}
      {overflow > 0 ? (
        <SiteTagPickerPopover
          site={site}
          align="start"
          trigger={<TagOverflowChip count={overflow} />}
        />
      ) : null}
      <SiteTagPickerPopover
        site={site}
        align="start"
        trigger={
          <button
            type="button"
            aria-label={`Edit tags on ${site.name || site.url}`}
            onClick={(e) => e.stopPropagation()}
            className={cn(
              "inline-flex size-5 shrink-0 items-center justify-center rounded-sm text-muted-foreground opacity-0 transition-opacity",
              "hover:bg-accent hover:text-accent-foreground",
              "group-hover:opacity-100 focus-visible:opacity-100",
              "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1",
            )}
          >
            <Plus aria-hidden="true" className="size-3.5" />
          </button>
        }
      />
    </div>
  );
}

// ---------------------------------------------------------------------------
// Column geometry
// ---------------------------------------------------------------------------

// Single source of column geometry for BOTH the sticky header and the
// virtualized body, rendered once as a <colgroup> on the table (matches
// FleetTable). Putting widths only on the <th> cells let the header and the
// virtualized rows drift out of alignment; the colgroup makes the column tracks
// authoritative and shared.
//
// The NUMBERS now live in sites-table-geometry.ts (a pure, unit-tested
// module) and are resolved against the measured container width, because a
// single implicitly-flexible column is what produced both halves of this
// bug: it clipped its neighbours when there was too little width (GH #255)
// and swallowed ~2500px of slack on an ultrawide display (GH #261). Every
// track is now explicit, the growable ones are capped, and the leftover is
// parked in a trailing spacer track. ORDER MUST MATCH buildColumns() plus
// the trailing spacer.
const COL_CHECKBOX_PX = trackWidth("select");
const COL_URL_MIN_PX = trackWidth("url");
const COL_CLIENT_PX = trackWidth("client");
const COL_TAGS_PX = trackWidth("tags");
const COL_WP_PX = trackWidth("wp_version");
const COL_PHP_PX = trackWidth("php_version");
const COL_AGENT_PX = trackWidth("agent_version");
const COL_UPDATES_PX = trackWidth("updates_count");
const COL_BACKUP_PX = trackWidth("backup_status");
const COL_UPTIME_PX = trackWidth("uptime_sparkline");
const COL_ACTIONS_PX = trackWidth("actions");

function trackWidth(id: string): number {
  const track = SITES_COLUMN_TRACKS.find((t) => t.id === id);
  // Unreachable for the ids above; the fallback keeps this total rather
  // than throwing at module scope if a column is ever renamed.
  return track?.base ?? 100;
}

const TABLE_MIN_WIDTH_PX = SITES_TABLE_MIN_WIDTH_PX;

/**
 * Resolved column widths (one per column, plus the trailing spacer) for the
 * current container width. Read by the hoisted <VirtuosoTable> slot, which
 * must keep a STABLE component identity across renders or Virtuoso remounts
 * its scroller on every resize tick, so the widths reach it through
 * context rather than through props.
 */
const ColumnWidthsContext = createContext<readonly number[]>(
  computeSitesColumnWidths(0),
);

// ---------------------------------------------------------------------------
// Column definitions
// ---------------------------------------------------------------------------

function buildColumns(
  selection: SitesSelection,
  visibleIds: readonly string[],
  onOpenAutoLogin: ((site: Site) => void) | undefined,
  onOpenDetail: ((site: Site) => void) | undefined,
  onDisconnect: ((site: Site) => void) | undefined,
  onReconnect: ((site: Site) => void) | undefined,
  onRemove: ((site: Site) => void) | undefined,
  agentReferenceSource: FleetAgentVersions["reference_source"] | undefined,
): ColumnDef<SiteRow>[] {
  const allVisibleSelected =
    visibleIds.length > 0 && visibleIds.every((id) => selection.selected.has(id));
  const someVisibleSelected =
    visibleIds.some((id) => selection.selected.has(id)) && !allVisibleSelected;

  return [
    {
      id: "select",
      enableSorting: false,
      size: COL_CHECKBOX_PX,
      header: () => (
        <Checkbox
          aria-label={allVisibleSelected ? "Clear selection" : "Select all sites"}
          checked={allVisibleSelected}
          ref={(el) => {
            if (el) el.indeterminate = someVisibleSelected;
          }}
          onChange={(e) =>
            selection.setMany(visibleIds, e.currentTarget.checked)
          }
        />
      ),
      cell: ({ row }) => (
        <Checkbox
          aria-label={`Select ${row.original.site.name}`}
          checked={selection.selected.has(row.original.site.id)}
          onChange={() => selection.toggle(row.original.site.id)}
          onClick={(e) => e.stopPropagation()}
        />
      ),
    },
    {
      id: "url",
      accessorFn: (row) => row.hostname,
      header: "Site",
      enableSorting: true,
      size: COL_URL_MIN_PX,
      cell: ({ row }) => {
        const { hostname, connectionState, lastSeenAt, site } = row.original;
        return (
          <div className="flex min-w-0 flex-col gap-0.5">
            {/* Site name — primary link; falls back to hostname when name is absent */}
            <Link
              to="/sites/$siteId"
              params={{ siteId: site.id }}
              className="truncate text-sm font-medium text-foreground hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
              onClick={(e) => e.stopPropagation()}
            >
              {site.name || hostname}
            </Link>
            {/* Hostname in font-mono; hidden when name === hostname to avoid repetition */}
            {site.name && site.name !== hostname ? (
              <span className="truncate font-mono text-xs text-muted-foreground">
                {hostname}
              </span>
            ) : null}
            {/* Phase 5 connection lifecycle badge — dot + label + relative time,
                auto-updating, with a one-shot pulse on state change. */}
            <div className="flex min-w-0 flex-wrap items-center gap-1.5">
              <ConnectionStateBadge
                state={connectionState}
                lastSeenAt={lastSeenAt}
                disconnectedReason={row.original.disconnectedReason}
              />
              {/* GH #414 — a pause you cannot see is a pause you forget. */}
              <PausedBadge site={site} />
            </div>
          </div>
        );
      },
    },
    {
      id: "client",
      accessorFn: (row) => row.site.client_name ?? "",
      header: "Client",
      enableSorting: false,
      size: COL_CLIENT_PX,
      cell: ({ row }) => {
        const name = row.original.site.client_name;
        const clientId = row.original.site.client_id;
        if (!name) {
          return (
            <span
              aria-hidden="true"
              className="text-xs text-[var(--color-muted-foreground)]/50"
            >
              —
            </span>
          );
        }
        const inner = (
          <>
            <span
              aria-hidden="true"
              className="inline-block size-2 shrink-0 rounded-full border border-[var(--color-border)] bg-[var(--color-muted)]"
            />
            <span className="truncate text-sm">{name}</span>
          </>
        );
        if (!clientId) {
          return <div className="flex min-w-0 items-center gap-1.5">{inner}</div>;
        }
        return (
          <Link
            to="/clients/$clientId"
            params={{ clientId }}
            className="flex min-w-0 items-center gap-1.5 underline-offset-4 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
            onClick={(e) => e.stopPropagation()}
          >
            {inner}
          </Link>
        );
      },
    },
    {
      id: "tags",
      accessorFn: (row) => row.site.tags.join(","),
      header: "Tags",
      enableSorting: false,
      size: COL_TAGS_PX,
      cell: ({ row }) => <TagsCell site={row.original.site} />,
    },
    {
      id: "wp_version",
      accessorFn: (row) => row.site.wp_version,
      header: "WP",
      enableSorting: true,
      size: COL_WP_PX,
      cell: ({ row }) => {
        const v = row.original.site.wp_version;
        if (!v) return <span aria-hidden="true" />;
        return (
          <span
            className={cn(
              "font-mono text-sm tabular-nums",
              row.original.wpVersionEol &&
                "rounded bg-warning-subtle px-1.5 py-0.5 text-warning-subtle-fg",
            )}
          >
            {v}
          </span>
        );
      },
    },
    {
      id: "php_version",
      accessorFn: (row) => row.site.php_version,
      header: "PHP",
      enableSorting: true,
      size: COL_PHP_PX,
      cell: ({ row }) => {
        const v = row.original.site.php_version;
        if (!v) return <span aria-hidden="true" />;
        return (
          <span
            className={cn(
              "font-mono text-sm tabular-nums",
              row.original.phpVersionEol &&
                "rounded bg-warning-subtle px-1.5 py-0.5 text-warning-subtle-fg",
            )}
          >
            {v}
          </span>
        );
      },
    },
    {
      id: "agent_version",
      accessorFn: (row) => row.agentVersion,
      header: "Agent",
      enableSorting: true,
      size: COL_AGENT_PX,
      cell: ({ row }) => {
        const { agentVersion: v, agentStatus } = row.original;
        // The rollup hasn't loaded (or is unavailable) yet: fall back to
        // the raw version text rather than guess a classification.
        if (!agentStatus) {
          if (!v)
            return (
              <span className="font-mono text-xs text-[var(--color-muted-foreground)]">
                —
              </span>
            );
          return (
            <span className="whitespace-nowrap font-mono text-sm tabular-nums">
              {v}
            </span>
          );
        }
        // Compact: icon + version only. The status word repeated on every
        // row (and the two-line "Current in fleet" wrap) is what clipped
        // this column into its neighbours; the classification survives as
        // icon shape + colour, is announced in full to assistive tech, and
        // the fleet caveat is stated once on the column header.
        return (
          <AgentStatusChip
            compact
            status={agentStatus}
            version={v || null}
            referenceSource={agentReferenceSource}
          />
        );
      },
    },
    {
      id: "updates_count",
      accessorFn: (row) => row.updatesCount,
      header: "Updates",
      enableSorting: true,
      size: COL_UPDATES_PX,
      cell: ({ row }) => {
        const n = row.original.updatesCount;
        if (n === 0) return <span aria-hidden="true" />;
        return (
          <UpdateChip count={n} severity={row.original.updatesSeverity} />
        );
      },
    },
    {
      id: "backup_status",
      accessorFn: (row) => row.backupStatus ?? "",
      header: "Backup",
      enableSorting: false,
      size: COL_BACKUP_PX,
      cell: ({ row }) => {
        const status = row.original.backupStatus;
        if (!status) return <span aria-hidden="true" />;
        // GH #231 — relative label in the chip, exact timestamp on hover
        // (matches the /backups page's relativeTime + title convention).
        // GH #255: `compact` drops the "Backed up" prefix the column header
        // already says; the chip renders "10h ago" and still announces
        // "Backed up 10h ago". Failures keep their word and their palette.
        const iso = row.original.backupTime;
        return (
          <BackupChip
            compact
            status={status}
            time={relativeTime(iso) ?? undefined}
            title={iso ? new Date(iso).toLocaleString() : undefined}
          />
        );
      },
    },
    {
      id: "uptime_sparkline",
      header: "Uptime",
      enableSorting: false,
      size: COL_UPTIME_PX,
      cell: ({ row }) => {
        const { up, uptime_pct } = row.original.site;
        // Neither field present: site has never been probed.
        if (up == null && uptime_pct === undefined) {
          return <span aria-hidden="true" />;
        }
        // GH #272 — tri-state (never green/foreground-as-ok unless
        // up === true); see uptime-badge.ts.
        const badge = siteUptimeBadge(up);
        const toneClass = siteUptimeTextClass(badge.status);
        // Show the 30-day percentage when available, otherwise the live
        // up/down/unknown indicator.
        if (uptime_pct !== undefined) {
          const pct = uptime_pct.toFixed(1);
          return (
            <span
              className={cn("tabular-nums text-xs font-medium", toneClass)}
              title={`${pct}% uptime (30 days)`}
            >
              {pct}%
            </span>
          );
        }
        // Only live up/down/unknown is available (no 30-day window yet).
        return (
          <span className={cn("text-xs font-medium", toneClass)}>
            {badge.label}
          </span>
        );
      },
    },
    {
      id: "actions",
      header: () => <span className="sr-only">Actions</span>,
      enableSorting: false,
      size: COL_ACTIONS_PX,
      cell: ({ row }) => (
        <SiteRowActions
          site={row.original.site}
          connectionState={row.original.connectionState}
          onOpenAutoLogin={onOpenAutoLogin}
          onOpenDetail={onOpenDetail}
          onDisconnect={onDisconnect}
          onReconnect={onReconnect}
          onRemove={onRemove}
        />
      ),
    },
  ];
}

// RowActions is now in site-row-actions.tsx (SiteRowActions). Imported above.
// The table uses SiteRowActions in the "actions" column cell renderer.

// ---------------------------------------------------------------------------
// Virtuoso component slots
// ---------------------------------------------------------------------------
//
// Hoisted out of the parent render so Virtuoso doesn't see a new component
// reference on every parent re-render (which would remount the scroller).

const VirtuosoScroller = forwardRef<HTMLDivElement, HTMLAttributes<HTMLDivElement>>(
  function VirtuosoScroller(props, ref) {
    return (
      <div
        ref={ref}
        {...props}
        className={cn(
          "overflow-auto focus-visible:outline-none",
          props.className,
        )}
      />
    );
  },
);

function VirtuosoTable({
  style,
  children,
  ...rest
}: TableHTMLAttributes<HTMLTableElement>) {
  const widths = useContext(ColumnWidthsContext);
  return (
    <table
      {...rest}
      style={{ ...style, width: "100%", minWidth: TABLE_MIN_WIDTH_PX, tableLayout: "fixed" }}
      className="border-collapse"
    >
      {/* Authoritative column geometry shared by the sticky header and the
          virtualized body. Without this, per-<th> widths do not propagate to
          the body rows and the columns drift. The last track is the spacer
          that soaks up ultrawide surplus (GH #261). It is zero-width at
          and below the table's minimum width. */}
      <colgroup>
        {widths.map((w, i) => (
          <col
            key={i}
            // The trailing spacer is left auto on purpose: as the ONLY
            // auto track it absorbs the exact remainder, including the few
            // pixels a vertical scrollbar takes off the measured width, so
            // the surplus can never fall back onto the Site column.
            style={i === widths.length - 1 ? undefined : { width: w }}
          />
        ))}
      </colgroup>
      {children}
    </table>
  );
}

/**
 * Width of `ref`'s element, tracked live. Returns 0 until first measured
 * (and in any environment without ResizeObserver), which
 * computeSitesColumnWidths resolves to the base widths.
 */
function useMeasuredWidth(ref: RefObject<HTMLElement | null>): number {
  const [width, setWidth] = useState(0);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    setWidth(el.clientWidth);
    if (typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (entry) setWidth(entry.contentRect.width);
    });
    observer.observe(el);
    return () => observer.disconnect();
  }, [ref]);
  return width;
}

const VirtuosoTableHead = forwardRef<
  HTMLTableSectionElement,
  HTMLAttributes<HTMLTableSectionElement>
>(function VirtuosoTableHead(props, ref) {
  return (
    <thead
      ref={ref}
      {...props}
      className={cn("sticky top-0 z-10 bg-background", props.className)}
    />
  );
});

// ---------------------------------------------------------------------------
// SitesTable — public surface
// ---------------------------------------------------------------------------

export function SitesTable({
  sites,
  isLoading,
  density: densityProp,
  selection: externalSelection,
  densityState: externalDensityState,
  agentStatusById,
  agentReferenceSource,
  agentReferenceCheck,
  onOpenAutoLogin,
  onOpenDetail,
  onDisconnect,
  onReconnect,
  onRemove,
}: SitesTableProps) {
  const internalSelection = useSitesSelection();
  const selection = externalSelection ?? internalSelection;

  const internalDensityState = useSitesDensity(densityProp);
  const [density, setDensity] = externalDensityState ?? internalDensityState;

  const rows = useMemo<SiteRow[]>(
    () => sites.map((s) => rowOf(s, agentStatusById)),
    [sites, agentStatusById],
  );
  const visibleIds = useMemo(() => sites.map((s) => s.id), [sites]);

  const [sorting, setSorting] = useState<SortingState>([]);

  const columns = useMemo(
    () =>
      buildColumns(
        selection,
        visibleIds,
        onOpenAutoLogin,
        onOpenDetail,
        onDisconnect,
        onReconnect,
        onRemove,
        agentReferenceSource,
      ),
    [
      selection,
      visibleIds,
      onOpenAutoLogin,
      onOpenDetail,
      onDisconnect,
      onReconnect,
      onRemove,
      agentReferenceSource,
    ],
  );

  const table = useReactTable({
    data: rows,
    columns,
    state: { sorting },
    onSortingChange: setSorting,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
    getRowId: (r) => r.site.id,
  });

  const sortedRows = table.getRowModel().rows;
  const rowHeight = rowHeightFor(density);

  // The TableRow component reads the live `selection` set so we recompute its
  // identity on selection changes. (Acceptable: row count is small, and
  // selection changes already invalidate the row models.)
  const virtuosoComponents = useMemo<TableComponents<Row<SiteRow>>>(
    () => ({
      Scroller: VirtuosoScroller,
      Table: VirtuosoTable,
      TableHead: VirtuosoTableHead,
      TableRow: ({ item, style, ...rest }) => {
        const selected = selection.selected.has(item.original.site.id);
        return (
          <tr
            {...rest}
            style={{ ...style, height: rowHeight }}
            data-state={selected ? "selected" : undefined}
            className={cn(
              "group border-b border-border transition-colors duration-[80ms] hover:bg-muted",
              // Selection is indicated by a background tint only. We deliberately
              // do NOT use `position: relative` + an absolute `before:` strip on
              // the row: a relatively-positioned table row with an abspos child
              // drops out of the `table-layout: fixed` column grid, so selected
              // rows would lose their column widths and drift from the header.
              selected && "bg-primary/5",
            )}
          />
        );
      },
    }),
    [rowHeight, selection],
  );

  // Surface 4.6 (sites-toolbar.tsx) owns the toolbar above the table — the
  // density toggle, selection counter, filters, and bulk actions all live
  // there now. The route lifts `selection` + `density` so both surfaces share
  // state. See features/sites/sites-toolbar.tsx.
  //
  // `setDensity` is intentionally surfaced into the closure so callers passing
  // an `externalDensityState` can ignore it; when the table owns density
  // internally, no surface currently changes it (the toolbar is the surface).
  void setDensity;

  // First-mount guard for the container fadeUp. `initial` only equals the
  // preset's "initial" state on the very first render — every subsequent
  // re-render (re-fetch, sort change, selection change) passes `false`,
  // which tells motion to skip the enter animation. This is the contract
  // that keeps the fadeUp from re-firing when react-query refreshes the
  // sites array under us.
  const hasMounted = useRef<boolean>(false);
  const firstMount = !hasMounted.current;
  hasMounted.current = true;

  // Live container width -> resolved column tracks (GH #255 / GH #261).
  // Measured on the scroll region (whose own width never changes when the
  // inner scroller gains a scrollbar), so there is no measure/relayout
  // feedback loop.
  const regionRef = useRef<HTMLDivElement>(null);
  const availableWidth = useMeasuredWidth(regionRef);
  const columnWidths = useMemo(
    () => computeSitesColumnWidths(availableWidth),
    [availableWidth],
  );

  return (
    <motion.div
      className="flex min-w-0 w-full flex-col bg-background"
      // Only the very first render gets the enter. After that, `initial=false`
      // means motion just renders at the "animate" target without easing.
      variants={fadeUp}
      initial={firstMount ? "initial" : false}
      animate="animate"
    >
      <div
        ref={regionRef}
        role="region"
        aria-label="Sites table"
        aria-busy={isLoading ? "true" : undefined}
        className="relative h-[calc(100vh-12rem)] min-h-[400px] w-full overflow-x-auto"
      >
        <ColumnWidthsContext.Provider value={columnWidths}>
          <TableVirtuoso<Row<SiteRow>, unknown>
            data={sortedRows}
            totalCount={sortedRows.length}
            components={virtuosoComponents}
            fixedHeaderContent={() => (
              <TableHeaderRow
                headerGroups={table.getHeaderGroups()}
                agentReferenceSource={agentReferenceSource}
                agentReferenceCheck={agentReferenceCheck}
              />
            )}
            itemContent={(_, row) => <TableBodyCells row={row} />}
          />
        </ColumnWidthsContext.Provider>
      </div>
    </motion.div>
  );
}


// ---------------------------------------------------------------------------
// Header + body cell rendering
// ---------------------------------------------------------------------------

function TableHeaderRow({
  headerGroups,
  agentReferenceSource,
  agentReferenceCheck,
}: {
  headerGroups: ReturnType<
    ReturnType<typeof useReactTable<SiteRow>>["getHeaderGroups"]
  >;
  agentReferenceSource?: FleetAgentVersions["reference_source"];
  agentReferenceCheck?: AgentMirrorStatus;
}) {
  return (
    <>
      {headerGroups.map((headerGroup) => (
        <tr
          key={headerGroup.id}
          className="h-11 border-b border-border bg-background"
        >
          {headerGroup.headers.map((header) => {
            // Column widths come from the <colgroup> (single source of geometry);
            // the header cells deliberately carry no width so they cannot drift
            // from the body rows.
            const sortDir = header.column.getIsSorted();
            const canSort = header.column.getCanSort();
            const isFirst = header.column.id === "select";
            const isActions = header.column.id === "actions";
            // The "compared against this fleet" caveat is a property of the
            // whole Agent column, so it is stated once here rather than
            // appended to every row. It sits OUTSIDE the sort button: a
            // button nested in a button is invalid and unreachable.
            const accessory =
              header.column.id === "agent_version" ? (
                <AgentColumnFleetNote
                  referenceSource={agentReferenceSource}
                  referenceCheck={agentReferenceCheck}
                />
              ) : null;
            const content = header.isPlaceholder ? null : canSort ? (
              <button
                type="button"
                onClick={header.column.getToggleSortingHandler()}
                className="group inline-flex h-full items-center gap-1 text-xs font-medium uppercase tracking-wide text-muted-foreground transition-colors hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
              >
                {flexRender(
                  header.column.columnDef.header,
                  header.getContext(),
                )}
                <SortGlyph dir={sortDir} />
              </button>
            ) : (
              flexRender(header.column.columnDef.header, header.getContext())
            );
            return (
              <th
                key={header.id}
                scope="col"
                className={cn(
                  "px-3 text-left align-middle text-xs font-medium uppercase tracking-wide text-muted-foreground",
                  isFirst && "pl-4 pr-2",
                  isActions && "pr-4 text-right",
                )}
              >
                {accessory ? (
                  // h-full so the wrapped sort button keeps resolving its
                  // own h-full against the header cell, not against a
                  // content-sized span.
                  <span className="inline-flex h-full items-center gap-1">
                    {content}
                    {accessory}
                  </span>
                ) : (
                  content
                )}
              </th>
            );
          })}
          {/* Trailing spacer track (GH #261). Present in every row so the
              column exists in the table grid and the row rule runs the full
              width; a <td> rather than a <th> so it adds no column name, and
              aria-hidden so it is skipped by assistive tech. */}
          <td aria-hidden="true" className="p-0" />
        </tr>
      ))}
    </>
  );
}

function SortGlyph({ dir }: { dir: false | "asc" | "desc" }) {
  if (dir === "asc")
    return <ChevronUp aria-hidden="true" className="size-3 text-foreground" />;
  if (dir === "desc")
    return (
      <ChevronDown aria-hidden="true" className="size-3 text-foreground" />
    );
  return (
    <ChevronsUpDown
      aria-hidden="true"
      className="size-3 opacity-0 transition-opacity group-hover:opacity-60"
    />
  );
}

function TableBodyCells({ row }: { row: Row<SiteRow> }) {
  const cells = row.getVisibleCells();
  return (
    <>
      {cells.map((cell, idx) => {
        const isFirst = idx === 0;
        const isLast = idx === cells.length - 1;
        const isActions = cell.column.id === "actions";
        return (
          <td
            key={cell.id}
            className={cn(
              "border-b border-border px-3 align-middle text-sm text-foreground",
              isFirst && "pl-4 pr-2",
              isActions && "pr-4 text-right",
              isLast && !isActions && "pr-4",
            )}
          >
            {flexRender(cell.column.columnDef.cell, cell.getContext())}
          </td>
        );
      })}
      {/* Trailing spacer track, see TableHeaderRow. Carries the row rule so
          the border still runs to the table's right edge. */}
      <td aria-hidden="true" className="border-b border-border p-0" />
    </>
  );
}
