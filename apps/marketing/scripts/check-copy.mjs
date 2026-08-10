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

// Disparagement vocabulary, banned on the comparison pages.
//
// This is the sharper rule that replaced the blunt one. Marketing may now name
// competitors, but the ORIGINAL purpose of the naming ban was never "pretend
// rivals do not exist", it was "do not sound like the sneering-competitor
// content that fills this niche". Dropping the naming ban without encoding
// that purpose would have thrown away the reason it existed.
//
// Comparison pages are also where a careless adjective does the most damage:
// they are the one surface whose entire value is being trusted about somebody
// else's product.
const DISPARAGEMENT = [
  "bloated",
  "outdated",
  "ripoff",
  "rip-off",
  "scam",
  "garbage",
  "abandoned",
  "inferior",
  "overpriced",
  "clunky",
  "crippled",
];

// Blanks out verbatim quotations so the style rules cannot reach inside them.
//
// THIS IS NOT A LOOPHOLE, IT IS THE POINT. Comparison pages quote vendors
// exactly, and both of our rules were rewriting those quotes:
//
//   The DASH rule fired on ManageWP's own sentence, "Security Check is a
//   messenger, not a cleaner-it alerts you, but does not remove malware",
//   which uses an em dash. An earlier draft "fixed" it by swapping the dash
//   for a comma, which silently altered a quotation and produced a comma
//   splice. A style rule that edits somebody else's words is a correctness
//   bug wearing a linter's clothes.
//
//   The DISPARAGEMENT rule fired on "Check for abandoned plugins & themes",
//   which is MainWP's own feature name, and on ManageWP's own bullet listing
//   "outdated software". Neither is us being rude about a competitor. Both are
//   us reporting what the vendor calls their own product.
//
// Applied only where we actually quote vendors verbatim, so the dash rule
// stays absolute on ordinary marketing copy, where a quoted span is far more
// likely to be scare quotes than a citation.
// THREE FORMS, BECAUSE A CLAIM IS NOT ALWAYS WRITTEN THE SAME WAY. Every
// vendor quotation on the comparison page today sits inside a double-quoted TS
// string, so the escaped form was the only one that had ever been hit. The
// first claim written as a single-quoted string or a template literal would
// have failed CI on the vendor's own em dash or on their own feature name, and
// the fix under deadline is to edit the quotation, which is the exact outcome
// this function exists to prevent.
//
// Deliberately NOT a blanket "..." pass. Blanking every plain double-quoted
// span would exempt the whole body of an ordinary claim string, since the claim
// itself is a double-quoted literal, and that is most of what the dash rule is
// here to read.
function stripQuotedSpans(text) {
  // 1. Inside a double-quoted TS string, an inner quotation is escaped, so it
  //    reads as \"...\" in the raw source.
  let out = text.replace(/\\"[\s\S]*?\\"/g, " [quoted] ");
  // 2. Curly quotation marks are never TS syntax, so a span between them is a
  //    quotation wherever it appears.
  out = out.replace(/“[^”\n]*”/g, " [quoted] ");
  // 3. Inside a single-quoted string or a template literal, an inner quotation
  //    needs no escaping and reads as a plain "...". Scoped to those two
  //    literal forms. The boundary conditions on the opening and closing quote
  //    keep an apostrophe in ordinary prose ("MainWP's own wording") from
  //    pairing with the next one and swallowing whatever lies between.
  out = out.replace(/(^|[^A-Za-z0-9_])('[^'\n]*'|`[^`\n]*`)(?![A-Za-z0-9_])/g, (all, before, lit) =>
    before + lit.replace(/"[^"\n]*"/g, " [quoted] "),
  );
  return out;
}

function checkDisparagement(file, line, text) {
  const lower = text.toLowerCase();
  for (const w of DISPARAGEMENT) {
    // Word-boundary match: "abandoned" must not fire on "abandonedCartPlugin",
    // and this file is itself scanned, so a naive includes() would flag the
    // list above.
    if (new RegExp(`\\b${w}\\b`).test(lower)) {
      report(
        file,
        line,
        `disparagement "${w}" on a comparison page. Describe what a product does, ` +
          `not how you feel about it: ${text.replace(/\s+/g, " ").trim().slice(0, 80)}`,
      );
    }
  }
}

// ---------------------------------------------------------------------------
// Citation integrity on the comparison pages.
//
// Every matrix cell, cost model and locality lane may carry `cites: ["mw-24"]`,
// and the sources page anchors on that exact id. A wrong id therefore does not
// break anything a build can see: the link still resolves, to a real claim
// about the wrong thing. Fourteen of them shipped that way, and on a page whose
// entire value is that a reader can check us, a footnote pointing at somebody
// else's fact is the one failure this page cannot afford.
//
// WHAT THIS CAN AND CANNOT CATCH. It catches the mechanical half: an id that
// does not exist, a duplicate id, and an id borrowed from the other product's
// column. It CANNOT tell that mw-20 is the wrong claim for a cell about Safe
// Updates, because both are real ManageWP claims. Reading the claim you are
// citing is still the job; this only stops the class of error a machine can
// see, and stops a renamed or deleted claim from silently orphaning a footnote.
const CITE_ID = /^[a-z]{2}-\d{2,}$/;

