<?php
declare(strict_types=1);

// Load the harness's check and CSS helpers without booting WordPress.
$source = file_get_contents(__DIR__ . '/harness.php');
$boundary = strpos($source, "// Arguments\n");
if ($boundary === false) {
    fwrite(STDERR, "FAIL native helper boundary missing\n");
    exit(1);
}
eval(substr($source, 5, $boundary - 5));
if (!is_file(__DIR__ . '/native.php')) {
    fwrite(STDERR, "FAIL native phase module missing\n");
    exit(1);
}
require_once __DIR__ . '/native.php';
define('ABSPATH', __DIR__ . '/');
define('WPMGR_AGENT_DIR', realpath(__DIR__ . '/../../apps/agent') . '/');
require_once WPMGR_AGENT_DIR . 'includes/class-autoloader.php';
spl_autoload_register(static function (string $class): void {
    $file = \WPMgr\Agent\Autoloader::resolve($class);
    if ($file !== null) require_once $file;
});
require_once __DIR__ . '/dump-widgets.php';

// A direct planted run exposes the phase's actual red lines and exit status.
if (isset($argv[1])) {
    if (str_starts_with($argv[1], 'finish-unavailable=')) {
        // Execute the unavailable phase's actual terminal block without WordPress.
        $native = file_get_contents(__DIR__ . '/native.php');
        $start = strpos($native, "        if (\$failed > 0) { rt_out('RESULT FAIL'); exit(1); }");
        $end = strpos($native, "    }\n    \$edits = rt_native_edits", $start === false ? 0 : $start);
        if ($start === false || $end === false) { fwrite(STDERR, "FAIL native terminal boundary missing\n"); exit(2); }
        $failed = (int) substr($argv[1], strlen('finish-unavailable='));
        eval(substr($native, $start, $end - $start));
        fwrite(STDERR, "FAIL native unavailable phase returned to legacy setup\n");
        exit(1);
    }
    $c = new RtChecks();
    $kind = $argv[1];
    if (str_starts_with($kind, 'plant=native_') || str_starts_with($kind, 'native_')) {
        // Exercise the harness's actual token parser before the offline phase checks.
        $start = strpos($source, '$rtArgs   = [];');
        $end = strpos($source, "foreach (['elementor', 'wp', 'php', 'zip_sha256', 'stored', 'layout']", $start);
        if ($start === false || $end === false) { fwrite(STDERR, "FAIL native argument parser boundary missing\n"); exit(2); }
        eval(substr($source, $start, $end - $start));
        if (array_keys($rtPlants) !== ['set-title'] || count($rtPlants['set-title']) !== 1) { fwrite(STDERR, "FAIL native self-test plant target\n"); exit(2); }
        $kind = ['native_css_missing' => 'plant=declaration', 'native_text_missing' => 'plant=text', 'native_rejected_edit' => 'plant=rejection'][$rtPlants['set-title'][0]] ?? '';
    }
    switch ($kind) {
        case 'plant=declaration':
        case 'plant=text':
            $probe = ['css' => [['el' => '4ead001', 'sel' => '.elementor-heading-title', 'media' => null, 'decl' => ['font-size' => '37px']]], 'texts' => ['Native title']];
            $css = '.elementor-element-4ead001 .elementor-heading-title{font-size:37px;}';
            $html = '<h2>Native title</h2>';
            if ($kind === 'plant=declaration') $css = str_replace('37px', '38px', $css);
            else $html = '<h2>Other title</h2>';
            rt_native_render_hold($c, $probe, $html, $css);
            break;
        case 'plant=rejection': rt_native_applied($c, ['ok' => false, 'code' => 'builder_save_refused']); break;
        default: fwrite(STDERR, "FAIL native self-test unknown plant\n"); exit(2);
    }
    foreach ($c->fails as $failure) fwrite(STDERR, 'FAIL [offline native] ' . $failure . "\n");
    exit($c->fails === [] ? 0 : 1);
}

$pass = 0;
$failed = 0;
function n09_assert(bool $ok, string $why): void { if (!$ok) throw new RuntimeException($why); }
function n09_reject(callable $run, string $why): void
{
    try { $run(); } catch (RuntimeException $e) {
        n09_assert(str_contains($e->getMessage(), $why), 'wrong refusal: ' . $e->getMessage());
        return;
    }
    throw new RuntimeException('invalid input accepted');
}

class N09Plugin
{
    public static object $instance;
}

