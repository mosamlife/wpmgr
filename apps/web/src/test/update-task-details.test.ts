import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { describe, expect, it } from "vitest";

import {
  CLOCK_403_DETAIL,
  CLOCK_403_RAW_ERROR,
  CORE_LEFT_AS_IS_DETAIL,
  CORE_NO_CHANGE_UNHEALTHY_DETAIL,
  ROLLBACK_RAW_ERROR,
  CORE_ROLLBACK_UNDELIVERABLE_DETAIL,
  FIREWALL_403_DETAIL,
  FIREWALL_403_RAW_ERROR,
  PLUGIN_SITE_DOWN_DETAIL,
  SKIP_CORE_MANAGED_DETAIL,
  SKIP_FILE_MODS_DISALLOWED_DETAIL,
  SKIP_NOT_INSTALLED_DETAIL,
  SKIP_SELF_TARGET_DETAIL,
} from "./update-task-details";

// The display tests for update outcomes quote the control plane's own
// sentences (./update-task-details.ts). This suite reads the Go source they
// were copied from and holds each constant to it, so a reworded sentence fails
// here, by name, instead of leaving the display tests green against text the
// server no longer writes.
//
// Resolved from the vitest root (apps/web), the same way
// features/email/credential-audience-contract.test.ts reaches the Go source.
const WORKER_GO = resolve(process.cwd(), "../api/internal/update/worker.go");
const COMMAND_ERROR_GO = resolve(
  process.cwd(),
  "../api/internal/agentcmd/command_error.go",
);
const ROUTER_PHP = resolve(process.cwd(), "../agent/includes/class-router.php");

/**
 * The value of the Go expression that starts at `start`: interpreted string
 * literals joined by "+", across lines. It throws on anything else, so a
 * restructured constant fails here instead of reading as an empty string.
 */
function goStringAt(source: string, start: number): string {
  let out = "";
  let i = start;
  for (;;) {
    while (/\s/.test(source.charAt(i))) i++;
    if (source.charAt(i) !== '"') {
      throw new Error(
        `expected a Go string literal at offset ${i}, found: ${source.slice(i, i + 40)}`,
      );
    }
    let j = i + 1;
    while (j < source.length && source.charAt(j) !== '"') {
      j += source.charAt(j) === "\\" ? 2 : 1;
    }
    out += JSON.parse(source.slice(i, j + 1)) as string;
    let k = j + 1;
    while (/\s/.test(source.charAt(k))) k++;
    if (source.charAt(k) !== "+") return out;
    i = k + 1;
  }
}

function indexOrThrow(source: string, marker: string): number {
  const at = source.indexOf(marker);
  if (at < 0) {
    throw new Error(
      `${marker} not found. It is where these fixtures were copied from; if it moved or was reworded, refresh ./update-task-details.ts from its new home.`,
    );
  }
  return at;
}

/** The string a Go declaration `marker` assigns. */
function goStringAfter(source: string, marker: string): string {
  return goStringAt(source, indexOrThrow(source, marker) + marker.length);
}

describe("update task detail fixtures", () => {
  const worker = readFileSync(WORKER_GO, "utf8");
  const commandError = readFileSync(COMMAND_ERROR_GO, "utf8");
  const router = readFileSync(ROUTER_PHP, "utf8");

  it("the three core outcomes are the strings worker.go writes", () => {
    expect(CORE_ROLLBACK_UNDELIVERABLE_DETAIL).toBe(
      goStringAfter(worker, "const coreRollbackUndeliverableDetail = "),
    );
    expect(CORE_LEFT_AS_IS_DETAIL).toBe(
      goStringAfter(worker, "const coreLeftAsIsDetail = "),
    );
    expect(CORE_NO_CHANGE_UNHEALTHY_DETAIL).toBe(
      goStringAfter(worker, "const coreNoChangeUnhealthyDetail = "),
    );
  });

  it("the sentences for a skip that says why are the ones worker.go writes (GH #367)", () => {
    expect(SKIP_CORE_MANAGED_DETAIL).toBe(
      goStringAfter(worker, "const skipCoreManagedDetail = "),
    );
    expect(SKIP_FILE_MODS_DISALLOWED_DETAIL).toBe(
      goStringAfter(worker, "const skipFileModsDisallowedDetail = "),
    );
    expect(SKIP_NOT_INSTALLED_DETAIL).toBe(
      goStringAfter(worker, "const skipNotInstalledDetail = "),
    );
    expect(SKIP_SELF_TARGET_DETAIL).toBe(
      goStringAfter(worker, "const skipSelfTargetDetail = "),
    );
  });

  it("the plugin and theme site-down sentence is the one worker.go writes", () => {
    const start = indexOrThrow(
      worker,
      '"site not responding: site-wide PHP fatal after update;',
    );
    expect(PLUGIN_SITE_DOWN_DETAIL).toBe(goStringAt(worker, start));
  });

  it("the firewall refusal is ForbiddenMessage's lead plus its fixed advice", () => {
    const lead = goStringAfter(commandError, "lead := action + ");
    const advice = goStringAfter(commandError, "forbiddenFirewallAdvice = ");
    expect(FIREWALL_403_DETAIL).toBe("Update" + lead + advice);
  });

  it("the clock refusal is ForbiddenMessage's lead plus its format, filled the way it fills it", () => {
    const lead = goStringAfter(commandError, "lead := action + ");
    // again := "then run the " + strings.ToLower(action) + " again." is not a
    // plain literal, so its two literals are read and joined around the action.
    expect(commandError).toContain(
      'again := "then run the " + strings.ToLower(action) + " again."',
    );
    const again = "then run the update again.";
    const caseAt = indexOrThrow(
      commandError,
      'case code == "wpmgr_token_expired" || code == "wpmgr_token_skew":',
    );
    const sprintfAt = commandError.indexOf("fmt.Sprintf(", caseAt);
    expect(sprintfAt).toBeGreaterThan(caseAt);
    const format = goStringAt(
      commandError,
      sprintfAt + "fmt.Sprintf(".length,
    );
    const filled = format
      .replace("%s", "wpmgr_token_expired")
      .replace("%s", again);
    expect(CLOCK_403_DETAIL).toBe("Update" + lead + filled);
  });

  it("the raw error is the format CommandError.Error() writes, over the agent's own 403 body", () => {
    expect(commandError).toContain(
      'fmt.Sprintf("%s command rejected by agent: status %d body=%s", e.Command, e.Status, e.snippet)',
    );
    expect(FIREWALL_403_RAW_ERROR).toMatch(
      /^update command rejected by agent: status 403 body=/,
    );
    expect(ROLLBACK_RAW_ERROR).toMatch(
      /^rollback command rejected by agent: status 500 body=/,
    );
    expect(CLOCK_403_RAW_ERROR).toMatch(
      /^update command rejected by agent: status 403 body=\{"code":"wpmgr_token_expired","message":"Forbidden\.","data":\{"status":403\}\}$/,
    );
    // The agent's refusal envelope is the one the clock body copies.
    expect(router).toContain(
      "new \\WP_Error('wpmgr_' . $code, 'Forbidden.', ['status' => 403])",
    );
    // And the agent names the code that body carries.
    expect(readFileSync(resolve(process.cwd(), "../agent/includes/class-token-failure.php"), "utf8")).toContain(
      "case TokenExpired = 'token_expired';",
    );
  });
});