function checkCitations(file) {
  let src;
  try {
    src = readFileSync(file, "utf8");
  } catch {
    return 0;
  }
  return checkCitationsIn(file, src);
}

function checkCitationsIn(file, src) {
  const lines = src.split("\n");

  // Pass 1: every declared claim, and which product declared it.
  const declared = new Map(); // id -> line
  const prefixOf = new Map(); // productKey -> Set of id prefixes
  let product = null;
  lines.forEach((line, i) => {
    const key = /^\s*key:\s*"([a-z0-9-]+)"/.exec(line);
    if (key) product = key[1];
    const id = /^\s*id:\s*"([^"]+)"/.exec(line);
    if (!id) return;
    if (!CITE_ID.test(id[1])) {
      report(file, i + 1, `claim id "${id[1]}" is not of the form xx-00`);
    }
    if (declared.has(id[1])) {
      report(file, i + 1, `duplicate claim id "${id[1]}", already declared on line ${declared.get(id[1])}`);
    }
    declared.set(id[1], i + 1);
    if (product) {
      if (!prefixOf.has(product)) prefixOf.set(product, new Set());
      prefixOf.get(product).add(id[1].split("-")[0]);
    }
  });

  // Pass 2: every citation, and the column it sits in. A matrix cell names its
  // product on the same line as its cites; a cost model and a locality lane
  // open with `productKey` a few lines above. Section keys reset the second
  // form so a stale productKey cannot leak across a section boundary.
  let owner = null;
  let citations = 0;
  // A `cites` array may be written across several lines, which prettier will do
  // on its own as soon as the list is long enough. Read each one as the single
  // logical line it is, still reported at the line the `cites` key sits on: a
  // per-line regex silently matched nothing on a wrapped array, so the gate
  // stopped applying to exactly the busiest cells, and no failure said so.
  const logical = lines.map((line, i) => {
    if (!/cites:\s*\[/.test(line) || /cites:\s*\[[^\]]*\]/.test(line)) return line;
    let joined = line;
    for (let j = i + 1; j < lines.length && !joined.includes("]"); j += 1) {
      joined += " " + lines[j].trim();
    }
    return joined;
  });
  logical.forEach((line, i) => {
    if (/^\s{2}[a-z]+:\s/.test(line)) owner = null;
    const pk = /productKey:\s*"([a-z0-9-]+)"/.exec(line);
    if (pk) owner = pk[1];
    const cites = /cites:\s*\[([^\]]*)\]/.exec(line);
    if (!cites) return;
    // The column key is usually on the same line as `cites`. When the whole
    // cell object wraps it is on an earlier one, and reading only this line
    // dropped the column, so the ownership rule stopped applying to precisely
    // the cells big enough to wrap. Walk back to the object this `cites`
    // belongs to, stopping at a boundary so a sibling cell cannot be claimed.
    let cell = /^\s*([a-z][a-z0-9-]*)\s*:\s*\{/.exec(line);
    for (let j = i - 1; !cell && j >= 0; j -= 1) {
      const prev = lines[j];
      if (/^\s*[}\]],?\s*$/.test(prev)) break;
      cell = /^\s*([a-z][a-z0-9-]*)\s*:\s*\{\s*$/.exec(prev);
    }
    const column = cell ? cell[1] : owner;
    const ids = [...cites[1].matchAll(/["']([^"']+)["']/g)].map((m) => m[1]);
    for (const id of ids) {
      citations += 1;
      if (!declared.has(id)) {
        report(file, i + 1, `cites "${id}", which is not a claim id in this file`);
        continue;
      }
      const expected = column ? prefixOf.get(column) : undefined;
      if (expected && expected.size === 1 && !expected.has(id.split("-")[0])) {
        report(
          file,
          i + 1,
          `the ${column} column cites "${id}", which belongs to another product ` +
            `(expected the ${[...expected][0]}- prefix)`,
        );
      }
    }
  });

  return citations;
}

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
function scanWholeFile(file, opts = {}) {
  let text;
  try {
    text = readFileSync(file, "utf8");
  } catch {
    return;
  }
  const prep = opts.quotesExempt ? stripQuotedSpans : (t) => t;
  text.split("\n").forEach((line, i) => checkText(file, i + 1, prep(line), ":", opts));
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
// Self-check.
//
// This script is the only thing standing between a vendor quotation and an
// editor "fixing" it, and there is no test runner in this app to hold it. Both
// pieces of real logic therefore assert themselves against named fixtures on
// every run, before any file is read. A gate that has quietly stopped matching
// is worse than no gate, because the summary line still says it ran.
// ---------------------------------------------------------------------------
function selfTest() {
  const cases = [];
  const strips = (name, source, { exempt }) => {
    const out = stripQuotedSpans(source);
    const gone = !out.includes("—");
    if (gone !== exempt) {
      cases.push(
        `${name}: expected the em dash to be ${exempt ? "blanked" : "left in place"}, got "${out.trim()}"`,
      );
    }
  };

  // The form every claim on the page uses today.
  strips("escaped inner quote inside a double-quoted claim", 'claim: "they say \\"a—b\\" here",', {
    exempt: true,
  });
  // The forms that would have failed CI on a vendor's own punctuation.
  strips("inner quote inside a single-quoted claim", "claim: 'they say \"a—b\" here',", {
    exempt: true,
  });
  strips("inner quote inside a template literal", "claim: `they say \"a—b\" here`,", {
    exempt: true,
  });
  strips("curly quotation marks", "claim: “a—b”,", { exempt: true });
  // The over-broad version of this function would pass the next two, and an
  // em dash in our own copy would ship.
  strips("our own copy in a double-quoted string is still checked", 'subhead: "a—b",', {
    exempt: false,
  });
  strips("an apostrophe in prose does not open a quotation", "subhead: \"MainWP's own a—b\",", {
    exempt: false,
  });

  // Shaped like the real content file: one key and one id per line, which is
  // what the line-oriented reader above expects.
  const CITED = [
    "  products: [",
    "    {",
    '      key: "managewp",',
    "      claims: [",
    "        {",
    '          id: "mw-01",',
    "        },",
    "      ],",
    "    },",
    "    {",
    '      key: "mainwp",',
    "      claims: [",
    "        {",
    '          id: "mn-01",',
    "        },",
    "      ],",
    "    },",
    "  ],",
    "  matrix: [",
  ];
  const citeCase = (name, extra, expected) => {
    const before = failures.length;
    checkCitationsIn("fixture", [...CITED, ...extra].join("\n"));
    const found = failures.length - before;
    failures.length = before;
    if (found !== expected) {
      cases.push(`${name}: expected ${expected} failure(s), got ${found}`);
    }
  };
  citeCase("a cell citing its own product", ['    managewp: { cites: ["mw-01"] },'], 0);
  citeCase("a cell citing an id that does not exist", ['    managewp: { cites: ["mw-99"] },'], 1);
  citeCase("a cell citing the other column's claim", ['    mainwp: { cites: ["mw-01"] },'], 1);

  // A `cites` array wraps as soon as prettier decides it is long enough, and a
  // per-line regex matched nothing at all on a wrapped one. The gate therefore
  // stopped applying to exactly the cells carrying the most sources, and said
  // nothing while it did. These three keep it applying whatever the formatting.
  citeCase(
    "a wrapped cites array is still read",
    ['    managewp: {', '      cites: [', '        "mw-99",', '      ],', '    },'],
    1,
  );
  citeCase(
    "a wrapped cites array still has its column checked",
    ['    mainwp: {', '      cites: [', '        "mw-01",', '      ],', '    },'],
    1,
  );
  citeCase(
    "a single-quoted id is still read",
    ["    managewp: { cites: ['mw-99'] },"],
    1,
  );
  citeCase(
    "a wrapped cites array that is correct still passes",
    ['    managewp: {', '      cites: [', '        "mw-01",', '      ],', '    },'],
    0,
  );
  citeCase(
    "a cost model citing across the product boundary",
    ['    productKey: "mainwp",', '    cites: ["mw-01"],'],
    1,
  );

  if (cases.length > 0) {
    for (const c of cases) console.error(`check-copy SELF-CHECK failed: ${c}`);
    console.error("\nThe gate itself is broken. Fix it before trusting a green run.");
    process.exit(1);
  }
}

selfTest();

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
// Any surface whose subject is somebody else's product. /compare/ is the
// obvious one; the plugin-stack calculator is the same thing wearing a
// different URL, since it names seven vendors and computes a number against
// them, and it would be absurd for it to escape the rule on a path technicality.
const isCompareFile = (f) =>
  f.includes("/content/compare/") ||
  f.includes("/compare/") ||
  f.includes("plugin-costs") ||
  f.includes("/plugin-stack/");
for (const file of marketingFiles) {
  scanWholeFile(file, { names: false, quotesExempt: isCompareFile(file) });
}

// Comparison surfaces get the extra disparagement rule. Scoped by path rather
// than applied everywhere, because words like "outdated" are perfectly ordinary
// when a security article is describing a stale plugin on your own site, and
// only become a problem when the subject is somebody else's product.
const compareFiles = marketingFiles.filter(isCompareFile);
let compareScanned = 0;
for (const file of compareFiles) {
  compareScanned += 1;
  try {
    readFileSync(file, "utf8")
      .split("\n")
      .forEach((line, i) => checkDisparagement(file, i + 1, stripQuotedSpans(line)));
  } catch {
    // unreadable file already reported by the whole-file pass above
  }
}

// Citation integrity, over the comparison DATA files only. The components that
// render a footnote hold no ids of their own.
let citationsChecked = 0;
for (const file of compareFiles) {
  if (!file.includes("/content/compare/") || file.endsWith("/index.ts")) continue;
  citationsChecked += checkCitations(file);
}

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
    `${agentCopyFiles.length} carry copy, ${compareScanned} comparison files also ` +
    `checked for disparagement, ${citationsChecked} citations resolved).`,
);
