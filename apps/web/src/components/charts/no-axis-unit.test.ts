// A source scan, not a render test, because this defect class is invisible at
// any single call site.
//
// GH #329. Recharts builds every tick label as
//
//   "".concat(tickFormatter ? tickFormatter(v, i) : v).concat(unit || "")
//
// (recharts/es6/cartesian/CartesianAxis.js:353). The `unit` prop is appended
// to the formatter's output unconditionally, with no warning and no opt-out.
// A formatter that emits a unit AND a `unit` prop is therefore a bug by
// construction, which is how an LCP axis came to print "3sms".
//
// The house rule is that the tick formatter is the sole owner of the unit
// string, so no XAxis or YAxis anywhere in the app may set `unit`. Fixing the
// one offending call site would not stop the next one; this test does.

import { describe, it, expect } from "vitest";
import { existsSync, readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";

// Vitest resolves its root to apps/web (vitest.config.ts lives there), so the
// app sources are cwd/src. `import.meta.url` is not a file URL under the jsdom
// environment, which is why this does not use it.
const SRC_ROOT = join(process.cwd(), "src");

function walk(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = join(dir, entry.name);
    if (entry.isDirectory()) {
      walk(full, out);
    } else if (/\.tsx?$/.test(entry.name) && !/\.test\.tsx?$/.test(entry.name)) {
      // Tests are excluded: this very file carries a deliberate violation as
      // its positive control, and no test renders shipped UI.
      out.push(full);
    }
  }
  return out;
}

/**
 * Returns the raw prop text of every <XAxis> / <YAxis> element in `source`.
 *
 * A plain regex is not good enough here: axis props routinely hold arrow
 * functions (`tickFormatter={(v) => ...}`), so a lazy `[^>]*?` scan would stop
 * at the arrow's own ">" and miss a `unit=` that comes after it. Missing a
 * violation is the dangerous direction, so this tracks brace depth instead and
 * only treats a ">" at depth zero as the end of the tag.
 */
export function axisPropBlocks(source: string): string[] {
  const blocks: string[] = [];
  const tagStart = /<(?:X|Y)Axis\b/g;
  let match: RegExpExecArray | null;
  while ((match = tagStart.exec(source)) !== null) {
    const start = match.index + match[0].length;
    let depth = 0;
    let i = start;
    for (; i < source.length; i += 1) {
      const ch = source[i];
      if (ch === "{") depth += 1;
      else if (ch === "}") depth -= 1;
      else if (ch === ">" && depth === 0) break;
    }
    blocks.push(source.slice(start, i));
  }
  return blocks;
}

const UNIT_PROP = /\bunit\s*=/;

describe("no recharts axis may set the `unit` prop (GH #329)", () => {
  const files = walk(SRC_ROOT).filter((f) =>
    readFileSync(f, "utf8").includes('from "recharts"'),
  );

  it("finds the recharts files it is supposed to be guarding", () => {
    // A broken walk or a moved source root would otherwise turn this whole
    // file into a silent pass. Eight files import recharts today.
    expect(existsSync(join(SRC_ROOT, "components", "charts"))).toBe(true);
    expect(files.length).toBeGreaterThanOrEqual(8);
  });

  it("finds axis elements to inspect", () => {
    const total = files.reduce(
      (n, f) => n + axisPropBlocks(readFileSync(f, "utf8")).length,
      0,
    );
    expect(total).toBeGreaterThanOrEqual(10);
  });

  it("detects a violation when one exists (positive control)", () => {
    const offending = `
      <YAxis
        dataKey="y"
        tickFormatter={(v: number) => (v >= 1000 ? \`\${v / 1000}s\` : \`\${v}\`)}
        width={44}
        unit={unit}
      />`;
    const blocks = axisPropBlocks(offending);
    expect(blocks).toHaveLength(1);
    // The arrow function inside tickFormatter must not end the scan early.
    expect(UNIT_PROP.test(blocks[0]!)).toBe(true);
  });

  it("does not flag a clean axis (negative control)", () => {
    const clean = `
      <YAxis
        domain={axis.domain}
        ticks={axis.ticks}
        tickFormatter={axis.tick}
        width={axis.width}
      />`;
    expect(UNIT_PROP.test(axisPropBlocks(clean)[0]!)).toBe(false);
  });

  it("has no XAxis or YAxis anywhere in the app that sets `unit`", () => {
    for (const file of files) {
      for (const block of axisPropBlocks(readFileSync(file, "utf8"))) {
        expect(UNIT_PROP.test(block), `${file} sets a recharts axis unit prop`).toBe(
          false,
        );
      }
    }
  });
});
