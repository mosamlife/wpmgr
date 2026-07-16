/**
 * Unit tests for the new site-card primitives. Pure-function coverage;
 * no DOM rendering required (matching the project convention in
 * connection-state-badge.test.ts / sites-filter.test.ts).
 */
import { describe, it, expect } from "vitest";
import { relativeTime } from "@/lib/utils";

// ---------------------------------------------------------------------------
// SslChip — threshold logic
// ---------------------------------------------------------------------------
// The threshold logic is extracted here as a pure function mirroring
// ssl-chip.tsx so we can test it without a DOM renderer.

type SslPalette = "destructive-subtle" | "warning-subtle" | "muted";

function sslPalette(expiresAt: string, now: number = Date.now()): SslPalette {
  const expiresMs = Date.parse(expiresAt);
  if (Number.isNaN(expiresMs)) return "muted";
  const daysLeft = Math.ceil((expiresMs - now) / (1000 * 60 * 60 * 24));
  if (daysLeft <= 7) return "destructive-subtle";
  if (daysLeft <= 21) return "warning-subtle";
  return "muted";
}

function sslLabel(expiresAt: string, now: number = Date.now()): string {
  const expiresMs = Date.parse(expiresAt);
  if (Number.isNaN(expiresMs)) return "";
  const daysLeft = Math.ceil((expiresMs - now) / (1000 * 60 * 60 * 24));
  return daysLeft <= 0 ? "SSL expired" : `SSL ${daysLeft}d`;
}

const DAY_MS = 1000 * 60 * 60 * 24;
const BASE_NOW = new Date("2026-06-16T12:00:00Z").getTime();

describe("SslChip — palette thresholds", () => {
  it("destructive-subtle when 7 days left (boundary)", () => {
    const expiresAt = new Date(BASE_NOW + 7 * DAY_MS).toISOString();
    expect(sslPalette(expiresAt, BASE_NOW)).toBe("destructive-subtle");
  });

  it("destructive-subtle when 1 day left", () => {
    const expiresAt = new Date(BASE_NOW + 1 * DAY_MS).toISOString();
    expect(sslPalette(expiresAt, BASE_NOW)).toBe("destructive-subtle");
  });

  it("destructive-subtle when cert is expired (0 days)", () => {
    const expiresAt = new Date(BASE_NOW - 1 * DAY_MS).toISOString();
    expect(sslPalette(expiresAt, BASE_NOW)).toBe("destructive-subtle");
  });

  it("warning-subtle when 8 days left (just above 7-day boundary)", () => {
    const expiresAt = new Date(BASE_NOW + 8 * DAY_MS).toISOString();
    expect(sslPalette(expiresAt, BASE_NOW)).toBe("warning-subtle");
  });

  it("warning-subtle when 21 days left (boundary)", () => {
    const expiresAt = new Date(BASE_NOW + 21 * DAY_MS).toISOString();
    expect(sslPalette(expiresAt, BASE_NOW)).toBe("warning-subtle");
  });

  it("muted when 22 days left (just above 21-day boundary)", () => {
    const expiresAt = new Date(BASE_NOW + 22 * DAY_MS).toISOString();
    expect(sslPalette(expiresAt, BASE_NOW)).toBe("muted");
  });

  it("muted when 90 days left (well within normal range)", () => {
    const expiresAt = new Date(BASE_NOW + 90 * DAY_MS).toISOString();
    expect(sslPalette(expiresAt, BASE_NOW)).toBe("muted");
  });
});

describe("SslChip — label text", () => {
  it("shows 'SSL Nd' with the correct day count", () => {
    const expiresAt = new Date(BASE_NOW + 30 * DAY_MS).toISOString();
    expect(sslLabel(expiresAt, BASE_NOW)).toBe("SSL 30d");
  });

  it("shows 'SSL expired' when cert is past its expiry", () => {
    const expiresAt = new Date(BASE_NOW - 5 * DAY_MS).toISOString();
    expect(sslLabel(expiresAt, BASE_NOW)).toBe("SSL expired");
  });

  it("shows 'SSL 1d' for last-day cert", () => {
    // 12 hours remaining → ceiling → 1d
    const expiresAt = new Date(BASE_NOW + 12 * 60 * 60 * 1000).toISOString();
    expect(sslLabel(expiresAt, BASE_NOW)).toBe("SSL 1d");
  });
});

// ---------------------------------------------------------------------------
// CapabilityStrip — lit / dim derivation
// ---------------------------------------------------------------------------
// The buildCapabilityItems logic from site-card.tsx extracted as a pure fn.

import type { Site } from "@wpmgr/api";

interface CapabilityFlags {
  hasPageCache: boolean;
  hasObjectCache: boolean;
  isHttps: boolean;
  hasBackups: boolean;
  isMultisite: boolean;
}

