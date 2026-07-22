/**
 * Unit tests for the Sites uptime badge helper (GH #272).
 *
 * Regression coverage: a never-probed site (`up` is `null` or `undefined`)
 * must render as a neutral "Unknown", never as a green "Up" — a fully-down
 * site with no probe result yet must not display a false "Up" during an
 * incident.
 */
import { describe, it, expect } from "vitest";

import { siteUptimeBadge, siteUptimeTextClass } from "@/features/sites/uptime-badge";

describe("siteUptimeBadge — tri-state mapping (GH #272)", () => {
  it("up === true -> 'Up' / success", () => {
    expect(siteUptimeBadge(true)).toEqual({
      status: "up",
      label: "Up",
      tone: "success",
    });
  });

  it("up === false -> 'Down' / destructive", () => {
    expect(siteUptimeBadge(false)).toEqual({
      status: "down",
      label: "Down",
      tone: "destructive",
    });
  });

  it("up === undefined -> 'Unknown' / muted (never success)", () => {
    const badge = siteUptimeBadge(undefined);
    expect(badge.status).toBe("unknown");
    expect(badge.label).toBe("Unknown");
    expect(badge.tone).toBe("muted");
    expect(badge.tone).not.toBe("success");
  });

  it("up === null -> 'Unknown' / muted (never success)", () => {
    const badge = siteUptimeBadge(null);
    expect(badge.status).toBe("unknown");
    expect(badge.label).toBe("Unknown");
    expect(badge.tone).toBe("muted");
    expect(badge.tone).not.toBe("success");
  });
});

describe("siteUptimeTextClass — sites-table color mapping", () => {
  it("uses the foreground token for up", () => {
    expect(siteUptimeTextClass("up")).toBe("text-[var(--color-foreground)]");
  });

  it("uses the destructive token for down", () => {
    expect(siteUptimeTextClass("down")).toBe(
      "text-[var(--color-destructive)]",
    );
  });

  it("uses the muted-foreground token for unknown (not destructive, not foreground)", () => {
    const cls = siteUptimeTextClass("unknown");
    expect(cls).toBe("text-[var(--color-muted-foreground)]");
    expect(cls).not.toBe("text-[var(--color-destructive)]");
    expect(cls).not.toBe("text-[var(--color-foreground)]");
  });
});
