import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

import {
  OUTLINE_LIMITS,
  buttonLabel,
  buttonsLabel,
  classifyLink,
  columnsLabel,
  imageAlignLabel,
  imageSizeLabel,
  indexPageMedia,
  isSameSiteHost,
  layoutSummary,
  parseLink,
  parsePagePreview,
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
