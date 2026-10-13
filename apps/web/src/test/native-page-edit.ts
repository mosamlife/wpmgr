import { readFileSync } from "node:fs";
import { join } from "node:path";
import { pageEditRow, type PageEditRow } from "./ability-request-rows";

export interface NativeGolden {
  name: string;
  input: string;
  facts: {
    changes: Array<{
      op: string; ref?: string; new_refs?: string[];
      native?: {
        widget?: string;
        keys?: Array<{
          key: string; from_the_site?: { before: unknown }; after?: unknown; removed?: boolean;
          links?: Array<{ text: string; external_domain?: string }>;
        }>;
        removed_count?: number;
        removed_outline?: Array<{ ref: string; kind: string; widget?: string }>;
        from?: { parent: string; index: number };
        to?: { parent: string; index: number };
      };
    }>;
    after_outline: { node_count: number; truncated: boolean; nodes: Array<{
      ref: string; parent: string; kind: string; widget?: string; summary?: string;
    }> };
  };
}

// This file is generated and checked by TestNativeCardFactsGolden through the production card path.
export const nativeCardCases = JSON.parse(readFileSync(join(process.cwd(), "..", "api", "internal", "mcp",
  "testdata", "native-card-facts.json"), "utf8")) as NativeGolden[];
if (nativeCardCases.length === 0) throw new Error("Native card goldens are empty");

export function nativeCardCase(name: string): NativeGolden {
  const c = nativeCardCases.find((candidate) => candidate.name === name);
  if (!c) throw new Error(`Missing native card golden: ${name}`);
  return structuredClone(c);
}

export function nativeEditRow(c: NativeGolden) {
  return pageEditRow({ input_json: c.input, page_edit: c.facts as PageEditRow });
}

/** Link fields use the PageEditNativeCardKey JSON tags in page_edit_native_verify.go. */
export function nativeLinkCase(): NativeGolden {
  const c = nativeCardCase("add_native");
  const link = { url: "https://outside.example/read?full=yes", is_external: "true" };
  const input = JSON.parse(c.input) as { operations: Array<{ settings: Record<string, unknown> }> };
  input.operations[0]!.settings.link = link;
  c.input = JSON.stringify(input);
  c.facts.changes[0]!.native!.keys!.push({ key: "link", after: link,
    links: [{ text: link.url, external_domain: "outside.example" }] });
  return c;
}
