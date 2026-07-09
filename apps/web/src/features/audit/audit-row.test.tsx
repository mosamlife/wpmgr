import { describe, it, expect } from "vitest";
import { screen } from "@testing-library/react";
import type { AuditEntry } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";

import { AuditEntryRow } from "./audit-row";
import type { SiteMin } from "./types";

// GH #201 — fleet Audit log: backup/restore/update entries are site-scoped
// but carry a target_type other than "site" ("backup_snapshot",
// "update_task", "backup_schedule", ...). The old TargetSlot only resolved +
// rendered a site name when target_type === "site", so every one of these
// rows showed the raw wire target_type string ("backup_snapshot",
// "update_task") instead of which site the backup/restore/update actually
// happened on. This pins the fix: for a non-"site" target_type, the row now
// derives a candidate site id from `metadata.site_id` (or, for
// "backup_schedule" specifically, `target_id` — the schedule row's id IS the
// site id, see backup/handler.go's recordScheduleChange) and resolves it
// against the known sites list, falling back to the original raw display
// when nothing resolves.

const SITE_ID = "11111111-1111-1111-1111-111111111111";
const OTHER_SITE_ID = "22222222-2222-2222-2222-222222222222";
const UNKNOWN_SITE_ID = "99999999-9999-9999-9999-999999999999";

const SITES: SiteMin[] = [
  { id: SITE_ID, name: "Acme Blog", url: "https://acme.example" },
  { id: OTHER_SITE_ID, name: "Other Site", url: "https://other.example" },
];

let seq = 0;
function entry(overrides: Partial<AuditEntry> = {}): AuditEntry {
  seq += 1;
  return {
    id: overrides.id ?? `entry-${seq}`,
    tenant_id: "tenant-1",
    actor_type: "user",
    actor_id: "user-1",
    action: "backup.started",
    target_type: "backup_snapshot",
    target_id: `snap-${seq}`,
    prev_hash: "prev",
    hash: "hash",
    created_at: "2026-07-08T12:00:00Z",
    ...overrides,
  };
}

describe("AuditEntryRow target site resolution (GH #201)", () => {
  it("resolves a backup_snapshot entry's site name from metadata.site_id, not the raw target_type", () => {
    const e = entry({
      target_type: "backup_snapshot",
      target_id: "snap-1",
      metadata: { site_id: SITE_ID, full: true },
    });

    renderWithProviders(<AuditEntryRow entry={e} sites={SITES} isToday={false} />);

    // The resolved site name is shown...
    expect(screen.getByText("Acme Blog")).toBeInTheDocument();
    // ...and the raw target_type string is never rendered as the target
    // label. This is the non-vacuous half of the assertion: the pre-fix
    // TargetSlot renders exactly "backup_snapshot" here, so this line fails
    // against the old target_type-only code and passes only with the fix.
    expect(screen.queryByText("backup_snapshot")).not.toBeInTheDocument();
  });

  it("resolves a backup_schedule entry's site via target_id (the schedule id IS the site id; no metadata.site_id present)", () => {
    const e = entry({
      action: "backup.schedule.changed",
      target_type: "backup_schedule",
      target_id: OTHER_SITE_ID,
      metadata: { cadence: "daily", kind: "full", enabled: true },
    });

    renderWithProviders(<AuditEntryRow entry={e} sites={SITES} isToday={false} />);

    expect(screen.getByText("Other Site")).toBeInTheDocument();
    expect(screen.queryByText("backup_schedule")).not.toBeInTheDocument();
  });

  it("falls back to the raw target_type when metadata.site_id matches no known site, without crashing", () => {
    const e = entry({
      target_type: "backup_snapshot",
      target_id: "snap-2",
      metadata: { site_id: UNKNOWN_SITE_ID },
    });

    expect(() =>
      renderWithProviders(<AuditEntryRow entry={e} sites={SITES} isToday={false} />),
    ).not.toThrow();

    expect(screen.getByText("backup_snapshot")).toBeInTheDocument();
  });

  it("falls back to the raw target_type when the entry carries no site id at all (e.g. update.run.created)", () => {
    const e = entry({
      action: "update.run.created",
      target_type: "update_run",
      target_id: "run-1",
      metadata: { dry_run: false, task_count: 3 },
    });

    expect(() =>
      renderWithProviders(<AuditEntryRow entry={e} sites={SITES} isToday={false} />),
    ).not.toThrow();

    expect(screen.getByText("update_run")).toBeInTheDocument();
  });

  it("still resolves a plain target_type: \"site\" entry unchanged", () => {
    const e = entry({
      action: "site.cache.purged",
      target_type: "site",
      target_id: SITE_ID,
      metadata: {},
    });

    renderWithProviders(<AuditEntryRow entry={e} sites={SITES} isToday={false} />);

    expect(screen.getByText("Acme Blog")).toBeInTheDocument();
  });
});
