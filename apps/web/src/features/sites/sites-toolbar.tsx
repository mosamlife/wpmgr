import {
  forwardRef,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { Link } from "@tanstack/react-router";
import {
  ChevronDown,
  Download,
  ExternalLink,
  LayoutGrid,
  List,
  MoreHorizontal,
  RefreshCw,
  RotateCcw,
  Rows2,
  Rows3,
  Rows4,
  Search,
  Square,
  Tag,
  Users,
  X,
} from "lucide-react";
import { AnimatePresence, motion, MotionConfig } from "motion/react";
import type { SiteTag } from "@wpmgr/api";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { SegmentedControl } from "@/components/ui/segmented-control";
import {
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { TagChip } from "@/features/sites/tag-chip";
import { resolveTagDot } from "@/lib/tag-color";
import { cn } from "@/lib/utils";
import { dur, ease } from "@/lib/motion-presets";
import type { SitesDensity } from "@/features/sites/use-sites-density";
import type { SitesSelection } from "@/features/sites/use-sites-selection";
import type { SitesView, CardSize } from "@/features/sites/use-sites-view";
import type { TagMatchMode } from "@/features/sites/use-sites";

// Surface 4.6 — the Sites toolbar.
//
// Two modes that transform into each other via a FLIP layout animation:
//
//   IDLE   [List|Grid] [Search  ⌘K] [Client▾] [Status▾] [Tags▾]  [Density|CardSize] [Add site]
//   ACTION [N sites selected · Clear] [Update plugins (N)▾] [Run backup]
//          [Restore...] [Open in wp-admin (N)] [More▾]
//
// P1: Status and Tags are now controlled MULTI-SELECT dropdowns (Radix
// DropdownMenuCheckboxItem) with an applied-count badge, in-menu text filter,
// and max-h scroll for long option lists. The menu stays open across toggles
// via e.preventDefault() on item select.
//
// P2: A [List | Grid] icon toggle sits leftmost in the right-hand cluster. In
// grid view the DensityToggle swaps for a CardSizeToggle. A "Select all (N)"
// inline control is shown only in grid view (the table keeps its header checkbox).

const TOOLBAR_MOTION = {
  layout: { duration: dur.base, ease: ease.out },
  fade: { duration: 0.08, ease: "linear" as const },
};

// ---------------------------------------------------------------------------
// Public surface
// ---------------------------------------------------------------------------

export interface ClientOption {
  id: string;
  name: string;
}

export interface SitesToolbarProps {
  /** Selection state, lifted from the route so both toolbar & table read the same set. */
  selection: SitesSelection;
  /** Density tuple, lifted so the toolbar (idle mode) can drive it. */
  densityState: [SitesDensity, (next: SitesDensity) => void];

  // ---- Idle-mode filter wiring -----------------------------------------------
  /** Controlled search input value. */
  search: string;
  onSearchChange: (next: string) => void;
  /**
   * Client options for the Client filter dropdown (id+name pairs from useClients).
   * When provided with onClientFilterChange, the dropdown shows real client options.
   */
  clientOptions?: readonly ClientOption[];
  /** Currently-applied client filter id (null = all). */
  appliedClientId?: string | null;
  /** Called when the client filter selection changes. */
  onClientFilterChange?: (clientId: string | null) => void;
  /** The tenant's tag registry (color + usage_count), used to populate the
   *  Tags filter dropdown. */
  tagRegistry?: readonly SiteTag[];
  /** Currently selected tags (controlled multi-select, by name). */
  selectedTags?: readonly string[];
  /** Called when a tag is toggled. */
  onTagToggle?: (tag: string) => void;
  /** Called to clear all tag filters. */
  onTagsClear?: () => void;
  /** "any" (OR) or "all" (AND) match semantics across the selected tags. */
  tagMode?: TagMatchMode;
  /** Called when the match-mode segmented control changes. */
  onTagModeChange?: (mode: TagMatchMode) => void;
  /** Available status values for the Status dropdown. */
  statusOptions?: readonly string[];
  /** Currently selected statuses (controlled multi-select). */
  selectedStatuses?: readonly string[];
  /** Called when a status is toggled. */
  onStatusToggle?: (status: string) => void;
  /** Called to clear all status filters. */
  onStatusesClear?: () => void;
  /** Available agent-freshness values for the Agent dropdown (agent-releases visibility). */
  agentStatusOptions?: readonly string[];
  /** Currently selected agent-freshness values (controlled multi-select). */
  selectedAgentStatuses?: readonly string[];
  /** Called when an agent-freshness value is toggled. */
  onAgentStatusToggle?: (status: string) => void;
  /** Called to clear all agent-freshness filters. */
  onAgentStatusesClear?: () => void;
  /** Total count of active filter axes (for the "Clear filters" pill). */
  activeFilterCount?: number;
  /** Called to clear ALL filters across all axes. */
  onClearAllFilters?: () => void;

  // ---- P2 view mode wiring --------------------------------------------------
  /** Current view mode (list | grid). */
  view?: SitesView;
  /** Called when the view toggle is clicked. */
  onViewChange?: (next: SitesView) => void;
  /** Current card size (comfortable | compact) — only relevant in grid view. */
  cardSize?: CardSize;
  /** Called when the card size toggle is clicked. */
  onCardSizeChange?: (next: CardSize) => void;
  /**
   * Visible site ids in the current view — used by the grid "Select all (N)"
   * control. The table keeps its own header checkbox.
   */
  visibleIds?: readonly string[];

  // ---- Permission gates ---------------------------------------------------
  /** Operator+ may see Add Site / bulk actions. */
  canOperate: boolean;

  // ---- Action-mode handlers (Sprint 3 stubs; wired progressively) ---------
  onBulkUpdate: (kind: "plugins" | "themes" | "core") => void;
  onBulkBackup: () => void;
  onBulkRestore: () => void;
  onBulkOpenWpAdmin: () => void;
  onBulkTag: () => void;
  onBulkSetClient: () => void;
  onBulkPauseMonitoring: () => void;
  onBulkDelete: () => void;
  /**
   * GH #255 Phase 2: arms the agent self-update confirmation dialog for the
   * current selection. Owner/admin only (gated by the caller via `canManage`,
   * a stricter check than `canOperate` below: this is infrastructure, not
   * content) AND only while the control-plane kill switch is on. Undefined
   * hides the menu item entirely: nothing appears usable while the channel
   * ships dark.
   */
  onUpdateAgent?: () => void;

  // ---- Idle-mode primary action ------------------------------------------
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
// IDLE mode — filter chips + view toggle + density/card-size + add site
// ---------------------------------------------------------------------------

function IdleMode({
  search,
  onSearchChange,
  clientOptions = [],
  appliedClientId,
  onClientFilterChange,
  tagRegistry = [],
  selectedTags = [],
  onTagToggle,
  onTagsClear,
  tagMode = "any",
  onTagModeChange,
  statusOptions = [],
  selectedStatuses = [],
  onStatusToggle,
  onStatusesClear,
  agentStatusOptions = [],
  selectedAgentStatuses = [],
  onAgentStatusToggle,
  onAgentStatusesClear,
  activeFilterCount = 0,
  onClearAllFilters,
  densityState,
  view = "list",
  onViewChange,
  cardSize = "comfortable",
  onCardSizeChange,
  visibleIds = [],
  selection,
  addSiteSlot,
}: SitesToolbarProps) {
  const [density, onDensityChange] = densityState;

  const activeClientLabel =
    appliedClientId && clientOptions.length > 0
      ? (clientOptions.find((c) => c.id === appliedClientId)?.name ?? "All clients")
      : "All clients";

  // GH #230 adversarial-verify MEDIUM — the active-tag chips render from
  // just a NAME (the URL's `tags` search param); look the registry color up
  // by name so these chips match the operator's chosen color, not always
  // "auto". `tagRegistry` is already threaded in as a prop (this component
  // is presentational, not a hook consumer), so a plain name -> color map
  // built from it is all that's needed.
  const tagColorMap = useMemo(() => {
    const map = new Map<string, string>();
    for (const tag of tagRegistry) map.set(tag.name, tag.color);
    return map;
  }, [tagRegistry]);

  return (
    <motion.div
      layout="position"
      initial={{ opacity: 0 }}
      animate={{ opacity: 1 }}
      exit={{ opacity: 0 }}
      transition={TOOLBAR_MOTION.fade}
      className="flex w-full flex-wrap items-center justify-between gap-2"
    >
      {/* Left cluster: search + filters */}
      <div className="flex flex-wrap items-center gap-2">
        <SearchInput value={search} onChange={onSearchChange} />

        {/* Client filter */}
        {onClientFilterChange ? (
          <ClientFilterDropdown
            label={activeClientLabel}
            options={clientOptions}
            appliedId={appliedClientId ?? null}
            onSelect={onClientFilterChange}
          />
        ) : null}

        {/* Status — controlled multi-select */}
        <MultiSelectDropdown
          label="Status"
          options={statusOptions}
          selected={selectedStatuses}
          onToggle={onStatusToggle ?? (() => {})}
          onClear={onStatusesClear ?? (() => {})}
          ariaLabel="Filter by status"
        />

        {/* Agent: freshness classification (agent-releases visibility) */}
        <MultiSelectDropdown
          label="Agent"
          options={agentStatusOptions}
          selected={selectedAgentStatuses}
          onToggle={onAgentStatusToggle ?? (() => {})}
          onClear={onAgentStatusesClear ?? (() => {})}
          ariaLabel="Filter by agent status"
        />

        {/* Tags — registry-backed multi-select with a match-mode segmented
            control and a "Manage tags" footer link. */}
        <TagsFilterDropdown
          registry={tagRegistry}
          selected={selectedTags}
          onToggle={onTagToggle ?? (() => {})}
          onClear={onTagsClear ?? (() => {})}
          mode={tagMode}
          onModeChange={onTagModeChange ?? (() => {})}
        />

        {/* Active tag chips — removable, inline next to the Tags button so
            the operator sees exactly which tags are filtering without
            opening the menu. */}
        {selectedTags.length > 0 ? (
          <div className="flex flex-wrap items-center gap-1">
            {selectedTags.map((name) => (
              <TagChip
                key={name}
                tag={{ name, color: tagColorMap.get(name) }}
                trailing={
                  <button
                    type="button"
                    onClick={() => onTagToggle?.(name)}
                    aria-label={`Remove tag filter ${name}`}
                    className="ml-0.5 rounded-full p-0.5 hover:bg-accent"
                  >
                    <X aria-hidden="true" className="size-3" />
                  </button>
                }
              />
            ))}
          </div>
        ) : null}

        {/* Clear all filters pill — shown when any filter axis is active */}
        {activeFilterCount > 0 && onClearAllFilters ? (
          <button
            type="button"
            onClick={onClearAllFilters}
            aria-label={`Clear ${activeFilterCount} active ${activeFilterCount === 1 ? "filter" : "filters"}`}
            className={cn(
              "inline-flex items-center gap-1.5 rounded-full border border-primary/50 bg-primary/5",
              "px-2.5 py-1 text-xs font-medium text-foreground",
              "transition-colors hover:bg-primary/10",
              "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
            )}
          >
            <X aria-hidden="true" className="size-3" />
            Clear filters ({activeFilterCount})
          </button>
        ) : null}
      </div>

      {/* Right cluster: view toggle + density/card-size + select-all-grid + add site */}
      <div className="flex items-center gap-2">
        {/* Grid "Select all (N)" control — only in grid view (table has header checkbox). */}
        {view === "grid" && visibleIds.length > 0 ? (
          <GridSelectAll
            visibleIds={visibleIds}
            selection={selection}
          />
        ) : null}

        {/* [List | Grid] view toggle */}
        {onViewChange ? (
          <ViewToggle view={view} onChange={onViewChange} />
        ) : null}

        {/* Density (list) or CardSize (grid) */}
        {view === "grid" && onCardSizeChange ? (
          <CardSizeToggle size={cardSize} onChange={onCardSizeChange} />
        ) : (
          <DensityToggle density={density} onChange={onDensityChange} />
        )}

        {addSiteSlot}
      </div>
    </motion.div>
  );
}

// ---------------------------------------------------------------------------
// SearchInput
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// MultiSelectDropdown — controlled multi-select with in-menu text filter
// ---------------------------------------------------------------------------

/**
 * A controlled multi-select dropdown using DropdownMenuCheckboxItem.
 * e.preventDefault() on item select keeps the menu open across toggles.
 * Applied state: border-primary/50 bg-primary/5 + inline count badge.
 * Long option lists get max-h-[60vh] overflow-y-auto + an in-menu filter.
 */
function MultiSelectDropdown({
  label,
  options,
  selected,
  onToggle,
  onClear,
  ariaLabel,
  icon: Icon,
}: {
  label: string;
  options: readonly string[];
  selected: readonly string[];
  onToggle: (value: string) => void;
  onClear: () => void;
  ariaLabel: string;
  icon?: typeof Tag;
}) {
  const [menuFilter, setMenuFilter] = useState("");
  const count = selected.length;
  const isActive = count > 0;

  const filtered = useMemo(() => {
    const q = menuFilter.trim().toLowerCase();
    if (!q) return options;
    return options.filter((o) => o.toLowerCase().includes(q));
  }, [options, menuFilter]);

  return (
    <DropdownMenu
      onOpenChange={(open) => {
        if (!open) setMenuFilter("");
      }}
    >
      <DropdownMenuTrigger asChild>
        <Button
          variant="outline"
          size="sm"
          aria-label={ariaLabel}
          className={cn(
            "h-9 gap-1.5",
            isActive && "border-primary/50 bg-primary/5",
          )}
        >
          {Icon ? <Icon aria-hidden="true" className="size-3.5" /> : null}
          {label}
          {isActive ? (
            <span className="ml-0.5 rounded-sm bg-primary/10 px-1 text-xs font-medium tabular-nums text-primary">
              {count}
            </span>
          ) : null}
          <ChevronDown aria-hidden="true" className="size-3" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent
        align="start"
        className="min-w-[14rem]"
        collisionPadding={8}
      >
        {/* In-menu text filter for long lists */}
        {options.length > 6 ? (
          <div className="px-2 pb-1 pt-0.5">
            <input
              type="text"
              value={menuFilter}
              onChange={(e) => setMenuFilter(e.target.value)}
              placeholder="Filter..."
              aria-label={`Filter ${label} options`}
              className={cn(
                "h-7 w-full rounded border border-border bg-background px-2 text-xs",
                "text-foreground placeholder:text-muted-foreground",
                "focus:outline-none focus:ring-1 focus:ring-ring",
              )}
            />
          </div>
        ) : null}

        {/* "Show all" clears the filter */}
        {isActive ? (
          <>
            <DropdownMenuItem
              onSelect={(e) => {
                e.preventDefault();
                onClear();
              }}
              className="text-xs"
            >
              Show all
            </DropdownMenuItem>
            <DropdownMenuSeparator />
          </>
        ) : null}

        {/* Option list with scroll for long lists */}
        <div className="max-h-[60vh] overflow-y-auto">
          {filtered.length === 0 ? (
            <div className="px-2 py-2 text-xs text-muted-foreground">
              No options
            </div>
          ) : (
            filtered.map((opt) => (
              <DropdownMenuCheckboxItem
                key={opt}
                checked={selected.includes(opt)}
                onCheckedChange={() => {
                  onToggle(opt);
                }}
                onSelect={(e) => {
                  // Keep the menu open so the user can toggle multiple options.
                  e.preventDefault();
                }}
                className="text-xs"
              >
                {opt}
              </DropdownMenuCheckboxItem>
            ))
          )}
        </div>

        {/* Labels for empty state */}
        {options.length === 0 ? (
          <DropdownMenuLabel className="text-xs text-muted-foreground">
            No options yet
          </DropdownMenuLabel>
        ) : null}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

// ---------------------------------------------------------------------------
// TagsFilterDropdown — registry-backed multi-select (GH #230 "rich tags")
// ---------------------------------------------------------------------------

/**
 * The Tags filter dropdown. Options come from the tenant's tag registry
 * (color dot + name + usage count), not the sites currently loaded — an
 * unused tag is still a valid filter target. A "Match any"/"Match all"
 * segmented control at the top controls whether the selected tags OR or AND
 * (disabled until 2+ tags are selected: a single tag's any/all reading is
 * identical, so the control stays inert until the choice is meaningful). A
 * "Manage tags" footer link routes to the tag registry settings page.
 */
function TagsFilterDropdown({
  registry,
  selected,
  onToggle,
  onClear,
  mode,
  onModeChange,
}: {
  registry: readonly SiteTag[];
  selected: readonly string[];
  onToggle: (name: string) => void;
  onClear: () => void;
  mode: TagMatchMode;
  onModeChange: (mode: TagMatchMode) => void;
}) {
  const [menuFilter, setMenuFilter] = useState("");
  const count = selected.length;
  const isActive = count > 0;

  const filtered = useMemo(() => {
    const q = menuFilter.trim().toLowerCase();
    if (!q) return registry;
    return registry.filter((t) => t.name.toLowerCase().includes(q));
  }, [registry, menuFilter]);

  return (
    <DropdownMenu
      onOpenChange={(open) => {
        if (!open) setMenuFilter("");
      }}
    >
      <DropdownMenuTrigger asChild>
        <Button
          variant="outline"
          size="sm"
          aria-label="Filter by tag"
          className={cn(
            "h-9 gap-1.5",
            isActive && "border-primary/50 bg-primary/5",
          )}
        >
          <Tag aria-hidden="true" className="size-3.5" />
          Tags
          {isActive ? (
            <span className="ml-0.5 rounded-sm bg-primary/10 px-1 text-xs font-medium tabular-nums text-primary">
              {count}
            </span>
          ) : null}
          <ChevronDown aria-hidden="true" className="size-3" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="min-w-[16rem]" collisionPadding={8}>
        <div className="px-1 pb-1.5 pt-0.5">
          <SegmentedControl
            aria-label="Tag match mode"
            value={mode}
            onChange={onModeChange}
            disabled={count < 2}
            options={[
              { value: "any", label: "Match any" },
              { value: "all", label: "Match all" },
            ]}
            className="w-full [&>button]:flex-1"
          />
        </div>
        <DropdownMenuSeparator />

        {registry.length > 6 ? (
          <div className="px-2 pb-1 pt-0.5">
            <input
              type="text"
              value={menuFilter}
              onChange={(e) => setMenuFilter(e.target.value)}
              placeholder="Filter..."
              aria-label="Filter tag options"
              className={cn(
                "h-7 w-full rounded border border-border bg-background px-2 text-xs",
                "text-foreground placeholder:text-muted-foreground",
                "focus:outline-none focus:ring-1 focus:ring-ring",
              )}
            />
          </div>
        ) : null}

        {isActive ? (
          <>
            <DropdownMenuItem
              onSelect={(e) => {
                e.preventDefault();
                onClear();
              }}
              className="text-xs"
            >
              Show all
            </DropdownMenuItem>
            <DropdownMenuSeparator />
          </>
        ) : null}

        <div className="max-h-[60vh] overflow-y-auto">
          {filtered.length === 0 ? (
            <div className="px-2 py-2 text-xs text-muted-foreground">
              {registry.length === 0 ? "No tags yet" : "No matching tags"}
            </div>
          ) : (
            filtered.map((t) => {
              const dot = resolveTagDot(t);
              return (
                <DropdownMenuCheckboxItem
                  key={t.id}
                  checked={selected.includes(t.name)}
                  onCheckedChange={() => onToggle(t.name)}
                  onSelect={(e) => {
                    // Keep the menu open so the user can toggle multiple tags.
                    e.preventDefault();
                  }}
                  className="gap-2 text-xs"
                >
                  <span
                    aria-hidden="true"
                    className={cn("size-2 shrink-0 rounded-full", dot.className)}
                    style={dot.style}
                  />
                  <span className="min-w-0 flex-1 truncate">{t.name}</span>
                  <span className="shrink-0 tabular-nums text-muted-foreground">
                    {t.usage_count}
                  </span>
                </DropdownMenuCheckboxItem>
              );
            })
          )}
        </div>

        <DropdownMenuSeparator />
        <Link
          to="/settings/tags"
          className="block px-2 py-1.5 text-xs font-medium text-primary underline-offset-4 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
        >
          Manage tags
        </Link>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

/**
 * Real client filter dropdown — shows id+name pairs; fires onSelect with the
 * client id (or null for "all"). Renders the applied client name in the label.
 */
function ClientFilterDropdown({
  label,
  options,
  appliedId,
  onSelect,
}: {
  label: string;
  options: readonly ClientOption[];
  appliedId: string | null;
  onSelect: (id: string | null) => void;
}) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          variant="outline"
          size="sm"
          aria-label="Filter by client"
          className={cn(
            "h-9 gap-1.5",
            appliedId && "border-primary/50 bg-primary/5",
          )}
        >
          <Users aria-hidden="true" className="size-3.5" />
          {label}
          <ChevronDown aria-hidden="true" className="size-3" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="min-w-[14rem]" collisionPadding={8}>
        <DropdownMenuItem
          onSelect={() => onSelect(null)}
          className={!appliedId ? "font-medium" : ""}
        >
          All clients
        </DropdownMenuItem>
        {options.length > 0 ? <DropdownMenuSeparator /> : null}
        {options.length === 0 ? (
          <DropdownMenuItem disabled>No clients yet</DropdownMenuItem>
        ) : (
          options.map((opt) => (
            <DropdownMenuItem
              key={opt.id}
              onSelect={() => onSelect(opt.id)}
              className={appliedId === opt.id ? "font-medium" : ""}
            >
              {opt.name}
            </DropdownMenuItem>
          ))
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

// ---------------------------------------------------------------------------
// ViewToggle — [List | Grid] icon buttons
// ---------------------------------------------------------------------------

function ViewToggle({
  view,
  onChange,
}: {
  view: SitesView;
  onChange: (next: SitesView) => void;
}) {
  return (
    <div
      role="group"
      aria-label="View mode"
      className="inline-flex items-center rounded-md border border-border bg-background p-0.5"
    >
      <DensityButton
        active={view === "list"}
        onClick={() => onChange("list")}
        title="List view"
        label="Switch to list view"
      >
        <List aria-hidden="true" className="size-4" />
      </DensityButton>
      <DensityButton
        active={view === "grid"}
        onClick={() => onChange("grid")}
        title="Grid view"
        label="Switch to grid view"
      >
        <LayoutGrid aria-hidden="true" className="size-4" />
      </DensityButton>
    </div>
  );
}

// ---------------------------------------------------------------------------
// CardSizeToggle — [Comfortable | Compact] for grid view
// ---------------------------------------------------------------------------

function CardSizeToggle({
  size,
  onChange,
}: {
  size: CardSize;
  onChange: (next: CardSize) => void;
}) {
  return (
    <div
      role="group"
      aria-label="Card size"
      className="inline-flex items-center rounded-md border border-border bg-background p-0.5"
    >
      <DensityButton
        active={size === "comfortable"}
        onClick={() => onChange("comfortable")}
        title="Comfortable card size"
        label="Comfortable card size"
      >
        <Rows3 aria-hidden="true" className="size-4" />
      </DensityButton>
      <DensityButton
        active={size === "compact"}
        onClick={() => onChange("compact")}
        title="Compact card size"
        label="Compact card size"
      >
        <Rows2 aria-hidden="true" className="size-4" />
      </DensityButton>
    </div>
  );
}

// ---------------------------------------------------------------------------
// DensityToggle — row density for list view
// ---------------------------------------------------------------------------

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
// GridSelectAll — "Select all (N)" control shown only in grid view
// ---------------------------------------------------------------------------

function GridSelectAll({
  visibleIds,
  selection,
}: {
  visibleIds: readonly string[];
  selection: SitesSelection;
}) {
  const allSelected =
    visibleIds.length > 0 &&
    visibleIds.every((id) => selection.selected.has(id));

  return (
    <button
      type="button"
      aria-label={
        allSelected
          ? "Clear selection"
          : `Select all ${visibleIds.length} visible sites`
      }
      onClick={() => {
        selection.setMany(visibleIds, !allSelected);
      }}
      className={cn(
        "inline-flex items-center gap-1.5 rounded border px-2.5 py-1 text-xs font-medium transition-colors",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
        allSelected
          ? "border-primary/50 bg-primary/5 text-primary"
          : "border-border text-muted-foreground hover:text-foreground",
      )}
    >
      <Square aria-hidden="true" className="size-3.5" />
      {allSelected ? "Deselect all" : `Select all (${visibleIds.length})`}
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
  onUpdateAgent,
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
            onUpdateAgent={onUpdateAgent}
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
  onUpdateAgent,
  count,
  sitesNoun,
}: {
  onBulkTag: () => void;
  onBulkSetClient: () => void;
  onBulkPauseMonitoring: () => void;
  onBulkDelete: () => void;
  onUpdateAgent?: () => void;
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
        {onUpdateAgent ? (
          <>
            <DropdownMenuSeparator />
            <DropdownMenuItem onSelect={onUpdateAgent}>
              Update WPMgr agent on {count} {sitesNoun}...
            </DropdownMenuItem>
          </>
        ) : null}
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
