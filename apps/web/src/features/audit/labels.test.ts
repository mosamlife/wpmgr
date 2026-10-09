/**
 * Tests for the audit-log action label map + severity classifier.
 *
 * Following the project convention (scan-findings.test.ts, use-hardening.test.ts):
 * pure-function tests only; no React renderer, no DOM.
 *
 * Contract goal: the redesign's two hard rules must hold for every key the
 * control plane actually emits (apps/api/internal/audit/audit.go plus each
 * domain's Record call sites) —
 *   1. a raw dotted action key never becomes the visible label.
 *   2. denied/sensitive/write are each correctly separated from the quiet
 *      "read" default, since that separation drives both the row's rail/pill
 *      and the read-burst collapsing eligibility in group-runs.ts.
 */
import { describe, it, expect } from "vitest";

import { actionLabel, classifySeverity } from "./labels";
import { humanizeTargetType } from "./metadata";

describe("actionLabel", () => {
  it("returns a hand-written label for a known key", () => {
    expect(actionLabel("site.files.read")).toBe("Read file");
    expect(actionLabel("backup.started")).toBe("Started backup");
  });

  it("never returns a raw dotted key, even for an unknown action", () => {
    const label = actionLabel("some.brand.new_event.type");
    expect(label).not.toContain(".");
    expect(label).toBe("Some brand new event type");
  });

  it("labels a denied variant distinctly rather than a generic suffix", () => {
    expect(actionLabel("site.files.delete.denied")).toBe("Blocked file deletion");
  });

  it("falls back to a recursive '(denied)' suffix for an unmapped denied key", () => {
    const label = actionLabel("some.new_write.denied");
    expect(label).toBe("Some new write (denied)");
    expect(label).not.toContain(".");
  });
});

describe("classifySeverity", () => {
  it("classifies any '.denied' action as denied, regardless of domain", () => {
    expect(classifySeverity("site.files.delete.denied")).toBe("denied");
    expect(classifySeverity("site.files.versions.list.denied")).toBe("denied");
    expect(classifySeverity("some.unknown.denied")).toBe("denied");
  });

  it("classifies security/credential/access-control actions as sensitive", () => {
    expect(classifySeverity("site_security_hardening.update")).toBe("sensitive");
    expect(classifySeverity("smtp.settings.update")).toBe("sensitive");
    expect(classifySeverity("site.cache.disabled")).toBe("sensitive");
    expect(classifySeverity("auth.login.failure")).toBe("sensitive");
    expect(classifySeverity("apikey.create")).toBe("sensitive");
  });

  it("classifies real mutations as write", () => {
    expect(classifySeverity("site.files.write")).toBe("write");
    expect(classifySeverity("site.files.delete")).toBe("write");
    expect(classifySeverity("site.files.mkdir")).toBe("write");
    expect(classifySeverity("restore.completed")).toBe("write");
    expect(classifySeverity("site.tags.set")).toBe("write");
    expect(classifySeverity("site.db.search.replace")).toBe("write");
  });

  it("keeps genuine reads quiet (the whole point of the redesign)", () => {
    expect(classifySeverity("site.files.read")).toBe("read");
    expect(classifySeverity("site.files.search")).toBe("read");
    expect(classifySeverity("site.files.versions.list")).toBe("read");
    expect(classifySeverity("auth.login.success")).toBe("read");
    expect(classifySeverity("site_diagnostics.refresh")).toBe("read");
  });

  it("overrides the read-only media-clean listing endpoints even though they contain a write-shaped segment", () => {
    expect(classifySeverity("site.media.clean.scan")).toBe("read");
    expect(classifySeverity("site.media.clean.quarantine")).toBe("read");
    // the actual mutating siblings stay writes
    expect(classifySeverity("site.media.clean.isolate")).toBe("write");
    expect(classifySeverity("site.media.clean.delete")).toBe("write");
  });
});

