#!/usr/bin/env node
// GH #479 - convert OpenAPI 3.0 `nullable: true` to the 3.1 JSON Schema form.
//
// packages/openapi/openapi.yaml declares `openapi: 3.1.0`, where `nullable`
// was removed as a keyword. The 3.1 spelling of "this may be null" is a type
// union: `type: [string, "null"]`.
//
// This edits YAML line-wise rather than parse-and-redump on purpose: the spec
// is 23k hand-maintained lines whose comments, block scalars and folded
// descriptions must survive byte-for-byte. Only the two lines involved in each
// conversion are touched.
//
// Usage:
//   node scripts/openapi31-nullable.mjs --dry-run [file]
//   node scripts/openapi31-nullable.mjs --write   [file]
//   node scripts/openapi31-nullable.mjs --only=NAME --write [file]   (probe one property)

import { readFileSync, writeFileSync } from "node:fs";

const args = process.argv.slice(2);
const write = args.includes("--write");
const dryRun = args.includes("--dry-run") || !write;
const onlyArg = args.find((a) => a.startsWith("--only="));
const only = onlyArg ? onlyArg.slice("--only=".length) : null;
const file =
  args.find((a) => !a.startsWith("--")) ?? "packages/openapi/openapi.yaml";

const src = readFileSync(file, "utf8");
const lines = src.split("\n");

const indentOf = (l) => l.length - l.trimStart().length;
const isBlank = (l) => l.trim() === "";
// A comment line belongs to whatever block follows it; never treat it as a sibling key.
const isComment = (l) => l.trimStart().startsWith("#");

/**
 * Collect the sibling keys of the mapping that owns `idx` (a `nullable: true`
 * line at indent `ind`). Walks out in both directions and stops at the first
 * line that dedents past `ind`, which is the mapping boundary.
 */
function siblings(idx, ind) {
  const out = [];
  const scan = (from, step) => {
    for (let i = from; i >= 0 && i < lines.length; i += step) {
      const l = lines[i];
      if (isBlank(l) || isComment(l)) continue;
      const li = indentOf(l);
      if (li < ind) break; // left the mapping
      if (li > ind) continue; // nested under a sibling
      out.push({ i, line: l });
    }
  };
  scan(idx - 1, -1);
  scan(idx + 1, +1);
  return out;
}

const KEY = /^\s*([A-Za-z_$][\w$-]*):(.*)$/;
const keyOf = (l) => (l.match(KEY) ?? [])[1];

const conversions = [];
const flowConversions = [];
const skipped = [];

// ---- pass 1: inline flow mappings ------------------------------------------
// `original_width: { type: integer, nullable: true }` puts the whole schema on
// one line, so there is no `nullable: true` line to anchor on. 30 of these hide
// behind a line-anchored regex, which is exactly where a silent miss lives.
for (let i = 0; i < lines.length; i++) {
  const l = lines[i];
  if (!/\bnullable:\s*true\b/.test(l)) continue;
  if (/^\s*nullable:\s*true\s*$/.test(l)) continue; // own-line form, pass 2
  if (!/\{[^}]*\bnullable:\s*true\b[^}]*\}/.test(l)) {
    skipped.push({
      line: i + 1,
      owner: (l.match(KEY) ?? [])[1] ?? l.trim(),
      reason: "nullable: true is inline but not inside a flow mapping",
    });
    continue;
  }
  const owner = (l.match(KEY) ?? [])[1] ?? "?";
  if (only && owner !== only) continue;

  const typeM = l.match(/\btype:\s*([a-z]+)\b/);
  if (!typeM) {
    skipped.push({ line: i + 1, owner, reason: "flow mapping has no `type:`" });
    continue;
  }
  if (/\benum:/.test(l)) {
    skipped.push({
      line: i + 1,
      owner,
      reason: "flow mapping carries an enum; needs a null member by hand",
    });
    continue;
  }
  flowConversions.push({ i, owner, typeVal: typeM[1] });
}

