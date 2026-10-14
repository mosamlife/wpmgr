<?php
declare(strict_types=1);

use Elementor\Plugin;
use Elementor\Core\Files\CSS\Post as NativePostCss;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentFingerprint;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentSnapshot;
use WPMgr\Agent\Abilities\Builders\BuilderPageEdit;
use WPMgr\Agent\Abilities\Builders\ElementorAdapter;
use WPMgr\Agent\Abilities\Builders\ElementorDocument;
use WPMgr\Agent\Abilities\Builders\ElementorWidgetDiscovery;
use WPMgr\Agent\Abilities\OwnAbilities;
use WPMgr\Agent\Commands\AbilityRunCommand;

/** The native phase registers stacks in the same early REST context as the agent. */
function rt_native_boot_context(): void
{
    if (class_exists(Plugin::class, false) || (function_exists('did_action') && did_action('elementor/loaded') > 0)) {
        throw new RuntimeException('native REST context must precede Elementor boot');
    }
    if ((defined('REST_REQUEST') && REST_REQUEST !== true) || (defined('WP_ADMIN') && WP_ADMIN === true)) {
        throw new RuntimeException('native phase requires a REST request outside admin');
    }
    if (!defined('REST_REQUEST')) define('REST_REQUEST', true);
}

/** Record the flag before and after every stack read, without overriding Performance state. */
function rt_native_registration_check(string $version, array &$context): void
{
    $class = 'Elementor\\Core\\Frontend\\Performance';
    $available = is_callable([$class, 'should_optimize_controls']);
    if (!$available && version_compare($version, '3.35.9', '>=')) {
        throw new RuntimeException('native Performance optimisation API unavailable');
    }
    $flag = $available ? $class::should_optimize_controls() : null;
    $context['should_optimize_controls'] = $flag;
    $context['optimization_check'] = $available ? 'performance_api' : 'api_not_available';
    ++$context['registration_checks'];
    if ($available && $flag !== false) throw new RuntimeException('native control registration is optimised');
}

/**
 * Documented discovery/oracle differences (each occurrence is printed):
 * - The agent omits its explicit unsafe-key and code-control denylist.
 * - The dump omits controls/repeaters with incomplete group metadata. Discovery
 *   can describe those controls; this phase does not claim they can be edited.
 * - A UI button can occur in discovery; the dump classifies that type as UI only.
 * - Discovery exposes responsive devices as the writable window and normalizes
 *   a duplicate root's max=desktop bound to null. The devices must still match.
 * - An absent switcher return_value means Elementor's default "yes".
 * Array order of options/devices and the dump-only responsive source tag are
 * representation details; the option/device sets themselves must be identical.
 */
function rt_native_denied(string $id, string $type): bool
{
    $id = strtolower($id);
    return in_array($id, ['__dynamic__', '__globals__', 'custom_css', '_attributes', 'custom_attributes', '_css_classes'], true)
        || str_starts_with($id, 'motion_fx_') || str_starts_with($id, 'sticky')
        || in_array($type, ['code', 'html', 'custom_code'], true);
}

/** Read whole compact descriptors, splitting truncated answers under the discovery cap. */
function rt_native_read_controls(object $discovery, string $widget, array $ids, ?callable $registrationCheck = null): array
{
    if ($ids === [] || count($ids) !== count(array_unique($ids))) {
        throw new RuntimeException('native controls request is empty or duplicated');
    }
    $out = [];
    $read = static function (array $batch) use (&$read, &$out, $discovery, $widget, $registrationCheck): void {
        if ($registrationCheck !== null) $registrationCheck();
        $answer = $discovery->schema((object) ['widget' => $widget, 'controls' => $batch]);
        if ($registrationCheck !== null) $registrationCheck();
        $row = $answer['output'] ?? null;
        if (!is_array($row) || ($row['builder'] ?? null) !== 'elementor' || !is_string($row['builder_version'] ?? null)
            || (defined('ELEMENTOR_VERSION') && $row['builder_version'] !== ELEMENTOR_VERSION)
            || ($row['mode'] ?? null) !== 'controls' || ($row['widget'] ?? null) !== $widget
            || ($row['usable'] ?? null) !== true || ($row['atomic'] ?? null) !== false
            || !is_bool($row['truncated'] ?? null) || (!is_array($row['controls'] ?? null) && !($row['controls'] ?? null) instanceof stdClass)
            || strlen(json_encode($row, JSON_THROW_ON_ERROR)) > ElementorWidgetDiscovery::MAX_BYTES) {
            throw new RuntimeException('native controls answer refused, malformed or over cap: ' . rt_brief($answer));
        }
        $controls = (array) $row['controls'];
        if (array_diff(array_keys($controls), $batch) !== []) {
            throw new RuntimeException('native controls answer contains an unrequested id');
        }
        if ($row['truncated']) {
            if (count($batch) === 1) throw new RuntimeException('native single control exceeds discovery cap: ' . $widget . '.' . $batch[0]);
            $cut = intdiv(count($batch), 2);
            $read(array_slice($batch, 0, $cut));
            $read(array_slice($batch, $cut));
            return;
        }
        if (array_diff($batch, array_keys($controls)) !== []) throw new RuntimeException('native controls answer lost requested ids');
        foreach ($batch as $id) {
            if (!is_array($controls[$id]) || isset($out[$id])
                || array_diff(['type', 'options', 'default', 'responsive', 'group'], array_keys($controls[$id])) !== []) {
                throw new RuntimeException('native controls descriptor malformed or duplicated');
            }
            $out[$id] = $controls[$id];
        }
    };
    foreach (array_chunk($ids, 128) as $batch) $read($batch);
    return $out;
}