function n09_runtime(array $controls, array $active = ['mobile', 'tablet']): \WPMgr\Agent\Abilities\Builders\ElementorRuntime
{
    if (!defined('ELEMENTOR_VERSION')) define('ELEMENTOR_VERSION', '3.35.9');
    if (!defined('ELEMENTOR_PATH')) define('ELEMENTOR_PATH', __DIR__ . '/fixtures/elementor/');
    require_once ELEMENTOR_PATH . 'includes/widgets/heading.php';
    $heading = new \Elementor\Widget_Roundtrip_Heading($controls);
    N09Plugin::$instance = (object) [
        'widgets_manager' => new class($heading) {
            public function __construct(private object $heading) {}
            public function get_widget_types(?string $name = null): array|object|null
            {
                return $name === null ? ['heading' => $this->heading] : ($name === 'heading' ? $this->heading : null);
            }
        },
        'breakpoints' => new class($active) {
            public function __construct(private array $active) {}
            public function get_active_breakpoints(): array { return array_fill_keys($this->active, new \stdClass()); }
        },
    ];
    return new \WPMgr\Agent\Abilities\Builders\ElementorRuntime(N09Plugin::class);
}

$test = static function (string $name, callable $run) use (&$pass, &$failed): void {
    try { $run(); ++$pass; echo "ok   native $name\n"; }
    catch (Throwable $e) { ++$failed; fwrite(STDERR, "FAIL native $name: {$e->getMessage()}\n"); }
};
$control = ['id' => 'title', 'type' => 'text', 'options' => [], 'responsive' => null, 'group' => null, 'fields' => []];
$live = ['title' => ['type' => 'text', 'options' => [], 'default' => '', 'responsive' => null, 'group' => null]];
$test('REST context precedes WordPress and leaves admin context off', static function (): void {
    rt_native_boot_context();
    n09_assert(defined('REST_REQUEST') && REST_REQUEST === true, 'REST_REQUEST missing');
    n09_assert(!defined('WP_ADMIN'), 'native request became an admin request');
});
$test('optimised stacks cannot be certified as a native REST read', static function (): void {
    eval('namespace Elementor\\Core\\Frontend; class Performance { public static bool $optimized = false; public static function should_optimize_controls(): bool { return self::$optimized; } }');
    $context = ['registration_checks' => 0];
    rt_native_registration_check('3.35.9', $context);
    n09_assert($context['should_optimize_controls'] === false && $context['registration_checks'] > 0, 'registration flag not recorded');
    \Elementor\Core\Frontend\Performance::$optimized = true;
    try { n09_reject(static function () use (&$context): void { rt_native_registration_check('3.35.9', $context); }, 'optimised'); }
    finally { \Elementor\Core\Frontend\Performance::$optimized = false; }
});
$test('matching descriptors pass', static function () use ($control, $live): void {
    n09_assert(rt_native_compare([$control], [], $live) === [], 'matching descriptor differs');
});
foreach (['type', 'options', 'responsive', 'group', 'return_value'] as $field) {
    $test("$field drift is reported and fatal", static function () use ($field, $control, $live): void {
        $bad = $live;
        $bad['title'][$field] = $field === 'options' ? ['invented'] : 'invented';
        $diffs = rt_native_compare([$control], [], $bad);
        n09_assert(count($diffs) === 1 && $diffs[0]['allowed'] === null && str_contains($diffs[0]['path'], $field), 'drift escaped');
    });
}
$test('all simultaneous differences are reported', static function () use ($control, $live): void {
    $live['title']['type'] = 'number';
    $live['title']['options'] = ['other'];
    $live['unexpected'] = $live['title'];
    $diffs = rt_native_compare([$control, $control + ['unused' => true]], [], $live);
    n09_assert(count(array_filter($diffs, static fn ($d) => $d['allowed'] === null)) >= 3, 'differences stopped at first mismatch');
});
$test('missing and extra control ids are fatal', static function () use ($control, $live): void {
    n09_assert(rt_native_compare([$control], [], [])[0]['allowed'] === null, 'missing id escaped');
    n09_assert(rt_native_compare([], [], $live)[0]['allowed'] === null, 'extra id escaped');
});
$test('denylist omissions are explicit and do not cover nearby keys', static function () use ($control): void {
    $denied = $control; $denied['id'] = '_css_classes';
    $nearby = $control; $nearby['id'] = 'css_classes';
    n09_assert(rt_native_compare([$denied], [], [])[0]['allowed'] === 'agent_denylist', 'denylist not documented');
    n09_assert(rt_native_compare([$nearby], [], [])[0]['allowed'] === null, 'nearby control excused');
});
$test('responsive devices compare as sets and bounds normalization is explicit', static function () use ($control, $live): void {
    $control['responsive'] = ['min' => null, 'max' => 'desktop', 'devices' => ['desktop', 'tablet'], 'source' => 'duplicates'];
    $live['title']['responsive'] = ['min' => null, 'max' => null, 'devices' => ['tablet', 'desktop']];
    $diffs = rt_native_compare([$control], [], $live);
    n09_assert(count($diffs) === 1 && $diffs[0]['allowed'] === 'responsive_bounds_normalized', 'bounds normalization missing');
    $live['title']['responsive']['devices'] = ['desktop'];
    n09_assert(array_filter(rt_native_compare([$control], [], $live), static fn ($d) => $d['allowed'] === null) !== [], 'lost tablet escaped');
});
$test('single-control responsive windows cannot lose a device', static function () use ($control, $live): void {
    $control['responsive'] = ['min' => null, 'max' => null, 'devices' => ['desktop', 'tablet', 'mobile'], 'source' => 'single_control'];
    $live['title']['responsive'] = ['min' => null, 'max' => null, 'devices' => ['mobile', 'tablet', 'desktop']];
    n09_assert(rt_native_compare([$control], [], $live) === [], 'single window differs');
    unset($live['title']['responsive']['devices'][1]);
    n09_assert(rt_native_compare([$control], [], $live)[0]['allowed'] === null, 'single window lost tablet');
});
$test('group family prefix and activator each compare', static function () use ($control, $live): void {
    $control['group'] = ['family' => 'typography', 'prefix' => 'typography_', 'activator' => 'typography_typography'];
    $live['title']['group'] = $control['group'];
    foreach (array_keys($control['group']) as $key) {
        $bad = $live; $bad['title']['group'][$key] = 'wrong';
        n09_assert(rt_native_compare([$control], [], $bad)[0]['allowed'] === null, "$key escaped");
    }
});
$test('repeater field drift is fatal', static function () use ($control, $live): void {
    $outer = $control; $outer['id'] = 'items'; $outer['type'] = 'repeater'; $outer['fields'] = [$control];
    $live = ['items' => ['type' => 'repeater', 'options' => [], 'responsive' => null, 'group' => null, 'fields' => (object) $live]];
    n09_assert(rt_native_compare([$outer], [], $live) === [], 'repeater match failed');
    $live['items']['fields']->title['type'] = 'textarea';
    n09_assert(rt_native_compare([$outer], [], $live)[0]['allowed'] === null, 'field drift escaped');
});
$test('only named oracle omissions are excused', static function () use ($live): void {
    $skipped = [['id' => 'title', 'reason' => 'incomplete_group_metadata']];
    n09_assert(rt_native_compare([], $skipped, $live)[0]['allowed'] === 'oracle_incomplete_group_metadata', 'oracle omission not reported');
    $skipped[0]['reason'] = 'new_reason';
    n09_assert(rt_native_compare([], $skipped, $live)[0]['allowed'] === null, 'new skip reason excused');
});
$test('the actual discovery matches the oracle on chains single mode and on-switches', static function (): void {
    $active = ['mobile', 'tablet', 'tablet_extra', 'laptop', 'widescreen', 'mobile_extra'];
    $raw = [
        'title' => ['type' => 'text'],
        'header_size' => ['type' => 'select', 'options' => ['h2' => 'Heading 2', 'h1' => 'Heading 1']],
        'width' => ['type' => 'slider', 'responsive' => ['max' => 'desktop']],
        'width_laptop' => ['type' => 'slider', 'parent' => 'width', 'responsive' => ['max' => 'laptop']],
        'width_tablet_extra' => ['type' => 'slider', 'parent' => 'width_laptop', 'responsive' => ['max' => 'tablet_extra']],
        'align' => ['type' => 'choose', 'options' => ['left' => [], 'right' => []], 'is_responsive' => true],
        'hide_tablet' => ['type' => 'switcher', 'return_value' => 'hidden-tablet'],
        'typography_typography' => ['type' => 'popover_toggle', 'return_value' => 'custom', 'groupPrefix' => 'typography_'],
        'typography_font_size' => ['type' => 'slider', 'is_responsive' => true, 'groupType' => 'typography', 'groupPrefix' => 'typography_', 'condition' => ['typography_typography' => 'custom']],
        'typography_font_weight' => ['type' => 'select', 'options' => ['' => 'Default', 700 => 'Bold'], 'groupType' => 'typography', 'groupPrefix' => 'typography_', 'condition' => ['typography_typography' => 'custom']],
        '_css_classes' => ['type' => 'text'],
    ];
    $api = n09_runtime($raw, $active);
    n09_assert($api->widgetIsCore('heading'), 'heading is not registered from the core widget directory');
    $d = new \WPMgr\Agent\Abilities\Builders\ElementorWidgetDiscovery($api);
    $catalogue = $d->catalogue((object) [])['output'];
    n09_assert($catalogue['widgets'][0]['usable'], 'actual catalogue refused heading');
    $ids = [];
    foreach ($d->schema((object) ['widget' => 'heading'])['output']['tabs'] as $tab) foreach ($tab['sections'] as $section) foreach ($section['controls'] as $c) $ids[] = $c['id'];
    $live = rt_native_read_controls($d, 'heading', $ids);
    $manager = new class { public function get_control_groups(string $family): object {
        return new class { public function get_options(): array { return ['popover' => ['starter_name' => 'typography']]; } };
    } };
    $oracle = rt_widget_dump_controls($raw, $manager, array_merge(['desktop'], $api->activeBreakpoints()));
    $diffs = rt_native_compare($oracle['controls'], $oracle['controls_skipped'], $live);
    n09_assert(array_filter($diffs, static fn ($d) => $d['allowed'] === null) === [], json_encode($diffs));
    n09_assert(!isset($live['width_laptop']) && in_array('tablet_extra', $live['width']['responsive']['devices'], true), 'chain not folded');
    n09_assert($live['typography_typography']['return_value'] === 'custom' && $live['typography_font_size']['group']['activator'] === 'typography_typography', 'group on-switch lost');
});
$test('inventory selects the exact pinned widget set and refuses missing input', static function (): void {
    $file = __DIR__ . '/../../apps/api/internal/elementornative/core_widgets.json';
    $pins = json_decode(file_get_contents($file), true, 512, JSON_THROW_ON_ERROR);
    foreach ($pins['elementor_versions'] as $version) {
        $want = array_column(array_filter($pins['widgets'], static fn ($w) => in_array($version, $w['versions'], true)), 'name'); sort($want);
        n09_assert(rt_native_inventory($file, $version) === $want && $want !== [], 'pin inventory differs');
    }
    n09_reject(static fn () => rt_native_inventory(__DIR__ . '/missing-native-inventory.json', '3.35.9'), 'missing');
    n09_reject(static fn () => rt_native_inventory($file, '0.0.1'), 'pin');
});
$test('native ability envelopes carry the actual command snapshot contracts', static function (): void {
    [$create, $edit] = rt_native_entries();
    $createEntry = json_decode($create[0], true, 512, JSON_THROW_ON_ERROR);
    $editEntry = json_decode($edit[0], true, 512, JSON_THROW_ON_ERROR);
    n09_assert($createEntry['snapshot'] === 'created_post_trash' && $createEntry['name'] === 'wpmgr/page-create', 'create entry differs');
    n09_assert($editEntry['snapshot'] === \WPMgr\Agent\Abilities\Builders\BuilderPageEdit::SNAPSHOT && $editEntry['name'] === 'wpmgr/page-edit', 'edit entry differs');
    n09_assert(hash('sha256', $create[0]) === $create[1] && hash('sha256', $edit[0]) === $edit[1], 'entry digest differs');
});

