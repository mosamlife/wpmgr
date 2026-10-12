import { readFileSync } from "node:fs";
import { join } from "node:path";
import { designSchema, type DesignedEditNode, type PageDesign } from "@/features/ability-requests/outline-model";
import { pageEditRow, type PageEditRow } from "./ability-request-rows";

const fixture = <T>(name: string): T => JSON.parse(readFileSync(
  join(process.cwd(), "..", "agent", "tests", "fixtures", "ability-run", name), "utf8",
)) as T;

interface Op {
  op: string;
  ref?: string;
  field?: string;
  text?: string;
  after?: string;
  before?: string;
  into?: string;
  position?: string;
  outline?: DesignedEditNode[];
}
interface Node {
  ref: string;
  parent: string;
  kind: string;
  level?: number;
  editable: string[];
  from_the_site?: Record<string, string>;
}
interface SharedCase { name: string; input: string; agent: string; go: string }
const shared = fixture<{
  v3_page: { nodes: Node[] };
  v3_cases: SharedCase[];
}>("page-edit-ops-cases.json");
export const designedEditCases = shared.v3_cases;
interface GoldenCase {
  name: string;
  input: string;
  expect: string;
  answer: { preview?: {
    post_id: number; title: string; builder_version: string; format: string;
    after_outline: NonNullable<PageEditRow["after_outline"]>;
    changes: Array<{ op: string; ref?: string; kind?: string; before?: Record<string, string>;
      after?: Record<string, string>; new_refs?: string[]; anchor?: unknown }>;
  } };
}
const golden = fixture<{ pages: { landing: { create_input: string } }; cases: GoldenCase[] }>("page-edit-designed-previews.json");
const create = JSON.parse(golden.pages.landing.create_input) as { design: unknown };
export const draftDesign: PageDesign = designSchema.parse(create.design);
export const goldenEditCases = golden.cases.filter((c) => c.expect === "prechecked");

export interface DesignedEditInput {
  post_id: number;
  base_fingerprint: string;
  operations: Op[];
  design: unknown;
  [key: string]: unknown;
}
export function readEditInput(input: string): DesignedEditInput {
  return JSON.parse(input) as DesignedEditInput;
}

/** The card conversion follows buildPageEditCardFacts in mcp/page_edit_precheck.go. */
export function goldenEditRow(c: GoldenCase) {
  const p = c.answer.preview;
  if (!p) throw new Error(`Missing checked preview: ${c.name}`);
  const changes = p.changes.map(({ before, after, ...change }) => ({ ...change,
    ...(before ? { from_the_site: { before } } : {}),
    ...(after ? { field: Object.keys(after)[0], after: Object.values(after)[0] } : {}),
  }));
  const facts = { kind: "builder_edit", post: { id: p.post_id, from_the_site: { title: p.title } },
    builder: { id: "elementor", version: p.builder_version, format: p.format }, changes,
    after_outline: p.after_outline, checked_at: "2026-10-01T09:52:00Z" };
  return pageEditRow({ input_json: c.input, page_edit: facts as PageEditRow });
}

export function designedEditRow(names: string[] = [
  "pattern-path-price", "insert-a-section-after-a-section", "replace-a-pattern-to-add-an-item",
]) {
  const inputs = names.map((name) => {
    const c = shared.v3_cases.find((candidate) => candidate.name === name);
    if (!c) throw new Error(`Missing shared edit case: ${name}`);
    return JSON.parse(c.input) as { post_id: number; base_fingerprint: string; operations: Op[] };
  });
  const input = { ...inputs[0]!, operations: inputs.flatMap((i) => i.operations), design: draftDesign };
  const nodes: Node[] = shared.v3_page.nodes.map(({ ref, parent, kind, level, editable }) => ({
    ref, parent, kind, ...(level === undefined ? {} : { level }), editable: [...editable], from_the_site: {},
  }));
  const changes = input.operations.map((op, i) => {
    const node = nodes.find((n) => n.ref === op.ref);
    if (op.op === "set_text") {
      if (node) node.from_the_site = { ...node.from_the_site, [op.field!]: op.text! };
      return { op: op.op, ref: op.ref, kind: node?.kind, field: op.field, after: op.text,
        from_the_site: { before: { [op.field!]: "Before this change" } } };
    }
    const ref = `new_${i}`;
    const kind = (op.outline?.[0] as { type: string } | undefined)?.type ?? "paragraph";
    const anchorRef = op.after ?? op.before ?? op.into;
    const anchor = nodes.find((n) => n.ref === anchorRef);
    nodes.push({ ref, parent: op.into ?? node?.parent ?? anchor?.parent ?? "root", kind, editable: [] });
    if (op.op === "insert") return { op: op.op, new_refs: [ref], anchor: {
      ref: anchorRef, kind: anchor?.kind, how: op.into ? "into" : op.before ? "before" : "after",
      ...(op.into ? { position: op.position ?? "last" } : {}),
    } };
    if (op.op === "replace") {
      const removed = new Set([op.ref]);
      for (const n of nodes) if (removed.has(n.parent)) removed.add(n.ref);
      for (let j = nodes.length - 1; j >= 0; j -= 1) if (removed.has(nodes[j]!.ref)) nodes.splice(j, 1);
      return { op: op.op, ref: op.ref, kind: node?.kind, new_refs: [ref] };
    }
    throw new Error(`Unsupported fixture operation: ${op.op}`);
  });
  // PageEditCardFacts carries the changed field and its site text as strings.
  const facts = { kind: "builder_edit", post: { id: input.post_id, from_the_site: { title: "Launch page" } },
    builder: { id: "elementor", version: "3.35.9", format: "classic" }, changes,
    after_outline: { node_count: nodes.length, truncated: false, nodes }, checked_at: "2026-10-01T09:52:00Z" };
  return pageEditRow({ input_json: JSON.stringify(input), page_edit: facts as PageEditRow });
}
