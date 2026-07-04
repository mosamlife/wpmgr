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
