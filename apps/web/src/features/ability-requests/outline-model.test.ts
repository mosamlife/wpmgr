import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

import {
  DESIGNED_LIMITS,
  OUTLINE_LIMITS,
  PATTERN_ICONS,
  SCALE_RATIOS,
  buttonLabel,
  buttonsLabel,
  classifyLink,
  columnsLabel,
  contrastRatio,
  imageAlignLabel,
  imageAltLabel,
  imageSizeLabel,
  indexPageMedia,
  isSameSiteHost,
  layoutSummary,
  parseDesignedPagePreview,
  parseLink,
  parsePagePreview,
  readInput,
  spacerLabel,
  tableLabel,
  usesLayout,
  type PagePreview,
} from "./outline-model";

// The outline grammar the card shows in full. It mirrors the control plane's
// (apps/api/internal/mcp/page_create_input.go), and the first block below holds
// it to the same accept/refuse table the agent and the control plane are held
// to, so the three cannot drift without a test going red.

const CASES_FILE = join(process.cwd(), "..", "agent", "tests", "fixtures", "ability-run", "page-create-layout-cases.json");

interface SharedCase {
  name: string;
  input: string;
  agent: string;
  go: "accept" | "refuse";
}

function sharedCases(): SharedCase[] {
  const parsed = JSON.parse(readFileSync(CASES_FILE, "utf8")) as { cases: SharedCase[] };
  return parsed.cases;
}

/**
 * A fact for every attachment id the input names (the whole-number part of a
 * fractional one too), so that structure alone decides the outcome.
 */
function factsFor(input: string): unknown[] {
  const ids = new Set<number>();
  for (const m of input.matchAll(/"attachment_id":(\d+)/g)) ids.add(Number(m[1]));
  return [...ids].map((id) => ({ id, filename: `f${id}.jpg`, mime: "image/jpeg", width: 10, height: 10 }));
}

describe("the card accepts exactly what the control plane accepts", () => {
  const cases = sharedCases();

  it("reads a real table (a missing or emptied file must not pass quietly)", () => {
    expect(cases.length).toBeGreaterThan(100);
    expect(cases.filter((c) => c.go === "accept").length).toBeGreaterThan(20);
    expect(cases.filter((c) => c.go === "refuse").length).toBeGreaterThan(60);
    const names = new Set(cases.map((c) => c.name));
    for (const probe of [
      "image-every-field",
      "widths-33-33-33",
      "columns-inside-column",
      "group-inside-group",
      "link-userinfo",
      "classic-columns",
      "table-ragged-rows",
      "image-id-zero",
    ]) {
      expect(names.has(probe), probe).toBe(true);
    }
  });

  it.each(cases.map((c) => [c.name, c] as const))("%s", (_name, c) => {
    const shown = parsePagePreview(c.input, factsFor(c.input)) !== null;
    expect(shown).toBe(c.go === "accept");
  });
});

describe("a request the card cannot show in full is not shown in part", () => {
  const withNode = (node: unknown) =>
    JSON.stringify({ post_type: "page", editor: "wordpress_blocks", title: "T", outline: [node] });

  it("accepts the plain case it is compared with", () => {
    expect(parsePagePreview(withNode({ type: "paragraph", text: "x" }))).not.toBeNull();
  });

  // One extra key per node type: a stripping parse would drop it and show the
  // rest, hiding a field the AI sent from the person approving it.
  const extraKey: Array<[string, unknown]> = [
    ["heading", { type: "heading", level: 2, text: "x", style: "color:red" }],
    ["paragraph", { type: "paragraph", text: "x", href: "https://evil.example" }],
    ["list", { type: "list", ordered: true, items: ["a"], start: 5 }],
    ["buttons", { type: "buttons", buttons: [{ text: "a", url: "/a" }], target: "_blank" }],
    ["a button", { type: "buttons", buttons: [{ text: "a", url: "/a", rel: "sponsored" }] }],
    ["quote", { type: "quote", paragraphs: ["a"], source: "x" }],
    ["separator", { type: "separator", color: "red" }],
    ["spacer", { type: "spacer", size: "small", px: 500 }],
    ["table", { type: "table", rows: [["a"]], footer: ["b"] }],
    ["group", { type: "group", children: [{ type: "paragraph", text: "a" }], background: "red" }],
    [
      "columns",
      {
        type: "columns",
        columns: [{ children: [{ type: "paragraph", text: "a" }] }, { children: [{ type: "paragraph", text: "b" }] }],
        gap: 99,
      },
    ],
    [
      "a column",
      {
        type: "columns",
        columns: [{ children: [{ type: "paragraph", text: "a" }], id: "x" }, { children: [{ type: "paragraph", text: "b" }] }],
      },
    ],
  ];
  it.each(extraKey)("an unknown key on %s makes the request not showable", (_name, node) => {
    expect(parsePagePreview(withNode(node), [])).toBeNull();
  });

  it("an unknown key on an image makes the request not showable", () => {
    const node = { type: "image", attachment_id: 5, alt: "a", link: "https://evil.example" };
    expect(parsePagePreview(withNode(node), [{ id: 5, filename: "a.jpg", mime: "image/jpeg", width: 1, height: 1 }])).toBeNull();
  });

  it("an unknown top-level key makes the request not showable", () => {
    const base = { post_type: "page", editor: "wordpress_blocks", title: "T", outline: [{ type: "paragraph", text: "x" }] };
    expect(parsePagePreview(JSON.stringify({ ...base, status: "publish" }))).toBeNull();
    expect(parsePagePreview(JSON.stringify(base))).not.toBeNull();
  });

  it.each(["html", "cover", "gallery", "shortcode", "embed", "", "Paragraph"])(
    "an unknown block type %j makes the request not showable",
    (type) => {
      expect(parsePagePreview(withNode({ type, text: "x" }))).toBeNull();
    },
  );

  it("refuses text that is not JSON, a list, or a number", () => {
    for (const bad of ["", "not json", "[]", "null", "42", "{}", '{"outline":[]}']) {
      expect(parsePagePreview(bad), bad).toBeNull();
    }
  });

  it("refuses a number written with a fraction or an exponent, wherever it sits", () => {
    const page = (outline: string) =>
      `{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[${outline}]}`;
    expect(parsePagePreview(page('{"type":"heading","level":2,"text":"x"}'))).not.toBeNull();
    for (const level of ["2.0", "2e0", "2E0", "0.2e1", "2.5"]) {
      expect(parsePagePreview(page(`{"type":"heading","level":${level},"text":"x"}`)), level).toBeNull();
    }
  });

  it("reads digits, dots and quotes inside text as text", () => {
    const page = (text: string) =>
      JSON.stringify({ post_type: "page", editor: "wordpress_blocks", title: "T", outline: [{ type: "paragraph", text }] });
    for (const text of ["Costs 1.5 or 2e3", 'She said "2.5" twice', "C:\\path\\1.0", "-3.14"]) {
      expect(parsePagePreview(page(text)), text).not.toBeNull();
    }
  });

  it("refuses a prototype key rather than reading around it", () => {
    const hostile = '{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[{"type":"paragraph","text":"x"}],"__proto__":{"x":1}}';
    expect(parsePagePreview(hostile)).toBeNull();
  });

  it("refuses a page over the page-wide limits", () => {
    const para = { type: "paragraph", text: "x" };
    const page = (outline: unknown[]) =>
      JSON.stringify({ post_type: "page", editor: "wordpress_blocks", title: "T", outline });
    expect(parsePagePreview(page(Array.from({ length: OUTLINE_LIMITS.topLevelNodes }, () => para)))).not.toBeNull();
    expect(parsePagePreview(page(Array.from({ length: OUTLINE_LIMITS.topLevelNodes + 1 }, () => para)))).toBeNull();
    // 400 nodes in all, counting the columns and the buttons inside blocks.
    const groups = Array.from({ length: 4 }, () => ({
      type: "group",
      children: Array.from({ length: 49 }, () => para),
    }));
    expect(parsePagePreview(page(groups))).not.toBeNull(); // 4 + 196 = 200
    const many = Array.from({ length: 5 }, () => ({
      type: "group",
      children: Array.from({ length: 50 }, () => para),
    }));
    expect(parsePagePreview(page(many))).not.toBeNull(); // 5 + 250 = 255
    const tooMany = Array.from({ length: 8 }, () => ({
      type: "group",
      children: Array.from({ length: 50 }, () => para),
    }));
    expect(parsePagePreview(page(tooMany))).toBeNull(); // 8 + 400 = 408
  });
});

