import type { SiteComponent } from "@wpmgr/api";

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Badge } from "@/components/ui/badge";

type ComponentType = "plugin" | "theme";

interface Row extends SiteComponent {
  type: ComponentType;
}

/** Combined table of installed plugins and themes reported by the agent. */
export function SiteComponentsTable({
  plugins = [],
  themes = [],
}: {
  plugins?: SiteComponent[];
  themes?: SiteComponent[];
}) {
  const rows: Row[] = [
    ...plugins.map((c) => ({ ...c, type: "plugin" as const })),
    ...themes.map((c) => ({ ...c, type: "theme" as const })),
  ];

  if (rows.length === 0) {
    return (
      <p className="text-sm text-[var(--color-muted-foreground)]">
        No components reported yet. They appear after the agent syncs.
      </p>
    );
  }

  return (
    <div className="rounded-xl border border-[var(--color-border)]">
      <Table>
        <caption className="sr-only">Installed plugins and themes</caption>
        <TableHeader>
          <TableRow>
            <TableHead>Name</TableHead>
            <TableHead>Type</TableHead>
            <TableHead>Version</TableHead>
            <TableHead>Active</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((row) => (
            <TableRow key={`${row.type}:${row.slug}`}>
              <TableCell className="font-medium">
                {row.name ?? row.slug}
              </TableCell>
              <TableCell className="capitalize">{row.type}</TableCell>
              <TableCell className="text-[var(--color-muted-foreground)]">
                {row.version ?? "—"}
              </TableCell>
              <TableCell>
                {row.active ? (
                  <Badge variant="success">Active</Badge>
                ) : (
                  <Badge variant="muted">Inactive</Badge>
                )}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}