/** Compare every semantic field, including nested repeater fields, and retain every difference. */
function rt_native_compare(array $oracle, array $skipped, array $live, string $path = ''): array
{
    $expected = array_column($oracle, null, 'id');
    $omitted = array_column($skipped, 'reason', 'id');
    $diffs = [];
    $add = static function (string $at, $want, $got, ?string $allowed = null) use (&$diffs): void {
        $diffs[] = ['path' => $at, 'expected' => $want, 'actual' => $got, 'allowed' => $allowed];
    };
    $set = static function ($values) {
        if (!is_array($values)) return $values;
        sort($values, SORT_STRING);
        return array_values($values);
    };
    foreach (array_unique(array_merge(array_keys($expected), array_keys($live))) as $id) {
        $at = $path . $id;
        $want = $expected[$id] ?? null;
        $got = $live[$id] ?? null;
        if ($want === null || $got === null) {
            $allowed = null;
            if ($want !== null && $got === null && rt_native_denied($id, $want['type'])) $allowed = 'agent_denylist';
            if ($want === null && $got !== null && ($omitted[$id] ?? null) === 'incomplete_group_metadata') $allowed = 'oracle_incomplete_group_metadata';
            if ($want === null && is_array($got) && ($omitted[$id] ?? null) === 'ui_only_type' && ($got['type'] ?? null) === 'button') $allowed = 'oracle_ui_button';
            $add($at . '.id', $want === null ? null : $id, $got === null ? null : $id, $allowed);
            continue;
        }
        if (!is_array($got)) { $add($at, $want, $got); continue; }
        foreach (['type', 'options', 'group', 'return_value'] as $field) {
            $a = $want[$field] ?? null;
            $b = $got[$field] ?? null;
            if ($field === 'options') { $a = $set($a); $b = $set($b); }
            // Group key order does not change the family's contract.
            if ($field === 'group' && is_array($a) && is_array($b)) { ksort($a); ksort($b); }
            if ($a !== $b) {
                $allowed = $field === 'return_value' && $want['type'] === 'switcher' && $a === 'yes' && $b === null
                    ? 'switcher_default_yes' : null;
                $add($at . '.' . $field, $a, $b, $allowed);
            }
        }
        $a = $want['responsive'] ?? null;
        $b = $got['responsive'] ?? null;
        if (is_array($a) && is_array($b)) {
            foreach (['devices', 'min', 'max'] as $field) {
                $av = $a[$field] ?? null; $bv = $b[$field] ?? null;
                if ($field === 'devices') { $av = $set($av); $bv = $set($bv); }
                if ($av !== $bv) {
                    $allowed = $field === 'max' && ($a['source'] ?? null) === 'duplicates' && $av === 'desktop' && $bv === null
                        && ($a['min'] ?? null) === null && ($b['min'] ?? null) === null
                        && $set($a['devices']) === $set($b['devices']) ? 'responsive_bounds_normalized' : null;
                    $add($at . '.responsive.' . $field, $av, $bv, $allowed);
                }
            }
        } elseif ($a !== $b) { $add($at . '.responsive', $a, $b); }
        $fields = (array) ($got['fields'] ?? []);
        array_push($diffs, ...rt_native_compare($want['fields'] ?? [], [], $fields, $at . '.fields.'));
    }
    return $diffs;
}

/** Stored JSON must equal the plan's JSON bytes, with no duplicate data rows. */
function rt_native_tree_bytes(array $planned, array $rows): ?string
{
    if (count($rows) !== 1 || !is_string($rows[0])) return 'expected one raw _elementor_data row';
    $json = json_encode($planned, JSON_THROW_ON_ERROR);
    if ($rows[0] === $json) return null;
    return 'stored _elementor_data bytes differ from the planned JSON (sha256 planned=' . hash('sha256', $json)
        . ' stored=' . hash('sha256', $rows[0]) . ')';
}

/** Every successful edit must reach Elementor's save through the agent's write path. */
function rt_native_applied(RtChecks $checks, array $answer): bool
{
    return $checks->ck('write', ($answer['ok'] ?? false) === true && ($answer['outcome'] ?? null) === 'applied', rt_brief($answer));
}

/** Undo is held to SQL bytes independently of the command's claimed result. */
function rt_native_undo_hold(RtChecks $checks, array $dataWas, array $dataNow, array $metaWas, array $metaNow, array $postWas, array $postNow): void
{
    $checks->ck('undo-data-bytes', $dataNow === $dataWas, 'undo changed original _elementor_data bytes');
    $why = rt_native_meta_diff($metaWas, $metaNow);
    $checks->ck('undo-meta-bytes', $why === null, (string) $why);
    $checks->ck('undo-post-bytes', $postNow === $postWas, 'undo changed preserved posts columns');
}

/** Describe every differing preserved key and the global row order using hashes of stored bytes. */
function rt_native_meta_diff(array $before, array $after): ?string
{
    if ($before === $after) return null;
    $group = static function (array $rows): array {
        $out = [];
        foreach ($rows as [$key, $value]) $out[$key][] = $value === null ? null : hash('sha256', $value);
        ksort($out);
        return $out;
    };
    $old = $group($before); $new = $group($after); $diffs = [];
    foreach (array_unique(array_merge(array_keys($old), array_keys($new))) as $key) {
        if (($old[$key] ?? []) !== ($new[$key] ?? [])) $diffs[] = ['key' => $key, 'expected_sha256' => $old[$key] ?? [], 'actual_sha256' => $new[$key] ?? []];
    }
    return 'preserved postmeta bytes or order differ: ' . json_encode(['keys' => $diffs,
        'expected_order' => array_column($before, 0), 'actual_order' => array_column($after, 0)], JSON_THROW_ON_ERROR);
}

/** The pinned legacy REST stack must give the same explicit refusal in both discovery modes. */
function rt_native_unavailable_hold(RtChecks $checks, string $pin, array $skeleton, array $named): void
{
    $checks->ck('availability-pin', $pin === '3.20.4', 'native controls are unexpectedly unavailable on ' . $pin);
    foreach (['skeleton' => $skeleton, 'controls' => $named] as $mode => $answer) {
        $checks->ck('availability-' . $mode, ($answer['refusal']['code'] ?? null) === ElementorWidgetDiscovery::CODE_CONTROLS_UNAVAILABLE
            && ($answer['refusal']['detail'] ?? null) === ElementorWidgetDiscovery::DETAIL_CONTROLS_UNAVAILABLE, rt_brief($answer));
    }
}