describe("image facts", () => {
  const input = JSON.stringify({
    post_type: "page",
    editor: "wordpress_blocks",
    title: "T",
    outline: [
      { type: "image", attachment_id: 42, alt: "a" },
      { type: "image", attachment_id: 7, alt: "" },
    ],
  });
  const fact = (id: number, over: Record<string, unknown> = {}) => ({
    id,
    filename: `f${id}.jpg`,
    mime: "image/jpeg",
    width: 10,
    height: 10,
    ...over,
  });

  it("an image with no fact makes the request not showable", () => {
    expect(parsePagePreview(input, [fact(42), fact(7)])).not.toBeNull();
    expect(parsePagePreview(input, [fact(42)])).toBeNull();
    expect(parsePagePreview(input, [])).toBeNull();
    expect(parsePagePreview(input, null)).toBeNull();
    expect(parsePagePreview(input, undefined)).toBeNull();
  });

  it("facts are kept by attachment id", () => {
    const p = parsePagePreview(input, [fact(7, { filename: "seven.png", mime: "image/png" }), fact(42)]) as PagePreview;
    expect(p.media.get(7)?.filename).toBe("seven.png");
    expect(p.media.get(42)?.filename).toBe("f42.jpg");
  });

  it("facts are trusted only as a whole", () => {
    expect(indexPageMedia(null).size).toBe(0);
    expect(indexPageMedia(undefined).size).toBe(0);
    expect(indexPageMedia([fact(1), fact(2)]).size).toBe(2);
    // A malformed member, an unknown key, an SVG, a duplicate id: none of the list is used.
    expect(indexPageMedia([fact(1), { id: 2 }]).size).toBe(0);
    expect(indexPageMedia([fact(1, { url: "https://x.example/a.jpg" })]).size).toBe(0);
    expect(indexPageMedia([fact(1, { mime: "image/svg+xml" })]).size).toBe(0);
    expect(indexPageMedia([fact(1), fact(1)]).size).toBe(0);
    expect(indexPageMedia([fact(0)]).size).toBe(0);
    expect(indexPageMedia([fact(1, { width: -1 })]).size).toBe(0);
    expect(indexPageMedia([fact(1, { filename: "" })]).size).toBe(0);
    expect(indexPageMedia("nope").size).toBe(0);
  });

  it("a malformed list leaves every image without a fact", () => {
    expect(parsePagePreview(input, [fact(42), fact(7), fact(7)])).toBeNull();
  });

  it("an outline with no image needs no fact", () => {
    const text = JSON.stringify({ post_type: "page", editor: "wordpress_blocks", title: "T", outline: [{ type: "paragraph", text: "x" }] });
    expect(parsePagePreview(text, null)).not.toBeNull();
    expect(parsePagePreview(text)).not.toBeNull();
  });
});

describe("button links", () => {
  const SITE = "example.com";

  it.each([
    ["/", "site"],
    ["/pricing", "site"],
    ["/contact?x=1&y=2#map", "site"],
    ["https://example.com/pricing", "site"],
    ["https://EXAMPLE.com/pricing", "site"],
    ["https://Example.Com", "site"],
    ["https://www.example.com/x", "site"],
    ["https://example.com:443/x", "site"],
  ])("%s leads to this site", (url) => {
    expect(classifyLink(url, SITE)).toEqual({ kind: "site" });
  });

  it.each([
    ["https://evil-example.com/", "evil-example.com"],
    ["https://example.com.evil.io/", "example.com.evil.io"],
    ["https://shop.example.com/", "shop.example.com"],
    ["https://notexample.com/", "notexample.com"],
    ["https://example.co/", "example.co"],
    ["https://example.com:8443/", "example.com:8443"],
    ["https://www.www.example.com/", "www.www.example.com"],
    ["https://xn--bcher-kva.example/", "xn--bcher-kva.example"],
    ["https://192.168.0.1/admin", "192.168.0.1"],
    ["https://BOOK.Example.NET/x", "book.example.net"],
  ])("%s is another website: %s", (url, host) => {
    expect(classifyLink(url, SITE)).toEqual({ kind: "external", host });
  });

  it("compares whole hosts, never a suffix or a substring", () => {
    expect(isSameSiteHost("evil-example.com", "example.com")).toBe(false);
    expect(isSameSiteHost("example.com.evil.io", "example.com")).toBe(false);
    expect(isSameSiteHost("xexample.com", "example.com")).toBe(false);
    expect(isSameSiteHost("example.com", "evil-example.com")).toBe(false);
    expect(isSameSiteHost("a.example.com", "example.com")).toBe(false);
    expect(isSameSiteHost("example.com", "a.example.com")).toBe(false);
  });

  it("drops one leading www. on each side, and nothing else", () => {
    expect(isSameSiteHost("www.example.com", "example.com")).toBe(true);
    expect(isSameSiteHost("example.com", "www.example.com")).toBe(true);
    expect(isSameSiteHost("www.example.com", "www.example.com")).toBe(true);
    expect(isSameSiteHost("www.www.example.com", "example.com")).toBe(false);
    expect(isSameSiteHost("wwwexample.com", "example.com")).toBe(false);
  });

  it("reads a site host with stray case or space as the same host", () => {
    expect(isSameSiteHost("example.com", " Example.COM ")).toBe(true);
  });

  it("treats every absolute link as another website when the site host is unknown", () => {
    expect(classifyLink("https://example.com/", "")).toEqual({ kind: "external", host: "example.com" });
    expect(classifyLink("/pricing", "")).toEqual({ kind: "site" });
  });

  // The control plane refuses all of these (page-create-layout-cases.json), so
  // the card must not offer a chip for them either: it refuses to show them.
  it.each([
    "",
    "http://example.com/",
    "HTTPS://example.com/",
    "javascript:alert(1)",
    "JaVaScRiPt:alert(1)",
    "data:text/html,x",
    "mailto:a@b.example",
    "tel:+15555550100",
    "//evil.example/x",
    "/\\evil.example",
    "pricing",
    "https://user@example.com/",
    "https://user:pass@example.com/",
    "https://example.com@evil.example/",
    "https://localhost/",
    "https://example.com./",
    "https:///x",
    "https://example.com:0/",
    "https://example.com:65536/",
    "https://exämple.com/",
    "https://example.com/a b",
    "https://example.com/<x>",
    "https://example.com/%zz",
    "https://example.com/%4",
    `https://example.com/${"a".repeat(OUTLINE_LIMITS.urlChars)}`,
    // An "&" that starts a character reference, in either kind of link.
    "https://example.com/?a=1&amp;b=2",
    "/shop?x=1&copy;=2",
    "https://example.com/?a=1&b;c",
    "https://example.com/a&#x2F;b",
    "https://example.com/a&#X2f;b",
    "/&#47;evil.example/login",
    // A path on the site holds no colon and no "&#", wherever they sit.
    "/shop/sale:summer",
    "/shop?time=10:30",
    "/shop/?time=10:30",
    "/shop#a:b",
    "/a&#58b",
    "/a&#x3ag",
    "/a?x=1&#top",
  ])("refuses %j", (url) => {
    expect(parseLink(url)).toBeNull();
    expect(classifyLink(url, SITE)).toBeNull();
  });

  it("accepts a link of exactly the length limit", () => {
    const url = `/${"a".repeat(OUTLINE_LIMITS.urlChars - 1)}`;
    expect(url.length).toBe(OUTLINE_LIMITS.urlChars);
    expect(parseLink(url)).toEqual({ kind: "path" });
  });

  // The near misses of the two rules above stay accepted: a guard that refuses
  // honest links gets switched off.
  it.each([
    ["/shop?x=1&copy=2#top", { kind: "path" }],
    ["/shop/sale%3Asummer", { kind: "path" }],
    ["https://example.com/?a=1&copy=2&b=3&;c&#;d&#x;e&#xg;", { kind: "absolute", host: "example.com" }],
    ["https://example.com/shop/sale:summer?t=10:30", { kind: "absolute", host: "example.com" }],
    ["https://shop.example.com:8443/a?x=1&y=2#top", { kind: "absolute", host: "shop.example.com:8443" }],
  ])("keeps %j", (url, parsed) => {
    expect(parseLink(url)).toEqual(parsed);
  });
});

