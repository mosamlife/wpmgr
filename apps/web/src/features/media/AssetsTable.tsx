import {
  forwardRef,
  useMemo,
  type CSSProperties,
  type HTMLAttributes,
  type TableHTMLAttributes,
} from "react";
import {
  flexRender,
  getCoreRowModel,
  useReactTable,
  type ColumnDef,
  type Row,
} from "@tanstack/react-table";
import { TableVirtuoso, type TableComponents } from "react-virtuoso";
import { ImageIcon } from "lucide-react";

import { cn, formatBytes } from "@/lib/utils";
import { Checkbox } from "@/components/ui/checkbox";

import { AssetStatusChip } from "./AssetStatusChip";
import { FormatBadge } from "./FormatBadge";
import { SavingsBadge } from "./SavingsBadge";
import type { MediaAsset } from "./types";

// AssetsTable — the media library table.
//
// Mirrors the proven sites-table.tsx architecture: TanStack Table v8 owns the
// column defs + row model; react-virtuoso's <TableVirtuoso> owns the scroll
// container, the sticky header, and row virtualization (it renders everything
// below ~100 rows, virtualizes above — the repo's standard "Tables virtualized
// at >100 rows" per DESIGN.md). Sticky checkbox + thumbnail columns are pinned
// via `position: sticky` on their cells. Row click opens the side drawer; the
// checkbox/links stop propagation so they don't trigger the row click.
//
// Selection is lifted to the parent (MediaTab) so the BulkActionBar can read it.

export interface AssetsTableProps {
  assets: MediaAsset[];
  selected: ReadonlySet<string>;
  onToggle: (id: string) => void;
  onToggleMany: (ids: string[], selected: boolean) => void;
  onRowClick: (asset: MediaAsset) => void;
  /** Failed-chip click → open the asset (reason lives in the job detail). */
  onReasonClick: (asset: MediaAsset) => void;
}

const COL_CHECKBOX_PX = 44;
const COL_THUMB_PX = 56;
const COL_FORMAT_PX = 130;
const COL_STATUS_PX = 170;
const COL_ORIG_PX = 100;
const COL_CUR_PX = 100;
const COL_SAVED_PX = 90;

function buildColumns(
  selected: ReadonlySet<string>,
  visibleIds: readonly string[],
  onToggle: (id: string) => void,
  onToggleMany: (ids: string[], selected: boolean) => void,
  onReasonClick: (asset: MediaAsset) => void,
): ColumnDef<MediaAsset>[] {
  const allSelected =
    visibleIds.length > 0 && visibleIds.every((id) => selected.has(id));
  const someSelected =
    visibleIds.some((id) => selected.has(id)) && !allSelected;

  return [
    {
      id: "select",
      enableSorting: false,
      size: COL_CHECKBOX_PX,
      header: () => (
        <Checkbox
          aria-label={allSelected ? "Clear selection" : "Select all assets"}
          checked={allSelected}
          ref={(el) => {
            if (el) el.indeterminate = someSelected;
          }}
          onChange={(e) => onToggleMany([...visibleIds], e.currentTarget.checked)}
        />
      ),
      cell: ({ row }) => (
        <Checkbox
          aria-label={`Select ${row.original.title || `attachment ${row.original.wp_attachment_id}`}`}
          checked={selected.has(row.original.id)}
          onChange={() => onToggle(row.original.id)}
          onClick={(e) => e.stopPropagation()}
        />
      ),
    },
    {
      id: "thumb",
      enableSorting: false,
      size: COL_THUMB_PX,
      header: () => <span className="sr-only">Thumbnail</span>,
      cell: ({ row }) => <Thumbnail asset={row.original} />,
    },
    {
      id: "title",
      header: "Asset",
      enableSorting: false,
      cell: ({ row }) => {
        const a = row.original;
        return (
          <div className="flex min-w-0 flex-col gap-0.5">
            <span className="truncate text-sm text-[var(--color-foreground)]">
              {a.title || `Attachment #${a.wp_attachment_id}`}
            </span>
            <span className="truncate font-mono text-[11px] tabular-nums text-[var(--color-muted-foreground)]">
              #{a.wp_attachment_id}
            </span>
          </div>
        );
      },
    },
    {
      id: "format",
      header: "Format",
      enableSorting: false,
      size: COL_FORMAT_PX,
      cell: ({ row }) => (
        <FormatBadge
          source={row.original.original_mime}
          current={row.original.current_format}
        />
      ),
    },
    {
      id: "status",
      header: "Status",
      enableSorting: false,
      size: COL_STATUS_PX,
      cell: ({ row }) => {
        const a = row.original;
        return (
          <AssetStatusChip
            status={a.status}
            onReasonClick={
              a.status === "failed" ? () => onReasonClick(a) : undefined
            }
          />
        );
      },
    },
    {
      id: "original",
      header: "Original",
      enableSorting: false,
      size: COL_ORIG_PX,
      cell: ({ row }) => (
        <span className="font-mono text-xs tabular-nums text-[var(--color-muted-foreground)]">
          {formatBytes(row.original.original_size_bytes)}
        </span>
      ),
    },
    {
      id: "current",
      header: "Current",
      enableSorting: false,
      size: COL_CUR_PX,
      cell: ({ row }) => (
        <span className="font-mono text-xs tabular-nums text-[var(--color-foreground)]">
          {formatBytes(row.original.current_size_bytes)}
        </span>
      ),
    },
    {
      id: "saved",
      header: "Saved",
      enableSorting: false,
      size: COL_SAVED_PX,
      cell: ({ row }) => (
        <SavingsBadge
          originalBytes={row.original.original_size_bytes}
          currentBytes={row.original.current_size_bytes}
        />
      ),
    },
  ];
}