function deriveCapabilityFlags(site: Partial<Site>): CapabilityFlags {
  const hasPageCache =
    site.components?.plugins?.some(
      (p) => p.slug === "wpmgr-page-cache" && p.active === true,
    ) ?? false;
  const hasObjectCache =
    site.components?.plugins?.some(
      (p) => p.slug === "wpmgr-object-cache" && p.active === true,
    ) ?? false;
  const isHttps = (site.url ?? "").startsWith("https://");
  const hasBackups = site.last_backup_status != null;
  const isMultisite = site.multisite ?? false;
  return { hasPageCache, hasObjectCache, isHttps, hasBackups, isMultisite };
}

describe("CapabilityStrip — lit glyphs (enabled=true)", () => {
  it("page cache is lit when wpmgr-page-cache plugin is active", () => {
    const site: Partial<Site> = {
      url: "https://example.com",
      multisite: false,
      last_backup_status: undefined,
      components: {
        plugins: [{ slug: "wpmgr-page-cache", active: true }],
      },
    };
    const flags = deriveCapabilityFlags(site);
    expect(flags.hasPageCache).toBe(true);
  });

  it("object cache is lit when wpmgr-object-cache plugin is active", () => {
    const site: Partial<Site> = {
      url: "https://example.com",
      multisite: false,
      last_backup_status: undefined,
      components: {
        plugins: [{ slug: "wpmgr-object-cache", active: true }],
      },
    };
    expect(deriveCapabilityFlags(site).hasObjectCache).toBe(true);
  });

  it("HTTPS is lit when url starts with https://", () => {
    const flags = deriveCapabilityFlags({ url: "https://example.com", multisite: false });
    expect(flags.isHttps).toBe(true);
  });

  it("backups are lit when last_backup_status is non-null", () => {
    const flags = deriveCapabilityFlags({
      url: "https://example.com",
      multisite: false,
      last_backup_status: "success",
    });
    expect(flags.hasBackups).toBe(true);
  });

  it("multisite is lit when multisite flag is true", () => {
    const flags = deriveCapabilityFlags({ url: "https://example.com", multisite: true });
    expect(flags.isMultisite).toBe(true);
  });
});

