// Sync the source OpenAPI spec and the vendored Scalar bundle into public/ so
// the API reference at wpmgr.app/docs always renders the CURRENT spec with
// ZERO external network at runtime (right for an AGPL self-hostable project).
// Runs before `next build` via the build script in package.json.
import { readFileSync, writeFileSync, mkdirSync, copyFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { parse } from "yaml";

// Must match the pinned devDependency in package.json.
const SCALAR_VERSION = "1.57.5";

const here = dirname(fileURLToPath(import.meta.url));
const root = join(here, ".."); // apps/marketing
const repoRoot = join(root, "..", ".."); // monorepo root
const specYaml = join(repoRoot, "packages/openapi/openapi.yaml");
const publicDir = join(root, "public");
const assetsDir = join(publicDir, "docs-assets");

// The Scalar standalone bundle lives at this path inside node_modules.
const scalarSrc = join(
  root,
  "node_modules/@scalar/api-reference/dist/browser/standalone.js",
);
const scalarOut = join(assetsDir, "scalar.js");

mkdirSync(assetsDir, { recursive: true });

// 1. Parse the source YAML and emit minified JSON.
//    Minified is smaller and faster for the browser to parse.
const spec = parse(readFileSync(specYaml, "utf8"));

// Self-healing version stamp: the rendered doc always shows the release
// version from the top CHANGELOG.md entry, so info.version can never drift
// on wpmgr.app/docs even if the yaml bump is forgotten. Falls back to the
// yaml's own value when the CHANGELOG cannot be parsed.
try {
  const changelog = readFileSync(join(repoRoot, "CHANGELOG.md"), "utf8");
  const m = changelog.match(/^## \[(\d+\.\d+\.\d+)\]/m);
  if (m && spec.info) {
    if (spec.info.version !== m[1]) {
      console.warn(
        `sync-openapi: overriding spec info.version ${spec.info.version} -> ${m[1]} (top CHANGELOG.md entry)`,
      );
    }
    spec.info.version = m[1];
  }
} catch {
  // CHANGELOG.md missing/unreadable: keep the yaml's version.
}

writeFileSync(join(publicDir, "openapi.json"), JSON.stringify(spec));

// 2. Vendor the pinned Scalar standalone bundle.
//    This eliminates any runtime CDN dependency.
copyFileSync(scalarSrc, scalarOut);

const pathCount = spec.paths ? Object.keys(spec.paths).length : 0;
console.log(
  `sync-openapi: openapi.json (v${spec.info?.version ?? "?"}, ${pathCount} paths) + scalar@${SCALAR_VERSION} -> public/docs-assets/`,
);
