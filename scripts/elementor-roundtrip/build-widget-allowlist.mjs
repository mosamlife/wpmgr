import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const defaultOutput = path.resolve(here, '../../apps/api/internal/elementornative/core_widgets.json');
const maxOutputBytes = 1024 * 1024;
const start = 'rt: WIDGETS_JSON_START';
const end = 'rt: WIDGETS_JSON_END';
const object = value => value !== null && typeof value === 'object' && !Array.isArray(value);
const token = value => typeof value === 'string' && /^[a-zA-Z0-9_-]+$/.test(value);
const demand = (ok, message) => { if (!ok) throw new Error(message); };
const excluded = name => ['html', 'shortcode', 'template', 'text-path', 'menu-anchor', 'sidebar'].includes(name)
  || /^(wp-widget-|e-|nested-)/.test(name);

// Canonical keys keep equal policy definitions together without changing array order.
function canonical(value) {
  if (Array.isArray(value)) return value.map(canonical);
  if (object(value)) return Object.fromEntries(Object.keys(value).sort().map(key => [key, canonical(value[key])]));
  return value;
}
function controls(list, at) {
  demand(Array.isArray(list), `${at}: controls must be a list`);
  const seen = new Set();
  return list.map(control => {
    demand(object(control) && token(control.id) && token(control.type), `${at}: invalid control id or type`);
    demand(!seen.has(control.id), `${at}: duplicate control ${control.id}`);
    seen.add(control.id);
    demand(Array.isArray(control.options) && control.options.every(key => typeof key === 'string')
      && new Set(control.options).size === control.options.length, `${at}.${control.id}: invalid options`);
    const responsive = control.responsive;
    demand(responsive === null || (object(responsive)
      && (responsive.min === null || token(responsive.min))
      && (responsive.max === null || token(responsive.max))
      && Array.isArray(responsive.devices) && responsive.devices.every(token)
      && new Set(responsive.devices).size === responsive.devices.length), `${at}.${control.id}: invalid responsive window`);
    const group = control.group;
    demand(group === null || (object(group) && token(group.family) && token(group.prefix)
      && (group.activator === null || token(group.activator))), `${at}.${control.id}: invalid group`);
    demand(typeof control.raw_css_value === 'boolean', `${at}.${control.id}: invalid raw_css_value`);
    const fields = controls(control.fields, `${at}.${control.id}.fields`);
    demand(control.type === 'repeater' || fields.length === 0, `${at}.${control.id}: only repeaters have fields`);
    const switchType = ['switcher', 'popover_toggle'].includes(control.type);
    demand(!switchType || (typeof control.return_value === 'string' && control.return_value !== ''), `${at}.${control.id}: invalid return_value`);
    return { ...(switchType ? { return_value: control.return_value } : {}), id: control.id, type: control.type, raw_css_value: control.raw_css_value, options: [...control.options].sort(),
      responsive: responsive === null ? null : {
        min: responsive.min, max: responsive.max, devices: [...responsive.devices].sort(),
      }, group: group === null ? null : { family: group.family, activator: group.activator }, fields };
  }).sort((a, b) => a.id.localeCompare(b.id, 'en'));
}
function skippedControls(list, at) {
  demand(Array.isArray(list), `${at}: controls_skipped must be a list`);
  const seen = new Set();
  return list.map(entry => {
    demand(object(entry) && token(entry.id) && token(entry.reason), `${at}: invalid skipped control`);
    demand(!seen.has(entry.id), `${at}: duplicate skipped control ${entry.id}`);
    seen.add(entry.id);
    return { id: entry.id, reason: entry.reason };
  }).sort((a, b) => a.id.localeCompare(b.id, 'en'));
}
function parseDump(file) {
  const raw = fs.readFileSync(file, 'utf8');
  const lines = raw.split(/\r?\n/);
  const starts = lines.flatMap((line, i) => line === start ? [i] : []);
  const ends = lines.flatMap((line, i) => line === end ? [i] : []);
  let json = raw;
  if (starts.length || ends.length) {
    demand(starts.length === 1 && ends.length === 1 && starts[0] < ends[0], `${file}: expected one complete widget JSON block`);
    json = lines.slice(starts[0] + 1, ends[0]).join('\n');
  }
  const data = JSON.parse(json);
  demand(object(data) && data.schema_version === 2 && typeof data.elementor_version === 'string', `${file}: invalid dump header (requires eligible-widget dump schema 2)`);
  demand(Array.isArray(data.widgets) && data.widgets.length > 0, `${file}: dump has no widgets`);
  const seen = new Set();
  data.widgets = data.widgets.map(widget => {
    demand(object(widget) && token(widget.name) && typeof widget.title === 'string' && widget.title !== '', `${file}: invalid widget`);
    demand(!seen.has(widget.name), `${file}: duplicate widget ${widget.name}`);
    seen.add(widget.name);
    const list = controls(widget.controls, `${file}.${widget.name}`);
    const skipped = skippedControls(Object.hasOwn(widget, 'controls_skipped') ? widget.controls_skipped : [], `${file}.${widget.name}`);
    const offered = new Set(list.map(control => control.id));
    demand(skipped.every(entry => !offered.has(entry.id)), `${file}.${widget.name}: a control cannot be both offered and skipped`);
    demand(list.length > 0 || skipped.length > 0, `${file}.${widget.name}: no controls`);
    return { name: widget.name, title: widget.title, controls: list, controls_skipped: skipped };
  }).filter(widget => !excluded(widget.name));
  demand(data.widgets.length > 0, `${file}: dump has no allowed widgets`);
  return data;
}
function versionOrder(a, b) {
  const left = a.split('.').map(Number), right = b.split('.').map(Number);
  for (let i = 0; i < Math.max(left.length, right.length); i++) {
    const delta = (left[i] || 0) - (right[i] || 0);
    if (delta) return delta;
  }
  return 0;
}
function sharedBindings(references) {
  const groups = new Map();
  for (const [id, variants] of references) {
    for (const reference of variants.values()) {
      const key = JSON.stringify(reference.versions);
      if (!groups.has(key)) groups.set(key, { versions: reference.versions, controls: new Map() });
      groups.get(key).controls.set(id, reference.definition);
    }
  }
  return [...groups].sort(([a], [b]) => a < b ? -1 : a > b ? 1 : 0).map(([, group]) => ({
    versions: group.versions,
    controls: Object.fromEntries([...group.controls].sort(([a], [b]) => a.localeCompare(b, 'en'))),
  }));
}
function build(dumps) {
  const pool = new Map();
  for (const dump of dumps) {
    for (const widget of dump.widgets) {
      for (const { id, ...definition } of widget.controls) {
        if (!pool.has(id)) pool.set(id, new Map());
        const variants = pool.get(id);
        const key = JSON.stringify(canonical(definition));
        if (!variants.has(key)) variants.set(key, { definition, widgets: new Set(), versions: new Set() });
        variants.get(key).widgets.add(widget.name);
        variants.get(key).versions.add(dump.elementor_version);
      }
    }
  }
  const shared = [];
  const sharedIndexes = new Map();
  for (const [id, variants] of [...pool].sort(([a], [b]) => a.localeCompare(b, 'en'))) {
    const repeated = [...variants].filter(([, variant]) => variant.widgets.size > 1)
      .sort(([a], [b]) => a < b ? -1 : a > b ? 1 : 0);
    if (!repeated.length) continue;
    sharedIndexes.set(id, new Map(repeated.map(([key], index) => [key, index])));
    shared.push([id, { definitions: repeated.map(([, variant]) => ({
      ...variant.definition, versions: [...variant.versions].sort(versionOrder),
    })) }]);
  }
  const widgets = new Map();
  for (const dump of dumps) {
    const version = dump.elementor_version;
    for (const widget of dump.widgets) {
      if (!widgets.has(widget.name)) widgets.set(widget.name, { name: widget.name, versions: [], controls: new Map(),
        shared_controls: new Map(), controls_skipped: new Map() });
      const entry = widgets.get(widget.name);
      entry.versions.push(version);
      entry.controls_skipped.set(version, widget.controls_skipped);
      for (const { id, ...definition } of widget.controls) {
        const key = JSON.stringify(canonical(definition));
        const sharedIndex = sharedIndexes.get(id)?.get(key);
        // References select an exact definition and its widget-specific versions.
        // Versions on the global definition describe availability, not permission.
        if (sharedIndex !== undefined) {
          if (!entry.shared_controls.has(id)) entry.shared_controls.set(id, new Map());
          const references = entry.shared_controls.get(id);
          if (!references.has(sharedIndex)) references.set(sharedIndex, { definition: sharedIndex, versions: [] });
          references.get(sharedIndex).versions.push(version);
          continue;
        }
        if (!entry.controls.has(id)) entry.controls.set(id, { id, versions: [], definitions: new Map() });
        const control = entry.controls.get(id);
        control.versions.push(version);
        // A definition's versions apply to its option keys, window, group and repeater fields.
        if (!control.definitions.has(key)) control.definitions.set(key, { ...definition, versions: [] });
        control.definitions.get(key).versions.push(version);
      }
    }
  }
  return { schema_version: 2, elementor_versions: dumps.map(d => d.elementor_version), shared_controls: Object.fromEntries(shared),
    widgets: [...widgets.values()].sort((a, b) => a.name.localeCompare(b.name, 'en')).map(widget => ({
      name: widget.name, versions: widget.versions,
      shared_controls: sharedBindings(widget.shared_controls),
      controls_skipped: Object.fromEntries(widget.controls_skipped),
      controls: [...widget.controls.values()].sort((a, b) => a.id.localeCompare(b.id, 'en')).map(control => ({
        id: control.id, versions: control.versions, definitions: [...control.definitions.entries()]
          .sort(([a], [b]) => a.localeCompare(b, 'en')).map(([, definition]) => definition),
      })),
    })) };
}

