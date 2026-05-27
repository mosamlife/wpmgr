import { useMemo } from "react";
import {
  flexRender,
  getCoreRowModel,
  useReactTable,
  type ColumnDef,
} from "@tanstack/react-table";
import { Link } from "@tanstack/react-router";
import type { Site } from "@wpmgr/api";

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Badge } from "@/components/ui/badge";
import { Checkbox } from "@/components/ui/checkbox";
import { relativeTime } from "@/lib/utils";
import { HealthBadge, EnrollmentBadge } from "@/features/sites/site-badges";

const baseColumns: ColumnDef<Site>[] = [
  {
    accessorKey: "name",
    header: "Name",
    cell: ({ row }) => (
      <Link
        to="/sites/$siteId"
        params={{ siteId: row.original.id }}
        className="font-medium underline-offset-4 hover:underline"
      >
        {row.original.name}
      </Link>
    ),
  },
  {
    accessorKey: "url",
    header: "URL",
    cell: ({ row }) => (
      <span className="text-[var(--color-muted-foreground)]">
        {row.original.url}
      </span>
    ),
  },
  {
    id: "enrollment",
    header: "Enrollment",
    cell: ({ row }) => <EnrollmentBadge site={row.original} />,
  },
  {
    accessorKey: "health_status",
    header: "Health",
    cell: ({ row }) => <HealthBadge status={row.original.health_status} />,
  },
  {
    id: "last_seen",
    header: "Last seen",
    cell: ({ row }) => {
      const rel = relativeTime(row.original.last_seen_at);
      return rel ? (
        <time
          dateTime={row.original.last_seen_at}
          className="text-[var(--color-muted-foreground)]"
        >
          {rel}
        </time>
      ) : (
        <span className="text-[var(--color-muted-foreground)]">—</span>
      );
    },
  },
  {
    id: "tags",
    header: "Tags",
    cell: ({ row }) =>
      row.original.tags.length > 0 ? (
        <div className="flex flex-wrap gap-1">
          {row.original.tags.map((tag) => (
            <Badge key={tag} variant="outline">
              {tag}
            </Badge>
          ))}
        </div>
      ) : (
        <span className="text-[var(--color-muted-foreground)]">—</span>
      ),
  },
];

export interface SitesTableSelection {
  selected: Set<string>;
  onToggle: (siteId: string) => void;
  onToggleAll: (siteIds: string[], select: boolean) => void;
}

export function SitesTable({
  sites,
  selection,
}: {
  sites: Site[];
  // When provided, a leading checkbox column enables multi-select for the bulk
  // update wizard.
  selection?: SitesTableSelection;
}) {
  const columns = useMemo<ColumnDef<Site>[]>(() => {
    if (!selection) return baseColumns;
    const allIds = sites.map((s) => s.id);
    const allSelected =
      allIds.length > 0 && allIds.every((id) => selection.selected.has(id));
    const selectColumn: ColumnDef<Site> = {
      id: "select",
      header: () => (
        <Checkbox
          aria-label="Select all sites"
          checked={allSelected}
          onChange={(e) => selection.onToggleAll(allIds, e.target.checked)}
        />
      ),
      cell: ({ row }) => (
        <Checkbox
          aria-label={`Select ${row.original.name}`}
          checked={selection.selected.has(row.original.id)}
          onChange={() => selection.onToggle(row.original.id)}
        />
      ),
    };
    return [selectColumn, ...baseColumns];
  }, [selection, sites]);

  const table = useReactTable({
    data: sites,
    columns,
    getCoreRowModel: getCoreRowModel(),
  });

  return (
    <div className="rounded-xl border border-[var(--color-border)]">
      <Table>
        <caption className="sr-only">List of WordPress sites</caption>
        <TableHeader>
          {table.getHeaderGroups().map((headerGroup) => (
            <TableRow key={headerGroup.id}>
              {headerGroup.headers.map((header) => (
                <TableHead key={header.id}>
                  {header.isPlaceholder
                    ? null
                    : flexRender(
                        header.column.columnDef.header,
                        header.getContext(),
                      )}
                </TableHead>
              ))}
            </TableRow>
          ))}
        </TableHeader>
        <TableBody>
          {table.getRowModel().rows.map((row) => (
            <TableRow
              key={row.id}
              data-state={
                selection?.selected.has(row.original.id) ? "selected" : undefined
              }
            >
              {row.getVisibleCells().map((cell) => (
                <TableCell key={cell.id}>
                  {flexRender(cell.column.columnDef.cell, cell.getContext())}
                </TableCell>
              ))}
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}
