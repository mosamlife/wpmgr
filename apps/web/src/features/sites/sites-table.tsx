import {
  forwardRef,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
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
import { Link } from "@tanstack/react-router";
import {
  TableVirtuoso,
  type TableComponents,
} from "react-virtuoso";
import {
  ChevronDown,
  ChevronUp,
  ChevronsUpDown,
  MoreHorizontal,
  RotateCw,
  Unplug,
  Zap,
} from "lucide-react";
import { motion } from "motion/react";
import type { Site } from "@wpmgr/api";

import { Badge } from "@/components/ui/badge";
import { Checkbox } from "@/components/ui/checkbox";
import { fadeUp } from "@/lib/motion-presets";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  BackupChip,
  ConnectionStateBadge,
  UpdateChip,
  type BackupChipStatus,
} from "@/components/status";
import {
  connectionStateOf,
  isReconnectable,
  type ConnectionState,
} from "@/features/sites/connection-state";
import { cn } from "@/lib/utils";
import {
  rowHeightFor,
  useSitesDensity,
  type SitesDensity,
} from "@/features/sites/use-sites-density";
import {
  useSitesSelection,
  type SitesSelection,
} from "@/features/sites/use-sites-selection";

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
}

interface SiteRow {
  readonly site: Site;
  readonly hostname: string;
  /** Phase 5 connection lifecycle state — drives the ConnectionStateBadge. */
  readonly connectionState: ConnectionState;
  /** ISO-8601 string for the <time datetime> attribute; null when unknown. */
  readonly lastSeenAt: string | null;
  readonly updatesCount: number;
  readonly updatesSeverity: "minor" | "major";
  readonly backupStatus: BackupChipStatus | null;
  readonly backupTime: string | null;
  readonly wpVersionEol: boolean;
  readonly phpVersionEol: boolean;
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

// TODO(sprint-4): updates_count + backup_status come from CP endpoints not yet
// wired. Defaults keep the table type-safe; the charts/backups subagent will
// thread real data through here in Sprint 4.
function rowOf(site: Site): SiteRow {
  return {
    site,
    hostname: hostnameFromUrl(site.url),
    connectionState: connectionStateOf(site),
    lastSeenAt: site.last_seen_at ?? null,
    updatesCount: 0,
    updatesSeverity: "minor",
    backupStatus: null,
    backupTime: null,
    wpVersionEol: false,
    phpVersionEol: false,
  };
}

// ---------------------------------------------------------------------------
// Column geometry
// ---------------------------------------------------------------------------

const COL_CHECKBOX_PX = 40;
const COL_URL_MIN_PX = 320;
const COL_TAGS_PX = 160;
const COL_WP_PX = 90;
const COL_PHP_PX = 90;
const COL_UPDATES_PX = 130;
const COL_BACKUP_PX = 180;
const COL_UPTIME_PX = 80;
const COL_ACTIONS_PX = 80;

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
            />
          </div>
        );
      },
    },
    {
      id: "tags",
      accessorFn: (row) => row.site.tags.join(","),
      header: "Client",
      enableSorting: false,
      size: COL_TAGS_PX,
      cell: ({ row }) => {
        const tags = row.original.site.tags;
        if (tags.length === 0) {
          return <span aria-hidden="true" />;
        }
        return (
          <div className="flex flex-wrap gap-1">
            {tags.slice(0, 3).map((tag) => (
              <Badge key={tag} variant="muted" className="rounded-sm">
                {tag}
              </Badge>
            ))}
            {tags.length > 3 ? (
              <Badge variant="muted" className="rounded-sm">
                +{tags.length - 3}
              </Badge>
            ) : null}
          </div>
        );
      },
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
        return (
          <BackupChip
            status={status}
            time={row.original.backupTime ?? undefined}
          />
        );
      },
    },
    {
      id: "uptime_sparkline",
      header: "Uptime",
      enableSorting: false,
      size: COL_UPTIME_PX,
      // TODO(sprint-4): swap this placeholder for the real sparkline once the
      // uptime series endpoint is plumbed.
      cell: () => <span aria-hidden="true" />,
    },
    {
      id: "actions",
      header: () => <span className="sr-only">Actions</span>,
      enableSorting: false,
      size: COL_ACTIONS_PX,
      cell: ({ row }) => (
        <RowActions
          site={row.original.site}
          connectionState={row.original.connectionState}
          onOpenAutoLogin={onOpenAutoLogin}
          onOpenDetail={onOpenDetail}
          onDisconnect={onDisconnect}
          onReconnect={onReconnect}
        />
      ),
    },
  ];
}

