import {
  forwardRef,
  useMemo,
  type ReactNode,
} from "react";
import {
  ChevronDown,
  Download,
  ExternalLink,
  MoreHorizontal,
  RefreshCw,
  RotateCcw,
  Rows2,
  Rows3,
  Rows4,
  Search,
  Tag,
  Users,
  X,
} from "lucide-react";
import { AnimatePresence, motion, MotionConfig } from "motion/react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { cn } from "@/lib/utils";
import { dur, ease } from "@/lib/motion-presets";
import type { SitesDensity } from "@/features/sites/use-sites-density";
import type { SitesSelection } from "@/features/sites/use-sites-selection";

// Surface 4.6 — the Sites toolbar.
//
// Two modes that transform into each other via a FLIP layout animation:
//
//   IDLE   [Search  ⌘K] [Client▾] [Status▾] [Tags▾]      [Density] [Add site]
//   ACTION [N sites selected · Clear] [Update plugins (N)▾] [Run backup]
//          [Restore...] [Open in wp-admin (N)] [More▾]
//
// The two modes are conditional children of a single <motion.div layout> parent;
// `layout` measures the bounds before and after the mode swap and runs the
// resulting transform on the GPU (translate + scale). No width / height / left
// / top is animated — strictly transform + opacity, per DESIGN.md.
//
// Why one parent instead of two stacked panels: the parent's bounds change as
// it re-flows around the children. With `layout`, motion turns that re-flow
// into a single 240ms outQuart transform.
//
// Reduced motion is honoured globally via <MotionConfig reducedMotion="user">
// which disables the layout transform but keeps the cross-fade.

// Phase 5: timing tokens imported from @/lib/motion-presets so the toolbar
// rides the same easing/duration tiers as the rest of the app (dialog, drawer,
// save bar). The cross-fade between idle/action modes stays short (80ms,
// linear) — it intentionally undercuts the layout transform so the bounds
// re-flow reads as the primary motion and the swap reads as instant.
const TOOLBAR_MOTION = {
  layout: { duration: dur.base, ease: ease.out },
  fade: { duration: 0.08, ease: "linear" as const },
};

// ---------------------------------------------------------------------------
// Public surface
// ---------------------------------------------------------------------------

export interface SitesToolbarProps {
  /** Selection state, lifted from the route so both toolbar & table read the same set. */
  selection: SitesSelection;
  /** Density tuple, lifted so the toolbar (idle mode) can drive it. */
  densityState: [SitesDensity, (next: SitesDensity) => void];

  // ---- Idle-mode filter wiring (basic — real wiring is Sprint 4) -----------
  /** Controlled search input value. */
  search: string;
  onSearchChange: (next: string) => void;
  /** Available client/tag values, used to populate the Client + Tag dropdowns. */
  clientOptions?: readonly string[];
  tagOptions?: readonly string[];

  // ---- Permission gates ---------------------------------------------------
  /** Operator+ may see Add Site / bulk actions. */
  canOperate: boolean;

  // ---- Action-mode handlers (Sprint 3 stubs; wired progressively) ---------
  /** Update plugins/themes/core for the selection. Opens the UpdateWizard. */
  onBulkUpdate: (kind: "plugins" | "themes" | "core") => void;
  /** Run a backup across the selection. Wires to the bulk-backup endpoint. */
  onBulkBackup: () => void;
  /** Restore from backup — Sprint 4 follow-up (no fleet-wide flow yet). */
  onBulkRestore: () => void;
  /** Open every selected site's wp-admin in a new tab (auto-login). */
  onBulkOpenWpAdmin: () => void;
  /** Tag / re-tag the selection. Sprint 4 wiring. */
  onBulkTag: () => void;
  /** Re-assign the selection to a client. Sprint 4 wiring. */
  onBulkSetClient: () => void;
  /** Pause monitoring for the selection. Sprint 4 wiring. */
  onBulkPauseMonitoring: () => void;
  /** Delete the selection. Sprint 4 wiring; requires confirm-by-typing. */
  onBulkDelete: () => void;

