// Copy-compliance gate. Fails the build if an em dash, an en dash, or a named
// competitor product slips into copy that ships to a human.
//
// TWO RULES, DIFFERENT SCOPES. Read this before adding a surface.
//
//   DASH rule        every surface below, no exceptions.
//   COMPETITOR rule  everything EXCEPT the marketing site.
//
// The competitor rule exists for CODE PROVENANCE. Features here were built
// clean-room against vendor reference, so crediting a rival product as the
// source of a technique inside shipped code is the thing to prevent. It was
// never meant to stop marketing from writing an honest comparison, and
// "X alternative" is the highest-intent query in this market, so applying it to
// apps/marketing was costing real demand for no benefit (owner's call,
// 2026-08-06). The rule still binds where it means something: agent PHP and the
// plugin header below, plus docs/ and root *.md via ci.yml's separate docs
// vocabulary check, which is where the 2026-07-06 leak actually happened.
//
// Two scopes:
//
//  1. The marketing site (apps/marketing). Every source file under app/,
//     components/, lib/content/ and content/, scanned whole. DASH RULE ONLY.
//
//  2. The WordPress plugin (apps/agent). This is the most public copy the
//     project ships and, until now, the only copy with no gate on it:
//       a. apps/agent/readme.txt, scanned whole. It is rendered verbatim as
//          the wordpress.org listing page.
//       b. The plugin-header fields of apps/agent/wpmgr-agent.php, which
//          WordPress prints in the site's Plugins list.
//       c. Every string literal in an agent PHP file that calls a WordPress
//          translation function. A file that translates is a file that speaks
//          to a human, so all of its literals are treated as copy -- including
//          the untranslated ones, which is how the em-dash placeholder on the
//          settings screen went unnoticed. PHP comments are developer prose,
//          not copy, and are exempt from the DASH rule: there are thousands of
//          legitimate dashes in them.
//       d. The competitor-name rule alone, over every shipped agent PHP file
//          including its comments, because a competitor named in a comment
//          still ships inside the plugin zip.
//
// The dash half of scope (2) deliberately excludes the agent's diagnostic and
// log strings. Those are machine-to-machine and are not part of the shipped
// listing or of any screen a site owner reads.
//
// WHERE THIS RUNS, and one place it deliberately does NOT.
//
// It runs in ci.yml (Security audit job) against a full repo checkout, and on
// demand via the "check-copy" script in package.json. It is NOT part of the
// marketing "build" script, and must not be added back to it. Scope (2) reads
// apps/agent, while Dockerfile.marketing copies only apps/marketing, packages
// and the root manifests, so inside the production image build those files are
// absent and the gate fails on every one of them. That is the correct answer
// to the question it was asked, which is exactly why the question is wrong
// there: a repo-wide copy lint is not a step in one app's image build.
//
// CI is also the stronger placement. It sees all 366 files; the Docker build
// could never see the agent at all.
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = fileURLToPath(new URL("..", import.meta.url));
const REPO = join(ROOT, "..", "..");
const AGENT = join(REPO, "apps", "agent");

const SCAN_DIRS = [
  join(ROOT, "app"),
  join(ROOT, "components"),
  join(ROOT, "lib", "content"),
];

// Try to also scan content/ if it exists (Phase 3 MDX)
const CONTENT_DIR = join(ROOT, "content");
try {
  statSync(CONTENT_DIR);
  SCAN_DIRS.push(CONTENT_DIR);
} catch {
  // content/ not yet created; skip.
}

const BANNED_CHARS = [
  { ch: "—", name: "em dash" },
  { ch: "–", name: "en dash" },
];

// Named competitor products that must never appear in shipped files.
const BANNED_WORDS = [
  "ManageWP",
  "MainWP",
  "WPvivid",
  "FlyingPress",
  "InfiniteWP",
  "WP Remote",
  "WPRemote",
];

function walk(dir, match) {
  let out = [];
  let entries;
  try {
    entries = readdirSync(dir);
  } catch {
    return out;
  }
  for (const entry of entries) {
    const p = join(dir, entry);
    let stat;
    try {
      stat = statSync(p);
    } catch {
      continue;
    }
    if (stat.isDirectory()) {
      out = out.concat(walk(p, match));
    } else if (match(entry)) {
      out.push(p);
    }
  }
  return out;
}

const failures = [];

function report(file, line, message) {
  failures.push(`${file}:${line}  ${message}`);
}

function checkWords(file, line, text, label) {
  const flat = text.replace(/\s+/g, " ").trim();
  const lower = text.toLowerCase();
  for (const w of BANNED_WORDS) {
    if (lower.includes(w.toLowerCase())) {
      report(file, line, `banned competitor name "${w}"${label} ${flat.slice(0, 80)}`);
    }
  }
}

