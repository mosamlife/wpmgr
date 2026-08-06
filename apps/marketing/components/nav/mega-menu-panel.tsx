// Presentational mega-menu panel body. Props-driven, no state. Renders inside
// the portaled fixed overlay managed by MegaMenu.

import { cn } from "@/lib/utils";
import { Icon } from "@/components/ui/icon";
import { Container } from "@/components/ui/primitives";
import type {
  PanelId,
  FeaturesColumn,
  SolutionsColumn,
  NavRow,
} from "./nav-data";

// ---------------------------------------------------------------------------
// Shared row cell
//
// `compact` (used by the dense 5-column Features panel) shows icon + title
// only, so titles never collide with a wrapping sentence. The wider 2-column
// Solutions panel keeps a one to two line summary.
// ---------------------------------------------------------------------------

function NavRowCell({
  row,
  onClick,
  compact = false,
}: {
  row: NavRow;
  onClick?: () => void;
  compact?: boolean;
}) {
  return (
    <a
      href={row.href}
      onClick={onClick}
      className={cn(
        "group flex items-center gap-3 rounded-[var(--radius)] px-3 py-2",
        "transition-colors duration-[var(--duration-fast)]",
        "hover:bg-[var(--accent)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)]",
        compact ? "items-center" : "items-start",
      )}
    >
      <span
        className={cn(
          "flex shrink-0 items-center justify-center rounded-[var(--radius)] bg-[var(--primary-subtle)]",
          compact ? "h-7 w-7" : "mt-0.5 h-8 w-8",
        )}
        aria-hidden
      >
        <Icon
          name={row.icon}
          size={compact ? 15 : 16}
          className="text-[var(--primary)]"
          strokeWidth={1.75}
        />
      </span>
      <span className="min-w-0 flex-1">
        <span className="block text-sm font-medium leading-snug text-foreground group-hover:text-[var(--primary)]">
          {row.title}
        </span>
        {!compact && row.summary ? (
          <span className="mt-0.5 block line-clamp-2 text-xs leading-snug text-[var(--muted-foreground)]">
            {row.summary}
          </span>
        ) : null}
      </span>
    </a>
  );
}

// ---------------------------------------------------------------------------
// Features panel: 5 cluster columns (title-only) + a featured callout card
// ---------------------------------------------------------------------------

function FeaturesPanel({
  columns,
  onClose,
}: {
  columns: FeaturesColumn[];
  onClose: () => void;
}) {
  return (
    <div className="mx-auto w-full max-w-7xl px-6 py-7 lg:px-10">
      {/* Cluster columns */}
      <div className="grid grid-cols-5 gap-x-5 gap-y-6">
        {columns.map((col) => (
          <div key={col.id} className="min-w-0">
            {/* Column header */}
            <div className="mb-2 flex items-center gap-2 px-3">
              <Icon
                name={col.icon}
                size={13}
                className="shrink-0 text-[var(--primary)]"
                strokeWidth={2}
              />
              <span className="text-2xs font-semibold uppercase tracking-[0.12em] text-[var(--muted-foreground)]">
                {col.name}
              </span>
            </div>
            {/* Rows (title only) */}
            <div className="flex flex-col gap-0.5">
              {col.rows.map((row) => (
                <NavRowCell key={row.href} row={row} onClick={onClose} compact />
              ))}
            </div>
          </div>
        ))}
      </div>

      {/* Footer link */}
      <div className="mt-6 border-t border-[var(--border)] pt-3.5">
        <a
          href="/features"
          onClick={onClose}
          className={cn(
            "inline-flex items-center gap-1.5 text-sm font-medium text-[var(--primary)]",
            "transition-colors duration-[var(--duration-fast)] hover:text-[var(--primary-hover)]",
            "focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)] rounded-sm",
          )}
        >
          View all features
          <Icon name="ArrowRight" size={14} strokeWidth={2} />
        </a>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Solutions panel: 2 wide columns with a short summary per row
// ---------------------------------------------------------------------------

function SolutionsPanel({
  columns,
  onClose,
}: {
  columns: SolutionsColumn[];
  onClose: () => void;
}) {
  return (
    <Container className="py-6">
      <div className="mx-auto grid max-w-4xl grid-cols-2 gap-x-10 gap-y-1">
        {columns.map((col) => (
          <div key={col.label}>
            {/* Column header */}
            <p className="mb-2 px-3 text-2xs font-semibold uppercase tracking-[0.12em] text-[var(--muted-foreground)]">
              {col.label}
            </p>
            <div className="flex flex-col gap-0.5">
              {col.rows.map((row) => (
                <NavRowCell key={row.href} row={row} onClick={onClose} />
              ))}
            </div>
          </div>
        ))}
      </div>

      {/* Footer link */}
      <div className="mx-auto mt-5 max-w-4xl border-t border-[var(--border)] pt-3">
        <a
          href="/solutions"
          onClick={onClose}
          className={cn(
            "inline-flex items-center gap-1.5 text-sm font-medium text-[var(--primary)]",
            "transition-colors duration-[var(--duration-fast)] hover:text-[var(--primary-hover)]",
            "focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--ring)] rounded-sm",
          )}
        >
          View all solutions
          <Icon name="ArrowRight" size={14} strokeWidth={2} />
        </a>
      </div>
    </Container>
  );
}

// ---------------------------------------------------------------------------
// Public component: dispatches to the right panel body
// ---------------------------------------------------------------------------

export type MegaMenuPanelProps = {
  panelId: PanelId;
  featuresColumns: FeaturesColumn[];
  solutionsColumns: SolutionsColumn[];
  onClose: () => void;
};

export function MegaMenuPanel({
  panelId,
  featuresColumns,
  solutionsColumns,
  onClose,
}: MegaMenuPanelProps) {
  if (panelId === "features") {
    return <FeaturesPanel columns={featuresColumns} onClose={onClose} />;
  }
  return <SolutionsPanel columns={solutionsColumns} onClose={onClose} />;
}
