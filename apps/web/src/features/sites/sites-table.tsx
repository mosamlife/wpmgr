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
import { relativeTime } from "@/lib/utils";
import { HealthBadge, EnrollmentBadge } from "@/features/sites/site-badges";

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
