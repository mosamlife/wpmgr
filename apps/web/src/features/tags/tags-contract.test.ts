import { describe, it, expect } from "vitest";
import type {
  ListSitesData,
  BulkTagApplyRequest,
  SiteTag,
  SiteTagCreate,
  SiteTagUpdate,
} from "@wpmgr/api";

import { Route as SitesIndexRoute } from "@/routes/_authed/sites/index";

// FE CONTRACT TEST (GH #230 "rich tags") — pins the generated @wpmgr/api
// client shapes this feature depends on, plus a round-trip of the Sites
// route's `tags`/`tagMode` URL search params through its real zod schema.
//
// The type-level assertions below are enforced by `pnpm -C apps/web
// typecheck`: if the backend contract renames/removes `tags`, `tags_match`,
// or any BulkTagApplyRequest/SiteTag field, these literal object
// constructions fail to COMPILE — the earliest possible signal, well before
// any runtime request goes out. The `it()` bodies also assert at runtime so
// a plain `vitest run` (no separate typecheck pass) still exercises them.

describe("GH #230 rich tags — @wpmgr/api generated client shapes", () => {
  it("ListSitesData.query accepts tags (repeated) + tags_match (any|all)", () => {
    const query: NonNullable<ListSitesData["query"]> = {
      tags: ["production", "client-a"],
      tags_match: "all",
    };
    expect(query.tags).toEqual(["production", "client-a"]);
    expect(query.tags_match).toBe("all");

    const anyQuery: NonNullable<ListSitesData["query"]> = { tags_match: "any" };
    expect(anyQuery.tags_match).toBe("any");
  });

  it("BulkTagApplyRequest body: site_ids required, add/remove both optional", () => {
    const body: BulkTagApplyRequest = {
      site_ids: ["site-1", "site-2"],
      add: ["staging"],
      remove: ["legacy"],
    };
    expect(body.site_ids).toHaveLength(2);
    expect(body.add).toEqual(["staging"]);
    expect(body.remove).toEqual(["legacy"]);

    // A single-axis bulk apply (add-only, or remove-only) is a valid body.
    const addOnly: BulkTagApplyRequest = { site_ids: ["site-1"], add: ["x"] };
    expect(addOnly.remove).toBeUndefined();
    const removeOnly: BulkTagApplyRequest = { site_ids: ["site-1"], remove: ["x"] };
    expect(removeOnly.add).toBeUndefined();
  });

  it("SiteTag carries id/name/color/usage_count/created_at, color '' meaning auto", () => {
    const tag: SiteTag = {
      id: "tag-1",
      name: "Production",
      color: "",
      usage_count: 3,
      created_at: "2026-01-01T00:00:00Z",
    };
    expect(tag.color).toBe("");
    expect(tag.usage_count).toBe(3);
  });

  it("SiteTagCreate accepts an optional color; SiteTagUpdate accepts name/color/merge", () => {
    const create: SiteTagCreate = { name: "Staging" };
    const createWithColor: SiteTagCreate = { name: "Staging", color: "#3b82f6" };
    expect(create.color).toBeUndefined();
    expect(createWithColor.color).toBe("#3b82f6");

    const update: SiteTagUpdate = { name: "Prod", color: "", merge: true };
    expect(update.merge).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// URL round-trip: tags + tagMode through the Sites route's real searchSchema
// ---------------------------------------------------------------------------

describe("Sites route searchSchema — tags/tagMode round-trip", () => {
  const schema = SitesIndexRoute.options.validateSearch as {
    parse: (input: unknown) => { tags?: string[]; tagMode?: "any" | "all" };
  };

  it("parses tags (existing z.array(z.string()) shape, unchanged)", () => {
    const parsed = schema.parse({ tags: ["production", "client-a"] });
    expect(parsed.tags).toEqual(["production", "client-a"]);
    expect(parsed.tagMode).toBeUndefined();
  });

  it("parses tagMode alongside tags", () => {
    const parsed = schema.parse({ tags: ["a", "b"], tagMode: "all" });
    expect(parsed.tags).toEqual(["a", "b"]);
    expect(parsed.tagMode).toBe("all");
  });

  it("accepts tagMode: 'any' explicitly", () => {
    const parsed = schema.parse({ tags: ["a"], tagMode: "any" });
    expect(parsed.tagMode).toBe("any");
  });

  it("rejects an invalid tagMode value", () => {
    expect(() => schema.parse({ tagMode: "some" })).toThrow();
  });

  it("both tags and tagMode are optional (bare navigation with neither present)", () => {
    const parsed = schema.parse({});
    expect(parsed.tags).toBeUndefined();
    expect(parsed.tagMode).toBeUndefined();
  });

  it("round-trips through a URLSearchParams-style string encode/decode cycle", () => {
    // Simulates how TanStack Router serializes array/string search params to
    // the URL and back — a bare object round trip through the schema is the
    // meaningful assertion (the router owns actual (de)serialization).
    const original = { tags: ["production", "client-a"], tagMode: "all" as const };
    const serialized: unknown = JSON.parse(JSON.stringify(original));
    const parsed = schema.parse(serialized);
    expect(parsed).toEqual(original);
  });
});
