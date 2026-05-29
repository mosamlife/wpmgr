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

// Forward-compatible shape: Track B's OpenAPI regen adds `available_update` to
// SiteComponent. Until the generated types catch up, treat the field as an
// optional opaque marker — its presence (not its value) is what we filter on.
type ComponentWithMaybeUpdate = SiteComponent & {
  available_update?: unknown;
};

interface Row extends SiteComponent {
  type: ComponentType;
}

/**
 * Combined table of installed plugins and themes reported by the agent —
 * EXCLUDING anything with an outstanding update. Those rows are surfaced by
 * `AvailableUpdatesCard` so the user has one obvious place to act on them;
 * this card is the long-tail "everything else that's already up to date".
 */
export function SiteComponentsTable({
  plugins = [],
  themes = [],
}: {
  plugins?: SiteComponent[];
  themes?: SiteComponent[];
}) {
  // A component is "up to date" (and so belongs in this table) iff it has no
  // `available_update` field on the wire. Track B will set it to null when
  // explicitly checked, so null/undefined both mean "no update available".
  const hasNoUpdate = (c: SiteComponent): boolean => {
    const ext = c as ComponentWithMaybeUpdate;
    return ext.available_update == null;
  };

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

/** Count of plugins+themes that have NO outstanding update. */
export function countUpToDate(
  plugins: SiteComponent[] = [],
  themes: SiteComponent[] = [],
): number {
  const hasNoUpdate = (c: SiteComponent): boolean => {
    const ext = c as ComponentWithMaybeUpdate;
    return ext.available_update == null;
  };
  return plugins.filter(hasNoUpdate).length + themes.filter(hasNoUpdate).length;
}