/** Prefer an offered alignment whose CSS value is independent of text direction. */
function rt_native_alignment(array $control): string
{
    foreach (['center', 'justify', 'end', 'right', 'start', 'left'] as $value) {
        if (in_array($value, $control['options'] ?? [], true)) return $value;
    }
    throw new RuntimeException('native heading alignment has no usable live option');
}

/** Both declaration and rendered-text positive controls are mandatory in every native case. */
function rt_native_render_hold(RtChecks $checks, array $probe, string $html, string $css): array
{
    $found = ['css' => 0, 'texts' => 0];
    $checks->ck('css-positive-control', ($probe['css'] ?? []) !== [], 'native case expects no declaration');
    $checks->ck('text-positive-control', ($probe['texts'] ?? []) !== [], 'native case expects no rendered text');
    $checks->ck('render-nonempty', trim($html) !== '', 'native page rendered nothing');
    $checks->ck('css-nonempty', trim($css) !== '', 'native post stylesheet is empty');
    $rules = rt_css_rules($css);
    foreach ($probe['css'] ?? [] as $exp) {
        [$ok, $why] = rt_css_expect($rules, $exp);
        if ($checks->ck('css', $ok, rt_css_label($exp) . ': ' . $why)) ++$found['css'];
    }
    $dom = rt_dom($html);
    $visible = [];
    foreach ((new DOMXPath($dom))->query('//text()[not(ancestor::script or ancestor::style or ancestor::template or ancestor::noscript or ancestor::head)]') as $node) {
        $visible[] = $node->nodeValue;
    }
    $text = rt_norm(implode(' ', $visible));
    foreach ($probe['texts'] ?? [] as $value) {
        if ($checks->ck('render-text', str_contains($text, rt_norm($value)), 'missing text ' . rt_brief($value))) ++$found['texts'];
    }
    foreach ($probe['gone'] ?? [] as $value) $checks->ck('removed-text', !str_contains($text, rt_norm($value)), 'removed text remains: ' . rt_brief($value));
    foreach ($dom->getElementsByTagName('*') as $el) {
        $checks->ck('render-safe', strtolower($el->nodeName) !== 'script', 'native content contains a script');
        foreach ($el->attributes as $attr) $checks->ck('render-safe', !str_starts_with(strtolower($attr->nodeName), 'on'), 'native content contains an on* attribute');
    }
    return $found;
}

/** The allowlist is an inventory; the live dump remains the descriptor oracle. */
function rt_native_inventory(string $file, string $version): array
{
    $raw = is_file($file) ? file_get_contents($file) : false;
    if (!is_string($raw) || $raw === '') throw new RuntimeException('native core inventory missing or empty');
    $catalogue = json_decode($raw, true, 512, JSON_THROW_ON_ERROR);
    if (($catalogue['schema_version'] ?? null) !== 2 || !in_array($version, $catalogue['elementor_versions'] ?? [], true)) {
        throw new RuntimeException('native core inventory does not name this pin');
    }
    $names = [];
    foreach ($catalogue['widgets'] ?? [] as $row) {
        if (in_array($version, $row['versions'] ?? [], true)) {
            $name = $row['name'] ?? null;
            if (!is_string($name) || $name === '' || isset($names[$name])) throw new RuntimeException('native core inventory has duplicate or invalid widget');
            $names[$name] = true;
        }
    }
    if ($names === []) throw new RuntimeException('native core inventory names no widgets');
    ksort($names);
    return array_keys($names);
}

