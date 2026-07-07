/**
 * SitesGrid — responsive card grid for the Sites page (the alternative to the
 * compact table view). Auto-fill columns with a 19rem min-width so the grid
 * is always 1 column on mobile without a breakpoint override.
 *
 * No virtualization v1 — the grid renders all cards. At fleet scales this is
 * fine for the initial release; if perf degrades a windowed grid (react-virtuoso
 * masonry or react-window) can be dropped in later.
 *
 * First-mount animation: one container fadeUp (same pattern as SitesTable),
 * NO per-card stagger — TanStack Router + Query can re-render the grid on
 * filter change and a per-card stagger would re-fire on every filter tick.
 *
 * Selection: shares the module-level useSitesSelection singleton so cross-view
 * selection (select in table, switch to grid) carries over with zero new code.
 */
import { motion } from "motion/react";
import type { Site } from "@wpmgr/api";

import { Skeleton } from "@/components/ui/skeleton";
import { fadeUp } from "@/lib/motion-presets";
import { SiteCard } from "@/features/sites/site-card";
import { useSitesSelection } from "@/features/sites/use-sites-selection";
import type { CardSize } from "@/features/sites/use-sites-view";

// ─── Props ────────────────────────────────────────────────────────────────────

export interface SitesGridProps {
  sites: Site[];
  cardSize: CardSize;
  onOpenAutoLogin?: (site: Site) => void;
  /** Opens the site's detail page — wired to the "Open site" menu action. */
  onOpenDetail?: (site: Site) => void;
  onDisconnect?: (site: Site) => void;
  onReconnect?: (site: Site) => void;
}

// ─── Component ────────────────────────────────────────────────────────────────

export function SitesGrid({
  sites,
  cardSize,
  onOpenAutoLogin,
  onOpenDetail,
  onDisconnect,
  onReconnect,
}: SitesGridProps) {
  const selection = useSitesSelection();

  return (
    <motion.div
      variants={fadeUp}
      // The grid is always mounted fresh when the user switches to grid view
      // (the route renders either <SitesGrid> or <SitesTable>). So "initial"
      // fires exactly once per mount — no hasMounted guard needed here.
      initial="initial"
      animate="animate"
    >
      <div
        role="region"
        aria-label="Sites grid"
        // auto-fill with min(100%, 19rem): 1 col on mobile (no breakpoint), fills
        // wider viewports with as many 19rem cards as fit.
        className="grid gap-4"
        style={{
          gridTemplateColumns: "repeat(auto-fill, minmax(min(100%, 19rem), 1fr))",
        }}
      >
        {sites.map((site) => (
          <SiteCard
            key={site.id}
            site={site}
            cardSize={cardSize}
            selectionCount={selection.count}
            onOpenAutoLogin={onOpenAutoLogin}
            onOpenDetail={onOpenDetail}
            onDisconnect={onDisconnect}
            onReconnect={onReconnect}
          />
        ))}
      </div>
    </motion.div>
  );
}

// ─── Skeleton ─────────────────────────────────────────────────────────────────

/**
 * SitesGridSkeleton — shown while the sites query is loading in grid view.
 * 8 card-shaped Skeleton blocks in the same grid to match spatial footprint.
 */
export function SitesGridSkeleton() {
  return (
    <div
      role="status"
      aria-busy="true"
      aria-label="Loading sites"
      className="grid gap-4"
      style={{
        gridTemplateColumns: "repeat(auto-fill, minmax(min(100%, 19rem), 1fr))",
      }}
    >
      <span className="sr-only">Loading sites</span>
      {Array.from({ length: 8 }).map((_, i) => (
        <div
          key={i}
          className="flex flex-col overflow-hidden rounded-lg border border-border"
        >
          {/* Thumbnail placeholder */}
          <Skeleton className="aspect-[16/10] w-full rounded-t-lg rounded-b-none" />
          {/* Body placeholders */}
          <div className="flex flex-col gap-2 p-3">
            <Skeleton className="h-4 w-3/4" />
            <Skeleton className="h-3 w-1/2" />
            <Skeleton className="h-3 w-2/3" />
          </div>
        </div>
      ))}
    </div>
  );
}
