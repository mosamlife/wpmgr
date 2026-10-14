import { describe, expect, it } from "vitest";
import { designedEditCases, designedEditRow, draftDesign, goldenEditCases, goldenEditRow, readEditInput, type DesignedEditInput } from "@/test/designed-page-edit";
import { pageEditRow } from "@/test/ability-request-rows";
import { PATTERN_ICONS, SCALE_RATIOS } from "./outline-model";
import { parsePageEdit } from "./page-edit-model";
import { nativeCardCase, nativeCardCases, nativeEditRow, nativeLinkCase, type NativeGolden } from "@/test/native-page-edit";

describe("native page edit facts", () => {
  it("accepts underscore widget names in input, changes and outline", () => {
    const c = nativeCardCase("add_native");
    const input = JSON.parse(c.input) as { operations: Array<{ widget: string }> };
    input.operations[0]!.widget = "google_maps";
    c.input = JSON.stringify(input);
    c.facts.changes[0]!.native!.widget = "google_maps";
    const ref = c.facts.changes[0]!.new_refs![0];
    const node = c.facts.after_outline.nodes.find((n) => n.ref === ref)!;
    node.widget = "google_maps";
    expect(parsePageEdit(nativeEditRow(c))).not.toBeNull();
  });
  it.each(nativeCardCases)("reads the production card $name", (c) => {
    expect(parsePageEdit(nativeEditRow(c)), c.name).not.toBeNull();
  });

  it("rejects an extra native member", () => {
    const c = nativeCardCase("set_native");
    Reflect.set(c.facts.changes[0]!.native!, "extra", "unshown");
    expect(parsePageEdit(nativeEditRow(c))).toBeNull();
  });

  const mutations: Array<[string, string, (c: NativeGolden) => void]> = [
    ["missing native facts", "set_native", (c) => { delete c.facts.changes[0]!.native; }],
    ["missing widget", "set_native", (c) => { delete c.facts.changes[0]!.native!.widget; }],
    ["missing key name", "set_native", (c) => { Reflect.deleteProperty(c.facts.changes[0]!.native!.keys![0]!, "key"); }],
    ["missing after", "set_native", (c) => { delete c.facts.changes[0]!.native!.keys![0]!.after; }],
    ["missing before wrapper value", "set_native", (c) => { Reflect.deleteProperty(c.facts.changes[0]!.native!.keys![0]!.from_the_site!, "before"); }],
    ["extra before wrapper member", "set_native", (c) => { Reflect.set(c.facts.changes[0]!.native!.keys![0]!.from_the_site!, "extra", true); }],
    ["extra key member", "set_native", (c) => { Reflect.set(c.facts.changes[0]!.native!.keys![0]!, "extra", true); }],
    ["unrequested key", "set_native", (c) => { c.facts.changes[0]!.native!.keys![0]!.key = "unrequested"; }],
    ["different after", "set_native", (c) => { c.facts.changes[0]!.native!.keys![0]!.after = "Other text"; }],
    ["duplicate key", "set_native", (c) => { c.facts.changes[0]!.native!.keys!.push(c.facts.changes[0]!.native!.keys![0]!); }],
    ["removed key without marker", "merge_delete", (c) => { delete c.facts.changes[0]!.native!.keys![0]!.removed; }],
    ["removed key with after", "merge_delete", (c) => { c.facts.changes[0]!.native!.keys![0]!.after = "Kept"; }],
    ["removed key without before", "merge_delete", (c) => { delete c.facts.changes[0]!.native!.keys![0]!.from_the_site; }],
    ["missing new refs", "add_native", (c) => { delete c.facts.changes[0]!.new_refs; }],
    ["missing duplicate widget", "duplicate", (c) => { delete c.facts.changes[0]!.native!.widget; }],
    ["missing duplicate refs", "duplicate", (c) => { delete c.facts.changes[0]!.new_refs; }],
    ["missing remove count", "remove", (c) => { delete c.facts.changes[0]!.native!.removed_count; }],
    ["missing remove outline", "remove", (c) => { delete c.facts.changes[0]!.native!.removed_outline; }],
    ["wrong remove count", "remove", (c) => { c.facts.changes[0]!.native!.removed_count = 99; }],
    ["extra removed node member", "remove", (c) => { Reflect.set(c.facts.changes[0]!.native!.removed_outline![0]!, "extra", true); }],
    ["missing move from", "move", (c) => { delete c.facts.changes[0]!.native!.from; }],
    ["missing move to", "move", (c) => { delete c.facts.changes[0]!.native!.to; }],
    ["missing move parent", "move", (c) => { Reflect.deleteProperty(c.facts.changes[0]!.native!.to!, "parent"); }],
    ["missing move index", "move", (c) => { Reflect.deleteProperty(c.facts.changes[0]!.native!.from!, "index"); }],
    ["extra move place member", "move", (c) => { Reflect.set(c.facts.changes[0]!.native!.from!, "extra", true); }],
    ["negative move index", "move", (c) => { c.facts.changes[0]!.native!.to!.index = -1; }],
    ["wrong destination parent", "move", (c) => { c.facts.changes[0]!.native!.to!.parent = "other"; }],
    ["legacy change member on native", "set_native", (c) => { Reflect.set(c.facts.changes[0]!, "after", "unshown"); }],
  ];
  it.each(mutations)("refuses %s", (_name, golden, mutate) => {
    const c = nativeCardCase(golden);
    mutate(c);
    expect(parsePageEdit(nativeEditRow(c))).toBeNull();
  });

  it.each(["ref", "parent", "kind", "widget", "summary"])("refuses a native outline missing %s", (key) => {
    const c = nativeCardCase("set_native");
    Reflect.deleteProperty(c.facts.after_outline.nodes.find((n) => n.kind === "native")!, key);
    expect(parsePageEdit(nativeEditRow(c))).toBeNull();
  });

  it.each(["extra", "editable", "from_the_site", "label", "level"])("refuses a native outline with %s", (key) => {
    const c = nativeCardCase("set_native");
    Reflect.set(c.facts.after_outline.nodes.find((n) => n.kind === "native")!, key,
      key === "editable" ? [] : key === "from_the_site" ? {} : key === "level" ? 2 : "extra");
    expect(parsePageEdit(nativeEditRow(c))).toBeNull();
  });

  it("reads complete links and refuses missing, changed or extra link members", () => {
    expect(parsePageEdit(nativeEditRow(nativeLinkCase()))).not.toBeNull();
    for (const key of ["text", "external_domain"]) {
      const c = nativeLinkCase();
      Reflect.deleteProperty(c.facts.changes[0]!.native!.keys![1]!.links![0]!, key);
      expect(parsePageEdit(nativeEditRow(c)), key).toBeNull();
    }
    const c = nativeLinkCase();
    Reflect.set(c.facts.changes[0]!.native!.keys![1]!.links![0]!, "extra", true);
    expect(parsePageEdit(nativeEditRow(c))).toBeNull();
  });

  it("accepts unchanged settings omitted by the producer", () => {
    const c = nativeCardCase("set_native");
    const input = JSON.parse(c.input) as { operations: Array<{ settings: unknown }> };
    input.operations[0]!.settings = { title: "Counter" };
    c.input = JSON.stringify(input);
    delete c.facts.changes[0]!.native!.keys;
    expect(parsePageEdit(nativeEditRow(c))).not.toBeNull();
  });

  it("accepts empty settings and a first placement on an added widget", () => {
    const c = nativeCardCase("add_native");
    const input = JSON.parse(c.input) as { operations: Array<{ settings: unknown; position?: string }> };
    input.operations[0]!.settings = [];
    input.operations[0]!.position = "first";
    c.input = JSON.stringify(input);
    delete c.facts.changes[0]!.native!.keys;
    expect(parsePageEdit(nativeEditRow(c))).not.toBeNull();
  });

  it("accepts links with literal host case and a different object key order", () => {
    const c = nativeLinkCase();
    const urls = { z: "https://Outside.example/z", a: "https://another.example/a" };
    const input = JSON.parse(c.input) as { operations: Array<{ settings: Record<string, unknown> }> };
    input.operations[0]!.settings.link = urls;
    c.input = JSON.stringify(input);
    const key = c.facts.changes[0]!.native!.keys![1]!;
    key.after = { a: urls.a, z: urls.z };
    key.links = [{ text: urls.z, external_domain: "Outside.example" }, { text: urls.a, external_domain: "another.example" }];
    expect(parsePageEdit(nativeEditRow(c))).not.toBeNull();
  });

  it.each(["set_native", "add_native", "duplicate", "move", "remove"])("refuses extra input members for %s", (name) => {
    const c = nativeCardCase(name);
    const input = JSON.parse(c.input) as { operations: Array<Record<string, unknown>> };
    input.operations[0]!.extra = "unshown";
    c.input = JSON.stringify(input);
    expect(parsePageEdit(nativeEditRow(c))).toBeNull();
  });

  it.each(["set_native", "add_native", "duplicate", "move", "remove"])("refuses each missing required input member for %s", (name) => {
    const c = nativeCardCase(name);
    const original = JSON.parse(c.input) as { operations: Array<Record<string, unknown>> };
    for (const key of Object.keys(original.operations[0]!).filter((k) => k !== "position")) {
      const input = structuredClone(original);
      delete input.operations[0]![key];
      expect(parsePageEdit(nativeEditRow({ ...c, input: JSON.stringify(input) })), key).toBeNull();
    }
  });

  it.each([true, 42, { title: true }, { title: 42 }, { title: { nested: null } }])("refuses unsupported input setting scalars %j", (settings) => {
    const c = nativeCardCase("set_native");
    const input = JSON.parse(c.input) as { operations: Array<{ settings: unknown }> };
    input.operations[0]!.settings = settings;
    c.input = JSON.stringify(input);
    expect(parsePageEdit(nativeEditRow(c))).toBeNull();
  });
});