/** Catalogue and controls-mode reads through the actual agent, before the oracle reads stacks. */
function rt_native_discovery(array $names, array &$context, array $plants, ?ElementorWidgetDiscovery $discovery = null, ?callable $readOracle = null): array
{
    $out = ['widgets' => 0, 'controls' => 0, 'unavailable' => 0, 'checks' => 0, 'failed' => 0, 'differences' => 0, 'allowed' => 0, 'alignment' => ''];
    $discovery ??= new ElementorWidgetDiscovery();
    $registration = static function () use (&$context): void { rt_native_registration_check(ELEMENTOR_VERSION, $context); };
    $registration();
    $catalogue = $discovery->catalogue((object) []);
    $registration();
    $root = new RtChecks();
    $rows = $catalogue['output'] ?? null;
    $root->ck('catalogue', is_array($rows) && strlen(json_encode($rows, JSON_THROW_ON_ERROR)) <= ElementorWidgetDiscovery::MAX_BYTES
        && ($rows['builder_version'] ?? null) === ELEMENTOR_VERSION, rt_brief($catalogue));
    $live = [];
    foreach ($names as $name) {
        $c = new RtChecks();
        ++$out['widgets'];
        try {
            $catalogueRows = $rows['widgets'] ?? [];
            if (($rows['truncated'] ?? false) === true) {
                $selected = $discovery->catalogue((object) ['search' => $name]);
                if (($selected['output']['truncated'] ?? true) !== false) throw new RuntimeException('native filtered catalogue truncated');
                $catalogueRows = $selected['output']['widgets'] ?? [];
            }
            $entry = array_column($catalogueRows, null, 'name')[$name] ?? null;
            $c->ck('catalogue-widget', is_array($entry) && ($entry['usable'] ?? false) === true
                && ($entry['atomic'] ?? true) === false && ($entry['pro'] ?? true) === false, 'core widget missing or unusable: ' . $name);
            $registration();
            $skeleton = $discovery->schema((object) ['widget' => $name]);
            $registration();
            if (($skeleton['refusal']['code'] ?? null) === ElementorWidgetDiscovery::CODE_CONTROLS_UNAVAILABLE) {
                $named = $discovery->schema((object) ['widget' => $name, 'controls' => ['_margin']]);
                $registration();
                rt_native_unavailable_hold($c, ELEMENTOR_VERSION, $skeleton, $named);
                ++$out['unavailable'];
                rt_out('NATIVE_UNAVAILABLE widget=' . $name . ' code=' . ElementorWidgetDiscovery::CODE_CONTROLS_UNAVAILABLE);
                rt_native_report('discovery-' . $name, $c, $out);
                continue;
            }
            $row = $skeleton['output'] ?? null;
            if (!is_array($row) || ($row['mode'] ?? null) !== 'skeleton' || ($row['truncated'] ?? true) !== false
                || ($row['widget'] ?? null) !== $name || strlen(json_encode($row, JSON_THROW_ON_ERROR)) > ElementorWidgetDiscovery::MAX_BYTES) {
                throw new RuntimeException('native skeleton refused, truncated or over cap: ' . rt_brief($skeleton));
            }
            $ids = [];
            foreach ($row['tabs'] ?? [] as $tab) foreach ($tab['sections'] ?? [] as $section) foreach ($section['controls'] ?? [] as $control) $ids[] = $control['id'];
            $live[$name] = rt_native_read_controls($discovery, $name, $ids, $registration);
            $out['controls'] += count($live[$name]);
            if ($name === 'heading') $out['alignment'] = rt_native_alignment($live[$name]['align'] ?? []);
        } catch (Throwable $e) { $c->ck('discovery', false, $e->getMessage()); }
        rt_native_report('discovery-' . $name, $c, $out);
    }

    if ($out['unavailable'] > 0) {
        $root->ck('availability-all-widgets', $out['unavailable'] === count($names) && $out['controls'] === 0, 'native availability differs between core widgets');
        rt_native_report('discovery', $root, $out);
        rt_out('NATIVE_REGISTRATION ' . json_encode($context, JSON_THROW_ON_ERROR));
        return $out;
    }
    $root->ck('availability-pin', ELEMENTOR_VERSION !== '3.20.4', 'legacy REST controls unexpectedly offered native settings');

    // This is the same projection used by dumpwidgets=1, with REST stack registration.
    $registration();
    try {
        $dump = $readOracle !== null ? $readOracle()
            : rt_widget_dump_inventory(Plugin::$instance, ELEMENTOR_PATH, ELEMENTOR_VERSION, $registration);
        rt_widget_dump_positive_controls($dump);
    } catch (Throwable $e) {
        $root->ck('oracle', false, $e->getMessage());
        rt_native_report('discovery', $root, $out);
        rt_out('NATIVE_REGISTRATION ' . json_encode($context, JSON_THROW_ON_ERROR));
        return $out;
    }
    $oracle = array_column($dump['widgets'], null, 'name');
    foreach (array_diff($names, array_keys($oracle)) as $name) $root->ck('oracle-widget', false, 'oracle lacks inventory widget ' . $name);
    foreach (array_diff(array_keys($oracle), $names) as $name) $root->ck('inventory-widget', false, 'inventory lacks oracle widget ' . $name);
    foreach ($names as $name) {
        if (!isset($oracle[$name])) continue;
        $c = new RtChecks();
        $controls = $live[$name] ?? [];
        if (in_array('native_discovery_drift', $plants[$name] ?? [], true) && isset($controls['title'])) $controls['title']['type'] = 'number';
        $diffs = rt_native_compare($oracle[$name]['controls'], $oracle[$name]['controls_skipped'], $controls, $name . '.');
        foreach ($diffs as $difference) {
            ++$out['differences'];
            rt_out('NATIVE_DIFF ' . json_encode($difference, JSON_UNESCAPED_SLASHES | JSON_THROW_ON_ERROR));
            if ($difference['allowed'] === null) $c->ck('oracle', false, $difference['path']);
            else ++$out['allowed'];
        }
        $c->ck('oracle-controls', ($live[$name] ?? []) !== [], 'widget discovery compared no controls');
        rt_native_report('oracle-' . $name, $c, $out);
    }
    $root->ck('discovery-positive-control', $out['widgets'] > 0 && $out['controls'] > 0, 'discovery looked at nothing');
    rt_native_report('discovery', $root, $out);
    rt_out('NATIVE_REGISTRATION ' . json_encode($context, JSON_THROW_ON_ERROR));
    return $out;
}

/** Emit checks on the same tags as other phases, retaining an explicit failure tally. */
function rt_native_report(string $name, RtChecks $checks, array &$out): void
{
    $out['checks'] += $checks->count;
    $tag = ELEMENTOR_VERSION . ' containers native-' . $name;
    if ($checks->fails === []) rt_out(sprintf('ok   [%s] checks=%d', $tag, $checks->count));
    else {
        ++$out['failed'];
        foreach ($checks->fails as $why) rt_out('FAIL [' . $tag . '] ' . $why);
    }
}

