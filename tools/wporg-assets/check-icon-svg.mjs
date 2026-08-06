// Rasterises release/wporg-assets/icon.svg at 512x512 through the same resvg
// that renders the banner, and writes it to /tmp so it can be diffed against
// apps/marketing/public/icon-512.png. The point is to prove the shipped SVG is
// the same composition as the shipped PNG icons, not a lookalike.
//
// Usage: node tools/wporg-assets/check-icon-svg.mjs

import { createRequire } from "node:module";
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(here, "..", "..");
const require = createRequire(join(repoRoot, "apps", "marketing", "package.json"));
const { ImageResponse } = require("next/dist/compiled/@vercel/og/index.node.js");
const React = require("react");

const svg = readFileSync(join(repoRoot, "release", "wporg-assets", "icon.svg"), "utf8");
const dataUri = `data:image/svg+xml;base64,${Buffer.from(svg).toString("base64")}`;

const res = new ImageResponse(
  React.createElement(
    "div",
    { style: { display: "flex", width: "100%", height: "100%" } },
    React.createElement("img", { src: dataUri, width: 512, height: 512 }),
  ),
  { width: 512, height: 512 },
);
const out = "/tmp/wporg-icon-svg-render.png";
writeFileSync(out, Buffer.from(await res.arrayBuffer()));
console.log(`wrote ${out}`);