function withInput(change: (input: DesignedEditInput) => void) {
  const row = designedEditRow();
  const input = readEditInput(row.input_json);
  change(input);
  return { ...row, input_json: JSON.stringify(input) };
}

describe("designed page edits", () => {
  it("shows the draft design, a section insert, a pattern replace and a pattern text change", () => {
    const view = parsePageEdit(designedEditRow());
    expect(view).not.toBeNull();
    expect(view?.lines.map((l) => l.op)).toEqual(["set_text", "insert", "replace"]);
    expect(view?.lines[0]).toMatchObject({ was: "Before this change", now: "49" });
    expect(view?.lines[0]?.title.map((s) => s.s).join("")).toContain("Plan 2 · price");
    expect(view?.after?.nodes.flatMap((n) => n.children).find((n) => n.ref === "e3c0002")?.texts)
      .toContainEqual({ field: "plans.1.price", text: "49" });
  });

  it.each(designedEditCases.filter((c) => c.agent === "ok" && c.name.startsWith("pattern-path")))("shows shared text case $name", (c) => {
      const row = designedEditRow([c.name]);
      const view = parsePageEdit(row);
      expect(view, c.name).not.toBeNull();
      const ops = readEditInput(c.input).operations;
      expect(view?.lines.map((l) => l.now)).toEqual(ops.map((o) => o.text));
    });

  it.each(["insert-a-pattern-into-a-section", "replace-a-pattern-to-add-an-item",
    "insert-a-section-after-a-section", "replace-a-section-keeping-its-anchor",
    "insert-a-hero-section-before-the-first-section"])("shows shared outline case %s", (name) => {
    expect(parsePageEdit(designedEditRow([name]))).not.toBeNull();
  });

  it.each(["pattern-path-leading-zero", "pattern-path-names-the-pattern-not-its-array",
    "pattern-path-without-a-field", "pattern-path-uppercase", "pattern-path-four-steps-deep",
    "pattern-path-trailing-line-break", "pattern-path-not-offered-by-the-node", "pattern-path-on-a-plain-node",
    "insert-a-pattern-with-two-features", "insert-a-pattern-with-an-unknown-icon",
    "insert-a-section-with-an-uppercase-anchor", "insert-a-section-with-a-reserved-anchor"])("refuses shared malformed case %s", (name) => {
      expect(parsePageEdit(designedEditRow([name]))).toBeNull();
    });

  it.each([null, {}, { ...draftDesign, density: "tight" },
    { ...draftDesign, colors: { ...draftDesign.colors, ink: draftDesign.colors.ground } },
    { ...draftDesign, extra: true }])("refuses malformed design %j", (design) => {
    expect(parsePageEdit(withInput((input) => { input.design = design; }))).toBeNull();
  });

  it.each(SCALE_RATIOS)("shares the design reader's ratio %s", (ratio) => {
    expect(parsePageEdit(withInput((input) => { input.design = { ...draftDesign, scale: { ...draftDesign.scale, ratio } }; }))).not.toBeNull();
  });

  it.each(PATTERN_ICONS)("shares the pattern reader's icon %s", (icon) => {
    const row = designedEditRow(["insert-a-pattern-into-a-section"]);
    const input = readEditInput(row.input_json);
    const node = input.operations[0]?.outline?.[0];
    if (node?.type !== "pattern" || node.pattern !== "features") throw new Error("Missing features pattern");
    node.features[0]!.icon = icon;
    expect(parsePageEdit({ ...row, input_json: JSON.stringify(input) })).not.toBeNull();
  });

  it.each(goldenEditCases)("reads the site's checked designed preview $name", (c) => {
    const view = parsePageEdit(goldenEditRow(c));
    expect(view, c.name).not.toBeNull();
    const ops = readEditInput(c.input).operations;
    expect(view?.lines.map((line) => line.op)).toEqual(ops.map((op) => op.op));
    for (const [i, op] of ops.entries()) if (op.op === "set_text") {
      expect(view?.lines[i]?.now).toBe(op.text);
      expect(view?.lines[i]?.was).toBe(c.answer.preview?.changes[i]?.before?.[op.field!]);
    }
  });

  it("keeps plain edits showable and refuses unseen input members", () => {
    expect(parsePageEdit(pageEditRow())).not.toBeNull();
    expect(parsePageEdit(withInput((input) => { input.page_frame = "canvas"; }))).toBeNull();
  });

  it("requires the checked text to agree with the requested pattern path", () => {
    const row = designedEditRow(["pattern-path-price"]);
    row.page_edit!.changes[0]!.after = "Different";
    expect(parsePageEdit(row)).toBeNull();
  });
});