for (const c of flowConversions) {
  let l = lines[c.i];
  l = l.replace(/\btype:\s*[a-z]+\b/, `type: [${c.typeVal}, "null"]`);
  // Drop the keyword together with exactly one adjacent separator, whichever
  // side it sits on, so the flow mapping stays well formed either way.
  l = l
    .replace(/,\s*nullable:\s*true\b/, "")
    .replace(/\bnullable:\s*true\s*,\s*/, "")
    .replace(/\{\s*nullable:\s*true\s*\}/, "{}");
  lines[c.i] = l;
}

// ---- pass 2: own-line `nullable: true` -------------------------------------
for (let i = 0; i < lines.length; i++) {
  const m = lines[i].match(/^(\s*)nullable:\s*true\s*$/);
  if (!m) continue;
  const ind = m[1].length;
  const sibs = siblings(i, ind);

  // The property this schema belongs to: nearest ancestor key at a lower indent.
  let owner = "?";
  for (let j = i - 1; j >= 0; j--) {
    if (isBlank(lines[j]) || isComment(lines[j])) continue;
    if (indentOf(lines[j]) < ind) {
      owner = keyOf(lines[j]) ?? lines[j].trim();
      break;
    }
  }

  const typeSib = sibs.find((s) => keyOf(s.line) === "type");
  const enumSib = sibs.find((s) => keyOf(s.line) === "enum");

  if (!typeSib) {
    // No `type:` sibling => $ref / allOf / oneOf / bare schema. The union form
    // does not apply; these need a oneOf null branch and a human decision.
    skipped.push({
      line: i + 1,
      owner,
      reason: "no sibling `type:` key",
      siblingKeys: sibs.map((s) => keyOf(s.line)).filter(Boolean),
    });
    continue;
  }

  const typeVal = typeSib.line.split(":").slice(1).join(":").trim();
  if (!/^[a-z]+$/.test(typeVal)) {
    skipped.push({
      line: i + 1,
      owner,
      reason: `sibling type is not a plain scalar: ${JSON.stringify(typeVal)}`,
    });
    continue;
  }

  if (only && owner !== only) continue;

  conversions.push({ nullableLine: i, typeSib, enumSib, typeVal, owner });
}

// ---- apply -----------------------------------------------------------------
// Highest line index first so earlier edits never shift later indices.
const ordered = [...conversions].sort((a, b) => b.nullableLine - a.nullableLine);
let enumsPatched = 0;

for (const c of ordered) {
  const ind = " ".repeat(indentOf(c.typeSib.line));
  lines[c.typeSib.i] = `${ind}type: [${c.typeVal}, "null"]`;

  // Drop the keyword line FIRST, then patch the enum against indices that
  // already account for the removal. The other order was a live bug: patching
  // a block enum sitting above `nullable: true` inserted a member, pushed the
  // keyword down one, and made this splice delete an innocent neighbour - the
  // type union line itself, or the `- null` just inserted - while the report
  // still printed a clean "converted" count over the wreckage.
  lines.splice(c.nullableLine, 1);

  // A union that admits null must also admit null in its enum, or the value
  // fails enum validation the moment the type union starts permitting it.
  if (c.enumSib) {
    const enumIdx =
      c.enumSib.i > c.nullableLine ? c.enumSib.i - 1 : c.enumSib.i;
    const patched = patchEnum(enumIdx);
    if (patched) enumsPatched++;
  }
}

/**
 * Append `null` to an enum, whether it is inline flow (`enum: [a, b]`),
 * multi-line flow (`enum:` then `[` on a following line), or a block sequence
 * (`enum:` then `- a` lines). Returns true when it added a member, false when
 * `null` was already one.
 *
 * Throws rather than returning quietly when the enum matches none of those
 * shapes. A silent no-op here is not harmless: the caller has already widened
 * the type to admit null, so declining to touch the enum ships a schema whose
 * type permits null and whose enum rejects it.
 */