  // ---- Idle-mode primary action ------------------------------------------
  /** Render-prop slot for the "Add site" affordance — keeps the dialog wiring in the route. */
  addSiteSlot?: ReactNode;
}

export function SitesToolbar(props: SitesToolbarProps) {
  const { selection } = props;
  const inAction = selection.count > 0;

  return (
    <MotionConfig reducedMotion="user">
      <motion.div
        layout
        transition={TOOLBAR_MOTION.layout}
        role="toolbar"
        aria-label={inAction ? "Bulk actions" : "Filter sites"}
        className={cn(
          "flex flex-wrap items-center justify-between gap-3 border-b border-border bg-background px-1 py-2",
          // Min-height keeps the row stable so the surrounding layout doesn't
          // jitter when the mode flips (transform/opacity-only — see DESIGN).
          "min-h-11",
        )}
        data-mode={inAction ? "action" : "idle"}
      >
        <AnimatePresence mode="wait" initial={false}>
          {inAction ? (
            <ActionMode key="action" {...props} />
          ) : (
            <IdleMode key="idle" {...props} />
          )}
        </AnimatePresence>
      </motion.div>
    </MotionConfig>
  );
}

// ---------------------------------------------------------------------------
// IDLE mode — filter chips + density + add site
// ---------------------------------------------------------------------------

function IdleMode({
  search,
  onSearchChange,
  clientOptions = [],
  tagOptions = [],
  densityState,
  addSiteSlot,
}: SitesToolbarProps) {
  const [density, onDensityChange] = densityState;
  return (
    <motion.div
      layout="position"
      initial={{ opacity: 0 }}
      animate={{ opacity: 1 }}
      exit={{ opacity: 0 }}
      transition={TOOLBAR_MOTION.fade}
      className="flex w-full flex-wrap items-center justify-between gap-2"
    >
      <div className="flex flex-wrap items-center gap-2">
        <SearchInput value={search} onChange={onSearchChange} />
        <FilterDropdown
          label="All clients"
          options={clientOptions}
          icon={Users}
          ariaLabel="Filter by client"
          filterKind="client"
        />
        <FilterDropdown
          label="Status: any"
          options={["Up", "Down", "Pending", "Disabled"]}
          ariaLabel="Filter by status"
          filterKind="status"
        />
        <FilterDropdown
          label="Tags"
          options={tagOptions}
          icon={Tag}
          ariaLabel="Filter by tag"
          filterKind="tag"
        />
      </div>

      <div className="flex items-center gap-2">
        <DensityToggle density={density} onChange={onDensityChange} />
        {addSiteSlot}
      </div>
    </motion.div>
  );
}

const SearchInput = forwardRef<
  HTMLInputElement,
  { value: string; onChange: (next: string) => void }
>(function SearchInput({ value, onChange }, ref) {
  return (
    <div className="relative">
      <Search
        aria-hidden="true"
        className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground"
      />
      <Input
        ref={ref}
        type="search"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        aria-label="Search sites"
        placeholder="Search sites"
        className="h-9 w-40 rounded-md pl-8 pr-12 sm:w-64"
      />
      <kbd
        aria-hidden="true"
        className="pointer-events-none absolute right-2 top-1/2 -translate-y-1/2 rounded border border-border bg-muted px-1.5 py-0.5 font-mono text-[10px] font-medium text-muted-foreground"
      >
        {"⌘K"}
      </kbd>
    </div>
  );
});

type FilterKind = "client" | "status" | "tag";

