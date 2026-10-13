#!/usr/bin/env node
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'widget-allowlist-test-'));
const versions = ['3.20.4', '3.35.9', '4.3.4'];
const control = (id, type = 'text', extra = {}) => ({
  id, type, ...(['switcher', 'popover_toggle'].includes(type) ? { return_value: 'yes' } : {}), options: [], default: '', responsive: null, group: null, fields: [], ...extra,
});
const widget = (name, controls = [control('title')]) => ({ name, title: name, controls });
const dump = (version, widgets) => ({ schema_version: 2, elementor_version: version, widgets });
const files = versions.map((version, i) => path.join(tmp, `${i}.log`));
const output = path.join(tmp, 'allowlist.json');
const run = (inputs = files) => spawnSync(path.join(here, 'build-widget-allowlist.sh'),
  [...inputs, output], { encoding: 'utf8' });
let passed = 0;
let failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log(`ok   ${name}`); }
  catch (error) { failed++; console.error(`FAIL ${name}: ${error.message}`); }
}
function rejected(name, edit, inputs = files) {
  test(name, () => {
    const before = fs.readFileSync(output, 'utf8');
    const restore = files.map(file => fs.readFileSync(file, 'utf8'));
    try {
      edit();
      const result = run(inputs);
      assert.ok(result.status !== 0, 'malformed input must fail');
      assert.match(result.stderr, /widget-allowlist:/);
      assert.equal(fs.readFileSync(output, 'utf8'), before, 'failed builds must preserve the output');
    } finally { files.forEach((file, i) => fs.writeFileSync(file, restore[i])); }
  });
}