class N09Discovery
{
    public array $calls = [];
    public string $fault = '';
    public function schema(object $input): array
    {
        $this->calls[] = $input->controls;
        $controls = [];
        foreach ($input->controls as $id) $controls[$id] = ['type' => 'text', 'options' => [], 'default' => '', 'responsive' => null, 'group' => null, 'padding' => str_repeat('x', 13000)];
        if ($this->fault === 'refusal') return ['refusal' => ['code' => 'widget_schema_unreadable']];
        if ($this->fault === 'missing') unset($controls[$input->controls[0]]);
        if ($this->fault === 'extra') $controls['unexpected'] = reset($controls);
        if ($this->fault === 'huge') $controls[$input->controls[0]]['padding'] = str_repeat('x', 40000);
        $out = ['builder' => 'elementor', 'builder_version' => '3.35.9', 'mode' => 'controls', 'widget' => $input->widget, 'usable' => true, 'atomic' => false, 'truncated' => false, 'controls' => (object) $controls];
        while (strlen(json_encode($out)) > 32768 && $this->fault !== 'huge') {
            array_pop($controls); $out['controls'] = (object) $controls; $out['truncated'] = true;
        }
        return ['output' => $out];
    }
}
$test('truncated batches split until whole controls fit', static function (): void {
    $d = new N09Discovery(); $ids = ['a', 'b', 'c', 'd', 'e'];
    $out = rt_native_read_controls($d, 'heading', $ids);
    n09_assert(array_keys($out) === $ids && count($d->calls) > 1, 'batch lost control');
    foreach ($d->calls as $batch) n09_assert(count($batch) <= 128, 'input count cap exceeded');
});
$test('large id lists are bounded before discovery calls', static function (): void {
    $d = new N09Discovery(); $ids = array_map(static fn ($i) => 'c' . $i, range(0, 130));
    n09_assert(count(rt_native_read_controls($d, 'heading', $ids)) === count($ids), 'large batch lost controls');
    foreach ($d->calls as $batch) n09_assert(count($batch) <= 128, 'oversized batch');
});
foreach (['missing', 'extra', 'huge', 'refusal'] as $fault) {
    $test("$fault controls answer cannot pass", static function () use ($fault): void {
        $d = new N09Discovery(); $d->fault = $fault;
        n09_reject(static fn () => rt_native_read_controls($d, 'heading', ['title']), 'controls');
    });
}
$test('a single truncated descriptor fails loudly', static function (): void {
    $d = new class { public function schema(object $input): array { return ['output' => ['builder' => 'elementor', 'builder_version' => '3.35.9', 'mode' => 'controls', 'widget' => 'heading', 'usable' => true, 'atomic' => false, 'truncated' => true, 'controls' => (object) []]]; } };
    n09_reject(static fn () => rt_native_read_controls($d, 'heading', ['title']), 'single control');
});

