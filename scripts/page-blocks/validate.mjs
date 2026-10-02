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
const { VirtualConsole } = await load('jsdom');
const dom = new JSDOM('<!doctype html><html><body></body></html>', { url: 'http://localhost/', virtualConsole: new VirtualConsole() });
dom.window.matchMedia = () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {} });
globalThis.window = dom.window;
globalThis.document = dom.window.document;
for (const k of Object.getOwnPropertyNames(dom.window)) {
  if (!(k in globalThis)) {
    Object.defineProperty(globalThis, k, { value: dom.window[k], configurable: true, writable: true });
  }
}

const blocks = await load('@wordpress/blocks');
const lib = await load('@wordpress/block-library');
const pkg = JSON.parse(readFileSync(req.resolve('@wordpress/blocks/package.json'), 'utf8'));
lib.registerCoreBlocks();
// The libraries log whole block objects on a failed validation; our own report
// below carries the message.
const say = (m) => process.stderr.write(m + '\n');
for (const k of ['error', 'warn', 'info', 'log']) console[k] = () => {};

const file = process.argv[2];
if (!file) {
  say('usage: validate.mjs <markup.json>');
  process.exit(2);
}
const { cases } = JSON.parse(readFileSync(file, 'utf8'));
if (!Array.isArray(cases) || cases.length === 0) {
  say('FAIL: zero cases');
  process.exit(1);
}

let total = 0;
let bad = 0;
const walk = (name, list) => {
  for (const b of list) {
    total++;
    if (b.name === 'core/freeform' || b.name === 'core/missing') {
      bad++;
      say(`  INVALID ${name}: block ${b.name} (markup not recognised)`);
    } else {
      const [ok, logs] = blocks.validateBlock(b);
      if (!ok || b.isValid === false) {
        bad++;
        say(`  INVALID ${name}: ${b.name} ${JSON.stringify((logs || []).map((l) => l.message || l)).slice(0, 200)}`);
      }
    }
    walk(name, b.innerBlocks || []);
  }
};
for (const c of cases) {
  const parsed = blocks.parse(c.content);
  if (parsed.length === 0) {
    bad++;
    say(`  INVALID ${c.name}: parsed to zero blocks`);
    continue;
  }
  walk(c.name, parsed);
}
if (total === 0) {
  say('FAIL: zero blocks validated');
  process.exit(1);
}
process.stdout.write(`@wordpress/blocks ${pkg.version}: ${cases.length} cases, ${total} blocks, ${bad} invalid` + "\n");
process.exit(bad === 0 ? 0 : 1);