/** Native operations and their independent visible expectations on the stored seed page. */
function rt_native_cases(string $tabletMedia, string $alignment = 'right'): array
{
    $baseline = ['el' => '4ead001', 'sel' => '.elementor-heading-title', 'media' => null, 'decl' => ['color' => '#24567A']];
    $cases = [
        ['name' => 'set-title', 'op' => ['op' => 'set_native', 'ref' => '4ead001', 'settings' => ['title' => 'Native edited heading']],
            'texts' => ['Native edited heading'], 'gone' => ['Native original heading'], 'css' => [$baseline]],
        ['name' => 'set-typography', 'op' => ['op' => 'set_native', 'ref' => '4ead001', 'settings' => [
            'typography_typography' => 'custom', 'typography_font_size' => ['unit' => 'px', 'size' => '37'],
            'typography_font_weight' => '700', 'typography_font_size_tablet' => ['unit' => 'px', 'size' => '29']]],
            'texts' => ['Native original heading'], 'css' => [
                ['el' => '4ead001', 'sel' => '.elementor-heading-title', 'media' => null, 'decl' => ['font-size' => '37px', 'font-weight' => '700']],
                ['el' => '4ead001', 'sel' => '.elementor-heading-title', 'media' => $tabletMedia, 'decl' => ['font-size' => '29px']]]],
        ['name' => 'set-color', 'op' => ['op' => 'set_native', 'ref' => '4ead001', 'settings' => ['title_color' => '#123456']],
            'texts' => ['Native original heading'], 'css' => [['el' => '4ead001', 'sel' => '.elementor-heading-title', 'media' => null, 'decl' => ['color' => '#123456']]]],
        ['name' => 'set-tablet-align', 'op' => ['op' => 'set_native', 'ref' => '4ead001', 'settings' => ['align_tablet' => $alignment]],
            'texts' => ['Native original heading'], 'css' => [['el' => '4ead001', 'sel' => '', 'media' => $tabletMedia, 'decl' => ['text-align' => $alignment]]]],
        ['name' => 'add-button', 'op' => ['op' => 'add_native', 'into' => 'c0ffee2', 'widget' => 'button',
            'settings' => ['text' => 'Native linked button', 'link' => ['url' => 'https://example.com/native-roundtrip', 'is_external' => '', 'nofollow' => 'on']]],
            'texts' => ['Native linked button'], 'link' => 'https://example.com/native-roundtrip', 'css' => [$baseline]],
        ['name' => 'duplicate-image', 'op' => ['op' => 'duplicate', 'ref' => '1a6e001', 'into' => 'c0ffee2'],
            'texts' => ['Native original heading'], 'images' => 2, 'css' => [$baseline]],
        ['name' => 'move-counter', 'op' => ['op' => 'move', 'ref' => 'c0a0001', 'into' => 'c0ffee2'],
            'texts' => ['Native counter label'], 'css' => [$baseline]],
        ['name' => 'remove-icon-box', 'op' => ['op' => 'remove', 'ref' => '1c0b001'],
            'texts' => ['Native original heading'], 'gone' => ['Native icon box label'], 'css' => [$baseline]],
    ];
    return $cases;
}

/** A real library attachment, made locally so the phase needs no image download. */
function rt_native_image(): int
{
    wp_set_current_user(1);
    $up = wp_upload_dir();
    if (!empty($up['error'])) throw new RuntimeException('native image upload directory unavailable');
    $file = $up['path'] . '/native-roundtrip.png';
    $gd = imagecreatetruecolor(320, 200);
    imagefill($gd, 0, 0, imagecolorallocate($gd, 30, 120, 200));
    if (!imagepng($gd, $file)) throw new RuntimeException('native image could not be written');
    unset($gd);
    $id = wp_insert_attachment(['post_mime_type' => 'image/png', 'post_title' => 'Native library image', 'post_status' => 'inherit'], $file, 0, true);
    if (is_wp_error($id) || $id < 1) throw new RuntimeException('native library image could not be registered');
    wp_update_attachment_metadata($id, wp_generate_attachment_metadata($id, $file));
    update_post_meta($id, '_wp_attachment_image_alt', 'Native library image alt');
    wp_set_current_user(0);
    return (int) $id;
}

/** Read the raw row through SQL independently of both the plan and the write's verification. */
function rt_native_data_rows(int $postId): array
{
    global $wpdb;
    $rows = $wpdb->get_col($wpdb->prepare("SELECT meta_value FROM {$wpdb->postmeta} WHERE post_id = %d AND meta_key = %s ORDER BY meta_id", $postId, '_elementor_data'));
    if (!is_array($rows) || $wpdb->last_error !== '') throw new RuntimeException('native raw data query failed');
    return $rows;
}

/** Hold the plan's result to the operation before comparing the entire stored page. */
function rt_native_operation_hold(RtChecks $c, array $case, array $before, array $after): void
{
    $index = static function (array $tree): array {
        $out = [];
        $walk = static function (array $nodes, ?string $parent) use (&$walk, &$out): void {
            foreach ($nodes as $node) {
                $out[$node['id']] = ['node' => $node, 'parent' => $parent];
                $walk($node['elements'] ?? [], $node['id']);
            }
        };
        $walk($tree, null);
        return $out;
    };
    $old = $index($before); $new = $index($after); $op = $case['op'];
    $ref = $op['ref'] ?? '';
    switch ($op['op']) {
        case 'set_native':
            $node = $old[$ref]['node'];
            foreach ($op['settings'] as $key => $value) $node['settings'][$key] = $value;
            $c->ck('operation', ($new[$ref]['node'] ?? null) === $node, 'set_native plan does not preserve the widget and apply its settings');
            break;
        case 'add_native':
        case 'duplicate':
            $added = array_diff(array_keys($new), array_keys($old));
            $c->ck('operation', count($added) === 1, 'native operation did not add exactly its widget');
            foreach ($added as $id) {
                $node = $new[$id]['node'];
                $c->ck('operation-parent', $new[$id]['parent'] === $op['into'], 'new widget is in the wrong container');
                if ($op['op'] === 'duplicate') {
                    $copy = $old[$ref]['node']; $copy['id'] = $id;
                    $c->ck('operation-copy', $node === $copy, 'image duplicate is not the Elementor-stored widget with only its id changed');
                } else {
                    $c->ck('operation-widget', ($node['widgetType'] ?? null) === $op['widget'], 'added widget type differs');
                    $c->ck('operation-settings', ($node['settings'] ?? null) === $op['settings'], 'added widget settings differ');
                }
            }
            break;
        case 'move':
            $c->ck('operation', ($new[$ref]['node'] ?? null) === $old[$ref]['node'] && ($new[$ref]['parent'] ?? null) === $op['into'], 'counter did not move unchanged into destination container');
            break;
        case 'remove':
            $c->ck('operation', !isset($new[$ref]) && count($new) === count($old) - 1, 'icon box was not removed');
            break;
    }
}