// `names` controls the COMPETITOR rule only. The dash rule always applies.
// See the scope table at the top of this file for which surfaces get which.
function checkText(file, line, text, label, { names = true } = {}) {
  const flat = text.replace(/\s+/g, " ").trim();
  for (const { ch, name } of BANNED_CHARS) {
    if (text.includes(ch)) {
      report(file, line, `banned ${name}${label} ${flat.slice(0, 80)}`);
    }
  }
  if (names) checkWords(file, line, text, label);
}

// ---------------------------------------------------------------------------
// Scope 1 + readme.txt: whole-file, line by line.
// ---------------------------------------------------------------------------
function scanWholeFile(file, opts) {
  let text;
  try {
    text = readFileSync(file, "utf8");
  } catch {
    return;
  }
  text.split("\n").forEach((line, i) => checkText(file, i + 1, line, ":", opts));
}

// ---------------------------------------------------------------------------
// Scope 2c: PHP string literals only.
//
// A small lexer rather than a regex, because a regex cannot tell a quote
// inside a comment from the start of a string, and stripping comments first
// would truncate any line holding a "//" inside a URL and hide whatever
// followed it. Handles line comments, "#" comments (but not "#[" attributes),
// block comments, single- and double-quoted strings, and heredoc/nowdoc.
// ---------------------------------------------------------------------------
function phpStringLiterals(src) {
  const out = [];
  let i = 0;
  let line = 1;
  const n = src.length;

  while (i < n) {
    const c = src[i];

    if (c === "\n") {
      line++;
      i++;
      continue;
    }

    // # comment, but #[ is a PHP 8 attribute
    if (c === "#" && src[i + 1] !== "[") {
      while (i < n && src[i] !== "\n") i++;
      continue;
    }
    if (c === "#") {
      i += 2;
      continue;
    }

    if (c === "/" && src[i + 1] === "/") {
      while (i < n && src[i] !== "\n") i++;
      continue;
    }

    if (c === "/" && src[i + 1] === "*") {
      i += 2;
      while (i < n && !(src[i] === "*" && src[i + 1] === "/")) {
        if (src[i] === "\n") line++;
        i++;
      }
      i += 2;
      continue;
    }

    // <<<IDENT / <<<'IDENT' / <<<"IDENT"
    if (c === "<" && src.startsWith("<<<", i)) {
      let j = i + 3;
      while (j < n && (src[j] === " " || src[j] === "\t")) j++;
      let quote = "";
      if (src[j] === "'" || src[j] === '"') {
        quote = src[j];
        j++;
      }
      let id = "";
      while (j < n && /[A-Za-z0-9_]/.test(src[j])) {
        id += src[j];
        j++;
      }
      if (!id || (quote && src[j] !== quote)) {
        i++;
        continue;
      }
      if (quote) j++;
      while (j < n && src[j] !== "\n") j++;
      j++;
      line++;
      const startLine = line;
      const closer = new RegExp("^[ \\t]*" + id + "\\b");
      let body = "";
      while (j < n) {
        let eol = src.indexOf("\n", j);
        if (eol === -1) eol = n;
        const l = src.slice(j, eol);
        if (closer.test(l)) {
          j = eol;
          break;
        }
        body += l + "\n";
        line++;
        j = eol + 1;
      }
      out.push({ line: startLine, text: body });
      i = j;
      continue;
    }

    if (c === "'" || c === '"') {
      const quote = c;
      const startLine = line;
      let j = i + 1;
      let buf = "";
      while (j < n) {
        if (src[j] === "\\") {
          // In single quotes only \\ and \' are escapes; in double quotes
          // every backslash escapes the next character. Skipping the next
          // character in both cases is safe for our purpose: a banned
          // character is never the escaped one.
          buf += src[j + 1] ?? "";
          j += 2;
          continue;
        }
        if (src[j] === quote) break;
        if (src[j] === "\n") line++;
        buf += src[j];
        j++;
      }
      out.push({ line: startLine, text: buf });
      i = j + 1;
      continue;
    }

    i++;
  }

  return out;
}

