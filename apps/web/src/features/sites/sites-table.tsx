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

const statusLabels: Record<Site["status"], string> = {
  pending: "Pending",
  active: "Active",
  error: "Error",
  disabled: "Disabled",
};

const columns: ColumnDef<Site>[] = [
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
    accessorKey: "status",
    header: "Status",
    cell: ({ row }) => statusLabels[row.original.status],
  },
  { accessorKey: "wp_version", header: "WP" },
  { accessorKey: "php_version", header: "PHP" },
];

export function SitesTable({ sites }: { sites: Site[] }) {
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
            <TableRow key={row.id}>
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