describe("the words on the card", () => {
  function preview(outline: unknown[]): PagePreview {
    const input = JSON.stringify({ post_type: "page", editor: "wordpress_blocks", title: "T", outline });
    const ids = [...new Set([...input.matchAll(/"attachment_id":(\d+)/g)].map((m) => Number(m[1])))];
    const media = ids.map((id) => ({ id, filename: `f${id}.jpg`, mime: "image/jpeg", width: 1, height: 1 }));
    const p = parsePagePreview(input, media);
    if (p === null) throw new Error("fixture outline is not showable");
    return p;
  }
  const p = { type: "paragraph", text: "x" };
  const img = (id: number) => ({ type: "image", attachment_id: id, alt: "" });
  const btn = (url = "/a") => ({ text: "go", url });

  it("summarises the layout in the design's example", () => {
    const outline = preview([
      { type: "group", children: [p] },
      { type: "columns", columns: [{ children: [p] }, { children: [p] }] },
      { type: "columns", columns: [{ children: [p] }, { children: [p] }] },
      img(1),
      img(2),
      img(3),
      { type: "buttons", buttons: [btn(), btn()] },
    ]).outline;
    expect(layoutSummary(outline)).toBe("1 section · 2 column blocks · 3 images · 2 buttons");
  });

  it("counts every kind, in the design's order", () => {
    const outline = preview([
      { type: "quote", paragraphs: ["q"] },
      { type: "table", rows: [["a"]] },
      { type: "buttons", buttons: [btn(), btn(), btn()] },
      { type: "buttons", buttons: [btn()] },
      img(1),
      { type: "columns", columns: [{ children: [p] }, { children: [p] }] },
      { type: "group", children: [p] },
      { type: "group", children: [p] },
    ]).outline;
    expect(layoutSummary(outline)).toBe("2 sections · 1 column block · 1 image · 4 buttons · 1 table · 1 quote");
  });

  it("counts each image placement, so one picture used twice is two images", () => {
    expect(layoutSummary(preview([img(5), img(5)]).outline)).toBe("2 images");
  });

  it("is absent for an outline of headings, paragraphs and lists", () => {
    const outline = preview([
      { type: "heading", level: 2, text: "h" },
      p,
      { type: "list", ordered: false, items: ["a"] },
    ]).outline;
    expect(usesLayout(outline)).toBe(false);
    expect(layoutSummary(outline)).toBeNull();
  });

  it("names separators and spacing when they are all there is, so the row is never empty", () => {
    expect(layoutSummary(preview([p, { type: "separator" }]).outline)).toBe("1 separator");
    expect(
      layoutSummary(preview([{ type: "separator" }, { type: "separator" }, { type: "spacer", size: "small" }]).outline),
    ).toBe("2 separators · 1 space");
    // Beside any other counted block they are not listed.
    expect(layoutSummary(preview([img(1), { type: "separator" }, { type: "spacer", size: "large" }]).outline)).toBe("1 image");
  });

  it("gives the spacer sizes the agent writes: 24, 48 and 96 px", () => {
    expect(spacerLabel("small")).toBe("Space · Small (24 px)");
    expect(spacerLabel("medium")).toBe("Space · Medium (48 px)");
    expect(spacerLabel("large")).toBe("Space · Large (96 px)");
  });

  it("labels columns by their widths", () => {
    expect(columnsLabel(3, undefined)).toBe("Columns · 3 · equal widths");
    expect(columnsLabel(3, [25, 50, 25])).toBe("Columns · 3 · 25% / 50% / 25%");
  });

  it("labels tables", () => {
    expect(tableLabel(2, 3, true)).toBe("Table · 2 columns × 3 rows · header row");
    expect(tableLabel(1, 1, false)).toBe("Table · 1 column × 1 row");
  });

  it("labels buttons", () => {
    expect(buttonsLabel(undefined)).toBe("Buttons");
    expect(buttonsLabel("left")).toBe("Buttons");
    expect(buttonsLabel("center")).toBe("Buttons · centred");
    expect(buttonLabel(undefined)).toBe("Button · Filled");
    expect(buttonLabel("fill")).toBe("Button · Filled");
    expect(buttonLabel("outline")).toBe("Button · Outline");
  });

  it("labels an image's size and alignment", () => {
    expect(imageSizeLabel(1200, 800)).toBe("1200 × 800");
    expect(imageSizeLabel(0, 0)).toBeNull();
    expect(imageSizeLabel(1200, 0)).toBeNull();
    expect(imageAlignLabel(undefined)).toBeNull();
    expect(imageAlignLabel("none")).toBeNull();
    expect(imageAlignLabel("center")).toBe("Centred");
    expect(imageAlignLabel("wide")).toBe("Wide");
    expect(imageAlignLabel("full")).toBe("Full width");
  });

  it("labels an image's alt text, and says when it is the media library's", () => {
    expect(imageAltLabel("A van", false)).toBe("Alt text:");
    expect(imageAltLabel("", false)).toBe("No alt text (decorative)");
    expect(imageAltLabel("A van", true)).toBe("Alt text (from the media library):");
    expect(imageAltLabel("", true)).toBe("No alt text in the media library");
  });
});

// --- a page Elementor builds ---------------------------------------------------

describe("a page Elementor builds", () => {
  // page_builder as apps/api/internal/mcp/page_create_input.go readPageCardBuilder returns it.
  const classic = { builder: "elementor", format: "classic", version: "3.35.9", layout: "containers" };
  const atomic = { ...classic, format: "atomic", version: "4.3.4" };
  const page = (outline: unknown[], extra: Record<string, unknown> = {}) =>
    JSON.stringify({ post_type: "page", editor: "builder:elementor", title: "T", outline, ...extra });
  const para = { type: "paragraph", text: "x" };

  const goldenFiles = ["elementor-classic-containers.json", "elementor-classic-sections.json"];
  const golden = goldenFiles.flatMap((file) => {
    const path = join(process.cwd(), "..", "agent", "tests", "fixtures", "ability-run", file);
    const parsed = JSON.parse(readFileSync(path, "utf8")) as { cases: Array<{ name: string; input: unknown[] }> };
    return parsed.cases.map((c) => [`${file} ${c.name}`, c.input] as const);
  });

  it("reads the agent's real golden outlines (a missing or emptied file must not pass quietly)", () => {
    expect(golden.length).toBeGreaterThanOrEqual(20);
  });

  it.each(golden)("shows every outline the Elementor mapper builds: %s", (_name, outline) => {
    const input = page(outline);
    const preview = parsePagePreview(input, factsFor(input), classic);
    expect(preview).not.toBeNull();
    expect(preview?.builder).toEqual(classic);
  });

  it("carries the site's builder facts, and none for a WordPress editor", () => {
    expect(parsePagePreview(page([para]), [], atomic)?.builder).toEqual(atomic);
    const blocks = JSON.stringify({ post_type: "page", editor: "wordpress_blocks", title: "T", outline: [para] });
    expect(parsePagePreview(blocks)?.builder).toBeNull();
    expect(parsePagePreview(blocks, [], null)?.builder).toBeNull();
  });

  it.each<[string, unknown]>([
    ["missing", undefined],
    ["null", null],
    ["another builder", { ...classic, builder: "bricks" }],
    ["the input's site_default, which the site resolves", { ...classic, format: "site_default" }],
    ["an unknown layout", { ...classic, layout: "grid" }],
    ["a version with markup", { ...classic, version: "3.35.9<script>" }],
    ["a version that is a number", { ...classic, version: 3.35 }],
    ["an empty version", { ...classic, version: "" }],
    ["a version with a prefix", { ...classic, version: "v3.35.9" }],
    ["an extra member", { ...classic, kit: 1 }],
    ["a missing member", { builder: "elementor", format: "classic", version: "3.35.9" }],
  ])("is not shown with builder facts that are %s", (_name, facts) => {
    expect(parsePagePreview(page([para]), [], facts)).toBeNull();
  });

  it.each(["3.20", "3.35.9", "4.3.4", "4.3.4-beta1", "3.35.9.1"])("accepts the version %s", (version) => {
    expect(parsePagePreview(page([para]), [], { ...classic, version })).not.toBeNull();
  });

  it("is shown with elementor_format absent, site_default or the site's own format", () => {
    expect(parsePagePreview(page([para]), [], classic)).not.toBeNull();
    expect(parsePagePreview(page([para], { elementor_format: "site_default" }), [], classic)).not.toBeNull();
    expect(parsePagePreview(page([para], { elementor_format: "classic" }), [], classic)).not.toBeNull();
    expect(parsePagePreview(page([para], { elementor_format: "atomic" }), [], atomic)).not.toBeNull();
  });

  it("is not shown when elementor_format and the site's format disagree, or the value is unknown", () => {
    expect(parsePagePreview(page([para], { elementor_format: "atomic" }), [], classic)).toBeNull();
    expect(parsePagePreview(page([para], { elementor_format: "classic" }), [], atomic)).toBeNull();
    expect(parsePagePreview(page([para], { elementor_format: "flexbox" }), [], classic)).toBeNull();
  });

  it("elementor_format or builder facts on a WordPress editor make the request not showable", () => {
    const blocks = { post_type: "page", editor: "wordpress_blocks", title: "T", outline: [para] };
    expect(parsePagePreview(JSON.stringify(blocks))).not.toBeNull();
    expect(parsePagePreview(JSON.stringify({ ...blocks, elementor_format: "classic" }))).toBeNull();
    expect(parsePagePreview(JSON.stringify(blocks), [], classic)).toBeNull();
  });

  // The outlines the control plane refuses for Elementor before any card
  // exists (apps/api/internal/mcp/ability_builder.go, elementorNodeProblem).
  const columnsOf = (child: unknown) => ({ type: "columns", columns: [{ children: [child] }, { children: [para] }] });
  it.each<[string, unknown]>([
    ["a paragraph that is one web address", { type: "paragraph", text: "https://example.com/watch?v=1" }],
    ["one address with white space and capitals", { type: "paragraph", text: " \tHTTP://EXAMPLE.COM/x \n" }],
    ["a quote paragraph that is one web address", { type: "quote", paragraphs: ["Hi", "http://example.com"] }],
    ["a wide image", { type: "image", attachment_id: 5, alt: "a", align: "wide" }],
    ["a full-width image", { type: "image", attachment_id: 5, alt: "a", align: "full" }],
    ["an outline button", { type: "buttons", buttons: [{ text: "Go", url: "/go", style: "outline" }] }],
    ["a button link with &", { type: "buttons", buttons: [{ text: "Go", url: "/go?a=1&b=2" }] }],
    ["an outline button in a section's columns", { type: "group", children: [columnsOf({ type: "buttons", buttons: [{ text: "Go", url: "/go", style: "outline" }] })] }],
  ])("is not shown with %s", (_name, node) => {
    const input = page([node]);
    expect(parsePagePreview(input, factsFor(input), classic)).toBeNull();
  });

  it.each<[string, unknown]>([
    ["an address with words around it", { type: "paragraph", text: "See https://example.com today" }],
    ["an address followed by words", { type: "paragraph", text: "https://example.com today" }],
    ["an address preceded by words", { type: "paragraph", text: "See https://example.com" }],
    ["an image aligned none or centred", { type: "image", attachment_id: 5, alt: "a", align: "center" }],
    ["a filled button", { type: "buttons", buttons: [{ text: "Go", url: "/go", style: "fill" }] }],
  ])("is shown with %s", (_name, node) => {
    const input = page([node]);
    expect(parsePagePreview(input, factsFor(input), classic)).not.toBeNull();
  });

  it("a block-editor page may still hold what Elementor refuses", () => {
    const outline = [
      { type: "image", attachment_id: 5, alt: "a", align: "wide" },
      { type: "buttons", buttons: [{ text: "Go", url: "/go?a=1&b=2", style: "outline" }] },
      { type: "paragraph", text: "https://example.com" },
    ];
    const input = JSON.stringify({ post_type: "page", editor: "wordpress_blocks", title: "T", outline });
    expect(parsePagePreview(input, factsFor(input))).not.toBeNull();
  });
});