$probe = ['css' => [['el' => '4ead001', 'sel' => '.elementor-heading-title', 'media' => null, 'decl' => ['font-size' => '37px']],
    ['el' => '4ead001', 'sel' => '.elementor-heading-title', 'media' => '(max-width:1024px)', 'decl' => ['font-size' => '29px']]], 'texts' => ['Native title']];
$css = '.elementor-element-4ead001 .elementor-heading-title{font-size:37px;} @media(max-width:1024px){.elementor-element-4ead001 .elementor-heading-title{font-size:29px;}}';
$html = '<div><h2>Native title</h2></div>';
$test('declarations text and tablet media hold together', static function () use ($probe, $css, $html): void {
    $c = new RtChecks(); $found = rt_native_render_hold($c, $probe, $html, $css);
    n09_assert($c->fails === [] && $found['css'] > 0 && $found['texts'] > 0, implode('; ', $c->fails));
});
foreach (['declaration', 'media', 'text', 'text only in styles', 'empty css', 'empty html', 'empty expectations'] as $fault) {
    $test("$fault plant turns render red", static function () use ($fault, $probe, $css, $html): void {
        if ($fault === 'declaration') $css = str_replace('37px', '38px', $css);
        if ($fault === 'media') $css = str_replace('1024px', '767px', $css);
        if ($fault === 'text') $html = '<h2>Another title</h2>';
        if ($fault === 'text only in styles') $html = '<style>.x::after{content:"Native title"}</style><h2>Other title</h2>';
        if ($fault === 'empty css') $css = '';
        if ($fault === 'empty html') $html = '';
        if ($fault === 'empty expectations') $probe = ['css' => [], 'texts' => []];
        $c = new RtChecks(); rt_native_render_hold($c, $probe, $html, $css);
        n09_assert($c->fails !== [], 'plant passed');
    });
}
$test('saved bytes preserve scalar types and key order', static function (): void {
    $tree = [['id' => 'abc1234', 'settings' => ['title' => 'Native / title', 'weight' => '700']]];
    $raw = json_encode($tree);
    n09_assert(rt_native_tree_bytes($tree, [$raw]) === null, 'honest bytes refused');
    n09_assert(rt_native_tree_bytes($tree, []) !== null, 'missing data passed');
    n09_assert(rt_native_tree_bytes($tree, [$raw, $raw]) !== null, 'duplicate data passed');
    $wrong = $tree; $wrong[0]['settings']['weight'] = 700;
    n09_assert(rt_native_tree_bytes($tree, [json_encode($wrong)]) !== null, 'scalar drift passed');
    $wrong[0]['settings'] = array_reverse($tree[0]['settings'], true);
    n09_assert(rt_native_tree_bytes($tree, [json_encode($wrong)]) !== null, 'key order drift passed');
});
$test('an Elementor rejection makes the positive edit check fail', static function (): void {
    foreach (['builder_would_change_layout', 'side_effect_detected', 'builder_save_refused'] as $code) {
        $c = new RtChecks(); rt_native_applied($c, ['ok' => false, 'code' => $code]);
        n09_assert($c->fails !== [], "$code passed");
    }
    $c = new RtChecks(); n09_assert(rt_native_applied($c, ['ok' => true, 'outcome' => 'applied']), 'valid write refused');
});
$test('undo checks exact original data metadata and post bytes', static function (): void {
    $data = ['[{"title":"Original \\/ title"}]'];
    $meta = [['_elementor_data', $data[0]], ['extra', null], ['extra', 'a\\b']];
    $post = ['post_title' => 'Title', 'post_modified' => '2026-10-13 10:00:00'];
    $c = new RtChecks(); rt_native_undo_hold($c, $data, $data, $meta, $meta, $post, $post);
    n09_assert($c->fails === [], 'honest undo refused');
    foreach (['data', 'meta', 'post', 'order', 'null'] as $fault) {
        $nowData = $data; $nowMeta = $meta; $nowPost = $post;
        if ($fault === 'data') $nowData[0] .= ' ';
        if ($fault === 'meta') $nowMeta[2][1] = 'ab';
        if ($fault === 'post') $nowPost['post_title'] = 'Other title';
        if ($fault === 'order') $nowMeta = array_reverse($meta);
        if ($fault === 'null') $nowMeta[1][1] = '';
        $c = new RtChecks(); rt_native_undo_hold($c, $data, $nowData, $meta, $nowMeta, $post, $nowPost);
        n09_assert($c->fails !== [], "$fault undo drift escaped");
    }
});
$test('operation checks independently require image copies moves removals and settings', static function (): void {
    $image = ['id' => '1a6e001', 'elType' => 'widget', 'settings' => ['image' => ['id' => '3', 'url' => 'https://example.com/image.png']], 'elements' => [], 'widgetType' => 'image'];
    $counter = ['id' => 'c0a0001', 'elType' => 'widget', 'settings' => ['title' => 'Counter'], 'elements' => [], 'widgetType' => 'counter'];
    $icon = ['id' => '1c0b001', 'elType' => 'widget', 'settings' => ['title_text' => 'Icon'], 'elements' => [], 'widgetType' => 'icon-box'];
    $heading = ['id' => '4ead001', 'elType' => 'widget', 'settings' => ['title' => 'Heading'], 'elements' => [], 'widgetType' => 'heading'];
    $before = [['id' => 'c0ffee1', 'elements' => [$heading, $image, $counter, $icon]], ['id' => 'c0ffee2', 'elements' => []]];
    foreach (rt_native_cases('(max-width:1024px)') as $case) {
        $after = $before;
        switch ($case['op']['op']) {
            case 'set_native': foreach ($case['op']['settings'] as $k => $v) $after[0]['elements'][0]['settings'][$k] = $v; break;
            case 'add_native': $after[1]['elements'][] = ['id' => 'b077001', 'widgetType' => 'button', 'settings' => $case['op']['settings'], 'elements' => []]; break;
            case 'duplicate': $copy = $image; $copy['id'] = 'aabbccd'; $after[1]['elements'][] = $copy; break;
            case 'move': unset($after[0]['elements'][2]); $after[1]['elements'][] = $counter; break;
            case 'remove': unset($after[0]['elements'][3]); break;
        }
        $c = new RtChecks(); rt_native_operation_hold($c, $case, $before, $after);
        n09_assert($c->fails === [], $case['name'] . ': ' . implode('; ', $c->fails));
        $c = new RtChecks(); rt_native_operation_hold($c, $case, $before, $before);
        n09_assert($c->fails !== [], $case['name'] . ' no-op escaped');
        if ($case['op']['op'] === 'duplicate') {
            $after[1]['elements'][0]['settings']['image']['url'] = 'https://example.com/other.png';
            $c = new RtChecks(); rt_native_operation_hold($c, $case, $before, $after);
            n09_assert($c->fails !== [], 'duplicate payload drift escaped');
        }
    }
});
$test('tablet alignment comes from live options and matches its CSS expectation', static function (): void {
    foreach ([[['start', 'center', 'end', 'justify'], 'center'], [['left', 'center', 'right', 'justify'], 'center'], [['end'], 'end'], [['right'], 'right']] as [$options, $want]) {
        $value = rt_native_alignment(['options' => $options]);
        n09_assert($value === $want && in_array($value, $options, true), 'alignment not offered');
        $case = array_column(rt_native_cases('(max-width:1024px)', $value), null, 'name')['set-tablet-align'];
        n09_assert($case['op']['settings']['align_tablet'] === $want && $case['css'][0]['decl']['text-align'] === $want, 'alignment CSS differs from edit');
    }
    n09_reject(static fn () => rt_native_alignment(['options' => []]), 'alignment');
});
$test('the old REST pin must refuse both skeleton and named schema reads', static function (): void {
    $refusal = ['refusal' => ['code' => 'elementor_controls_unavailable', 'detail' => 'this Elementor version builds widget settings only in the editor; native edits need a newer Elementor']];
    $c = new RtChecks(); rt_native_unavailable_hold($c, '3.20.4', $refusal, $refusal);
    n09_assert($c->fails === [], 'expected refusal failed');
    foreach (['modern pin', 'wrong code', 'named success', 'wrong detail'] as $fault) {
        $a = $refusal; $b = $refusal; $pin = '3.20.4';
        if ($fault === 'modern pin') $pin = '3.35.9';
        if ($fault === 'wrong code') $a['refusal']['code'] = 'native_settings_invalid';
        if ($fault === 'named success') $b = ['output' => ['mode' => 'controls']];
        if ($fault === 'wrong detail') $a['refusal']['detail'] = 'Other detail';
        $c = new RtChecks(); rt_native_unavailable_hold($c, $pin, $a, $b);
        n09_assert($c->fails !== [], "$fault escaped");
    }
});
$test('metadata failures identify row keys byte changes and order without loosening preservation', static function (): void {
    $before = [['_elementor_data', 'tree'], ['_elementor_version', 'version'], ['custom', 'value']];
    $after = [$before[2], $before[0], $before[1]];
    $why = rt_native_meta_diff($before, $after);
    n09_assert(is_string($why) && str_contains($why, '_elementor_data') && str_contains($why, '_elementor_version') && str_contains($why, 'order'), 'order diagnostics missing');
    $after = $before; $after[2][1] = 'different';
    $why = rt_native_meta_diff($before, $after);
    n09_assert(is_string($why) && str_contains($why, 'custom') && str_contains($why, 'sha256'), 'byte diagnostics missing');
    n09_assert(rt_native_meta_diff($before, $before) === null, 'honest metadata refused');
});
$test('an oracle failure preserves live discovery counts and the offered tablet alignment', static function (): void {
    $api = n09_runtime([
        'title' => ['type' => 'text'],
        'align' => ['type' => 'choose', 'options' => ['start' => [], 'center' => [], 'end' => [], 'justify' => []], 'is_responsive' => true],
    ]);
    $context = ['registration_checks' => 0];
    $calls = 0;
    $oracle = static function () use (&$calls): array { ++$calls; throw new RuntimeException('offline oracle failure'); };
    $out = rt_native_discovery(['heading'], $context, [], new \WPMgr\Agent\Abilities\Builders\ElementorWidgetDiscovery($api), $oracle);
    n09_assert($calls === 1 && $out['failed'] > 0, 'oracle failure was not reported');
    n09_assert($out['widgets'] === 1 && $out['controls'] === 2, 'live discovery evidence was lost');
    n09_assert($out['alignment'] === 'center', 'oracle failure hid the live alignment or chose a direction-dependent declaration');
    $case = array_column(rt_native_cases('(max-width:1024px)', $out['alignment']), null, 'name')['set-tablet-align'];
    n09_assert($case['op']['settings']['align_tablet'] === 'center', 'edit did not use the live choice');
});
$test('the native phase runs before legacy fixture and probe setup', static function () use ($source): void {
    $site = substr($source, strpos($source, '// The principal is the agent'));
    $native = strpos($site, "rt_native_phase(\$principal,");
    $fixtures = strpos($site, "// The media the fixtures name");
    $designed = strpos($site, "// The images the designed golden names");
    $probes = strpos($site, '$rtProbeSet = rt_probe_load();');
    n09_assert($native !== false && $fixtures !== false && $designed !== false && $probes !== false
        && $native < $fixtures && $native < $designed && $native < $probes, 'native boot still prepares the legacy fixtures and probes');
});
$test('the native baseline includes preserved metadata written by the initial render', static function (): void {
    if (!defined('ARRAY_A')) define('ARRAY_A', 'ARRAY_A');
    eval('namespace Elementor; class Plugin { public static object $instance; }');
    $GLOBALS['wpdb'] = new class {
        public string $postmeta = 'postmeta';
        public string $posts = 'posts';
        public string $last_error = '';
        public array $rows = [['_elementor_data', '[{"id":"heading"}]'], ['custom', null]];
        public function prepare(string $sql, ...$args): string { return $sql; }
        public function get_results(string $sql, $format): array {
            return array_map(static fn ($row) => ['meta_key' => $row[0], 'meta_value' => $row[1]], $this->rows);
        }
        public function get_col(string $sql): array { return array_column(array_filter($this->rows, static fn ($row) => $row[0] === '_elementor_data'), 1); }
        public function get_row(string $sql, $format): array { return ['post_title' => 'Native page', 'post_status' => 'draft']; }
    };
    \Elementor\Plugin::$instance = (object) ['frontend' => new class($GLOBALS['wpdb']) {
        public function __construct(private object $db) {}
        public function get_builder_content(int $id, bool $css): string {
            $this->db->rows[] = ['_elementor_migrations_state_d2a1', 'render state'];
            return '<h2>Native original heading</h2>';
        }
    }];
    $baseline = rt_native_baseline(37, ['_elementor_css']);
    n09_assert($baseline['meta'] === $GLOBALS['wpdb']->rows, 'baseline omitted the render migration row');
    n09_assert($baseline['data'] === ['[{"id":"heading"}]'], 'baseline changed stored tree bytes');
    $c = new RtChecks(); rt_native_undo_hold($c, $baseline['data'], $baseline['data'], $baseline['meta'], $baseline['meta'], $baseline['post'], $baseline['post']);
    n09_assert($c->fails === [], 'unchanged rendered baseline failed');
    $c = new RtChecks(); $changed = $baseline['meta']; $changed[2][1] = 'different state';
    rt_native_undo_hold($c, $baseline['data'], $baseline['data'], $baseline['meta'], $changed, $baseline['post'], $baseline['post']);
    n09_assert($c->fails !== [], 'migration metadata was excluded from byte checks');
});