function Thumbnail({ asset }: { asset: MediaAsset }) {
  // We never proxy image bytes through the CP; the thumbnail uses the asset's
  // original_url directly (served by the site). Falls back to a placeholder
  // glyph on load error.
  return (
    <div className="flex size-9 items-center justify-center overflow-hidden rounded border border-[var(--color-border)] bg-[var(--color-muted)]">
      {asset.original_url ? (
        <img
          src={asset.original_url}
          alt=""
          loading="lazy"
          decoding="async"
          className="size-full object-cover"
          onError={(e) => {
            e.currentTarget.style.display = "none";
          }}
        />
      ) : (
        <ImageIcon
          aria-hidden="true"
          className="size-4 text-[var(--color-muted-foreground)]"
        />
      )}
    </div>
  );
}

// ── Virtuoso slots (hoisted so Virtuoso keeps a stable component identity) ──

const VirtuosoScroller = forwardRef<
  HTMLDivElement,
  HTMLAttributes<HTMLDivElement>
>(function VirtuosoScroller(props, ref) {
  return (
    <div
      ref={ref}
      {...props}
      className={cn("overflow-auto focus-visible:outline-none", props.className)}
    />
  );
});

function VirtuosoTable({ style, ...rest }: TableHTMLAttributes<HTMLTableElement>) {
  return (
    <table
      {...rest}
      style={{ ...style, width: "100%", minWidth: "720px", tableLayout: "fixed" }}
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
      className={cn(
        "sticky top-0 z-10 bg-[var(--color-background)]",
        props.className,
      )}
    />
  );
});

// Sticky-column geometry: the checkbox + thumbnail columns stay pinned on
// horizontal scroll. left offsets are the cumulative widths of the columns to
// their left.
function stickyStyle(colId: string): CSSProperties | undefined {
  if (colId === "select")
    return { position: "sticky", left: 0, zIndex: 2 };
  if (colId === "thumb")
    return { position: "sticky", left: COL_CHECKBOX_PX, zIndex: 2 };
  return undefined;
}