// --- A designed page (outline version 3) -------------------------------------
//
// The same hold, for the designed-page input: the table the agent and the
// control plane read is read here from the agent's fixture path, never copied.

const V3_CASES_FILE = join(process.cwd(), "..", "agent", "tests", "fixtures", "ability-run", "page-create-v3-cases.json");
const CREATE_SCHEMA_FILE = join(process.cwd(), "..", "agent", "tests", "fixtures", "ability-run", "page-create-schema.json");

interface V3Case extends SharedCase {
  rule: string;
  generate?: { append: string; to_bytes: number };
  site?: { elementor_containers?: boolean; attachment_mimes?: Record<string, string> };
}

const v3Table = JSON.parse(readFileSync(V3_CASES_FILE, "utf8")) as { icons: string[]; cases: V3Case[] };

/** The exact input text of a case: a "generate" case is padded to its stated byte length. */
function v3InputOf(c: V3Case): string {
  if (c.generate === undefined) return c.input;
  const have = Buffer.byteLength(c.input, "utf8");
  const unit = Buffer.byteLength(c.generate.append, "utf8");
  return c.input + c.generate.append.repeat(Math.max(0, Math.floor((c.generate.to_bytes - have) / unit)));
}

/** The facts of a site that can build a designed page. */
const DESIGNED_BUILDER = { builder: "elementor", format: "classic", version: "3.35.9", layout: "containers" };

function designed(input: string, builder: unknown = DESIGNED_BUILDER, media: unknown = factsFor(input)) {
  return parseDesignedPagePreview(input, media, builder);
}

type Doc = Record<string, unknown>;
const asDoc = (v: unknown): Doc => v as Doc;
const asList = (v: unknown): Doc[] => v as Doc[];

function v3Case(name: string): V3Case {
  const found = v3Table.cases.find((c) => c.name === name);
  if (found === undefined) throw new Error(`the shared table has no case ${name}`);
  return found;
}

/** A fresh copy of a case's page, ready to be changed and shown again. */
function v3Doc(name: string): Doc {
  return JSON.parse(v3InputOf(v3Case(name))) as Doc;
}

const sectionsOf = (d: Doc): Doc[] => asList(d.outline);

function sectionAt(d: Doc, index: number): Doc {
  const section = sectionsOf(d)[index];
  if (section === undefined) throw new Error(`the page has no section ${index}`);
  return section;
}

function patternIn(d: Doc, name: string): Doc {
  for (const s of sectionsOf(d)) {
    for (const c of asList(s.children)) if (c.type === "pattern" && c.pattern === name) return c;
  }
  throw new Error(`the page has no ${name} pattern`);
}

const showDoc = (d: Doc, builder: unknown = DESIGNED_BUILDER) => designed(JSON.stringify(d), builder);

describe("a designed page: the card accepts exactly what the control plane accepts", () => {
  const { cases, icons } = v3Table;

  it("reads a real table (a missing or emptied file must not pass quietly)", () => {
    expect(cases.length).toBeGreaterThanOrEqual(50);
    expect(cases.filter((c) => c.go === "accept").length).toBeGreaterThan(10);
    expect(cases.filter((c) => c.go === "refuse").length).toBeGreaterThan(30);
    expect(icons).toHaveLength(40);
    const names = new Set(cases.map((c) => c.name));
    for (const probe of [
      "v3-full-landing-page",
      "anchor-duplicate",
      "anchor-reserved",
      "design-ink-on-ground-contrast-low",
      "design-colour-pure-black",
      "hero-not-the-first-child",
      "h1-twice-in-the-hero",
      "link-anchor-missing",
      "icon-not-in-the-list",
      "images-21-counting-backgrounds",
      "landing-page-over-the-input-limit",
      "two-heroes",
      "two-heroes-the-first-out-of-place",
      "design-ratio-as-a-number",
      "design-ratio-unknown-name",
    ]) {
      expect(names.has(probe), probe).toBe(true);
    }
  });

  it.each(cases.map((c) => [c.name, c] as const))("%s", (_name, c) => {
    expect(designed(v3InputOf(c)) !== null).toBe(c.go === "accept");
  });

  it("the card's icon list is the table's icon list", () => {
    expect([...PATTERN_ICONS]).toEqual(icons);
  });

  it("the card's ratio names are the agent's page-create schema's", () => {
    const schema = JSON.parse(readFileSync(CREATE_SCHEMA_FILE, "utf8")) as {
      properties: { design: { properties: { scale: { properties: { ratio: { type: string; enum: string[] } } } } } };
    };
    const ratio = schema.properties.design.properties.scale.properties.ratio;
    expect(ratio.type).toBe("string");
    expect([...SCALE_RATIOS]).toEqual(ratio.enum);
  });

  it.each(["design-ratio-as-a-number", "design-ratio-unknown-name"])(
    "%s is refused for its ratio alone: with a ratio name in its place the card shows it",
    (name) => {
      const doc = v3Doc(name);
      expect(showDoc(doc)).toBeNull();
      asDoc(asDoc(doc.design).scale).ratio = "major_third";
      expect(showDoc(doc)).not.toBeNull();
    },
  );

  it("the version 2 reader shows none of them, so a designed page is never shown in part by it", () => {
    const shown = cases
      .filter((c) => {
        const input = v3InputOf(c);
        return parsePagePreview(input, factsFor(input), DESIGNED_BUILDER) !== null;
      })
      .map((c) => c.name);
    expect(shown).toEqual([]);
  });

  it("the designed reader shows no version 2 input", () => {
    const v2 = sharedCases();
    expect(v2.length).toBeGreaterThan(100);
    const shown = v2.filter((c) => designed(c.input) !== null).map((c) => c.name);
    expect(shown).toEqual([]);
  });
});

