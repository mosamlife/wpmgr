import { describe, it, expect } from "vitest";

import {
  computeSitesColumnWidths,
  SITES_COLUMN_TRACKS,
  SITES_TABLE_MAX_CONTENT_WIDTH_PX,
  SITES_TABLE_MIN_WIDTH_PX,
} from "./sites-table-geometry";

// GH #255 (columns clipping into each other at laptop widths) and GH #261
// (the Site column swallowing ~2500px on a 5120px display) are the same
// geometry bug from opposite ends. These pin both ends.

const trackIndex = (id: string) =>
  SITES_COLUMN_TRACKS.findIndex((t) => t.id === id);
const SPACER_INDEX = SITES_COLUMN_TRACKS.length;

const sum = (widths: readonly number[]) => widths.reduce((a, b) => a + b, 0);

describe("sites table column tracks", () => {
  it("declares tracks in the same order buildColumns() renders them", () => {
    // The colgroup is authoritative for both the sticky header and the
    // virtualized body, so a reorder here silently mis-sizes every column.
    expect(SITES_COLUMN_TRACKS.map((t) => t.id)).toEqual([
      "select",
      "url",
      "client",
      "tags",
      "wp_version",
      "php_version",
      "agent_version",
      "updates_count",
      "backup_status",
      "uptime_sparkline",
      "actions",
    ]);
  });

  it("caps every growable track (no column may absorb unbounded width)", () => {
    for (const track of SITES_COLUMN_TRACKS) {
      if ((track.grow ?? 0) > 0) {
        expect(track.max, `${track.id} grows without a max`).toBeGreaterThan(
          track.base,
        );
      }
    }
  });

  it("keeps the minimum width at the sum of the base tracks", () => {
    expect(SITES_TABLE_MIN_WIDTH_PX).toBe(
      sum(SITES_COLUMN_TRACKS.map((t) => t.base)),
    );
  });
});

describe("computeSitesColumnWidths", () => {
  it("falls back to the base tracks before the container is measured", () => {
    const widths = computeSitesColumnWidths(0);
    expect(widths.slice(0, SPACER_INDEX)).toEqual(
      SITES_COLUMN_TRACKS.map((t) => t.base),
    );
    expect(widths[SPACER_INDEX]).toBe(0);
  });

  it("holds every track at its base below the minimum width so the container scrolls (GH #255)", () => {
    // Narrow viewport: the fix for the clipping is that nothing is squeezed,
    // the table keeps its widths and scrolls horizontally instead.
    const widths = computeSitesColumnWidths(600);
    expect(widths.slice(0, SPACER_INDEX)).toEqual(
      SITES_COLUMN_TRACKS.map((t) => t.base),
    );
    expect(sum(widths)).toBe(SITES_TABLE_MIN_WIDTH_PX);
  });

  it("gives the Agent, Updates and Backup tracks their full base at the minimum width", () => {
    // The three columns that overlapped in the report. Under
    // `table-layout: fixed` a track narrower than its content overflows into
    // its neighbour, so these must never be shaved to make room.
    const widths = computeSitesColumnWidths(SITES_TABLE_MIN_WIDTH_PX);
    for (const id of ["agent_version", "updates_count", "backup_status"]) {
      const i = trackIndex(id);
      expect(widths[i]).toBe(SITES_COLUMN_TRACKS[i]!.base);
    }
  });

  it("spends a moderate surplus on Site, Tags and Backup with nothing left over", () => {
    const widths = computeSitesColumnWidths(1440);
    expect(widths[trackIndex("url")]).toBeGreaterThan(
      SITES_COLUMN_TRACKS[trackIndex("url")]!.base,
    );
    expect(widths[trackIndex("tags")]).toBeGreaterThan(
      SITES_COLUMN_TRACKS[trackIndex("tags")]!.base,
    );
    expect(widths[trackIndex("backup_status")]).toBeGreaterThan(
      SITES_COLUMN_TRACKS[trackIndex("backup_status")]!.base,
    );
    // 1440 is below the point where every cap is reached, so the spacer is
    // still empty and no width is wasted.
    expect(1440).toBeLessThan(SITES_TABLE_MAX_CONTENT_WIDTH_PX);
    expect(widths[SPACER_INDEX]).toBeLessThanOrEqual(2);
  });

  it("never grows a non-growable track", () => {
    const widths = computeSitesColumnWidths(5120);
    for (const [i, track] of SITES_COLUMN_TRACKS.entries()) {
      if (!(track.grow ?? 0)) {
        expect(widths[i], `${track.id} grew`).toBe(track.base);
      }
    }
  });

  it("caps the Site column and parks the rest in the spacer on a 5120px display (GH #261)", () => {
    const widths = computeSitesColumnWidths(5120);
    const site = widths[trackIndex("url")]!;

    // The reported symptom: Site took roughly half the table.
    expect(site).toBe(SITES_COLUMN_TRACKS[trackIndex("url")]!.max);
    expect(site / 5120).toBeLessThan(0.1);

    // Every other track is at its cap too, so the surplus has nowhere to go
    // except the trailing spacer.
    expect(widths[SPACER_INDEX]).toBeGreaterThan(3000);
    expect(sum(widths)).toBe(5120);
  });

  it("never exceeds a track's cap at any width", () => {
    const widthsToCheck = [
      SITES_TABLE_MIN_WIDTH_PX,
      1280,
      1440,
      1920,
      2560,
      3840,
      5120,
      8000,
    ];
    for (const available of widthsToCheck) {
      const widths = computeSitesColumnWidths(available);
      for (const [i, track] of SITES_COLUMN_TRACKS.entries()) {
        expect(
          widths[i],
          `${track.id} at ${available}px`,
        ).toBeLessThanOrEqual(track.max ?? track.base);
      }
      expect(sum(widths)).toBe(Math.max(available, SITES_TABLE_MIN_WIDTH_PX));
    }
  });

  it("grows monotonically as the container widens", () => {
    let previous = computeSitesColumnWidths(SITES_TABLE_MIN_WIDTH_PX);
    for (let available = SITES_TABLE_MIN_WIDTH_PX + 40; available <= 2600; available += 40) {
      const widths = computeSitesColumnWidths(available);
      for (const [i, track] of SITES_COLUMN_TRACKS.entries()) {
        expect(
          widths[i],
          `${track.id} shrank at ${available}px`,
        ).toBeGreaterThanOrEqual(previous[i]!);
      }
      previous = widths;
    }
  });
});