/** The command envelopes use the same snapshot policies as the other harness phases. */
function rt_native_entries(): array
{
    return [rt_entry(OwnAbilities::NAME_PAGE_CREATE, 'created_post_trash'), rt_entry(OwnAbilities::NAME_PAGE_EDIT, BuilderPageEdit::SNAPSHOT)];
}

/** The editable baseline is the stored page after its initial view; every preserved row remains covered. */
function rt_native_baseline(int $postId, array $skip): array
{
    $data = rt_native_data_rows($postId);
    $meta = rt_raw_rows($postId, $skip);
    $html = (string) Plugin::$instance->frontend->get_builder_content($postId, false);
    setlocale(LC_NUMERIC, 'C');
    if (trim($html) === '') throw new RuntimeException('native seed rendered nothing');
    $after = rt_native_data_rows($postId);
    if ($after !== $data) throw new RuntimeException('native seed render changed stored tree bytes');
    $rows = rt_raw_rows($postId, $skip);
    $difference = rt_native_meta_diff($meta, $rows);
    if ($difference !== null) rt_out('NATIVE_SEED_RENDER ' . $difference);
    return ['data' => $after, 'meta' => $rows, 'post' => rt_raw_post($postId)];
}

/** Native page edits, each undone through the same ability command before the next starts. */
function rt_native_edits(int $principal, array $plants, string $alignment, bool $unavailable = false): array
{
    $out = ['edits' => 0, 'ops' => [], 'css' => 0, 'texts' => 0, 'refused' => 0, 'checks' => 0, 'failed' => 0];
    $cmd = new AbilityRunCommand();
    [$create, $edit] = rt_native_entries();
    $c = new RtChecks();
    [$pid, $why] = rt_make_draft($cmd, $create, [['type' => 'heading', 'level' => 2, 'text' => 'Native seed']]);
    if (!$c->ck('seed', $pid > 0, $why)) { rt_native_report('seed', $c, $out); return $out; }
    try {
        $imageId = rt_native_image();
        $widget = static fn (string $id, string $type, array $settings): array =>
            ['id' => $id, 'elType' => 'widget', 'settings' => $settings, 'elements' => [], 'widgetType' => $type];
        $tree = [
            ['id' => 'c0ffee1', 'elType' => 'container', 'isInner' => false, 'settings' => [], 'elements' => [
                $widget('4ead001', 'heading', ['title' => 'Native original heading', 'header_size' => 'h2', 'title_color' => '#24567A']),
                $widget('b077001', 'button', ['text' => 'Native original button']),
                $widget('1a6e001', 'image', ['image' => ['id' => (string) $imageId, 'url' => wp_get_attachment_url($imageId)], 'image_size' => 'full']),
                $widget('c0a0001', 'counter', ['title' => 'Native counter label', 'ending_number' => '17']),
                $widget('1c0b001', 'icon-box', ['title_text' => 'Native icon box label', 'description_text' => 'Native icon box description',
                    'selected_icon' => ['value' => 'fas fa-star', 'library' => 'fa-solid']]),
            ]],
            ['id' => 'c0ffee2', 'elType' => 'container', 'isInner' => false, 'settings' => [], 'elements' => []],
        ];
        if (!$c->ck('seed-save', rt_save_tree($pid, $tree, $principal), 'Elementor did not save real core widgets')) { rt_native_report('seed', $c, $out); return $out; }
        [$n, $before] = rt_stored_tree($pid);
        if (!$c->ck('seed-stored', $n === 1 && is_array($before) && $before !== [], 'native seed has no stored tree')) { rt_native_report('seed', $c, $out); return $out; }
        $c->ck('seed-widgets', count($before[0]['elements'] ?? []) === count($tree[0]['elements']), 'Elementor dropped a seed widget');
        $c->ck('library-image', is_array($before[0]['elements'][2]['settings']['image'] ?? null)
            && (string) $before[0]['elements'][2]['settings']['image']['id'] === (string) $imageId, 'seed has no real library image');
        rt_native_report('seed', $c, $out);
        $skip = array_merge([BuilderDocumentSnapshot::EDIT_LOCK_KEY], (new ElementorAdapter())->descriptor()->derivedKeys);
        $baseline = rt_native_baseline($pid, $skip);
        $originalRows = $baseline['meta'];
        $originalPost = $baseline['post'];
        $originalData = $baseline['data'];
        $tablet = Plugin::$instance->breakpoints->get_active_breakpoints()['tablet'] ?? null;
        if (!is_object($tablet) || !is_callable([$tablet, 'get_value'])) throw new RuntimeException('native tablet breakpoint unavailable');
        $cases = rt_native_cases('(max-width:' . $tablet->get_value() . 'px)', $alignment);
        foreach ($cases as $case) {
            ++$out['edits'];
            $c = new RtChecks();
            $kinds = $plants[$case['name']] ?? [];
            $c->ck('original-bytes', rt_native_data_rows($pid) === $originalData, 'previous undo did not restore the original bytes');
            $fp = BuilderDocumentFingerprint::ofPost($pid, ElementorDocument::DESCRIPTOR_KEYS);
            $rid = wp_generate_uuid4();
            $input = json_encode(['post_id' => $pid, 'base_fingerprint' => $fp, 'operations' => [$case['op']]], JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR);
            $more = ['input' => $input, 'allowed_draft_ids' => [$pid]];
            $pre = rt_call($cmd, $edit, 'precheck', $rid, $more);
            if ($unavailable) {
                foreach (['precheck' => $pre, 'write' => rt_call($cmd, $edit, 'write', $rid, $more + ['expected' => [
                    'precheck_digest' => str_repeat('0', 64), 'preview_digest' => str_repeat('0', 64)]])] as $mode => $answer) {
                    if ($c->ck('unavailable-' . $mode, ($answer['ok'] ?? true) === false
                        && ($answer['code'] ?? null) === ElementorWidgetDiscovery::CODE_CONTROLS_UNAVAILABLE
                        && ($answer['detail'] ?? null) === ElementorWidgetDiscovery::DETAIL_CONTROLS_UNAVAILABLE, rt_brief($answer))) ++$out['refused'];
                }
                rt_native_undo_hold($c, $originalData, rt_native_data_rows($pid), $originalRows, rt_raw_rows($pid, $skip), $originalPost, rt_raw_post($pid));
                rt_native_report($case['name'], $c, $out);
                continue;
            }
            if ($c->ck('precheck', ($pre['ok'] ?? false) === true && ($pre['outcome'] ?? null) === 'prechecked', rt_long($pre))) {
                $planned = $pre['preview']['tree'] ?? null;
                $c->ck('preview-before', ($pre['preview']['before_tree'] ?? null) === $before, 'native preview before_tree differs from SQL seed');
                if ($c->ck('preview-tree', is_array($planned) && $planned !== [], 'native preview is empty')) rt_native_operation_hold($c, $case, $before, $planned);
                $fired = 0;
                $reject = static function ($value) use (&$fired) { ++$fired; throw new RuntimeException('native rejected edit positive control'); };
                if (in_array('native_rejected_edit', $kinds, true)) add_filter('sanitize_post_meta__elementor_data', $reject, 10, 1);
                try { $w = rt_call($cmd, $edit, 'write', $rid, $more + ['expected' => rt_digests($pre)]); }
                finally { remove_filter('sanitize_post_meta__elementor_data', $reject, 10); }
                if (in_array('native_rejected_edit', $kinds, true)) $c->ck('plant', $fired > 0, 'Elementor rejection plant did not run');
                if (rt_native_applied($c, $w)) {
                    $out['ops'][] = $case['op']['op'];
                    $c->ck('changes-applied', ($w['changes_applied'] ?? null) === 1, 'native write did not apply its operation');
                    $c->ck('write-digest', ($w['preview_digest'] ?? null) === ($pre['preview_digest'] ?? null), 'native write changed preview digest');
                    $data = rt_native_data_rows($pid);
                    if (in_array('native_stored_drift', $kinds, true)) $data[0] .= ' ';
                    $why = is_array($planned) ? rt_native_tree_bytes($planned, $data) : 'native plan unreadable';
                    $c->ck('stored-bytes', $why === null, (string) $why);
                    // Read what the agent's save produced before rendering can generate styles.
                    $file = NativePostCss::create($pid);
                    clearstatcache();
                    $path = (string) $file->get_path();
                    $css = is_file($path) ? (string) file_get_contents($path) : '';
                    $renderRows = rt_raw_rows($pid, $skip);
                    $html = (string) Plugin::$instance->frontend->get_builder_content($pid, false);
                    setlocale(LC_NUMERIC, 'C');
                    $renderDiff = rt_native_meta_diff($renderRows, rt_raw_rows($pid, $skip));
                    if ($renderDiff !== null) rt_out('NATIVE_RENDER case=' . $case['name'] . ' ' . $renderDiff);
                    if (in_array('native_css_missing', $kinds, true)) $css = '';
                    if (in_array('native_text_missing', $kinds, true)) $html = '<div>Native plant</div>';
                    $found = rt_native_render_hold($c, $case, $html, $css);
                    $out['css'] += $found['css']; $out['texts'] += $found['texts'];
                    if (isset($case['link'])) {
                        $links = [];
                        foreach (rt_dom($html)->getElementsByTagName('a') as $a) if ($a->getAttribute('href') === $case['link']) $links[] = $a;
                        $c->ck('render-link', count($links) === 1 && str_contains($links[0]->getAttribute('rel'), 'nofollow'), 'added button link or nofollow missing');
                    }
                    if (isset($case['images'])) {
                        $images = 0;
                        foreach (rt_dom($html)->getElementsByTagName('img') as $img) if (str_contains($img->getAttribute('class'), 'wp-image-' . $imageId)
                            && $img->getAttribute('alt') === 'Native library image alt') ++$images;
                        $c->ck('render-image', $images === $case['images'], 'duplicate does not render both library images with alt text');
                    }
                    $revert = ['input' => '{}', 'revert' => ['snapshot_sha256' => $w['snapshot_sha256'] ?? '']];
                    $undo = rt_call($cmd, $edit, 'revert', $rid, $revert);
                    $c->ck('undo', ($undo['ok'] ?? false) === true && ($undo['outcome'] ?? null) === 'reverted' && ($undo['restored'] ?? false) === true, rt_brief($undo));
                    $restored = rt_native_data_rows($pid);
                    if (in_array('native_undo_drift', $kinds, true)) $restored[0] .= ' ';
                    rt_native_undo_hold($c, $originalData, $restored, $originalRows, rt_raw_rows($pid, $skip), $originalPost, rt_raw_post($pid));
                    $c->ck('undo-fingerprint', BuilderDocumentFingerprint::ofPost($pid, ElementorDocument::DESCRIPTOR_KEYS) === $fp, 'undo fingerprint differs');
                    $again = rt_call($cmd, $edit, 'revert', $rid, $revert);
                    $c->ck('undo-twice', ($again['ok'] ?? false) === true && ($again['outcome'] ?? null) === 'already_reverted', rt_brief($again));
                }
            }
            rt_native_report($case['name'], $c, $out);
        }

        if ($unavailable) return $out;

        // A stored rewrite must be refused and restored, never counted as an applied edit.
        $c = new RtChecks();
        $rid = wp_generate_uuid4();
        $fp = BuilderDocumentFingerprint::ofPost($pid, ElementorDocument::DESCRIPTOR_KEYS);
        $input = json_encode(['post_id' => $pid, 'base_fingerprint' => $fp, 'operations' => [$cases[0]['op']]], JSON_THROW_ON_ERROR);
        $more = ['input' => $input, 'allowed_draft_ids' => [$pid]];
        $pre = rt_call($cmd, $edit, 'precheck', $rid, $more);
        if ($c->ck('precheck', ($pre['ok'] ?? false) === true, rt_brief($pre))) {
            $fired = 0;
            $rewrite = static function ($value) use (&$fired) {
                if (!is_string($value)) return $value;
                $value = str_replace('Native edited heading', 'Native rewritten heading', $value, $n);
                $fired += $n;
                return $value;
            };
            add_filter('sanitize_post_meta__elementor_data', $rewrite, 10, 1);
            try { $w = rt_call($cmd, $edit, 'write', $rid, $more + ['expected' => rt_digests($pre)]); }
            finally { remove_filter('sanitize_post_meta__elementor_data', $rewrite, 10); }
            $c->ck('rewrite-fired', $fired > 0, 'native refusal scenario reached no Elementor save');
            if ($c->ck('refused', ($w['ok'] ?? true) === false && ($w['code'] ?? null) === 'verify_mismatch' && ($w['restored'] ?? false) === true, rt_brief($w))) ++$out['refused'];
            $c->ck('refusal-data-bytes', rt_native_data_rows($pid) === $originalData, 'refused write did not restore original tree bytes');
            $why = rt_native_meta_diff($originalRows, rt_raw_rows($pid, $skip));
            $c->ck('refusal-meta-bytes', $why === null, (string) $why);
            $c->ck('refusal-post-bytes', rt_raw_post($pid) === $originalPost, 'refused write did not restore original post bytes');
        }
        rt_native_report('refusal', $c, $out);
    } catch (Throwable $e) {
        $c = new RtChecks(); $c->ck('native-edits', false, get_class($e) . ': ' . $e->getMessage()); rt_native_report('edits', $c, $out);
    } finally { rt_trash($pid); }
    return $out;
}