function FilterDropdown({
  label,
  options,
  ariaLabel,
  icon: Icon,
  filterKind,
}: {
  label: string;
  options: readonly string[];
  ariaLabel: string;
  icon?: typeof Tag;
  filterKind: FilterKind;
}) {
  // Sprint 3 keeps the dropdown UI live but defers real filter wiring to
  // Sprint 4 (touching useSites() is out of this scope). We surface the
  // intent via console.debug so changes are observable in dev.
  const handleSelect = (value: string | null) => {
     
    console.debug("[sites-toolbar] filter change", {
      kind: filterKind,
      value,
    });
  };

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          variant="outline"
          size="sm"
          aria-label={ariaLabel}
          className="h-9 gap-1.5"
        >
          {Icon ? <Icon aria-hidden="true" className="size-3.5" /> : null}
          {label}
          <ChevronDown aria-hidden="true" className="size-3" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="min-w-[14rem]">
        <DropdownMenuItem onSelect={() => handleSelect(null)}>
          Show all
        </DropdownMenuItem>
        {options.length > 0 ? <DropdownMenuSeparator /> : null}
        {options.length === 0 ? (
          <DropdownMenuItem disabled>No options yet</DropdownMenuItem>
        ) : (
          options.slice(0, 24).map((opt) => (
            <DropdownMenuItem
              key={opt}
              onSelect={() => handleSelect(opt)}
              className="font-mono text-xs"
            >
              {opt}
            </DropdownMenuItem>
          ))
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

function DensityToggle({
  density,
  onChange,
}: {
  density: SitesDensity;
  onChange: (next: SitesDensity) => void;
}) {
  return (
    <div
      role="group"
      aria-label="Row density"
      className="inline-flex items-center rounded-md border border-border bg-background p-0.5"
    >
      <DensityButton
        active={density === "comfortable"}
        onClick={() => onChange("comfortable")}
        title="Set comfortable density"
        label="Set comfortable density"
      >
        <Rows3 aria-hidden="true" className="size-4" />
      </DensityButton>
      <DensityButton
        active={density === "compact"}
        onClick={() => onChange("compact")}
        title="Set compact density"
        label="Set compact density"
      >
        <Rows2 aria-hidden="true" className="size-4" />
      </DensityButton>
      <DensityButton
        active={density === "dense"}
        onClick={() => onChange("dense")}
        title="Set dense density"
        label="Set dense density"
      >
        <Rows4 aria-hidden="true" className="size-4" />
      </DensityButton>
    </div>
  );
}

function DensityButton({
  active,
  onClick,
  title,
  label,
  children,
}: {
  active: boolean;
  onClick: () => void;
  title: string;
  label: string;
  children: ReactNode;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      title={title}
      aria-label={label}
      aria-pressed={active}
      className={cn(
        "inline-flex size-7 items-center justify-center rounded-sm text-muted-foreground transition-colors",
        "hover:text-foreground",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
        active && "bg-accent text-accent-foreground",
      )}
    >
      {children}
    </button>
  );
}

// ---------------------------------------------------------------------------
// ACTION mode — selection counter + bulk action buttons
// ---------------------------------------------------------------------------

function ActionMode({
  selection,
  canOperate,
  onBulkUpdate,
  onBulkBackup,
  onBulkRestore,
  onBulkOpenWpAdmin,
  onBulkTag,
  onBulkSetClient,
  onBulkPauseMonitoring,
  onBulkDelete,
}: SitesToolbarProps) {
  const count = selection.count;
  const sitesNoun = useMemo(() => (count === 1 ? "site" : "sites"), [count]);

  return (
    <motion.div
      layout="position"
      initial={{ opacity: 0 }}
      animate={{ opacity: 1 }}
      exit={{ opacity: 0 }}
      transition={TOOLBAR_MOTION.fade}
      className="flex w-full flex-wrap items-center justify-between gap-2"
    >
      <div className="flex flex-wrap items-center gap-3">
        <span
          aria-live="polite"
          className="flex flex-wrap items-center gap-2 text-sm text-foreground"
        >
          <span className="font-mono font-medium tabular-nums">{count}</span>
          <span className="text-muted-foreground">{sitesNoun} selected</span>
          <span aria-hidden="true" className="text-muted-foreground">
            ·
          </span>
          <button
            type="button"
            onClick={() => selection.clear()}
            aria-label="Clear selection"
            className="inline-flex items-center gap-1 rounded text-sm font-medium text-primary underline-offset-4 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
          >
            <X aria-hidden="true" className="size-3.5" />
            Clear selection
          </button>
        </span>
      </div>

      <div className="flex flex-wrap items-center gap-2">
        {canOperate ? (
          <>
            <UpdateSplitButton count={count} onSelect={onBulkUpdate} />
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={onBulkBackup}
              aria-label={`Run backup on ${count} ${sitesNoun}`}
              className="h-9 gap-1.5"
            >
              <Download aria-hidden="true" className="size-3.5" />
              Run backup
            </Button>
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={onBulkRestore}
              aria-label={`Restore from backup on ${count} ${sitesNoun}`}
              className="h-9 gap-1.5"
            >
              <RotateCcw aria-hidden="true" className="size-3.5" />
              Restore...
            </Button>
          </>
        ) : null}

        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={onBulkOpenWpAdmin}
          aria-label={`Open ${count} ${sitesNoun} in wp-admin`}
          className="h-9 gap-1.5"
        >
          <ExternalLink aria-hidden="true" className="size-3.5" />
          Open in wp-admin ({count})
        </Button>

        {canOperate ? (
          <MoreActions
            onBulkTag={onBulkTag}
            onBulkSetClient={onBulkSetClient}
            onBulkPauseMonitoring={onBulkPauseMonitoring}
            onBulkDelete={onBulkDelete}
            count={count}
            sitesNoun={sitesNoun}
          />
        ) : null}
      </div>
    </motion.div>
  );
}

function UpdateSplitButton({
  count,
  onSelect,
}: {
  count: number;
  onSelect: (kind: "plugins" | "themes" | "core") => void;
}) {
  return (
    <div className="inline-flex items-stretch overflow-hidden rounded-md">
      <Button
        type="button"
        size="sm"
        onClick={() => onSelect("plugins")}
        aria-label={`Update plugins on ${count} ${count === 1 ? "site" : "sites"}`}
        className="h-9 gap-1.5 rounded-r-none"
      >
        <RefreshCw aria-hidden="true" className="size-3.5" />
        Update plugins ({count})
      </Button>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            type="button"
            size="sm"
            aria-label="Choose update target"
            className="h-9 rounded-l-none border-l border-primary-foreground/20 px-2"
          >
            <ChevronDown aria-hidden="true" className="size-3.5" />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" className="min-w-[14rem]">
          <DropdownMenuLabel>Update target</DropdownMenuLabel>
          <DropdownMenuItem onSelect={() => onSelect("plugins")}>
            Update plugins
          </DropdownMenuItem>
          <DropdownMenuItem onSelect={() => onSelect("themes")}>
            Update themes
          </DropdownMenuItem>
          <DropdownMenuItem onSelect={() => onSelect("core")}>
            Update WordPress core
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    </div>
  );
}

function MoreActions({
  onBulkTag,
  onBulkSetClient,
  onBulkPauseMonitoring,
  onBulkDelete,
  count,
  sitesNoun,
}: {
  onBulkTag: () => void;
  onBulkSetClient: () => void;
  onBulkPauseMonitoring: () => void;
  onBulkDelete: () => void;
  count: number;
  sitesNoun: string;
}) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          type="button"
          variant="outline"
          size="sm"
          aria-label="More bulk actions"
          className="h-9 gap-1"
        >
          <MoreHorizontal aria-hidden="true" className="size-3.5" />
          More
          <ChevronDown aria-hidden="true" className="size-3" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="min-w-[14rem]">
        <DropdownMenuItem onSelect={onBulkTag}>
          Tag {count} {sitesNoun}
        </DropdownMenuItem>
        <DropdownMenuItem onSelect={onBulkSetClient}>
          Set client on {count} {sitesNoun}
        </DropdownMenuItem>
        <DropdownMenuItem onSelect={onBulkPauseMonitoring}>
          Pause monitoring on {count} {sitesNoun}
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem
          onSelect={onBulkDelete}
          className="text-destructive focus:bg-destructive/10 focus:text-destructive"
        >
          Delete {count} {sitesNoun}...
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