function RowActions({
  site,
  connectionState,
  onOpenAutoLogin,
  onOpenDetail,
  onDisconnect,
  onReconnect,
}: {
  site: Site;
  connectionState: ConnectionState;
  onOpenAutoLogin: ((site: Site) => void) | undefined;
  onOpenDetail: ((site: Site) => void) | undefined;
  onDisconnect: ((site: Site) => void) | undefined;
  onReconnect: ((site: Site) => void) | undefined;
}) {
  // pending_enrollment ("Awaiting agent") also needs the code action — the raw
  // code is shown once, so a stuck-pending site has no other way back to it.
  const canReconnect =
    isReconnectable(connectionState) ||
    connectionState === "pending_enrollment";
  const reconnectLabel =
    connectionState === "pending_enrollment"
      ? "Get enrollment code"
      : "Reconnect";
  const canDisconnect =
    connectionState === "connected" || connectionState === "degraded";
  return (
    <div className="flex items-center justify-end gap-1">
      <button
        type="button"
        aria-label={`Log in to ${site.name}`}
        title="Log in to site"
        onClick={(e) => {
          e.stopPropagation();
          onOpenAutoLogin?.(site);
        }}
        disabled={!onOpenAutoLogin}
        className="inline-flex size-7 items-center justify-center rounded text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 disabled:opacity-50"
      >
        <Zap aria-hidden="true" className="size-4" />
      </button>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <button
            type="button"
            aria-label={`More actions for ${site.name}`}
            onClick={(e) => e.stopPropagation()}
            className="inline-flex size-7 items-center justify-center rounded text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
          >
            <MoreHorizontal aria-hidden="true" className="size-4" />
          </button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuItem
            onSelect={(e) => {
              e.preventDefault();
              onOpenDetail?.(site);
            }}
          >
            Open site
          </DropdownMenuItem>
          <DropdownMenuItem
            onSelect={(e) => {
              e.preventDefault();
              onOpenAutoLogin?.(site);
            }}
            disabled={!onOpenAutoLogin}
          >
            Log in to site
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem
            onSelect={(e) => {
              e.preventDefault();
              window.open(site.url, "_blank", "noopener,noreferrer");
            }}
          >
            Open site URL
          </DropdownMenuItem>
          {canReconnect && onReconnect ? (
            <>
              <DropdownMenuSeparator />
              <DropdownMenuItem
                onSelect={(e) => {
                  e.preventDefault();
                  onReconnect(site);
                }}
              >
                <RotateCw aria-hidden="true" className="size-4" />
                {reconnectLabel}
              </DropdownMenuItem>
            </>
          ) : null}
          {canDisconnect && onDisconnect ? (
            <>
              <DropdownMenuSeparator />
              <DropdownMenuItem
                className="text-destructive focus:text-destructive"
                onSelect={(e) => {
                  e.preventDefault();
                  onDisconnect(site);
                }}
              >
                <Unplug aria-hidden="true" className="size-4" />
                Disconnect
              </DropdownMenuItem>
            </>
          ) : null}
        </DropdownMenuContent>
      </DropdownMenu>
    </div>
  );
}

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
  ...rest
}: TableHTMLAttributes<HTMLTableElement>) {
  return (
    <table
      {...rest}
      style={{ ...style, width: "100%", minWidth: "860px", tableLayout: "fixed" }}
      className="border-collapse"
    />
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
      ),
    [
      selection,
      visibleIds,
      onOpenAutoLogin,
      onOpenDetail,
      onDisconnect,
      onReconnect,
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
              "relative border-b border-border transition-colors duration-[80ms] hover:bg-muted",
              selected && "bg-muted",
              // 2px left ring on selected rows. Tailwind has no directional
              // ring utility, so we paint a 2px strip via the `before:` pseudo.
              selected &&
                "before:absolute before:left-0 before:top-0 before:bottom-0 before:w-0.5 before:bg-primary before:content-['']",
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
            <TableHeaderRow
              headerGroups={table.getHeaderGroups()}
              columns={columns}
            />
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
  columns,
}: {
  headerGroups: ReturnType<
    ReturnType<typeof useReactTable<SiteRow>>["getHeaderGroups"]
  >;
  columns: ColumnDef<SiteRow>[];
}) {
  return (
    <>
      {headerGroups.map((headerGroup) => (
        <tr
          key={headerGroup.id}
          className="h-11 border-b border-border bg-background"
        >
          {headerGroup.headers.map((header) => {
            const col = columns.find((c) => c.id === header.column.id);
            const width = (col?.size ?? 0) || undefined;
            const sortDir = header.column.getIsSorted();
            const canSort = header.column.getCanSort();
            const style: CSSProperties = width
              ? { width, minWidth: width }
              : {};
            const isFirst = header.column.id === "select";
            const isActions = header.column.id === "actions";
            return (
              <th
                key={header.id}
                scope="col"
                style={style}
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