/** Every native boot ends with its own verdict; legacy checks use a fresh ordinary boot. */
function rt_native_phase(int $principal, string $inventory, array $plants): void
{
    $context = ['method' => 'rest_request_before_wp_load', 'rest_request' => defined('REST_REQUEST') && REST_REQUEST === true,
        'wp_admin' => is_admin(), 'should_optimize_controls' => null, 'registration_checks' => 0];
    $discovery = ['widgets' => 0, 'controls' => 0, 'unavailable' => 0, 'failed' => 0, 'alignment' => ''];
    $plantChecks = new RtChecks();
    $caseNames = array_column(rt_native_cases('(max-width:1024px)'), 'name');
    foreach ($plants as $name => $kinds) foreach ($kinds as $kind) {
        $valid = $kind === 'native_discovery_drift' ? $name === 'heading'
            : in_array($name, $caseNames, true) && in_array($kind, ['native_css_missing', 'native_text_missing', 'native_stored_drift', 'native_undo_drift', 'native_rejected_edit'], true);
        $plantChecks->ck('plant', $valid, 'native plant has no applicable target: ' . $kind . '@' . $name);
    }
    try {
        if (!$context['rest_request'] || $context['wp_admin']) throw new RuntimeException('native phase did not boot in REST context');
        $names = rt_native_inventory($inventory, ELEMENTOR_VERSION);
        $offered = array_diff(array_keys(Plugin::$instance->breakpoints->get_breakpoints()), ['desktop']);
        $active = array_keys(Plugin::$instance->breakpoints->get_active_breakpoints());
        if (array_diff($offered, $active) !== []) throw new RuntimeException('native oracle requires every offered breakpoint active');
        $discovery = rt_native_discovery($names, $context, $plants);
    } catch (Throwable $e) { ++$discovery['failed']; rt_out('FAIL [' . ELEMENTOR_VERSION . ' containers native-discovery] ' . $e->getMessage()); }
    if ($discovery['unavailable'] > 0) {
        $plantChecks->ck('plants', $plants === [], 'native edit plants require available native controls');
        $edits = rt_native_edits($principal, $plants, 'right', true);
        rt_native_report('plants', $plantChecks, $edits);
        $failed = $discovery['failed'] + $edits['failed'];
        rt_out(sprintf('NATIVE_SUMMARY elementor=%s layout=containers mode=unavailable widgets=%d controls=0 unavailable=%d edits=%d ops=- css=0 texts=0 refused=%d registration_checks=%d failed=%d',
            ELEMENTOR_VERSION, $discovery['widgets'], $discovery['unavailable'], $edits['edits'], $edits['refused'], $context['registration_checks'], $failed));
        if ($failed > 0) { rt_out('RESULT FAIL'); exit(1); }
        rt_out('RESULT OK');
        exit(0);
    }
    $edits = rt_native_edits($principal, $plants, $discovery['alignment']);
    rt_native_report('plants', $plantChecks, $edits);
    $coverage = new RtChecks();
    $coverage->ck('widgets', $discovery['widgets'] > 0 && $discovery['controls'] > 0, 'native discovery ran no widgets or controls');
    $coverage->ck('edits', $edits['edits'] > 0, 'native phase ran no edit');
    foreach (array_diff(['set_native', 'add_native', 'duplicate', 'move', 'remove'], $edits['ops']) as $op) $coverage->ck('operations', false, 'native phase applied no ' . $op);
    $coverage->ck('css', $edits['css'] > 0, 'native phase found no declaration');
    $coverage->ck('texts', $edits['texts'] > 0, 'native phase found no rendered text');
    $coverage->ck('refusal', $edits['refused'] > 0, 'native phase ran no refusal');
    rt_native_report('coverage', $coverage, $edits);
    $ops = array_values(array_unique($edits['ops'])); sort($ops);
    $failed = $discovery['failed'] + $edits['failed'];
    rt_out(sprintf('NATIVE_SUMMARY elementor=%s layout=containers mode=available widgets=%d controls=%d edits=%d ops=%s css=%d texts=%d refused=%d registration_checks=%d failed=%d',
        ELEMENTOR_VERSION, $discovery['widgets'], $discovery['controls'], $edits['edits'], implode(',', $ops), $edits['css'], $edits['texts'], $edits['refused'], $context['registration_checks'], $failed));
    rt_out('RESULT ' . ($failed === 0 ? 'OK' : 'FAIL'));
    exit($failed === 0 ? 0 : 1);
}
