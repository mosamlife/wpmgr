// Usage: node validate.mjs <markup.json>, run with cwd = a version dir whose
// dependencies are installed. Parses every case with @wordpress/blocks and runs
// validateBlock on every block, innerBlocks included. Exits non-zero on any
// invalid block, any freeform/missing block, any case that parses to zero
// blocks, or zero cases.
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { pathToFileURL } from 'node:url';

const req = createRequire(pathToFileURL(process.cwd() + '/'));
const load = (id) => import(pathToFileURL(req.resolve(id)).href);
const { JSDOM } = await load('jsdom');
const dom = new JSDOM('<!doctype html><html><body></body></html>', { url: 'http://localhost/' });
globalThis.window = dom.window;
globalThis.document = dom.window.document;
for (const k of ['navigator', 'HTMLElement', 'Node', 'Element', 'DOMParser', 'getComputedStyle', 'CustomEvent', 'Event']) {
  if (!(k in globalThis)) {
    Object.defineProperty(globalThis, k, { value: dom.window[k], configurable: true, writable: true });
  }
}

const blocks = await load('@wordpress/blocks');
const lib = await load('@wordpress/block-library');
const pkg = JSON.parse(readFileSync(req.resolve('@wordpress/blocks/package.json'), 'utf8'));
lib.registerCoreBlocks();

const file = process.argv[2];
if (!file) {
  console.error('usage: validate.mjs <markup.json>');
  process.exit(2);
}
const { cases } = JSON.parse(readFileSync(file, 'utf8'));
if (!Array.isArray(cases) || cases.length === 0) {
  console.error('FAIL: zero cases');
  process.exit(1);
}

let total = 0;
let bad = 0;
const walk = (name, list) => {
  for (const b of list) {
    total++;
    if (b.name === 'core/freeform' || b.name === 'core/missing') {
      bad++;
      console.error(`  INVALID ${name}: block ${b.name} (markup not recognised)`);
    } else {
      const [ok, logs] = blocks.validateBlock(b);
      if (!ok || b.isValid === false) {
        bad++;
        console.error(`  INVALID ${name}: ${b.name} ${JSON.stringify((logs || []).map((l) => l.message || l))}`);
      }
    }
    walk(name, b.innerBlocks || []);
  }
};
for (const c of cases) {
  const parsed = blocks.parse(c.content);
  if (parsed.length === 0) {
    bad++;
    console.error(`  INVALID ${c.name}: parsed to zero blocks`);
    continue;
  }
  walk(c.name, parsed);
}
if (total === 0) {
  console.error('FAIL: zero blocks validated');
  process.exit(1);
}
console.log(`@wordpress/blocks ${pkg.version}: ${cases.length} cases, ${total} blocks, ${bad} invalid`);
process.exit(bad === 0 ? 0 : 1);