$n09ImageDir = '';
$n09ImageCalls = [];
function wp_set_current_user(int $id): void { $GLOBALS['n09ImageCalls'][] = ['user', $id]; }
function wp_upload_dir(): array { return ['path' => $GLOBALS['n09ImageDir'], 'error' => false]; }
function wp_insert_attachment(array $post, string $file, int $parent, bool $error): int {
    $GLOBALS['n09ImageCalls'][] = ['insert', $post, $file];
    return 710;
}
function is_wp_error($value): bool { return false; }
function wp_generate_attachment_metadata(int $id, string $file): array { $GLOBALS['n09ImageCalls'][] = ['generate', $id]; return ['file' => $file]; }
function wp_update_attachment_metadata(int $id, array $meta): void { $GLOBALS['n09ImageCalls'][] = ['metadata', $id]; }
function update_post_meta(int $id, string $key, string $value): void { $GLOBALS['n09ImageCalls'][] = ['alt', $id, $key, $value]; }
$test('the native library image uses the id WordPress allocates', static function () use (&$n09ImageDir, &$n09ImageCalls): void {
    $n09ImageDir = sys_get_temp_dir() . '/native-image-' . bin2hex(random_bytes(8));
    n09_assert(mkdir($n09ImageDir), 'temporary image directory unavailable');
    $n09ImageCalls = [];
    try {
        n09_assert(rt_native_image() === 710, 'native image assumes an attachment id');
        $insert = array_values(array_filter($n09ImageCalls, static fn ($call) => $call[0] === 'insert'))[0];
        n09_assert(!array_key_exists('import_id', $insert[1]) && !array_key_exists('ID', $insert[1]), 'native image requests a fixed post id');
        foreach (['generate', 'metadata', 'alt'] as $kind) {
            $call = array_values(array_filter($n09ImageCalls, static fn ($call) => $call[0] === $kind))[0];
            n09_assert($call[1] === 710, $kind . ' did not use the returned attachment id');
        }
        n09_assert(is_file($insert[2]) && getimagesize($insert[2])[0] === 320, 'library fixture is not a real image');
    } finally {
        if (is_file($n09ImageDir . '/native-roundtrip.png')) unlink($n09ImageDir . '/native-roundtrip.png');
        rmdir($n09ImageDir);
    }
});
$test('non-native phases require their legacy operations without demanding native writes', static function (): void {
    $ops = [];
    foreach ([__DIR__ . '/../../apps/agent/tests/fixtures/ability-run/elementor-edit-cases.json', __DIR__ . '/edit-cases-extra.json'] as $file) {
        $cases = json_decode(file_get_contents($file), true, 512, JSON_THROW_ON_ERROR);
        foreach ($cases['cases'] as $case) foreach ($case['ops'] as $op) $ops[] = $op['op'];
    }
    n09_assert(rt_edit_missing_ops($ops) === [], 'legacy coverage demands native operations');
    n09_assert(in_array('move', rt_edit_missing_ops(array_values(array_diff($ops, ['move']))), true), 'legacy operation loss passed');
});
echo "native_test: $pass passed, $failed failed\n";
exit($failed === 0 ? 0 : 1);
