import {
  forwardRef,
  useMemo,
  useRef,
  useState,
  type HTMLAttributes,
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
import type { Site } from "@wpmgr/api";

import { Checkbox } from "@/components/ui/checkbox";
import { TagChip, TagOverflowChip } from "@/features/sites/tag-chip";
import { SiteTagPickerPopover } from "@/features/sites/tag-picker";
import { useTagColorMap } from "@/features/tags/use-tag-color-map";
import { fadeUp } from "@/lib/motion-presets";
import {
  BackupChip,
  ConnectionStateBadge,
  UpdateChip,
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

function rowOf(site: Site): SiteRow {
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

const COL_CHECKBOX_PX = 40;
const COL_URL_MIN_PX = 280;
const COL_CLIENT_PX = 120;
const COL_TAGS_PX = 140;
const COL_WP_PX = 80;
const COL_PHP_PX = 80;
const COL_AGENT_PX = 100;
const COL_UPDATES_PX = 120;
const COL_BACKUP_PX = 160;
const COL_UPTIME_PX = 70;
const COL_ACTIONS_PX = 80;

// Single source of column geometry for BOTH the sticky header and the
// virtualized body, rendered once as a <colgroup> on the table (matches
// FleetTable). Putting widths only on the <th> cells let the header and the
// virtualized rows drift out of alignment; the colgroup makes the column tracks
// authoritative and shared. ORDER MUST MATCH buildColumns(). `undefined` marks
// the flexible column (Site) that absorbs the remaining width.
const COLUMN_WIDTHS_PX: ReadonlyArray<number | undefined> = [
  COL_CHECKBOX_PX, // select
  undefined, // url (Site) — flexible
  COL_CLIENT_PX, // client
  COL_TAGS_PX, // tags
  COL_WP_PX, // wp_version
  COL_PHP_PX, // php_version
  COL_AGENT_PX, // agent_version
  COL_UPDATES_PX, // updates_count
  COL_BACKUP_PX, // backup_status
  COL_UPTIME_PX, // uptime_sparkline
  COL_ACTIONS_PX, // actions
];

// Minimum table width = the sum of every column at its intended size (the
// flexible Site column counted at a sensible minimum). When the viewport is
// narrower than this (mobile), the table keeps these widths and the container
// scrolls horizontally instead of squeezing the columns into each other — the
// previous 860px floor was BELOW the fixed columns' total, so on mobile the
// fixed-layout table compressed every column and the headers overlapped.
const COL_SITE_MIN_PX = 260;
const TABLE_MIN_WIDTH_PX = COLUMN_WIDTHS_PX.reduce<number>(
  (sum, w) => sum + (w ?? COL_SITE_MIN_PX),
  0,
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
            <ConnectionStateBadge
              state={connectionState}
              lastSeenAt={lastSeenAt}
              disconnectedReason={row.original.disconnectedReason}
            />
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
        const v = row.original.agentVersion;
        if (!v)
          return (
            <span className="font-mono text-xs text-[var(--color-muted-foreground)]">
              —
            </span>
          );
        return <span className="font-mono text-sm tabular-nums">{v}</span>;
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
        const iso = row.original.backupTime;
        return (
          <BackupChip
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
        if (up === undefined && uptime_pct === undefined) {
          return <span aria-hidden="true" />;
        }
        // Show the 30-day percentage when available, otherwise the live
        // up/down indicator.
        if (uptime_pct !== undefined) {
          const pct = uptime_pct.toFixed(1);
          const isDown = up === false;
          return (
            <span
              className={cn(
                "tabular-nums text-xs font-medium",
                isDown
                  ? "text-[var(--color-destructive)]"
                  : "text-[var(--color-foreground)]",
              )}
              title={`${pct}% uptime (30 days)`}
            >
              {pct}%
            </span>
          );
        }
        // Only live up/down is available (no 30-day window yet).
        return (
          <span
            className={cn(
              "text-xs font-medium",
              up ? "text-[var(--color-foreground)]" : "text-[var(--color-destructive)]",
            )}
          >
            {up ? "Up" : "Down"}
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
  return (
    <table
      {...rest}
      style={{ ...style, width: "100%", minWidth: TABLE_MIN_WIDTH_PX, tableLayout: "fixed" }}
      className="border-collapse"
    >
      {/* Authoritative column geometry shared by the sticky header and the
          virtualized body. Without this, per-<th> widths do not propagate to
          the body rows and the columns drift. */}
      <colgroup>
        {COLUMN_WIDTHS_PX.map((w, i) => (
          <col key={i} style={w != null ? { width: w } : undefined} />
        ))}
      </colgroup>
      {children}
    </table>
  );
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

  const rows = useMemo<SiteRow[]>(() => sites.map(rowOf), [sites]);
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
      ),
    [
      selection,
      visibleIds,
      onOpenAutoLogin,
      onOpenDetail,
      onDisconnect,
      onReconnect,
      onRemove,
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
        role="region"
        aria-label="Sites table"
        aria-busy={isLoading ? "true" : undefined}
        className="relative h-[calc(100vh-12rem)] min-h-[400px] w-full overflow-x-auto"
      >
        <TableVirtuoso<Row<SiteRow>, unknown>
          data={sortedRows}
          totalCount={sortedRows.length}
          components={virtuosoComponents}
          fixedHeaderContent={() => (
            <TableHeaderRow headerGroups={table.getHeaderGroups()} />
          )}
          itemContent={(_, row) => <TableBodyCells row={row} />}
        />
      </div>
    </motion.div>
  );
}


// ---------------------------------------------------------------------------
// Header + body cell rendering
// ---------------------------------------------------------------------------

function TableHeaderRow({
  headerGroups,
}: {
  headerGroups: ReturnType<
    ReturnType<typeof useReactTable<SiteRow>>["getHeaderGroups"]
  >;
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
                {header.isPlaceholder ? null : canSort ? (
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
                  flexRender(
                    header.column.columnDef.header,
                    header.getContext(),
                  )
                )}
              </th>
            );
          })}
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
    </>
  );
}