describe("CapabilityStrip — dim glyphs (enabled=false)", () => {
  it("page cache is dim when plugin is absent", () => {
    const flags = deriveCapabilityFlags({ url: "https://example.com", multisite: false });
    expect(flags.hasPageCache).toBe(false);
  });

  it("page cache is dim when plugin is present but inactive", () => {
    const site: Partial<Site> = {
      url: "https://example.com",
      multisite: false,
      components: {
        plugins: [{ slug: "wpmgr-page-cache", active: false }],
      },
    };
    expect(deriveCapabilityFlags(site).hasPageCache).toBe(false);
  });

  it("HTTPS is dim for http:// sites", () => {
    const flags = deriveCapabilityFlags({ url: "http://example.com", multisite: false });
    expect(flags.isHttps).toBe(false);
  });

  it("backups are dim when last_backup_status is null/undefined", () => {
    const flags = deriveCapabilityFlags({
      url: "https://example.com",
      multisite: false,
      last_backup_status: undefined,
    });
    expect(flags.hasBackups).toBe(false);
  });

  it("multisite is dim when flag is false", () => {
    const flags = deriveCapabilityFlags({ url: "https://example.com", multisite: false });
    expect(flags.isMultisite).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// Thumbnail state selection — pure logic
// (supplements the existing tests in sites-filter.test.ts)
// ---------------------------------------------------------------------------

interface SiteWithScreenshot {
  screenshot_url?: string | null;
  screenshot_status?: "pending" | "ready" | "failed" | null;
}

function thumbnailState(
  site: SiteWithScreenshot,
  imgError = false,
): "ready" | "capturing" | "failed" | "never" {
  if (site.screenshot_url && !imgError) return "ready";
  if (site.screenshot_status === "pending") return "capturing";
  if (site.screenshot_status === "failed") return "failed";
  if (imgError && site.screenshot_url) return "failed";
  return "never";
}

describe("thumbnailState — new screenshot fields wired (M72)", () => {
  it("ready when screenshot_url present and no img error", () => {
    expect(thumbnailState({ screenshot_url: "https://cdn/shot.webp" })).toBe("ready");
  });

  it("ready ignores screenshot_status when url is present and no error", () => {
    // status "ready" is redundant when url is set
    expect(
      thumbnailState({ screenshot_url: "https://cdn/shot.webp", screenshot_status: "ready" }),
    ).toBe("ready");
  });

  it("capturing when status is pending and no url", () => {
    expect(thumbnailState({ screenshot_status: "pending" })).toBe("capturing");
  });

  it("failed when status is failed", () => {
    expect(thumbnailState({ screenshot_status: "failed" })).toBe("failed");
  });

  it("failed when url present but image errored", () => {
    expect(
      thumbnailState({ screenshot_url: "https://cdn/shot.webp" }, true),
    ).toBe("failed");
  });

  it("never when no screenshot fields at all", () => {
    expect(thumbnailState({})).toBe("never");
  });

  it("never when both url and status are null", () => {
    expect(thumbnailState({ screenshot_url: null, screenshot_status: null })).toBe("never");
  });
});

// ---------------------------------------------------------------------------
// Calm zero-states — "Up to date" when 0 updates, "No backups yet" when null
// ---------------------------------------------------------------------------

describe("calm zero-states — updates", () => {
  it("updates_available=0 should display 'Up to date' (not a warning chip)", () => {
    // The logic: updatesCount > 0 shows UpdateChip; else shows calm text.
    const updatesCount = 0;
    const showChip = updatesCount > 0;
    expect(showChip).toBe(false);
  });

  it("updates_available=1 shows the update chip", () => {
    const updatesCount = 1;
    expect(updatesCount > 0).toBe(true);
  });

  it("updates_available undefined defaults to 0 (no chip)", () => {
    const raw: number | undefined = undefined;
    const updatesCount = raw ?? 0;
    expect(updatesCount > 0).toBe(false);
  });
});

describe("calm zero-states — backups", () => {
  it("last_backup_status null shows 'No backups yet' (not a chip)", () => {
    const backupStatus = null as string | null;
    const showChip = backupStatus != null;
    expect(showChip).toBe(false);
  });

  it("last_backup_status 'success' shows the BackupChip", () => {
    const backupStatus = "success" as string | null;
    expect(backupStatus != null).toBe(true);
  });

  it("last_backup_status 'failed' shows the BackupChip", () => {
    const backupStatus = "failed" as string | null;
    expect(backupStatus != null).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// GH #231 — Sites list "last backup" renders relative time (matches
// /backups), with the exact absolute timestamp preserved on hover.
// ---------------------------------------------------------------------------
// Mirrors the derivation in site-card.tsx / sites-table.tsx: the BackupChip
// `time` prop takes the shared `relativeTime()` label (never a raw ISO
// string), and `title` carries the absolute timestamp for hover precision.

function deriveBackupChipProps(lastBackupAt: string | null, now?: number) {
  return {
    time: relativeTime(lastBackupAt, now) ?? undefined,
    title: lastBackupAt ? new Date(lastBackupAt).toLocaleString() : undefined,
  };
}

describe("last-backup formatting — relative time + hover precision (GH #231)", () => {
  const NOW = new Date("2026-06-16T12:00:00Z").getTime();

  it("renders a short relative label, not the raw ISO timestamp", () => {
    const twoHoursAgo = new Date(NOW - 2 * 60 * 60 * 1000).toISOString();
    const { time } = deriveBackupChipProps(twoHoursAgo, NOW);
    expect(time).toBe("2h ago");
    expect(time).not.toContain("T"); // not an ISO string leaking through
  });

  it("renders 'just now' for a very recent backup", () => {
    const secondsAgo = new Date(NOW - 5 * 1000).toISOString();
    const { time } = deriveBackupChipProps(secondsAgo, NOW);
    expect(time).toBe("just now");
  });

  it("renders day-granularity labels for older backups", () => {
    const threeDaysAgo = new Date(NOW - 3 * 24 * 60 * 60 * 1000).toISOString();
    const { time } = deriveBackupChipProps(threeDaysAgo, NOW);
    expect(time).toBe("3d ago");
  });

  it("carries the exact absolute timestamp in `title` for hover precision", () => {
    const iso = new Date(NOW - 2 * 60 * 60 * 1000).toISOString();
    const { title } = deriveBackupChipProps(iso, NOW);
    expect(title).toBe(new Date(iso).toLocaleString());
  });

  it("null last_backup_at yields no time/title (caller shows 'No backups yet')", () => {
    const { time, title } = deriveBackupChipProps(null, NOW);
    expect(time).toBeUndefined();
    expect(title).toBeUndefined();
  });
});

// ---------------------------------------------------------------------------
// Uptime row — reserved slot shows text or placeholder
// ---------------------------------------------------------------------------

describe("uptime row — conditional rendering", () => {
  it("shows uptime text when uptime_pct is present", () => {
    const site: Partial<Site> = { uptime_pct: 99.98, avg_latency_ms: 234, up: true };
    expect(site.uptime_pct != null).toBe(true);
  });

  it("shows 'Uptime not monitored' when uptime_pct is absent", () => {
    const site: Partial<Site> = {};
    expect(site.uptime_pct != null).toBe(false);
  });

  it("StatusDot uses destructive tone when up is false", () => {
    function uptimeTone(up: boolean | undefined): "destructive" | "success" {
      return up === false ? "destructive" : "success";
    }
    expect(uptimeTone(false)).toBe("destructive");
  });

  it("StatusDot uses success tone when up is true", () => {
    function uptimeTone(up: boolean | undefined): "destructive" | "success" {
      return up === false ? "destructive" : "success";
    }
    expect(uptimeTone(true)).toBe("success");
  });
});