describe("a designed page as data", () => {
  it("returns the page as the AI wrote it, with its design and frame", () => {
    const doc = v3Doc("v3-full-landing-page");
    const shown = showDoc(doc);
    expect(shown).not.toBeNull();
    expect(shown?.title).toBe(doc.title);
    expect(shown?.pageFrame).toBe("full_width");
    expect(shown?.design.colors).toEqual(asDoc(doc.design).colors);
    expect(shown?.design.scale).toEqual({ base_px: 17, ratio: "major_third" });
    expect(shown?.sections.map((s) => s.anchor)).toEqual(sectionsOf(doc).map((s) => s.anchor));
    expect((shown?.sections.length ?? 0) > 5).toBe(true);
    expect(shown?.builder.layout).toBe("containers");
    expect(shown?.media.get(42)?.id).toBe(42);
  });

  it("keeps the AI's words exactly, markup and all (the agent judges text)", () => {
    const shown = showDoc(v3Doc("pattern-text-with-markup"));
    const faq = asList(asDoc(shown?.sections[0]?.children[0]).faq);
    expect(faq[0]?.answer).toBe("Visit <b>our</b> page.");
  });

  describe("the page frame", () => {
    it("is full width when the page opens with a hero and says nothing", () => {
      const doc = v3Doc("v3-full-landing-page");
      delete doc.page_frame;
      expect(showDoc(doc)?.pageFrame).toBe("full_width");
    });
    it("is the theme's when the page has no hero and says nothing", () => {
      const doc = v3Doc("v3-minimal-page");
      delete doc.page_frame;
      expect(showDoc(doc)?.pageFrame).toBe("theme");
    });
    it.each([
      ["v3-minimal-page", "canvas"],
      ["v3-minimal-page", "full_width"],
      ["v3-full-landing-page", "theme"],
      ["v3-full-landing-page", "canvas"],
    ])("keeps what %s asks for (%s)", (name, frame) => {
      const doc = v3Doc(name);
      doc.page_frame = frame;
      expect(showDoc(doc)?.pageFrame).toBe(frame);
    });
    it("refuses a frame it does not know", () => {
      const doc = v3Doc("v3-minimal-page");
      doc.page_frame = "popup";
      expect(showDoc(doc)).toBeNull();
    });
  });

  describe("the design", () => {
    const colours = (d: Doc) => asDoc(asDoc(d.design).colors);
    const design = (d: Doc) => asDoc(d.design);

    it("returns colours in upper case, whatever case they were sent in", () => {
      const doc = v3Doc("v3-minimal-page");
      colours(doc).accent = "#0d7a63";
      colours(doc).muted = "#5a606c";
      const shown = showDoc(doc);
      expect(shown?.design.colors.accent).toBe("#0D7A63");
      expect(shown?.design.colors.muted).toBe("#5A606C");
    });

    it.each<[string, (d: Doc) => void, boolean]>([
      ["a colour without the hash", (d) => { colours(d).accent = "0D7A63"; }, false],
      ["a three digit colour", (d) => { colours(d).accent = "#0D7"; }, false],
      ["an eight digit colour", (d) => { colours(d).accent = "#0D7A63FF"; }, false],
      ["a colour with a letter past F", (d) => { colours(d).accent = "#0D7A6G"; }, false],
      ["a colour that ends with a line feed", (d) => { colours(d).accent = "#0D7A63\n"; }, false],
      ["a colour name", (d) => { colours(d).accent = "teal"; }, false],
      ["a colour that is a number", (d) => { colours(d).accent = 123456; }, false],
      ["pure black", (d) => { colours(d).muted = "#000000"; }, false],
      ["ink that just passes on white (4.54 to 1)", (d) => { colours(d).ground = "#FFFFFF"; colours(d).ink = "#767676"; }, true],
      ["ink that just misses on white (4.48 to 1)", (d) => { colours(d).ground = "#FFFFFF"; colours(d).ink = "#777777"; }, false],
      ["ink that misses on the surface only", (d) => { colours(d).surface = "#4A4F5A"; }, false],
      ["text on the accent that misses", (d) => { colours(d).on_accent = "#9AD9CB"; }, false],
      ["a colour role left out", (d) => { delete colours(d).muted; }, false],
      ["a seventh colour role", (d) => { colours(d).highlight = "#FFFF00"; }, false],
      ["system as a font family", (d) => { asDoc(design(d).fonts).heading = "system"; }, true],
      ["a font family of 60 characters", (d) => { asDoc(design(d).fonts).heading = "A".repeat(60); }, true],
      ["a font family of 61 characters", (d) => { asDoc(design(d).fonts).heading = "A".repeat(61); }, false],
      ["an empty font family", (d) => { asDoc(design(d).fonts).body = ""; }, false],
      ["a font family that can reach a style rule", (d) => { asDoc(design(d).fonts).body = "Inter; color: red"; }, false],
      ["a font family with a quote", (d) => { asDoc(design(d).fonts).body = "Inter'"; }, false],
      ["a font family with a line feed at the end", (d) => { asDoc(design(d).fonts).body = "Inter\n"; }, false],
      ["a base size of 14", (d) => { asDoc(design(d).scale).base_px = 14; }, false],
      ["a base size of 15", (d) => { asDoc(design(d).scale).base_px = 15; }, true],
      ["a base size of 20", (d) => { asDoc(design(d).scale).base_px = 20; }, true],
      ["a base size of 21", (d) => { asDoc(design(d).scale).base_px = 21; }, false],
      ["a base size sent as text", (d) => { asDoc(design(d).scale).base_px = "17"; }, false],
      ["the ratio major_second", (d) => { asDoc(design(d).scale).ratio = "major_second"; }, true],
      ["the ratio minor_third", (d) => { asDoc(design(d).scale).ratio = "minor_third"; }, true],
      ["the ratio major_third", (d) => { asDoc(design(d).scale).ratio = "major_third"; }, true],
      ["the ratio perfect_fourth", (d) => { asDoc(design(d).scale).ratio = "perfect_fourth"; }, true],
      ["a ratio name the list does not hold", (d) => { asDoc(design(d).scale).ratio = "golden"; }, false],
      ["a ratio name in capitals", (d) => { asDoc(design(d).scale).ratio = "MAJOR_THIRD"; }, false],
      ["a ratio name with a line feed at the end", (d) => { asDoc(design(d).scale).ratio = "major_third\n"; }, false],
      ["a ratio name with a space before it", (d) => { asDoc(design(d).scale).ratio = " major_third"; }, false],
      ["an empty ratio", (d) => { asDoc(design(d).scale).ratio = ""; }, false],
      ["a ratio that is a fraction", (d) => { asDoc(design(d).scale).ratio = 1.25; }, false],
      ["a ratio that is the whole number 1", (d) => { asDoc(design(d).scale).ratio = 1; }, false],
      ["a ratio sent as the text of its number", (d) => { asDoc(design(d).scale).ratio = "1.25"; }, false],
      ["a ratio that is null", (d) => { asDoc(design(d).scale).ratio = null; }, false],
      ["a ratio left out", (d) => { delete asDoc(design(d).scale).ratio; }, false],
      ["radius none", (d) => { design(d).radius = "none"; }, true],
      ["radius lg", (d) => { design(d).radius = "lg"; }, true],
      ["radius xl", (d) => { design(d).radius = "xl"; }, false],
      ["density airy", (d) => { design(d).density = "airy"; }, true],
      ["density dense", (d) => { design(d).density = "dense"; }, false],
      ["a design that is not an object", (d) => { d.design = "dark"; }, false],
      ["a design with no fonts", (d) => { delete design(d).fonts; }, false],
    ])("%s", (_name, edit, shown) => {
      const doc = v3Doc("v3-minimal-page");
      edit(doc);
      expect(showDoc(doc) !== null).toBe(shown);
    });

    it("measures contrast the way the accessibility guidelines do", () => {
      expect(contrastRatio("#000000", "#FFFFFF")).toBeCloseTo(21, 5);
      expect(contrastRatio("#FFFFFF", "#000000")).toBeCloseTo(21, 5);
      expect(contrastRatio("#767676", "#FFFFFF")).toBeCloseTo(4.54, 2);
      expect(contrastRatio("#777777", "#FFFFFF")).toBeCloseTo(4.48, 2);
      expect(contrastRatio("#15171C", "#15171C")).toBe(1);
    });
  });

  describe("whole numbers", () => {
    const base = () => v3InputOf(v3Case("v3-full-landing-page"));

    it("holds the type ratio as a name, so the whole input has no number with a fraction", () => {
      expect(base()).toContain('"ratio":"major_third"');
      expect(readInput(base())).not.toBeUndefined();
      expect(designed(base())).not.toBeNull();
    });
    it.each([
      ['"outline_version":3', '"outline_version":3.0'],
      ['"base_px":17', '"base_px":17.0'],
      ['"base_px":17', '"base_px":17.5'],
      ['"attachment_id":42', '"attachment_id":42.0'],
      ['"attachment_id":42', '"attachment_id":4.2e1'],
      ['"value":1200', '"value":1200.0'],
      ['"ratio":"major_third"', '"ratio":1.25'],
      ['"ratio":"major_third"', '"ratio":1.0'],
      ['"ratio":"major_third"', '"ratio":1'],
    ])("refuses %s written as %s", (from, to) => {
      expect(base()).toContain(from);
      expect(designed(base().replace(from, to))).toBeNull();
    });

    it("reads no number with a fraction or an exponent at any key, the ratio's included", () => {
      for (const text of [
        '{"design":{"scale":{"ratio":1.25}}}',
        '{"ratio":1.0}',
        '{"ratio":1e0}',
        '{ "ratio" : 1.25 }',
        '{"a":{"b":[0,1.5]}}',
        "[-0.5]",
      ]) {
        expect(readInput(text), text).toBeUndefined();
      }
    });

    it("gives no key an exemption, whatever a caller passes after the text", () => {
      const withKeys = readInput as (text: string, keys: string[]) => unknown;
      expect(withKeys('{"ratio":1.25}', ["ratio"])).toBeUndefined();
    });

    it("reads whole numbers, names, and text that only looks like a number", () => {
      const text = '{"ratio":"major_third","n":[0,-3,42],"text":"1.25 and 2e3 and \\"ratio\\":1.5"}';
      expect(readInput(text)).toEqual({
        ratio: "major_third",
        n: [0, -3, 42],
        text: '1.25 and 2e3 and "ratio":1.5',
      });
    });
  });

  describe("a request the card cannot show in full is not shown in part", () => {
    it("shows the page these edits start from", () => {
      expect(showDoc(v3Doc("v3-full-landing-page"))).not.toBeNull();
    });

    const extraKeys: Array<[string, (d: Doc) => void]> = [
      ["the page", (d) => { d.status = "publish"; }],
      ["the design", (d) => { asDoc(d.design).shadow = "none"; }],
      ["the colours", (d) => { asDoc(asDoc(d.design).colors).highlight = "#FFFF00"; }],
      ["the fonts", (d) => { asDoc(asDoc(d.design).fonts).mono = "Courier"; }],
      ["the scale", (d) => { asDoc(asDoc(d.design).scale).step = 3; }],
      ["a section", (d) => { sectionAt(d, 0).padding = 48; }],
      ["a section background", (d) => { asDoc(sectionAt(d, 0).background).blur = 8; }],
      ["the hero", (d) => { patternIn(d, "hero").columns = 2; }],
      ["a hero heading", (d) => { asDoc(asList(patternIn(d, "hero").children)[0]).style = "color:red"; }],
      ["a feature", (d) => { asDoc(asList(patternIn(d, "features").features)[0]).colour = "red"; }],
      ["a testimonial", (d) => { asDoc(asList(patternIn(d, "testimonials").testimonials)[0]).link = "/x"; }],
      ["a plan", (d) => { asDoc(asList(patternIn(d, "pricing").plans)[0]).badge = "new"; }],
      ["a plan button", (d) => { asDoc(asDoc(asList(patternIn(d, "pricing").plans)[0]).cta).target = "_blank"; }],
      ["a stat", (d) => { asDoc(asList(patternIn(d, "stats").stats)[0]).unit = "x"; }],
      ["a logo", (d) => { asDoc(asList(patternIn(d, "logos").logos)[0]).href = "/x"; }],
      ["a question", (d) => { asDoc(asList(patternIn(d, "faq").faq)[0]).open = true; }],
      ["the call to action", (d) => { patternIn(d, "cta").items = []; }],
    ];
    it.each(extraKeys)("an unknown key on %s makes the request not showable", (_name, edit) => {
      const doc = v3Doc("v3-full-landing-page");
      edit(doc);
      expect(showDoc(doc)).toBeNull();
    });

    const badValues: Array<[string, (d: Doc) => void]> = [
      ["a section tone", (d) => { sectionAt(d, 1).tone = "dark"; }],
      ["a section width", (d) => { sectionAt(d, 0).width = "wide"; }],
      ["a section spacing", (d) => { sectionAt(d, 1).spacing = "sm"; }],
      ["a section alignment", (d) => { sectionAt(d, 0).align = "left"; }],
      ["a section height", (d) => { sectionAt(d, 0).min_height = "half"; }],
      ["a background overlay", (d) => { asDoc(sectionAt(d, 0).background).overlay = "heavy"; }],
      ["a background focal point", (d) => { asDoc(sectionAt(d, 0).background).focal = "left"; }],
      ["a background that is not an attachment number", (d) => { asDoc(sectionAt(d, 0).background).attachment_id = "42"; }],
      ["a hero variant", (d) => { patternIn(d, "hero").variant = "wide"; }],
      ["a features variant", (d) => { patternIn(d, "features").variant = "carousel"; }],
      ["a pattern name", (d) => { patternIn(d, "logos").pattern = "carousel"; }],
      ["a heading level of 0", (d) => { asDoc(asList(patternIn(d, "hero").children)[0]).level = 0; }],
      ["a heading level of 5", (d) => { asDoc(asList(patternIn(d, "hero").children)[0]).level = 5; }],
      ["a button style", (d) => { asDoc(asList(asDoc(asList(patternIn(d, "hero").children)[2]).buttons)[0]).style = "ghost"; }],
      ["a stat value below zero", (d) => { asDoc(asList(patternIn(d, "stats").stats)[0]).value = -1; }],
      ["a stat value past the cap", (d) => { asDoc(asList(patternIn(d, "stats").stats)[0]).value = 1_000_000_000; }],
      ["a stat value sent as text", (d) => { asDoc(asList(patternIn(d, "stats").stats)[0]).value = "12"; }],
      ["a plan without a button", (d) => { delete asDoc(asList(patternIn(d, "pricing").plans)[0]).cta; }],
      ["a plan with no features", (d) => { asDoc(asList(patternIn(d, "pricing").plans)[0]).features = []; }],
      ["a plan with nine features", (d) => { asDoc(asList(patternIn(d, "pricing").plans)[0]).features = Array.from({ length: 9 }, () => "x"); }],
      ["a highlighted flag sent as text", (d) => { asDoc(asList(patternIn(d, "pricing").plans)[0]).highlighted = "yes"; }],
      ["a logo without its attachment", (d) => { delete asDoc(asList(patternIn(d, "logos").logos)[0]).attachment_id; }],
      ["a feature without its icon", (d) => { delete asDoc(asList(patternIn(d, "features").features)[0]).icon; }],
      ["a hero with seven children", (d) => { patternIn(d, "hero").children = Array.from({ length: 7 }, () => ({ type: "paragraph", text: "x" })); }],
      ["a call to action with five children", (d) => { patternIn(d, "cta").children = Array.from({ length: 5 }, () => ({ type: "paragraph", text: "x" })); }],
      ["a hero that holds a list", (d) => { patternIn(d, "hero").children = [{ type: "list", ordered: false, items: ["a"] }]; }],
      ["a call to action that holds an image", (d) => { patternIn(d, "cta").children = [{ type: "image", attachment_id: 42, alt: "a" }]; }],
      ["a section that is not an object", (d) => { sectionsOf(d)[1] = "section" as unknown as Doc; }],
      ["an editor that is not Elementor", (d) => { d.editor = "wordpress_blocks"; }],
      ["a post type of media", (d) => { d.post_type = "attachment"; }],
      ["a blank title", (d) => { d.title = "   "; }],
    ];
    it.each(badValues)("%s makes the request not showable", (_name, edit) => {
      const doc = v3Doc("v3-full-landing-page");
      edit(doc);
      expect(showDoc(doc)).toBeNull();
    });
  });

  describe("what a section may hold", () => {
    const withChildren = (children: unknown[]): Doc => {
      const doc = v3Doc("v3-minimal-page");
      sectionAt(doc, 0).children = children;
      return doc;
    };
    const para = { type: "paragraph", text: "x" };
    const columns = {
      type: "columns",
      columns: [{ children: [{ type: "paragraph", text: "a" }] }, { children: [{ type: "paragraph", text: "b" }] }],
    };
    const features = (n: number) => ({
      type: "pattern",
      pattern: "features",
      variant: "split",
      features: Array.from({ length: n }, () => ({ icon: "bolt", title: "t", text: "x" })),
    });

    it.each<[string, unknown[], boolean]>([
      ["a column block", [columns], true],
      ["a group of leaves and a column block", [{ type: "group", children: [para, columns] }], true],
      ["a pattern between leaves", [para, features(3), para], true],
      ["every leaf a version 2 page has", [
        { type: "heading", level: 3, text: "h" }, para, { type: "list", ordered: true, items: ["a"] },
        { type: "image", attachment_id: 5, alt: "a", align: "center" }, { type: "quote", paragraphs: ["q"] },
        { type: "separator" }, { type: "spacer", size: "large" }, { type: "table", rows: [["a"]] },
      ], true],
      ["a section inside a section", [{ type: "section", anchor: "inner", children: [para] }], false],
      ["a pattern inside a group", [{ type: "group", children: [features(3)] }], false],
      ["a pattern inside a column", [{ type: "columns", columns: [{ children: [features(3)] }, { children: [para] }] }], false],
      ["a group inside a group", [{ type: "group", children: [{ type: "group", children: [para] }] }], false],
      ["a heading of level one in a group", [{ type: "group", children: [{ type: "heading", level: 1, text: "h" }] }], false],
      ["a heading of level one in a column", [{ type: "columns", columns: [{ children: [{ type: "heading", level: 1, text: "h" }] }, { children: [para] }] }], false],
      ["a heading of level one in a call to action", [{ type: "pattern", pattern: "cta", variant: "band", children: [{ type: "heading", level: 1, text: "h" }] }], false],
      ["50 children", Array.from({ length: 50 }, () => para), true],
      ["51 children", Array.from({ length: 51 }, () => para), false],
      ["a pattern with an unlisted item array", [{ type: "pattern", pattern: "faq", variant: "accordion", items: [] }], false],
      ["a features pattern holding a faq array too", [{ ...features(3), faq: [{ question: "q", answer: "a" }] }], false],
    ])("%s", (_name, children, shown) => {
      expect(showDoc(withChildren(children)) !== null).toBe(shown);
    });

    it("refuses a page of 450 nodes and accepts one of 250", () => {
      const sections = (paragraphs: number) => {
        const doc = v3Doc("v3-minimal-page");
        const out: Doc[] = [];
        let left = paragraphs;
        for (let i = 0; left > 0; i += 1) {
          const n = Math.min(left, 50);
          out.push({ type: "section", anchor: `part-${String.fromCharCode(97 + i)}`, children: Array.from({ length: n }, () => para) });
          left -= n;
        }
        doc.outline = out;
        return doc;
      };
      // 450 paragraphs are past the cap of 400 nodes whatever else is counted; 250 are under it.
      expect(showDoc(sections(450))).toBeNull();
      expect(showDoc(sections(250))).not.toBeNull();
    });
  });

  describe("links", () => {
    const heroButtonUrl = (d: Doc, url: string) => {
      asDoc(asList(asDoc(asList(patternIn(d, "hero").children)[2]).buttons)[0]).url = url;
    };
    const planUrl = (d: Doc, url: string) => {
      asDoc(asDoc(asList(patternIn(d, "pricing").plans)[0]).cta).url = url;
    };

    it.each<[string, boolean]>([
      ["#pricing", true],
      ["#top", true],
      ["#nope", false],
      ["#Pricing", false],
      ["#a", false],
      ["#", false],
      ["#pri cing", false],
      ["/pricing#plans", true],
      ["/contact", true],
      ["https://example.com/call", true],
      ["http://example.com/call", false],
      ["javascript:alert(1)", false],
      ["//evil.example/x", false],
      ["mailto:a@example.com", false],
      ["", false],
    ])("a button link of %j", (url, shown) => {
      const doc = v3Doc("v3-full-landing-page");
      heroButtonUrl(doc, url);
      expect(showDoc(doc) !== null).toBe(shown);
    });

    it.each<[string, boolean]>([
      ["#signup", true],
      ["#missing", false],
      ["javascript:alert(1)", false],
      ["https://example.com/buy", true],
    ])("a plan button link of %j", (url, shown) => {
      const doc = v3Doc("v3-full-landing-page");
      planUrl(doc, url);
      expect(showDoc(doc) !== null).toBe(shown);
    });

    // An Elementor page cannot keep an ampersand in a link, so a link holding one
    // is refused wherever the page holds a link: a button of a section, of the
    // hero, of the call to action, of a group or of a column, and the button of
    // every plan. The same rule the agent and the control plane answer
    // (the shared table's link_no_ampersand rows).
    describe("a link holds no ampersand", () => {
      const WITH_AMPERSAND = "/signup?plan=pro&seats=5";
      const WITHOUT_AMPERSAND = "/signup?plan=pro";
      const buttonsNode = (url: string): Doc => ({ type: "buttons", buttons: [{ text: "Go", url }] });
      const addToSection = (d: Doc, node: Doc) => {
        asList(sectionAt(d, 2).children).push(node);
      };
      const planCta = (d: Doc, index: number): Doc => asDoc(asDoc(asList(patternIn(d, "pricing").plans)[index]).cta);

      const places: Array<[string, (d: Doc, url: string) => void]> = [
        ["a button of a section", (d, url) => addToSection(d, buttonsNode(url))],
        ["a button of the hero", (d, url) => heroButtonUrl(d, url)],
        [
          "a button of the call to action",
          (d, url) => {
            asDoc(asList(asDoc(asList(patternIn(d, "cta").children)[2]).buttons)[0]).url = url;
          },
        ],
        ["a button of a group", (d, url) => addToSection(d, { type: "group", children: [buttonsNode(url)] })],
        [
          "a button of a column",
          (d, url) =>
            addToSection(d, {
              type: "columns",
              columns: [{ children: [buttonsNode(url)] }, { children: [{ type: "paragraph", text: "Beside it." }] }],
            }),
        ],
        [
          "the button of the first plan",
          (d, url) => {
            planCta(d, 0).url = url;
          },
        ],
        [
          "the button of the second plan",
          (d, url) => {
            planCta(d, 1).url = url;
          },
        ],
        [
          "the button of the last plan",
          (d, url) => {
            planCta(d, 2).url = url;
          },
        ],
      ];

      it.each(places)("%s: refused with an ampersand, shown without one", (_name, put) => {
        const refused = v3Doc("v3-full-landing-page");
        put(refused, WITH_AMPERSAND);
        expect(showDoc(refused)).toBeNull();

        const shown = v3Doc("v3-full-landing-page");
        put(shown, WITHOUT_AMPERSAND);
        expect(showDoc(shown)).not.toBeNull();
      });

      it.each<[string, string]>([
        ["an absolute address", "https://example.com/buy?plan=pro&seats=5"],
        ["an ampersand after the query", "/buy?plan=pro&"],
        ["an ampersand in the fragment", "/buy#a&b"],
        ["a character reference", "/buy?a=1&amp;b=2"],
      ])("%s is refused in a plan button and in a section button", (_name, url) => {
        const plan = v3Doc("v3-full-landing-page");
        planCta(plan, 1).url = url;
        expect(showDoc(plan)).toBeNull();

        const section = v3Doc("v3-full-landing-page");
        addToSection(section, buttonsNode(url));
        expect(showDoc(section)).toBeNull();
      });

      it("keeps an ampersand that is text: a heading, a button label, a plan's name, price, feature and button label", () => {
        const doc = v3Doc("v3-full-landing-page");
        const plan = asDoc(asList(patternIn(doc, "pricing").plans)[0]);
        plan.name = "Team & Agency";
        plan.price = "49 & up";
        plan.features = ["5 sites & staging"];
        asDoc(plan.cta).text = "Start & save";
        addToSection(doc, { type: "heading", level: 2, text: "Terms & pricing" });
        addToSection(doc, { type: "buttons", buttons: [{ text: "Terms & conditions", url: "/terms" }] });
        const shown = showDoc(doc);
        expect(shown).not.toBeNull();
        const text = JSON.stringify(shown?.sections);
        expect(text).toContain("Team & Agency");
        expect(text).toContain("Terms & conditions");
      });
    });

    it("judges a link to an anchor after the whole page, not at the place it stands", () => {
      expect(showDoc(v3Doc("link-to-a-later-anchor"))).not.toBeNull();
    });

    it("loses its target when the section it points at is dropped", () => {
      const doc = v3Doc("v3-full-landing-page");
      doc.outline = sectionsOf(doc).filter((s) => s.anchor !== "pricing");
      expect(showDoc(doc)).toBeNull();
    });
  });

  describe("anchors", () => {
    it.each<[string, boolean]>([
      ["ab", true],
      ["a", false],
      ["a1", true],
      ["1a", false],
      ["a_b", false],
      ["a b", false],
      ["Ab", false],
      ["a-b-c", true],
      ["site-header", false],
      ["wpadminbar", false],
      ["masthead", false],
      ["content", false],
      ["contents", true],
    ])("an anchor of %j", (anchor, shown) => {
      const doc = v3Doc("v3-minimal-page");
      sectionAt(doc, 0).anchor = anchor;
      expect(showDoc(doc) !== null).toBe(shown);
    });
  });

  describe("what Elementor builds as asked", () => {
    const into = (d: Doc, node: unknown) => {
      asList(sectionAt(d, 2).children).push(node as Doc);
    };
    it.each<[string, unknown, boolean]>([
      ["an address-only paragraph", { type: "paragraph", text: "https://example.com/watch" }, false],
      ["an address with words around it", { type: "paragraph", text: "See https://example.com today" }, true],
      ["an address-only quote", { type: "quote", paragraphs: ["http://example.com"] }, false],
      ["an image aligned wide", { type: "image", attachment_id: 5, alt: "a", align: "wide" }, false],
      ["an image aligned full", { type: "image", attachment_id: 5, alt: "a", align: "full" }, false],
      ["an image aligned centre", { type: "image", attachment_id: 5, alt: "a", align: "center" }, true],
      ["an outline button", { type: "buttons", buttons: [{ text: "Go", url: "/go", style: "outline" }] }, true],
      ["a text button", { type: "buttons", buttons: [{ text: "Go", url: "/go", style: "text" }] }, true],
      ["a button link with an ampersand", { type: "buttons", buttons: [{ text: "Go", url: "/go?a=1&b=2" }] }, false],
    ])("%s", (_name, node, shown) => {
      const doc = v3Doc("v3-full-landing-page");
      into(doc, node);
      expect(showDoc(doc) !== null).toBe(shown);
    });

    it("refuses an address-only paragraph inside the hero too", () => {
      const doc = v3Doc("v3-full-landing-page");
      asList(patternIn(doc, "hero").children).push({ type: "paragraph", text: "https://example.com/watch" });
      expect(showDoc(doc)).toBeNull();
    });
  });

  describe("the site's facts", () => {
    const input = () => v3InputOf(v3Case("v3-full-landing-page"));

    it("needs Elementor's classic format with containers, as stated by the site", () => {
      expect(designed(input())).not.toBeNull();
      expect(designed(input(), { ...DESIGNED_BUILDER, format: "atomic" })).toBeNull();
      expect(designed(input(), { ...DESIGNED_BUILDER, layout: "sections" })).toBeNull();
      // No facts at all (an argument left out is not the helper's default).
      expect(parseDesignedPagePreview(input(), factsFor(input()))).toBeNull();
      expect(designed(input(), null)).toBeNull();
      expect(designed(input(), { ...DESIGNED_BUILDER, version: "x" })).toBeNull();
    });

    it("keeps a designed page on a section and column site off the card, as the control plane's site check does", () => {
      const c = v3Case("sections-on-a-section-and-column-site");
      expect(c.site?.elementor_containers).toBe(false);
      expect(c.go).toBe("accept");
      expect(designed(v3InputOf(c))).not.toBeNull();
      expect(designed(v3InputOf(c), { ...DESIGNED_BUILDER, layout: "sections" })).toBeNull();
    });

    it("needs a fact for the section background, and the site's fact must be an image", () => {
      const c = v3Case("background-not-an-image");
      expect(c.site?.attachment_mimes?.["43"]).toBe("application/pdf");
      const text = v3InputOf(c);
      expect(designed(text)).not.toBeNull();
      expect(designed(text, DESIGNED_BUILDER, [])).toBeNull();
      expect(designed(text, DESIGNED_BUILDER, [{ id: 43, filename: "a.pdf", mime: "application/pdf", width: 0, height: 0 }])).toBeNull();
    });

    it("needs a fact for every image block", () => {
      const doc = v3Doc("v3-minimal-page");
      asList(sectionAt(doc, 0).children).push({ type: "image", attachment_id: 77, alt: "a" });
      const text = JSON.stringify(doc);
      expect(designed(text)).not.toBeNull();
      expect(designed(text, DESIGNED_BUILDER, [])).toBeNull();
    });

    it.each([
      ["a section background", 42],
      ["a logo", 101],
      ["a testimonial avatar", 201],
    ])("needs a fact for %s (attachment %i)", (_name, id) => {
      const text = input();
      const facts = factsFor(text) as Array<{ id: number }>;
      expect(facts.some((f) => f.id === id)).toBe(true);
      expect(designed(text, DESIGNED_BUILDER, facts)).not.toBeNull();
      expect(designed(text, DESIGNED_BUILDER, facts.filter((f) => f.id !== id))).toBeNull();
    });

    it("refuses facts that are not a clean list, as a whole", () => {
      const text = input();
      const fact = { id: 42, filename: "f42.jpg", mime: "image/jpeg", width: 10, height: 10 };
      expect(designed(text, DESIGNED_BUILDER, [fact, fact])).toBeNull();
      expect(designed(text, DESIGNED_BUILDER, [{ ...fact, mime: "text/html" }])).toBeNull();
    });
  });

  describe("sizes", () => {
    it("counts the input in bytes, not characters", () => {
      const withText = (n: number) => {
        const doc = v3Doc("v3-minimal-page");
        const first = asList(sectionAt(doc, 0).children)[0];
        if (first === undefined) throw new Error("the page has no first block");
        // Two bytes in UTF-8, one character in the string.
        first.text = "é".repeat(n);
        return JSON.stringify(doc);
      };
      const room = Math.floor((DESIGNED_LIMITS.inputBytes - Buffer.byteLength(withText(0), "utf8")) / 2);
      expect(Buffer.byteLength(withText(room), "utf8")).toBeLessThanOrEqual(DESIGNED_LIMITS.inputBytes);
      expect(Buffer.byteLength(withText(room + 1), "utf8")).toBeGreaterThan(DESIGNED_LIMITS.inputBytes);
      expect(withText(room + 1).length).toBeLessThan(DESIGNED_LIMITS.inputBytes);
      expect(designed(withText(room))).not.toBeNull();
      expect(designed(withText(room + 1))).toBeNull();
    });

    it("holds the images of a page, backgrounds included, to 20", () => {
      const page = (backgrounds: number, blocks: number) => {
        const doc = v3Doc("v3-minimal-page");
        doc.outline = Array.from({ length: Math.max(backgrounds, 1) }, (_v, i) => ({
          type: "section",
          anchor: `part-${String.fromCharCode(97 + i)}`,
          ...(i < backgrounds ? { background: { attachment_id: 40 + i } } : {}),
          children: i === 0 ? [{ type: "paragraph", text: "x" }, ...Array.from({ length: blocks }, () => ({ type: "image", attachment_id: 100, alt: "a" }))] : [{ type: "paragraph", text: "x" }],
        }));
        return doc;
      };
      expect(showDoc(page(10, 10))).not.toBeNull();
      expect(showDoc(page(10, 11))).toBeNull();
      expect(showDoc(page(11, 10))).toBeNull();
      expect(showDoc(page(0, 21))).toBeNull();
    });

    it("counts a logo and a testimonial avatar among the 20 images, as a background is", () => {
      const withBlocks = (n: number) => {
        const doc = v3Doc("v3-full-landing-page");
        // The page already names 1 background, 6 logos and 3 avatars: 10 images.
        // One attachment named n times: the cap counts every mention, while the site's facts stay few.
        const blocks = Array.from({ length: n }, () => ({ type: "image", attachment_id: 300, alt: "a" }));
        asList(sectionAt(doc, 2).children).push(...blocks);
        return doc;
      };
      expect(showDoc(withBlocks(10))).not.toBeNull();
      expect(showDoc(withBlocks(11))).toBeNull();
    });

    it("holds the buttons of a page to 12 and the tables to 10, as a version 2 page is", () => {
      const buttons = (n: number) => {
        const doc = v3Doc("v3-minimal-page");
        sectionAt(doc, 0).children = Array.from({ length: n }, () => ({ type: "buttons", buttons: [{ text: "Go", url: "/go" }] }));
        return doc;
      };
      expect(showDoc(buttons(12))).not.toBeNull();
      expect(showDoc(buttons(13))).toBeNull();
      const tables = (n: number) => {
        const doc = v3Doc("v3-minimal-page");
        sectionAt(doc, 0).children = Array.from({ length: n }, () => ({ type: "table", rows: [["a"]] }));
        return doc;
      };
      expect(showDoc(tables(10))).not.toBeNull();
      expect(showDoc(tables(11))).toBeNull();
    });
  });

  describe("reading the text", () => {
    it("refuses text that is not JSON, and a key that hides from a parse", () => {
      expect(designed("not json")).toBeNull();
      expect(designed("")).toBeNull();
      expect(designed("[]")).toBeNull();
      expect(designed("null")).toBeNull();
      const doc = JSON.stringify(v3Doc("v3-minimal-page")).replace('"post_type"', '"__proto__":{"x":1},"post_type"');
      expect(designed(doc)).toBeNull();
    });
  });
});