try {
  const excluded = ['html', 'shortcode', 'template', 'text-path', 'menu-anchor', 'sidebar',
    'wp-widget-search', 'e-heading', 'nested-tabs'];
  files.forEach((file, i) => {
    const heading = [control('enabled', 'switcher', { return_value: i === 0 ? 'yes' : 'enabled' }), control('title_typography', 'popover_toggle', { return_value: 'custom' }), control('title'), control('align', 'choose', {
      options: i === 0 ? ['left'] : ['left', 'right'], default: i === 0 ? 'left' : 'right',
    })];
    if (i === 1) heading.push(control('middle_only'));
    if (i !== 0) heading.push(control('_box_shadow_box_shadow_type', 'popover_toggle', {
      group: { family: 'box-shadow', prefix: '_box_shadow_', activator: '_box_shadow_box_shadow_type' },
    }));
    heading.push(control('font_size', 'slider', {
      default: { unit: 'px', size: 12 },
      responsive: { min: null, max: 'tablet', devices: ['mobile', 'tablet'] },
      group: { family: 'typography', prefix: 'title_', activator: 'title_typography' },
    }));
    const doc = dump(versions[i], [widget('heading', heading), widget('list', [control('items', 'repeater', {
      default: [], fields: [control('caption'), control('url', 'url', { default: { url: '' } })],
    })]), ...excluded.map(name => widget(name))]);
    doc.widgets[0].controls_skipped = i === 0
      ? [{ id: '_box_shadow_box_shadow_type', reason: 'incomplete_group_metadata' }] : [];
    const json = JSON.stringify(doc);
    fs.writeFileSync(file, i === 0 ? json : `boot log\nrt: WIDGETS_JSON_START\n${json}\nrt: WIDGETS_JSON_END\nrt: RESULT OK\n`);
  });
  test('builds a versioned union from plain JSON and marked harness logs', () => {
    const result = run();
    assert.equal(result.status, 0, result.error?.message || result.stderr);
    const data = JSON.parse(fs.readFileSync(output, 'utf8'));
    assert.deepEqual(data.elementor_versions, versions);
    assert.deepEqual(data.widgets.map(w => w.name), ['heading', 'list']);
    const heading = data.widgets[0];
    assert.deepEqual(heading.versions, versions);
    assert.ok(!Object.hasOwn(heading, 'titles'));
    const only = heading.controls.find(c => c.id === 'middle_only');
    assert.deepEqual(only.versions, [versions[1]]);
    assert.deepEqual(only.definitions[0].versions, [versions[1]]);
    const align = heading.controls.find(c => c.id === 'align');
    assert.equal(align.definitions.length, 2);
    assert.deepEqual(align.definitions.find(d => d.options.includes('right')).versions, versions.slice(1));
    assert.deepEqual(heading.controls.find(c => c.id === 'font_size').definitions[0].responsive.devices,
      ['mobile', 'tablet']);
    assert.equal(heading.controls.find(c => c.id === 'font_size').definitions[0].group.activator, 'title_typography');
    assert.equal(data.widgets[1].controls[0].definitions[0].fields[1].type, 'url');
  });
  test('switch return values survive version definitions', () => {
    const heading = JSON.parse(fs.readFileSync(output, 'utf8')).widgets[0];
    assert.equal(heading.controls.find(c => c.id === 'title_typography').definitions[0].return_value, 'custom');
    assert.deepEqual(heading.controls.find(c => c.id === 'enabled').definitions.map(d => d.return_value).sort(), ['enabled', 'yes']);
  });
  rejected('non-string switch return value is refused', () => fs.writeFileSync(files[0], JSON.stringify(dump(versions[0], [widget('heading', [control('enabled', 'switcher', { return_value: true })])]))));
  test('skipped controls retain their source version and never gain that version in the allowlist', () => {
    const heading = JSON.parse(fs.readFileSync(output, 'utf8')).widgets[0];
    assert.deepEqual(heading.controls_skipped, {
      [versions[0]]: [{ id: '_box_shadow_box_shadow_type', reason: 'incomplete_group_metadata' }],
      [versions[1]]: [], [versions[2]]: [],
    });
    const shadow = heading.controls.find(c => c.id === '_box_shadow_box_shadow_type');
    assert.deepEqual(shadow.versions, versions.slice(1));
    assert.deepEqual(shadow.definitions[0].versions, versions.slice(1));
  });
  test('deterministic output regardless of input order', () => {
    const first = fs.readFileSync(output, 'utf8');
    assert.equal(run([...files].reverse()).status, 0);
    assert.equal(fs.readFileSync(output, 'utf8'), first);
  });
  test('a version with every control skipped contributes no allowed control versions', () => {
    const original = fs.readFileSync(files[0], 'utf8');
    try {
      fs.writeFileSync(files[0], JSON.stringify(dump(versions[0], [{ ...widget('heading', []), controls_skipped: [
        { id: 'title', reason: 'incomplete_group_metadata' },
        { id: 'skipped_everywhere', reason: 'incomplete_group_metadata' },
      ] }])));
      const result = run();
      assert.equal(result.status, 0, result.stderr);
      const heading = JSON.parse(fs.readFileSync(output, 'utf8')).widgets[0];
      assert.deepEqual(heading.controls.find(c => c.id === 'title').versions, versions.slice(1));
      assert.ok(!heading.controls.some(c => c.id === 'skipped_everywhere'));
      assert.equal(heading.controls_skipped[versions[0]].length, 2);
    } finally {
      fs.writeFileSync(files[0], original);
      assert.equal(run().status, 0);
    }
  });
  rejected('offered and skipped controls cannot overlap in one version', () => fs.writeFileSync(files[0], JSON.stringify(dump(versions[0], [
    { ...widget('heading'), controls_skipped: [{ id: 'title', reason: 'incomplete_group_metadata' }] },
  ]))));
  rejected('a malformed skipped control record is refused', () => fs.writeFileSync(files[0], JSON.stringify(dump(versions[0], [
    { ...widget('heading'), controls_skipped: [{ id: 'missing_reason' }] },
  ]))));
  rejected('null skipped control lists are refused', () => fs.writeFileSync(files[0], JSON.stringify(dump(versions[0], [
    { ...widget('heading'), controls_skipped: null },
  ]))));
  rejected('duplicate skipped control ids are refused', () => fs.writeFileSync(files[0], JSON.stringify(dump(versions[0], [
    { ...widget('heading'), controls_skipped: [
      { id: 'missing', reason: 'incomplete_group_metadata' },
      { id: 'missing', reason: 'incomplete_group_metadata' },
    ] },
  ]))));
  rejected('a widget with neither offered nor skipped controls is refused', () => fs.writeFileSync(files[0], JSON.stringify(dump(versions[0], [
    { ...widget('heading', []), controls_skipped: [] },
  ]))));
  rejected('invalid JSON is refused', () => fs.writeFileSync(files[0], '{'));
  rejected('earlier unfiltered dump schemas are refused', () => fs.writeFileSync(files[0], JSON.stringify({
    ...dump(versions[0], [widget('heading')]), schema_version: 1,
  })));
  rejected('missing input is refused', () => {}, [path.join(tmp, 'missing'), ...files.slice(1)]);
  rejected('empty widgets are refused', () => fs.writeFileSync(files[0], JSON.stringify(dump(versions[0], []))));
  rejected('a dump with no allowed widgets is refused', () => fs.writeFileSync(files[0], JSON.stringify(dump(versions[0], [widget('html')]))));
  rejected('malformed controls are refused', () => fs.writeFileSync(files[0], JSON.stringify(dump(versions[0], [widget('heading', [{ id: 'title' }])]))));
  rejected('duplicate widget names are refused', () => fs.writeFileSync(files[0], JSON.stringify(dump(versions[0], [widget('heading'), widget('heading')]))));
  rejected('duplicate control ids are refused', () => fs.writeFileSync(files[0], JSON.stringify(dump(versions[0], [widget('heading', [control('title'), control('title')])]))));
  rejected('malformed repeater fields are refused', () => fs.writeFileSync(files[0], JSON.stringify(dump(versions[0], [widget('list', [control('items', 'repeater', { fields: [{}] })])]))));
  rejected('duplicate versions are refused', () => fs.copyFileSync(files[0], files[1]));
  rejected('an unpinned version is refused', () => fs.writeFileSync(files[0], JSON.stringify(dump('0.0.1', [widget('heading')]))));
  rejected('unterminated marker block is refused', () => fs.appendFileSync(files[0], '\nrt: WIDGETS_JSON_START\n{}'));
  rejected('multiple marker blocks are refused', () => fs.appendFileSync(files[1], '\nrt: WIDGETS_JSON_START\n{}\nrt: WIDGETS_JSON_END\n'));
  test('shared table round trip preserves each widget version and definition exactly', () => {
    const originals = files.map(file => fs.readFileSync(file, 'utf8'));
    const expected = [];
    try {
      files.forEach((file, i) => {
        const common = control('_margin', 'dimensions', { default: `ignored-${i}` });
        const left = control('choice', 'choose', { options: ['left'] });
        const right = control('choice', 'choose', { options: ['right'] });
        const ownVariant = control('choice', 'choose', { options: ['middle'] });
        const repeatedFields = control('items', 'repeater', { default: [{ label: 'Unused' }], fields: [
          control('label'), control('details', 'repeater', { fields: [control('url', 'url', { default: { url: '' } })] }),
        ] });
        const source = [widget('alpha', [common, i === 1 ? ownVariant : left, control('only_alpha')]),
          widget('beta', [...(i === 1 ? [] : [common]), right]),
          widget('gamma', [left, repeatedFields]), widget('delta', [right, repeatedFields])];
        source[1].controls_skipped = i === 1 ? [{ id: '_margin', reason: 'incomplete_group_metadata' }] : [];
        expected.push(source);
        fs.writeFileSync(file, JSON.stringify(dump(versions[i], source)));
      });
      const result = run();
      assert.equal(result.status, 0, result.stderr);
      const data = JSON.parse(fs.readFileSync(output, 'utf8'));
      assert.ok(data.shared_controls, 'missing shared control table');
      assert.equal(data.shared_controls._margin.definitions.length, 1, 'defaults must not split policy definitions');
      assert.equal(data.shared_controls.choice.definitions.length, 2, 'different options must retain separate definitions');
      const project = ({ id, type, options, responsive, group, fields }) => ({ id, type,
        options: [...options].sort(), responsive, group: group === null ? null : { family: group.family, activator: group.activator },
        fields: fields.map(project).sort((a, b) => a.id.localeCompare(b.id, 'en')) });
      for (let i = 0; i < versions.length; i++) {
        const version = versions[i];
        for (const source of expected[i]) {
          const entry = data.widgets.find(w => w.name === source.name);
          const expanded = entry.controls.flatMap(c => c.definitions.filter(d => d.versions.includes(version))
            .map(d => project({ id: c.id, ...d })));
          const referencedIds = new Set();
          for (const binding of entry.shared_controls.filter(binding => binding.versions.includes(version))) {
            for (const [id, index] of Object.entries(binding.controls)) {
              assert.ok(!referencedIds.has(id), 'a shared id must resolve to at most one definition on a version');
              referencedIds.add(id);
              const definition = data.shared_controls[id].definitions[index];
              assert.ok(definition.versions.includes(version));
              expanded.push(project({ id, ...definition }));
            }
          }
          expanded.sort((a, b) => a.id.localeCompare(b.id, 'en'));
          assert.deepEqual(expanded, source.controls.map(project).sort((a, b) => a.id.localeCompare(b.id, 'en')),
            `${source.name} on ${version}`);
        }
      }
      assert.ok(!Object.hasOwn(data.widgets[0], 'titles'));
      const noDefaults = value => {
        if (value && typeof value === 'object') {
          assert.ok(!Object.hasOwn(value, 'default'), 'policy output must not carry defaults');
          for (const child of Object.values(value)) noDefaults(child);
        }
      };
      noDefaults(data);
      assert.match(result.stdout, /size=\d+ bytes/);
      assert.match(result.stdout, /names=alpha, beta, delta, gamma/);
    } finally {
      files.forEach((file, i) => fs.writeFileSync(file, originals[i]));
      assert.equal(run().status, 0);
    }
  });
  test('the byte size cap writes nothing and preserves existing output', () => {
    const original = fs.readFileSync(files[0], 'utf8');
    const before = fs.readFileSync(output, 'utf8');
    const absent = path.join(tmp, 'not-created', 'too-big.json');
    try {
      fs.writeFileSync(files[0], JSON.stringify(dump(versions[0], [widget('heading', [
        control('choice', 'select', { options: ['é'.repeat(600_000)] }),
      ])])));
      for (const target of [output, absent]) {
        const result = spawnSync(path.join(here, 'build-widget-allowlist.sh'), [...files, target], { encoding: 'utf8' });
        assert.ok(result.status !== 0, 'oversized output must fail');
        assert.match(result.stderr, /size=\d+ bytes.*exceeds.*1048576/);
        assert.equal(fs.readFileSync(output, 'utf8'), before);
        assert.ok(!fs.existsSync(absent));
        assert.ok(!fs.existsSync(path.dirname(absent)));
        assert.ok(!fs.readdirSync(tmp).some(name => name.includes('.tmp-')));
      }
    } finally { fs.writeFileSync(files[0], original); }
  });
  test('repeated advanced controls fit compactly with exact per-widget references', () => {
    const originals = files.map(file => fs.readFileSync(file, 'utf8'));
    try {
      const advanced = Array.from({ length: 266 }, (_, i) => control(`_advanced_${i}`, 'dimensions'));
      files.forEach((file, i) => fs.writeFileSync(file, JSON.stringify(dump(versions[i],
        Array.from({ length: 30 }, (_, j) => widget(`widget_${j}`, advanced))))));
      const result = run();
      assert.equal(result.status, 0, result.stderr);
      const data = JSON.parse(fs.readFileSync(output, 'utf8'));
      assert.equal(Object.keys(data.shared_controls).length, advanced.length);
      assert.ok(data.widgets.every(entry => entry.controls.length === 0 && entry.shared_controls.length === 1
        && Object.keys(entry.shared_controls[0].controls).length === advanced.length));
      const size = fs.statSync(output).size;
      assert.ok(size <= 1024 * 1024);
      console.log(`widget-allowlist_test: compact sample size=${size} bytes`);
    } finally {
      files.forEach((file, i) => fs.writeFileSync(file, originals[i]));
      assert.equal(run().status, 0);
    }
  });
  console.log(`widget-allowlist_test: ${passed} passed, ${failed} failed`);
  if (failed) process.exitCode = 1;
} catch (error) {
  console.error(`FAIL widget-allowlist_test: ${error.message}`);
  process.exitCode = 1;
} finally { fs.rmSync(tmp, { recursive: true, force: true }); }