export function AssetsTable({
  assets,
  selected,
  onToggle,
  onToggleMany,
  onRowClick,
  onReasonClick,
}: AssetsTableProps) {
  const visibleIds = useMemo(() => assets.map((a) => a.id), [assets]);

  const columns = useMemo(
    () =>
      buildColumns(selected, visibleIds, onToggle, onToggleMany, onReasonClick),
    [selected, visibleIds, onToggle, onToggleMany, onReasonClick],
  );

  const table = useReactTable({
    data: assets,
    columns,
    getCoreRowModel: getCoreRowModel(),
    getRowId: (a) => a.id,
  });

  const rows = table.getRowModel().rows;

  const components = useMemo<TableComponents<Row<MediaAsset>>>(
    () => ({
      Scroller: VirtuosoScroller,
      Table: VirtuosoTable,
      TableHead: VirtuosoTableHead,
      TableRow: ({ item, style, ...rest }) => {
        const isSelected = selected.has(item.original.id);
        return (
          <tr
            {...rest}
            style={style}
            data-state={isSelected ? "selected" : undefined}
            onClick={() => onRowClick(item.original)}
            className={cn(
              "relative h-14 cursor-pointer border-b border-[var(--color-border)] transition-colors duration-[80ms] hover:bg-[var(--color-muted)]",
              isSelected && "bg-[var(--color-muted)]",
              isSelected &&
                "before:absolute before:left-0 before:top-0 before:bottom-0 before:z-[3] before:w-0.5 before:bg-[var(--color-primary)] before:content-['']",
            )}
          />
        );
      },
    }),
    [selected, onRowClick],
  );

  return (
    <div
      role="region"
      aria-label="Media assets table"
      className="relative h-[calc(100vh-22rem)] min-h-[360px] w-full overflow-hidden rounded-lg border border-[var(--color-border)] bg-[var(--color-card)]"
    >
      <TableVirtuoso<Row<MediaAsset>, unknown>
        data={rows}
        totalCount={rows.length}
        components={components}
        fixedHeaderContent={() => (
          <HeaderRow
            headerGroups={table.getHeaderGroups()}
            columns={columns}
          />
        )}
        itemContent={(_, row) => <BodyCells row={row} />}
      />
    </div>
  );
}

function HeaderRow({
  headerGroups,
  columns,
}: {
  headerGroups: ReturnType<
    ReturnType<typeof useReactTable<MediaAsset>>["getHeaderGroups"]
  >;
  columns: ColumnDef<MediaAsset>[];
}) {
  return (
    <>
      {headerGroups.map((hg) => (
        <tr
          key={hg.id}
          className="h-10 border-b border-[var(--color-border)] bg-[var(--color-background)]"
        >
          {hg.headers.map((header) => {
            const col = columns.find((c) => c.id === header.column.id);
            const width = (col?.size ?? 0) || undefined;
            const sticky = stickyStyle(header.column.id);
            const style: CSSProperties = {
              ...(width ? { width, minWidth: width } : {}),
              ...(sticky ?? {}),
              ...(sticky
                ? { background: "var(--color-background)", zIndex: 11 }
                : {}),
            };
            const numeric =
              header.column.id === "original" ||
              header.column.id === "current" ||
              header.column.id === "saved";
            return (
              <th
                key={header.id}
                scope="col"
                style={style}
                className={cn(
                  "px-3 text-left align-middle text-xs font-medium uppercase tracking-[0.02em] text-[var(--color-muted-foreground)]",
                  header.column.id === "select" && "pl-4",
                  numeric && "text-right",
                )}
              >
                {header.isPlaceholder
                  ? null
                  : flexRender(
                      header.column.columnDef.header,
                      header.getContext(),
                    )}
              </th>
            );
          })}
        </tr>
      ))}
    </>
  );
}

function BodyCells({ row }: { row: Row<MediaAsset> }) {
  const cells = row.getVisibleCells();
  const isSelected = row.getIsSelected?.() ?? false;
  void isSelected;
  return (
    <>
      {cells.map((cell) => {
        const sticky = stickyStyle(cell.column.id);
        const numeric =
          cell.column.id === "original" ||
          cell.column.id === "current" ||
          cell.column.id === "saved";
        const style: CSSProperties = {
          ...(sticky ?? {}),
          // Sticky cells need an opaque background to occlude scrolled-under
          // content. Use card bg so it matches the row.
          ...(sticky ? { background: "var(--color-card)" } : {}),
        };
        return (
          <td
            key={cell.id}
            style={style}
            className={cn(
              "border-b border-[var(--color-border)] px-3 align-middle text-sm text-[var(--color-foreground)]",
              cell.column.id === "select" && "pl-4",
              numeric && "text-right",
            )}
          >
            {flexRender(cell.column.columnDef.cell, cell.getContext())}
          </td>
        );
      })}
    </>
  );
}