// A file that calls a WordPress translation function is a file that renders
// copy for a human.
const TRANSLATES =
  /\b(?:esc_html__|esc_attr__|esc_html_e|esc_attr_e|esc_html_x|esc_attr_x|_nx|_ex|__|_e|_x|_n)\s*\(/;

// Never walk into the agent's vendored, test-only or build-only trees. These
// are all excluded from the shipped zip by the agent-zip-wporg Makefile target,
// so nothing in them is copy.
const AGENT_SKIP_DIRS = new Set([
  "vendor",
  "node_modules",
  "tests",
  "tests-e2e",
  "tools",
  ".phpunit.cache",
]);

function walkAgentPhp(dir, out = []) {
  let entries;
  try {
    entries = readdirSync(dir);
  } catch {
    return out;
  }
  for (const entry of entries) {
    if (AGENT_SKIP_DIRS.has(entry)) continue;
    const p = join(dir, entry);
    let stat;
    try {
      stat = statSync(p);
    } catch {
      continue;
    }
    if (stat.isDirectory()) walkAgentPhp(p, out);
    else if (entry.endsWith(".php")) out.push(p);
  }
  return out;
}

// ---------------------------------------------------------------------------
// Run
// ---------------------------------------------------------------------------
const marketingFiles = SCAN_DIRS.flatMap((d) =>
  walk(d, (entry) => /\.(tsx?|mdx?|css|html)$/.test(entry)),
);
// Marketing gets the DASH rule but NOT the competitor rule. Naming a rival
// product on a marketing page is legitimate comparison, and "X alternative" is
// the highest-intent query in this market; refusing to write those pages was
// costing real demand for no benefit. The clean-room rule this gate grew out of
// is about CODE PROVENANCE (do not credit a competitor as the source of a
// technique we implemented), and it still binds everywhere it matters: agent
// PHP below, plus docs/ and root *.md via ci.yml's docs vocabulary check.
for (const file of marketingFiles) scanWholeFile(file, { names: false });

// 2a. The wordpress.org listing. KEEPS the competitor rule even though
// marketing dropped it: this file IS the directory listing page, and naming
// rival plugins there is a plugin-review risk on the one surface where a
// reviewer decides whether we stay published.
const AGENT_README = join(AGENT, "readme.txt");
let agentReadmeScanned = 0;
try {
  statSync(AGENT_README);
  scanWholeFile(AGENT_README, { names: true });
  agentReadmeScanned = 1;
} catch {
  report(AGENT_README, 0, "missing: the wordpress.org listing copy must exist");
}

// 2b. The plugin-header fields WordPress prints in the Plugins list.
const AGENT_MAIN = join(AGENT, "wpmgr-agent.php");
let agentHeaderScanned = 0;
try {
  const src = readFileSync(AGENT_MAIN, "utf8");
  // Fail CLOSED on a header this cannot read. `indexOf` returns -1 when the
  // docblock is unterminated, and slice(0, -1 + 2) is the FIRST CHARACTER of
  // the file: the loop below would then match nothing, find no violations and
  // count the header as scanned. A gate that reports success because it read
  // one character is worse than no gate, because the summary line says it ran.
  const end = src.indexOf("*/");
  const header = end === -1 ? "" : src.slice(0, end + 2);
  if (!/^\s*\*\s*Plugin Name:/m.test(header)) {
    report(
      AGENT_MAIN,
      0,
      end === -1
        ? "plugin header not scanned: no closing */ in the file"
        : "plugin header not scanned: the first */ closes a block with no Plugin Name: field",
    );
  } else {
    header.split("\n").forEach((line, i) => {
      if (/^\s*\*\s*(Plugin Name|Plugin URI|Description|Author|Author URI):/.test(line)) {
        checkText(AGENT_MAIN, i + 1, line, " in plugin header:");
      }
    });
    agentHeaderScanned = 1;
  }
} catch {
  report(AGENT_MAIN, 0, "missing: the agent plugin entry file must exist");
}

// 2c. String literals in the agent files that render copy, plus a
//     competitor-name sweep over EVERY shipped agent PHP file including its
//     comments. Comments are exempt from the dash rule (there are thousands of
//     legitimate dashes in them) but not from the naming rule: a competitor
//     named in a code comment still ships inside the plugin zip, and the
//     markdown-only vocabulary gate in ci.yml never looked at PHP.
const agentPhp = walkAgentPhp(AGENT);
const agentCopyFiles = [];
for (const file of agentPhp) {
  let src;
  try {
    src = readFileSync(file, "utf8");
  } catch {
    continue;
  }

  src.split("\n").forEach((line, i) => checkWords(file, i + 1, line, ":"));

  if (!TRANSLATES.test(src)) continue;
  agentCopyFiles.push(file);
  for (const { line, text } of phpStringLiterals(src)) {
    checkText(file, line, text, " in string literal:");
  }
}

if (failures.length > 0) {
  for (const f of failures) console.error(f);
  console.error(`\ncheck-copy FAILED with ${failures.length} issue(s).`);
  process.exit(1);
}

const scanned =
  marketingFiles.length + agentReadmeScanned + agentHeaderScanned + agentPhp.length;
console.log(
  `check-copy passed: no em dashes, en dashes, or competitor names across ${scanned} files ` +
    `(${marketingFiles.length} marketing, ${agentReadmeScanned} agent readme, ` +
    `${agentHeaderScanned} plugin header, ${agentPhp.length} agent PHP of which ` +
    `${agentCopyFiles.length} carry copy).`,
);
