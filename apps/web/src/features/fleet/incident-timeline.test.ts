import { describe, it, expect } from "vitest";

import type {
  UpdateTask,
  BackupSnapshot,
  SiteActivityEvent,
  PhpError,
} from "@wpmgr/api";

import { serverRetryFields } from "@/test/update-task-fixtures";

import {
  updateTasksToTimeline,
  backupsToTimeline,
  activityToTimeline,
  phpErrorsToTimeline,
  mergeIncidentTimeline,
  type TimelineItem,
} from "./incident-timeline";

// ---------------------------------------------------------------------------
// Fixture factories — minimal required fields, override what a test cares
// about. Keeps each test focused on the field(s) it is actually exercising.
// ---------------------------------------------------------------------------

function makeUpdateTask(overrides: Partial<UpdateTask> = {}): UpdateTask {
  const status = overrides.status ?? "succeeded";
  return {
    id: "task-1",
    run_id: "run-1",
    tenant_id: "tenant-1",
    site_id: "site-1",
    target_type: "plugin",
    target_slug: "akismet",
    status: "succeeded",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    // GH #336: the server always writes these, and always as the pair its own
    // retryClassify would produce for this status.
    ...serverRetryFields(status),
    ...overrides,
  };
}

function makeBackupSnapshot(
  overrides: Partial<BackupSnapshot> = {},
): BackupSnapshot {
  return {
    id: "snap-1",
    tenant_id: "tenant-1",
    site_id: "site-1",
    kind: "full",
    status: "completed",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

function makeActivityEvent(
  overrides: Partial<SiteActivityEvent> = {},
): SiteActivityEvent {
  return {
    id: "evt-1",
    seq: 1,
    event_type: "plugin.activated",
    object_type: "plugin",
    object_id: "akismet",
    object_label: "Akismet",
    actor_user_id: 1,
    actor_login: "admin",
    actor_ip: "127.0.0.1",
    summary: "Activated plugin Akismet",
    meta: {},
    severity: "low",
    prev_hash: "0".repeat(64),
    this_hash: "1".repeat(64),
    chain_valid: true,
    occurred_at: "2026-01-01T00:00:00Z",
    received_at: "2026-01-01T00:00:01Z",
    ...overrides,
  };
}

function makePhpError(overrides: Partial<PhpError> = {}): PhpError {
  return {
    id: "err-1",
    md5: "abc123",
    code: 256,
    severity: "fatal",
    message: "Call to undefined function foo()",
    file: "/wp-content/plugins/broken/broken.php",
    line: 42,
    request_path: "/",
    first_seen_at: "2026-01-01T00:00:00Z",
    last_seen_at: "2026-01-01T00:00:00Z",
    occurrence_count: 1,
    silenced: false,
    backtrace: [],
    ...overrides,
  };
}

// ---------------------------------------------------------------------------
// Per-source mappers
// ---------------------------------------------------------------------------

describe("updateTasksToTimeline", () => {
  it("labels a succeeded task as 'Updated <type> <slug>'", () => {
    const item = updateTasksToTimeline([
      makeUpdateTask({ status: "succeeded", target_type: "plugin", target_slug: "akismet" }),
    ])[0]!;
    expect(item.label).toBe("Updated plugin akismet");
    expect(item.source).toBe("update");
  });

  it("labels a failed task distinctly", () => {
    const item = updateTasksToTimeline([makeUpdateTask({ status: "failed" })])[0]!;
    expect(item.label).toBe("Failed to update plugin akismet");
  });

  it("labels a rolled-back task distinctly", () => {
    const item = updateTasksToTimeline([makeUpdateTask({ status: "rolled_back" })])[0]!;
    expect(item.label).toBe("Rolled back plugin akismet update");
  });

  it("prefers finished_at, then started_at, then updated_at, then created_at for the timestamp", () => {
    const withFinished = updateTasksToTimeline([
      makeUpdateTask({
        finished_at: "2026-01-02T00:00:00Z",
        started_at: "2026-01-01T12:00:00Z",
        created_at: "2026-01-01T00:00:00Z",
      }),
    ])[0]!;
    expect(withFinished.timestamp).toBe("2026-01-02T00:00:00Z");

    const withoutFinished = updateTasksToTimeline([
      makeUpdateTask({
        finished_at: undefined,
        started_at: "2026-01-01T12:00:00Z",
        created_at: "2026-01-01T00:00:00Z",
      }),
    ])[0]!;
    expect(withoutFinished.timestamp).toBe("2026-01-01T12:00:00Z");
  });

  it("includes a from -> to version detail when both are present", () => {
    const item = updateTasksToTimeline([
      makeUpdateTask({ from_version: "5.0", to_version: "5.1" }),
    ])[0]!;
    expect(item.detail).toBe("5.0 to 5.1");
  });

  it("omits the detail when versions are absent", () => {
    const item = updateTasksToTimeline([makeUpdateTask()])[0]!;
    expect(item.detail).toBeUndefined();
  });
});

describe("backupsToTimeline", () => {
  it("labels a completed full backup", () => {
    const item = backupsToTimeline([
      makeBackupSnapshot({ kind: "full", status: "completed" }),
    ])[0]!;
    expect(item.label).toBe("Full backup completed");
    expect(item.source).toBe("backup");
  });

  it("labels a failed database backup and carries the error as detail", () => {
    const item = backupsToTimeline([
      makeBackupSnapshot({ kind: "db", status: "failed", error: "disk full" }),
    ])[0]!;
    expect(item.label).toBe("Database backup failed");
    expect(item.detail).toBe("disk full");
  });

  it("uses finished_at over created_at when present", () => {
    const item = backupsToTimeline([
      makeBackupSnapshot({
        finished_at: "2026-01-02T00:00:00Z",
        created_at: "2026-01-01T00:00:00Z",
      }),
    ])[0]!;
    expect(item.timestamp).toBe("2026-01-02T00:00:00Z");
  });
});

describe("activityToTimeline", () => {
  it("maps summary/occurred_at/actor_login straight through", () => {
    const item = activityToTimeline([
      makeActivityEvent({ summary: "Updated theme Twenty Twenty-Five", actor_login: "jane" }),
    ])[0]!;
    expect(item.source).toBe("activity");
    expect(item.label).toBe("Updated theme Twenty Twenty-Five");
    expect(item.detail).toBe("jane");
  });

  it("omits detail for a system event with no actor login", () => {
    const item = activityToTimeline([makeActivityEvent({ actor_login: "" })])[0]!;
    expect(item.detail).toBeUndefined();
  });
});

describe("phpErrorsToTimeline", () => {
  it("labels with severity + message and details the file:line", () => {
    const item = phpErrorsToTimeline([
      makePhpError({
        severity: "fatal",
        message: "Call to undefined function foo()",
        file: "/wp-content/plugins/broken/broken.php",
        line: 42,
      }),
    ])[0]!;
    expect(item.source).toBe("php_error");
    expect(item.label).toBe("PHP fatal: Call to undefined function foo()");
    expect(item.detail).toBe("/wp-content/plugins/broken/broken.php:42");
  });
});

// ---------------------------------------------------------------------------
// mergeIncidentTimeline — windowing + per-source limit + global sort
// ---------------------------------------------------------------------------

const ISO_START = "2026-06-01T12:00:00Z";
const ISO_END = "2026-06-01T13:00:00Z";

function item(source: TimelineItem["source"], id: string, timestamp: string): TimelineItem {
  return { source, id, timestamp, label: `${source}-${id}` };
}

describe("mergeIncidentTimeline", () => {
  it("keeps items inside the incident window and drops items far outside it", () => {
    const inside = item("backup", "in", "2026-06-01T12:30:00Z");
    const farBefore = item("backup", "before", "2026-06-01T09:00:00Z");
    const farAfter = item("backup", "after", "2026-06-01T18:00:00Z");

    const result = mergeIncidentTimeline(
      [[inside, farBefore, farAfter]],
      { start: ISO_START, end: ISO_END },
    );

    expect(result.map((r) => r.id)).toEqual(["in"]);
  });

  it("includes items within the context padding just outside the strict window", () => {
    // 10 minutes before started_at — inside the default 15-minute padding.
    const justBefore = item("backup", "just-before", "2026-06-01T11:50:00Z");
    const result = mergeIncidentTimeline(
      [[justBefore]],
      { start: ISO_START, end: ISO_END },
    );
    expect(result.map((r) => r.id)).toEqual(["just-before"]);
  });

  it("excludes items outside the padded window", () => {
    // 20 minutes before started_at — outside the default 15-minute padding.
    const tooEarly = item("backup", "too-early", "2026-06-01T11:40:00Z");
    const result = mergeIncidentTimeline(
      [[tooEarly]],
      { start: ISO_START, end: ISO_END },
    );
    expect(result).toEqual([]);
  });

  it("treats a null end (ongoing incident) as extending to now", () => {
    const recent = item("activity", "recent", new Date().toISOString());
    const ancient = item("activity", "ancient", "2020-01-01T00:00:00Z");
    const result = mergeIncidentTimeline(
      [[recent, ancient]],
      { start: "2026-01-01T00:00:00Z", end: null },
    );
    expect(result.map((r) => r.id)).toEqual(["recent"]);
  });

  it("sorts newest first within a single merged result", () => {
    const older = item("update", "older", "2026-06-01T12:10:00Z");
    const newer = item("update", "newer", "2026-06-01T12:50:00Z");
    const result = mergeIncidentTimeline(
      [[older, newer]],
      { start: ISO_START, end: ISO_END },
    );
    expect(result.map((r) => r.id)).toEqual(["newer", "older"]);
  });

  it("caps each source list to perSourceLimit before merging", () => {
    const items = Array.from({ length: 8 }, (_, i) =>
      item("update", `u${i}`, `2026-06-01T12:${String(i * 5).padStart(2, "0")}:00Z`),
    );
    const result = mergeIncidentTimeline([items], { start: ISO_START, end: ISO_END }, {
      perSourceLimit: 3,
    });
    expect(result).toHaveLength(3);
    // Newest three of the eight: u7, u6, u5 (indices 7,6,5 have the latest timestamps).
    expect(result.map((r) => r.id)).toEqual(["u7", "u6", "u5"]);
  });

  it("caps the final merged result to totalLimit across all sources", () => {
    const backupItems = [item("backup", "b1", "2026-06-01T12:10:00Z")];
    const updateItems = [item("update", "u1", "2026-06-01T12:20:00Z")];
    const activityItems = [item("activity", "a1", "2026-06-01T12:30:00Z")];
    const phpItems = [item("php_error", "p1", "2026-06-01T12:40:00Z")];

    const result = mergeIncidentTimeline(
      [backupItems, updateItems, activityItems, phpItems],
      { start: ISO_START, end: ISO_END },
      { totalLimit: 2 },
    );
    expect(result).toHaveLength(2);
    // Newest two overall: p1 (12:40) then a1 (12:30).
    expect(result.map((r) => r.id)).toEqual(["p1", "a1"]);
  });

  it("drops items with an unparseable timestamp instead of throwing", () => {
    const bad = item("backup", "bad", "not-a-real-date");
    const good = item("backup", "good", "2026-06-01T12:30:00Z");
    const result = mergeIncidentTimeline(
      [[bad, good]],
      { start: ISO_START, end: ISO_END },
    );
    expect(result.map((r) => r.id)).toEqual(["good"]);
  });

  it("returns an empty array when every source list is empty", () => {
    expect(
      mergeIncidentTimeline([[], [], [], []], { start: ISO_START, end: ISO_END }),
    ).toEqual([]);
  });
});
