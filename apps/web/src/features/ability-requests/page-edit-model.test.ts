import { describe, expect, it } from "vitest";
import { designedEditCases, designedEditRow, draftDesign, goldenEditCases, goldenEditRow, readEditInput, type DesignedEditInput } from "@/test/designed-page-edit";
import { pageEditRow } from "@/test/ability-request-rows";
import { PATTERN_ICONS, SCALE_RATIOS } from "./outline-model";
import { parsePageEdit } from "./page-edit-model";

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
