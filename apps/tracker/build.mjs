/**
 * esbuild IIFE bundler for the @wpmgr/tracker RUM collector.
 *
 * Produces a minified IIFE (dist/wpmgr-rum.min.js) and a readable,
 * non-minified IIFE built from the identical entry/bundle/format/target
 * (dist/wpmgr-rum.js), so the plugin zip ships a human-readable source
 * alongside the minified asset it actually loads (mirrors wpmgr-delay.js /
 * wpmgr-delay.min.js).
 *
 * Both are copied into the agent assets directory.
 *
 * Run with: node build.mjs
 */

import { build } from 'esbuild';
import { copyFileSync, mkdirSync } from 'fs';
import { join, dirname } from 'path';
import { fileURLToPath } from 'url';

const __dirname = dirname(fileURLToPath(import.meta.url));

const DIST = join(__dirname, 'dist');
const OUT_FILE = join(DIST, 'wpmgr-rum.min.js');
const OUT_FILE_READABLE = join(DIST, 'wpmgr-rum.js');
const AGENT_ASSETS = join(__dirname, '../../apps/agent/assets');

const SHARED_OPTIONS = {
  entryPoints: [join(__dirname, 'src/index.ts')],
  bundle: true,
  format: 'iife',
  platform: 'browser',
  target: ['es2017', 'chrome70', 'firefox68', 'safari12'],
  // web-vitals ships as a bundled dependency; include it entirely.
  // No external dependencies: the whole bundle must be self-contained.
  external: [],
  define: {
    // Ensure no Node.js globals leak into the bundle.
    'process.env.NODE_ENV': '"production"',
  },
  legalComments: 'none',
};

mkdirSync(DIST, { recursive: true });

await build({
  ...SHARED_OPTIONS,
  minify: true,
  outfile: OUT_FILE,
});

// Readable, non-minified build of the exact same entry/bundle/format/target
// as above, for the wp.org readable-source requirement.
await build({
  ...SHARED_OPTIONS,
  minify: false,
  outfile: OUT_FILE_READABLE,
});

// Copy the artifacts into the agent's assets directory.
mkdirSync(AGENT_ASSETS, { recursive: true });
copyFileSync(OUT_FILE, join(AGENT_ASSETS, 'wpmgr-rum.min.js'));
copyFileSync(OUT_FILE_READABLE, join(AGENT_ASSETS, 'wpmgr-rum.js'));

console.log('Built: dist/wpmgr-rum.min.js');
console.log('Copied -> apps/agent/assets/wpmgr-rum.min.js');
console.log('Built: dist/wpmgr-rum.js');
console.log('Copied -> apps/agent/assets/wpmgr-rum.js');