// AI cache-clear requests (tracka-cache-purge design v7, S2.7). Every action
// key m151's rail and worker actually write, exercised the same two ways as
// the rest of this file: the label is never a raw dotted key, and the
// severity separates "asked/approved/failed" from "nothing ran".
describe("AI cache-clear request actions", () => {
  it("labels every lifecycle key distinctly, never as a raw dotted key", () => {
    expect(actionLabel("mcp.tool.called")).toBe("AI tool call");
    expect(actionLabel("assistant.request.approved")).toBe("Approved AI cache-clear request");
    expect(actionLabel("assistant.request.declined")).toBe("Declined AI cache-clear request");
    expect(actionLabel("assistant.request.withdrawn")).toBe("Withdrew AI cache-clear request");
    expect(actionLabel("assistant.request.not_sent")).toBe("AI cache-clear request not sent");
    expect(actionLabel("assistant.request.dispatched")).toBe("Sent AI cache-clear request");
    expect(actionLabel("assistant.request.expired")).toBe(
      "AI cache-clear request expired unanswered",
    );
    expect(actionLabel("assistant.request.failed")).toBe("AI cache-clear request failed");
  });

  it("labels the denial through the exact key, not the recursive '.denied' fallback", () => {
    // mcp.tool.denied ends in ".denied", so without its own ACTION_LABELS
    // entry this would fall through to the generic recursive-suffix path
    // (actionLabel("mcp.tool") + " (denied)"). It has its own entry instead.
    expect(actionLabel("mcp.tool.denied")).toBe("Blocked AI tool call");
  });

  it("classifies the denial as denied like every other '.denied' action", () => {
    expect(classifySeverity("mcp.tool.denied")).toBe("denied");
  });

  it("classifies the request and its approval as sensitive, not a quiet read", () => {
    expect(classifySeverity("mcp.tool.called")).toBe("sensitive");
    expect(classifySeverity("assistant.request.approved")).toBe("sensitive");
    expect(classifySeverity("assistant.request.failed")).toBe("sensitive");
  });

  it("classifies the moment WPMgr actually sends the clear as a write", () => {
    // "dispatched" carries no write-shaped stem, so this only passes because
    // of the explicit WRITE_OVERRIDES entry.
    expect(classifySeverity("assistant.request.dispatched")).toBe("write");
  });

  it("keeps the non-effecting closures quiet: nothing ran, so nothing is sensitive", () => {
    expect(classifySeverity("assistant.request.declined")).toBe("read");
    expect(classifySeverity("assistant.request.withdrawn")).toBe("read");
    expect(classifySeverity("assistant.request.not_sent")).toBe("read");
    expect(classifySeverity("assistant.request.expired")).toBe("read");
  });
});

describe("AI page-creation actions", () => {
  it("labels the completion, the undo and the switch in plain words", () => {
    expect(actionLabel("assistant.request.completed")).toBe("AI change applied");
    expect(actionLabel("assistant.request.undone")).toBe("AI change undone");
    expect(actionLabel("site.content_editing.enabled")).toBe("AI page creation turned on");
  });

  it("classifies the applied change and its undo as writes, and the switch as sensitive", () => {
    expect(classifySeverity("assistant.request.completed")).toBe("write");
    expect(classifySeverity("assistant.request.undone")).toBe("write");
    expect(classifySeverity("site.content_editing.enabled")).toBe("sensitive");
  });

  it("names the assistant_ability_request target type", () => {
    expect(humanizeTargetType("assistant_ability_request")).toBe("AI change request");
  });
});

describe("AI page request lifecycle actions", () => {
  it("pins each label and severity", () => {
    const expected: Array<[string, string, string]> = [
      ["assistant.ability_request.approved", "Approved AI page request", "sensitive"],
      ["assistant.ability_request.declined", "Declined AI page request", "read"],
      ["assistant.ability_request.expired", "AI page request expired unanswered", "read"],
      ["assistant.ability_request.withdrawn", "Withdrew AI page request", "read"],
      ["assistant.ability_request.not_sent", "AI page request not sent", "read"],
      ["assistant.ability_request.dispatched", "Sent AI page request to the site", "write"],
      ["assistant.ability_request.failed", "AI page request failed", "sensitive"],
    ];
    for (const [key, label, severity] of expected) {
      expect(actionLabel(key)).toBe(label);
      expect(classifySeverity(key)).toBe(severity);
    }
  });
});
