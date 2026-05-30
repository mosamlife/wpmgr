import type { SiteComponent } from "@wpmgr/api";

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { StatusChip } from "@/components/status";

type ComponentType = "plugin" | "theme";

interface Row extends SiteComponent {
  type: ComponentType;
}

// A component belongs in this table iff it has no outstanding update.
// `available_update == null` catches both null (explicitly checked, no update)
// and undefined (field absent from wire, i.e. not yet checked).
function hasNoUpdate(c: SiteComponent): boolean {
  return c.available_update == null;
}

/**
 * Combined table of installed plugins and themes reported by the agent —
 * EXCLUDING anything with an outstanding update. Those rows are surfaced by
 * `AvailableUpdatesCard` so the user has one obvious place to act on them;
 * this card is the long-tail "everything else that's already up to date".
 *
 * Layout mirrors the activity-row density: name is the visual lead; slug in
 * font-mono beneath it for uniqueness; version + status as structured columns.
 * Tokens only — no off-token palette colors; StatusChip for active state.
 */
export function SiteComponentsTable({
  plugins = [],
  themes = [],
}: {
  plugins?: SiteComponent[];
  themes?: SiteComponent[];
}) {
  const rows: Row[] = [
    ...plugins
      .filter(hasNoUpdate)
      .map((c) => ({ ...c, type: "plugin" as const })),
    ...themes
      .filter(hasNoUpdate)
      .map((c) => ({ ...c, type: "theme" as const })),
  ];

  if (rows.length === 0) {
    return (
      <p className="text-sm text-muted-foreground">
        No components reported yet. They appear after the agent syncs.
      </p>
    );
  }

  return (
    <div className="overflow-hidden rounded-xl border border-border">
      <div className="w-full overflow-x-auto">
        <Table className="min-w-[400px]">
          <caption className="sr-only">Installed plugins and themes</caption>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Type</TableHead>
              <TableHead>Version</TableHead>
              <TableHead>Status</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
          {rows.map((row) => (
            <TableRow key={`${row.type}:${row.slug}`}>
              {/* Name + slug: name as the visual lead, slug in mono beneath */}
              <TableCell>
                <span className="flex flex-col gap-0.5">
                  <span className="font-medium text-foreground">
                    {row.name ?? row.slug}
                  </span>
                  {row.name ? (
                    <span
                      className="font-mono text-[11px] text-muted-foreground"
                      title={row.slug}
                    >
                      {row.slug}
                    </span>
                  ) : null}
                </span>
              </TableCell>

              {/* Type — capitalize */}
              <TableCell>
                <span className="font-mono text-xs capitalize text-muted-foreground">
                  {row.type}
                </span>
              </TableCell>

              {/* Version — mono + tabular-nums */}
              <TableCell className="font-mono tabular-nums text-xs text-muted-foreground">
                {row.version ?? (
                  <span aria-hidden="true">{"–"}</span>
                )}
              </TableCell>

              {/* Active status — StatusChip (dot + label), never bare Badge */}
              <TableCell>
                {row.active ? (
                  <StatusChip tone="success" label="Active" />
                ) : (
                  <StatusChip tone="muted" label="Inactive" />
                )}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
      </div>
    </div>
  );
}
