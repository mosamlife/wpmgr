import { describe, expect, it } from "vitest";

import { ACTION_PRESETS } from "./action-presets";
import { actionLabel } from "./labels";

// The quick filters of the audit page send their value as the action prefix
// the API matches (apps/api/db/query/audit_log.sql ListAuditEntriesFiltered).
// A chip is only as good as the action it is a prefix of.
describe("the audit page's quick filters", () => {
  it("offers a chip for what an AI connection was refused, which is the action the control plane writes", () => {
    // apps/api/internal/audit/audit.go: ActionMCPToolDenied = "mcp.tool.denied".
    const chip = ACTION_PRESETS.find((p) => p.label === "Blocked AI calls");
    expect(chip?.value).toBe("mcp.tool.denied");
    expect(actionLabel(chip?.value ?? "")).toBe("Blocked AI tool call");
  });

  it("keeps All events first and every other value a distinct, non-empty prefix", () => {
    expect(ACTION_PRESETS[0]).toEqual({ label: "All events", value: "" });
    const values = ACTION_PRESETS.slice(1).map((p) => p.value);
    expect(values.every((v) => v.length > 0)).toBe(true);
    expect(new Set(values).size).toBe(values.length);
  });
});