let temporary;
let ownsTemporary = false;
try {
  const args = process.argv.slice(2);
  if (args[0] === '--check-dump') {
    demand(args.length === 3, 'usage: --check-dump <log> <version>');
    const data = parseDump(args[1]);
    demand(data.elementor_version === args[2], 'widget dump version does not match the boot');
    console.log(`widget-allowlist: validated widget dump for ${data.elementor_version}`);
  } else {
    demand(args.length === 3 || args.length === 4, 'usage: build-widget-allowlist.sh <dump1> <dump2> <dump3> [output.json]');
    const output = path.resolve(args[3] || defaultOutput);
    demand(!args.slice(0, 3).some(file => path.resolve(file) === output), 'output must differ from the inputs');
    const pinned = fs.readFileSync(path.join(here, 'pins.txt'), 'utf8').split(/\r?\n/)
      .filter(line => /^elementor\s/.test(line)).map(line => line.trim().split(/\s+/)[1]).sort(versionOrder);
    demand(pinned.length === 3 && new Set(pinned).size === pinned.length, 'expected distinct Elementor pins');
    const dumps = args.slice(0, 3).map(parseDump).sort((a, b) => versionOrder(a.elementor_version, b.elementor_version));
    demand(JSON.stringify(dumps.map(d => d.elementor_version)) === JSON.stringify(pinned), 'dumps must cover each pinned Elementor version exactly once');
    const data = build(dumps);
    const json = JSON.stringify(data) + '\n';
    const size = Buffer.byteLength(json, 'utf8');
    demand(size <= maxOutputBytes, `size=${size} bytes exceeds ${maxOutputBytes} byte output limit`);
    fs.mkdirSync(path.dirname(output), { recursive: true });
    temporary = `${output}.tmp-${process.pid}`;
    const fd = fs.openSync(temporary, 'wx');
    ownsTemporary = true;
    try { fs.writeFileSync(fd, json); } finally { fs.closeSync(fd); }
    fs.renameSync(temporary, output);
    temporary = undefined;
    console.log(`widget-allowlist: wrote ${data.widgets.length} widgets to ${output}`);
    console.log(`widget-allowlist: size=${size} bytes`);
    console.log(`widget-allowlist: names=${data.widgets.map(widget => widget.name).join(', ')}`);
  }
} catch (error) {
  if (temporary && ownsTemporary && fs.existsSync(temporary)) fs.unlinkSync(temporary);
  console.error(`widget-allowlist: ${error.message}`);
  process.exitCode = 1;
}