function patchEnum(start) {
  const ind = indentOf(lines[start]);
  const after = lines[start].split(":").slice(1).join(":").trim();

  // Where does a flow sequence open? On the `enum:` line itself, or - equally
  // legal YAML, and the shape this spec actually uses - on a line indented
  // beneath it. Only checking the `enum:` line misread the second shape as a
  // block sequence, found no `- ` members, and silently gave up.
  let flowStart = after.startsWith("[") ? start : -1;
  if (flowStart === -1 && after === "") {
    for (let i = start + 1; i < lines.length; i++) {
      if (isBlank(lines[i]) || isComment(lines[i])) continue;
      if (indentOf(lines[i]) <= ind) break;
      if (lines[i].trimStart().startsWith("[")) flowStart = i;
      break;
    }
  }

  if (flowStart !== -1) {
    // Flow sequence, possibly spanning several lines. Find its closing bracket.
    let end = flowStart;
    let depth = 0;
    let done = false;
    for (let i = flowStart; i < lines.length && !done; i++) {
      for (const ch of lines[i]) {
        if (ch === "[") depth++;
        else if (ch === "]") {
          depth--;
          if (depth === 0) {
            end = i;
            done = true;
            break;
          }
        }
      }
    }
    if (!done) {
      throw new Error(
        `${file}:${start + 1}: enum flow sequence never closes; refusing to guess`,
      );
    }
    const joined = lines.slice(flowStart, end + 1).join("\n");
    if (/(^|[\s[,])null([\s\],])/.test(joined)) return false; // already there
    const idx = lines[end].lastIndexOf("]");
    lines[end] = `${lines[end].slice(0, idx)}, null${lines[end].slice(idx)}`;
    return true;
  }

  // Block sequence: `enum:` then `- value` lines at a deeper indent.
  let last = start;
  for (let i = start + 1; i < lines.length; i++) {
    if (isBlank(lines[i]) || isComment(lines[i])) continue;
    if (indentOf(lines[i]) <= ind) break;
    if (lines[i].trimStart().startsWith("- ")) {
      if (lines[i].trim() === "- null") return false;
      last = i;
    }
  }
  if (last === start) {
    throw new Error(
      `${file}:${start + 1}: enum is neither a flow sequence nor a block ` +
        `sequence with members, so nothing was added to it - but the type has ` +
        `already been widened to admit null, which the enum would then ` +
        `reject. Fix this enum by hand.`,
    );
  }
  const itemInd = " ".repeat(indentOf(lines[last]));
  lines.splice(last + 1, 0, `${itemInd}- null`);
  return true;
}

// ---- report ----------------------------------------------------------------
const total = (src.match(/\bnullable:\s*true\b/g) ?? []).length;
console.log(`file:              ${file}`);
console.log(`nullable: true     ${total} occurrences in the input`);
console.log(`converted          ${conversions.length + flowConversions.length}`);
console.log(`  own-line form    ${conversions.length}`);
console.log(`  flow-mapping     ${flowConversions.length}`);
console.log(`  of which enums   ${enumsPatched} also gained a null member`);
console.log(`skipped            ${skipped.length}`);
const accounted = conversions.length + flowConversions.length + skipped.length;
if (!only && accounted !== total) {
  console.error(
    `\nFATAL: ${total} occurrences but only ${accounted} accounted for. ` +
      `${total - accounted} unaccounted - refusing to write.`,
  );
  process.exit(1);
}
if (only && conversions.length + flowConversions.length === 0) {
  console.error(
    `\nFATAL: --only=${only} matched no convertible declaration in ${file}. ` +
      `Nothing to convert - refusing to write.`,
  );
  process.exit(1);
}
if (skipped.length) {
  console.log("\n--- SKIPPED (need a human decision, not the union form) ---");
  for (const s of skipped) {
    console.log(
      `  ${file}:${s.line}  ${s.owner}  -- ${s.reason}` +
        (s.siblingKeys ? `  siblings=[${s.siblingKeys.join(", ")}]` : ""),
    );
  }
}

const byType = {};
for (const c of conversions) byType[c.typeVal] = (byType[c.typeVal] ?? 0) + 1;
console.log("\n--- converted by base type ---");
for (const [t, n] of Object.entries(byType).sort((a, b) => b[1] - a[1])) {
  console.log(`  ${String(n).padStart(4)}  ${t}`);
}

if (write) {
  writeFileSync(file, lines.join("\n"));
  console.log(`\nWROTE ${file}`);
} else {
  console.log(`\n(dry run - nothing written)`);
}
