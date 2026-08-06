// Renders the wordpress.org directory banner for the "Fleet Agent Site Manager"
// listing.
//
// It is NOT a new design. It reuses, unchanged:
//   - the locked OG palette from apps/marketing/app/opengraph-image.tsx
//   - the Fleet Hub mark geometry from apps/marketing/app/icon.svg, which is the
//     same geometry that apps/marketing/public/icon-512.png (and therefore the
//     wp.org icon-128x128 / icon-256x256) is a raster of
//   - the two-tone "wpmgr" wordmark from apps/marketing/lib/og-logo.tsx
//   - the same renderer the site already uses for its OG card (next/og)
//
// It renders the retina banner once at 1544x500 and the standard 772x250 is a
// Lanczos downscale of that exact raster, so the two are guaranteed to be the
// same composition at exactly half scale.
//
// Usage:
//   node tools/wporg-assets/render-banner.mjs [outDir]
//
// Optional copy (leave unset for the shipped logo-lockup banner):
//   WPORG_BANNER_HEADLINE="..."  WPORG_BANNER_SUBLINE="..."
// If a headline is supplied the lockup moves to the top left and the copy sits
// under it, left aligned. Only set these once the readme title and short
// description are final, so the banner cannot contradict them.

import { createRequire } from "node:module";
import { mkdirSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(here, "..", "..");
const marketing = join(repoRoot, "apps", "marketing");

const require = createRequire(join(marketing, "package.json"));
const { ImageResponse } = require("next/dist/compiled/@vercel/og/index.node.js");
const React = require("react");

const outDir = resolve(process.argv[2] || join(repoRoot, "release", "wporg-assets"));
mkdirSync(outDir, { recursive: true });

// Locked palette, copied verbatim from apps/marketing/app/opengraph-image.tsx.
const COLORS = {
  bg: "#101F22",
  teal: "#1791A6",
  fg: "#E8F0F1",
  muted: "#748A8D",
};

const W = 1544;
const H = 500;

const headline = process.env.WPORG_BANNER_HEADLINE || "";
const subline = process.env.WPORG_BANNER_SUBLINE || "";
const h = React.createElement;

// Fleet Hub mark. Geometry copied verbatim from apps/marketing/app/icon.svg
// (the solid-satellite variant), so the banner mark matches the listing icon
// that sits directly beside it on the directory page.
function Mark(size) {
  const teal = COLORS.teal;
  return h(
    "svg",
    { width: size, height: size, viewBox: "0 0 32 32", fill: "none" },
    h("rect", { x: 11.5, y: 11.5, width: 9, height: 9, rx: 2.5, fill: teal }),
    h("circle", { cx: 6.5, cy: 6.5, r: 2.6, fill: teal }),
    h("circle", { cx: 25.5, cy: 6.5, r: 2.6, fill: teal }),
    h("circle", { cx: 6.5, cy: 25.5, r: 2.6, fill: teal }),
    h("circle", { cx: 25.5, cy: 25.5, r: 2.6, fill: teal }),
    h("line", { x1: 9, y1: 9, x2: 11.6, y2: 11.6, stroke: teal, strokeWidth: 2, strokeLinecap: "round" }),
    h("line", { x1: 23, y1: 9, x2: 20.4, y2: 11.6, stroke: teal, strokeWidth: 2, strokeLinecap: "round" }),
    h("line", { x1: 9, y1: 23, x2: 11.6, y2: 20.4, stroke: teal, strokeWidth: 2, strokeLinecap: "round" }),
    h("line", { x1: 23, y1: 23, x2: 20.4, y2: 20.4, stroke: teal, strokeWidth: 2, strokeLinecap: "round" }),
  );
}

// Two-tone wordmark, same construction as apps/marketing/lib/og-logo.tsx.
function Lockup(markSize, fontSize) {
  return h(
    "div",
    { style: { display: "flex", alignItems: "center", gap: `${Math.round(markSize * 0.3)}px` } },
    Mark(markSize),
    h(
      "div",
      { style: { display: "flex", fontSize: `${fontSize}px`, fontWeight: 700, letterSpacing: "-0.02em" } },
      h("span", { style: { color: COLORS.fg } }, "wp"),
      h("span", { style: { color: COLORS.teal } }, "mgr"),
    ),
  );
}

function LogoOnlyBanner() {
  return h(
    "div",
    {
      style: {
        width: "100%",
        height: "100%",
        display: "flex",
        flexDirection: "column",
        alignItems: "center",
        justifyContent: "center",
        background: COLORS.bg,
        gap: "34px",
      },
    },
    Lockup(170, 140),
    h("div", { style: { fontSize: "34px", color: COLORS.muted, letterSpacing: "0.01em" } }, "wpmgr.app"),
  );
}

function CopyBanner() {
  const children = [
    h("div", { key: "lockup", style: { display: "flex" } }, Lockup(56, 44)),
    h(
      "div",
      {
        key: "headline",
        style: {
          fontSize: "62px",
          fontWeight: 600,
          color: COLORS.fg,
          lineHeight: 1.1,
          letterSpacing: "-0.02em",
          maxWidth: "1120px",
        },
      },
      headline,
    ),
  ];
  if (subline) {
    children.push(
      h(
        "div",
        {
          key: "subline",
          style: { fontSize: "30px", color: COLORS.muted, lineHeight: 1.4, maxWidth: "1120px" },
        },
        subline,
      ),
    );
  }
  return h(
    "div",
    {
      style: {
        width: "100%",
        height: "100%",
        display: "flex",
        flexDirection: "column",
        alignItems: "flex-start",
        justifyContent: "center",
        background: COLORS.bg,
        padding: "88px",
        gap: "26px",
      },
    },
    ...children,
  );
}

const element = headline ? CopyBanner() : LogoOnlyBanner();
const res = new ImageResponse(element, { width: W, height: H });
const buf = Buffer.from(await res.arrayBuffer());
const retinaPath = join(outDir, "banner-1544x500.png");
writeFileSync(retinaPath, buf);
console.log(`wrote ${retinaPath} (${buf.length} bytes)`);
console.log("now downscale to banner-772x250.png with tools/wporg-assets/downscale-banner.py");
