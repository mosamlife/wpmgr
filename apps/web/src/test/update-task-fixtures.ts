import type { UpdateTask } from "@wpmgr/api";

import { isRetryWireTask } from "@/features/updates/retry-contract";

// Test support for the Updates domain (GH #336).
//
// Every `UpdateTask` fixture in the suite must be a shape the control plane
// actually produces, not a hand-built object that happens to satisfy the
// TypeScript type. GH #322 shipped a feature that rendered nothing while its
// tests passed against fixtures no server ever emitted; the two guards here
// exist so that cannot happen to the retry surface:
//
//   1. `serverRetryFields` mirrors the control plane's own `retryClassify`
//      (apps/api/internal/update/model.go) EXACTLY, so a fixture's
//      retryable/retry_class pair is always the pair the server would have
//      written for that status. It is TEST ONLY: application code reads the
//      server's fields and never re-derives them.
//   2. `parseWireTask` / `parseWireRunTasks` run raw JSON through the same
//      runtime guard the application uses, so a fixture missing a required
//      field fails the test instead of silently proving nothing.

/**
 * The control plane's retryClassify(status), mirrored for fixtures.
 *
 * apps/api/internal/update/model.go:288
 *   not terminal            -> (false, not_applicable)
 *   cancelled               -> (true,  never_ran)
 *   failed                  -> (true,  failed)
 *   rolled_back             -> (true,  reverted)
 *   skipped                 -> (true,  skipped)
 *   succeeded               -> (false, not_applicable)
 */
export function serverRetryFields(
  status: UpdateTask["status"],
): Pick<UpdateTask, "retryable" | "retry_class"> {
  switch (status) {
    case "cancelled":
      return { retryable: true, retry_class: "never_ran" };
    case "failed":
      return { retryable: true, retry_class: "failed" };
    case "rolled_back":
      return { retryable: true, retry_class: "reverted" };
    case "skipped":
      return { retryable: true, retry_class: "skipped" };
    default:
      // succeeded, pending, running
      return { retryable: false, retry_class: "not_applicable" };
  }
}

/**
 * Build a task exactly as the run detail response carries it. The retry pair
 * is derived from the FINAL status, so a fixture can never claim a
 * combination the server would not write, and an explicit override still
 * wins for the tests that need a deliberately odd row.
 */
export function makeUpdateTask(overrides: Partial<UpdateTask> = {}): UpdateTask {
  const merged: Omit<UpdateTask, "retryable" | "retry_class"> = {
    // A REAL UUID. The retry endpoint parses task_ids as UUIDs and 422s on
    // anything else, so a fixture id like "task-1" builds a request the server
    // would refuse, which is the kind of fiction this file exists to prevent.
    id: "44444444-4444-4444-4444-444444444444",
    run_id: "11111111-1111-1111-1111-111111111111",
    tenant_id: "22222222-2222-2222-2222-222222222222",
    site_id: "33333333-3333-3333-3333-333333333333",
    target_type: "plugin",
    target_slug: "akismet/akismet.php",
    status: "failed",
    created_at: "2026-08-04T00:00:00Z",
    updated_at: "2026-08-04T00:00:00Z",
    ...overrides,
  };

  // retryable and retry_class are DERIVED FROM THE FINAL STATUS, exactly as the
  // server derives them, and an explicit override still wins so a test can
  // build a deliberately odd row.
  //
  // Previously the defaults carried retryable:true / retry_class:"failed" and
  // were spread AFTER serverRetryFields, so the server mirror was dead code and
  // makeUpdateTask({status:"succeeded"}) produced retryable:true with
  // retry_class:"failed", a pair the server cannot emit. A fixture that models
  // an impossible server response is worse than no fixture, and this file was
  // written to stop precisely that.
  return {
    ...merged,
    ...serverRetryFields(merged.status),
    ...(overrides.retryable !== undefined ? { retryable: overrides.retryable } : {}),
    ...(overrides.retry_class !== undefined ? { retry_class: overrides.retry_class } : {}),
  };
}

/** JSON round trip, so a fixture is a value that survived the wire. */
function toWire(value: unknown): unknown {
  return JSON.parse(JSON.stringify(value)) as unknown;
}

/**
 * Round-trip one task through JSON and the application's own wire guard.
 * Throws when the fixture is not a shape the server produces.
 */
export function parseWireTask(value: unknown): UpdateTask {
  const wire = toWire(value);
  if (!isRetryWireTask(wire)) {
    throw new Error(
      `fixture is not a valid UpdateTask wire shape: ${JSON.stringify(wire)}`,
    );
  }
  return wire;
}

/** The same guard over a whole run's task array. */
export function parseWireRunTasks(values: readonly unknown[]): UpdateTask[] {
  return values.map((value) => parseWireTask(value));
}
