import { describe, it, expect } from "vitest";
import type { UpdateEvent, UpdateRun, UpdateTask } from "@wpmgr/api";

import { applyEvent } from "./use-updates";

// Unit coverage for `applyEvent` — the pure reducer the SSE handler calls to
// patch the run-detail cache. Pinning its contract here decouples the cache
// shape from the React tree, which is the layer that the v0.9.0 "stuck at
// Queued" bug actually surfaced through (the named-event mis-listen happened
// in the SSE wiring, but the reducer is what re-projects the row state once
// the frame is delivered, so getting it wrong here would resurrect the same
// symptom).

const RUN_ID = "11111111-1111-1111-1111-111111111111";
const TENANT_ID = "22222222-2222-2222-2222-222222222222";
const SITE_ID = "33333333-3333-3333-3333-333333333333";
const TASK_ID = "44444444-4444-4444-4444-444444444444";

function makeRun(taskStatus: UpdateTask["status"]): UpdateRun {
  return {
    id: RUN_ID,
    tenant_id: TENANT_ID,
    status: "pending",
    dry_run: false,
    created_at: "2026-05-29T00:00:00Z",
    updated_at: "2026-05-29T00:00:00Z",
    tasks: [
      {
        id: TASK_ID,
        run_id: RUN_ID,
        tenant_id: TENANT_ID,
        site_id: SITE_ID,
        target_type: "plugin",
        target_slug: "woocommerce/woocommerce.php",
        status: taskStatus,
        created_at: "2026-05-29T00:00:00Z",
        updated_at: "2026-05-29T00:00:00Z",
      },
    ],
  };
}

describe("applyEvent", () => {
  it("transitions a matching task to the event status and lifts the run-level status", () => {
    const prev = makeRun("pending");
    const ev: UpdateEvent = {
      run_id: RUN_ID,
      task_id: TASK_ID,
      site_id: SITE_ID,
      target_type: "plugin",
      target_slug: "woocommerce/woocommerce.php",
      status: "succeeded",
      from_version: "10.8.0",
      to_version: "10.8.1",
      detail: "updated and healthy",
      run_status: "completed",
    };
    const next = applyEvent(prev, ev);
    expect(next.status).toBe("completed");
    const updated = next.tasks?.[0];
    expect(updated).toBeDefined();
    expect(updated?.status).toBe("succeeded");
    expect(updated?.to_version).toBe("10.8.1");
    expect(updated?.detail).toBe("updated and healthy");
  });

  it("appends an unseen task instead of dropping the event", () => {
    const prev = makeRun("pending");
    const otherTaskId = "55555555-5555-5555-5555-555555555555";
    const ev: UpdateEvent = {
      run_id: RUN_ID,
      task_id: otherTaskId,
      site_id: SITE_ID,
      target_type: "theme",
      target_slug: "bricks",
      status: "running",
      run_status: "running",
    };
    const next = applyEvent(prev, ev);
    expect(next.tasks).toHaveLength(2);
    expect(next.tasks?.find((t) => t.id === otherTaskId)?.status).toBe("running");
  });
});
