// Sites table column geometry for GH #255 / GH #261.
//
// Both reports are the same bug seen from opposite ends of the viewport
// range:
//
//   • GH #255 (22-site self-hosted fleet, ordinary laptop width): the Agent,
//     Updates and Backup cells rendered content wider than their fixed
//     tracks, so under `table-layout: fixed` they overflowed into each other
//     and Uptime was pushed past the right edge.
//   • GH #261 (5120px display): the Site column was the ONLY track without a
//     width, so under `table-layout: fixed` it absorbed 100% of the leftover
//     space (roughly 2500px of it) while every other column stayed crushed
//     against the right edge.
//
// The fix is to stop having an implicitly-flexible column at all. Every
// track declares an explicit base width; the ones that genuinely benefit
// from extra room (Site, Tags, Backup) declare a growth weight and a HARD
// CAP; whatever surplus survives every cap is parked in a trailing spacer
// track so nothing is stretched absurdly.
//
// This module is deliberately pure (no React, no DOM): the colgroup in
// sites-table.tsx is still the single authoritative source of geometry for
// both the sticky header and the virtualized body, and this is the single
// authoritative source of the numbers it renders.

export interface SitesColumnTrack {
  /** Must match the TanStack column id in buildColumns(). */
  readonly id: string;
  /** Width at (and below) the table's minimum width. */
  readonly base: number;
  /**
   * Relative share of any surplus width. Omitted/zero means the track never
   * grows, which is the right default for a track holding fixed-shape
   * content (a version number, a checkbox, an icon button row).
   */
  readonly grow?: number;
  /** Hard ceiling for a growable track. Required whenever `grow` is set. */
  readonly max?: number;
}

/**
 * Column tracks in render order. THIS ORDER MUST MATCH buildColumns() in
 * sites-table.tsx; the trailing spacer is appended by
 * computeSitesColumnWidths and has no TanStack column of its own.
 *
 * Base widths are sized to the widest realistic content each cell renders
 * so the fixed layout never has to overflow a track (GH #255):
 *   • agent: status icon + a mono semver ("0.61.100") after the per-row
 *     status word moved to an icon and the "in fleet" qualifier moved to
 *     the column header.
 *   • backup: success icon + a relative time ("10h ago", "3mo ago") after
 *     the redundant "Backed up" prefix was dropped.
 *   • updates: the widest UpdateChip label ("12 updates").
 */
export const SITES_COLUMN_TRACKS: readonly SitesColumnTrack[] = [
  { id: "select", base: 40 },
  // Site is the widest column but no longer unbounded: it grows fastest and
  // stops at 420px, past which a hostname is already fully legible and the
  // extra width is pure whitespace (GH #261).
  { id: "url", base: 248, grow: 3, max: 420 },
  { id: "client", base: 112 },
  // Tags benefit from real width: extra room converts an overflow chip
  // ("+2") back into readable tag names.
  { id: "tags", base: 124, grow: 2, max: 220 },
  // Version tracks fit a mono "6.8.2" / "8.3.14" plus the odd vendor
  // suffix without wrapping.
  { id: "wp_version", base: 76 },
  { id: "php_version", base: 80 },
  { id: "agent_version", base: 104 },
  { id: "updates_count", base: 120 },
  { id: "backup_status", base: 120, grow: 1, max: 160 },
  { id: "uptime_sparkline", base: 72 },
  { id: "actions", base: 68 },
];

/**
 * Minimum table width: every track at its base. Below this the table keeps
 * these widths and the container scrolls horizontally rather than crushing
 * the columns into each other.
 */
export const SITES_TABLE_MIN_WIDTH_PX = SITES_COLUMN_TRACKS.reduce(
  (sum, t) => sum + t.base,
  0,
);

/**
 * Width at which every growable track has reached its cap. Past this point
 * all further surplus goes to the trailing spacer.
 */
export const SITES_TABLE_MAX_CONTENT_WIDTH_PX = SITES_COLUMN_TRACKS.reduce(
  (sum, t) => sum + (t.max ?? t.base),
  0,
);

/**
 * Resolve every column track to a concrete pixel width for the measured
 * `availableWidth` of the table's scroll container.
 *
 * Returns SITES_COLUMN_TRACKS.length + 1 entries: one per column, plus the
 * trailing spacer. The returned widths always sum to
 * `max(availableWidth, SITES_TABLE_MIN_WIDTH_PX)`, so the colgroup fully
 * describes the table and no track is left to absorb an unbounded
 * remainder.
 *
 * `availableWidth` of 0 (not yet measured) or a non-finite value resolves to
 * the base widths, which is also what the server-side/first paint renders.
 */
export function computeSitesColumnWidths(availableWidth: number): number[] {
  const widths = SITES_COLUMN_TRACKS.map((t) => t.base);

  if (!Number.isFinite(availableWidth) || availableWidth <= SITES_TABLE_MIN_WIDTH_PX) {
    return [...widths, 0];
  }

  let surplus = availableWidth - SITES_TABLE_MIN_WIDTH_PX;

  // Multiple passes: a track that hits its cap returns its unused share to
  // the pool so the remaining growable tracks can still use it. Bounded by
  // the track count, so it always terminates.
  for (let pass = 0; pass <= SITES_COLUMN_TRACKS.length && surplus > 0; pass += 1) {
    const active = SITES_COLUMN_TRACKS.map((track, i) => ({ track, i })).filter(
      ({ track, i }) =>
        (track.grow ?? 0) > 0 && widths[i]! < (track.max ?? track.base),
    );
    if (active.length === 0) break;

    const totalWeight = active.reduce((sum, { track }) => sum + track.grow!, 0);
    let consumed = 0;
    for (const { track, i } of active) {
      const share = (surplus * track.grow!) / totalWeight;
      const room = (track.max ?? track.base) - widths[i]!;
      const give = Math.min(share, room);
      widths[i] = widths[i]! + give;
      consumed += give;
    }
    // Nothing moved (floating point stall): stop rather than spin.
    if (consumed <= 0) break;
    surplus -= consumed;
  }

  // Integerize: floor each track, then hand every leftover pixel to the
  // spacer so the tracks sum exactly to the available width.
  const rounded = widths.map((w) => Math.floor(w));
  const used = rounded.reduce((sum, w) => sum + w, 0);
  return [...rounded, Math.max(0, Math.round(availableWidth) - used)];
}
