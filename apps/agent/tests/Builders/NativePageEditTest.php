<?php

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\Builders\BuilderContract;
use WPMgr\Agent\Abilities\Builders\ElementorClassicMapper;
use WPMgr\Agent\Abilities\Builders\IdSeed;
use WPMgr\Agent\Abilities\Builders\LayoutOps;
use WPMgr\Agent\Abilities\Builders\LeafPolicy;
use WPMgr\Agent\Abilities\Builders\NativeWidgetPolicy;
use WPMgr\Agent\Abilities\Builders\NativeSettingsValidator;
use WPMgr\Agent\Abilities\Builders\PageEditValidator;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\LayoutOps
 * @covers \WPMgr\Agent\Abilities\Builders\NativeWidgetPolicy
 * @covers \WPMgr\Agent\Abilities\Builders\PageEditValidator
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorClassicMapper
 */
final class NativePageEditTest extends TestCase
{
    private FakeElementorApi $api;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        Functions\when('wp_kses_post')->alias(static fn ($s) => $s);
        $this->api = new FakeElementorApi();
        $this->api->widgetTypes = ['counter', 'heading', 'vendor-widget', 'html', 'e-heading', 'premium'];
        $this->api->coreWidgets = ['counter', 'heading', 'html', 'e-heading', 'premium'];
        foreach ($this->api->widgetTypes as $type) {
            $this->api->widgetCatalogue[] = ['name' => $type, 'title' => ucfirst($type), 'categories' => ['basic'], 'pro' => $type === 'premium', 'atomic' => $type === 'e-heading'];
            $this->api->widgetControls[$type] = [
                'title' => ['type' => 'text', 'label' => 'Title'],
                'number' => ['type' => 'number'],
                'image' => ['type' => 'media'],
                'icon' => ['type' => 'icons'],
                'color' => ['type' => 'color', 'responsive' => true],
                'rows' => ['type' => 'repeater', 'fields' => [['name' => 'text', 'type' => 'text']]],
            ];
        }
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    private static function widget(string $type = 'counter', array $settings = ['title' => 'Hello'], string $id = 'aaaaaaa'): array
    {
        return ['id' => $id, 'elType' => 'widget', 'widgetType' => $type, 'isInner' => false, 'settings' => $settings, 'elements' => []];
    }

    private static function tree(): array
    {
        return [['id' => '1111111', 'elType' => 'container', 'isInner' => false, 'settings' => [], 'elements' => [self::widget(), self::widget('heading', ['title' => 'Heading'], 'bbbbbbb')]],
            ['id' => '2222222', 'elType' => 'container', 'isInner' => false, 'settings' => [], 'elements' => []]];
    }

    private static function parse(array $ops): array
    {
        return PageEditValidator::parse(json_encode(['post_id' => 41, 'base_fingerprint' => str_repeat('a', 64), 'operations' => $ops]));
    }

    private function apply(array $ops, ?array $tree = null, array $media = []): array
    {
        $parsed = self::parse($ops);
        $this->assertArrayHasKey('input', $parsed, json_encode($parsed));
        $tree = $tree ?? self::tree();
        $policy = new NativeWidgetPolicy($this->api);
        $rule = PageEditValidator::againstPage($parsed['input'], ElementorClassicMapper::project($tree, $policy)->nodes(), ['operations' => BuilderContract::OPS]);
        if ($rule !== null) {
            return $rule;
        }
        return LayoutOps::apply($tree, $parsed['input']['operations'], new IdSeed('native-edit'), $media, true, null, $policy, ['mobile', 'tablet']);
    }

    public function test_real_size_controls_use_only_operation_documents(): void
    {
        RealNativeControls::install($this->api);
        foreach (['counter' => 'title', 'button' => 'text'] as $type => $key) {
            $tree = self::tree();
            $tree[0]['elements'][0] = self::widget($type, [$key => 'Short title']);
            $policy = new NativeWidgetPolicy($this->api);
            $full = $policy->schema($type, [], null, ['mobile', 'tablet']);
            $this->assertNotNull($full);
            $this->assertGreaterThan(32768, strlen(json_encode($full->document)));
            $this->assertTrue(NativeSettingsValidator::validateWritten($full, [], [$key => 'Title'], [$key => 'Title'], ['mobile', 'tablet'])['ok']);
            foreach ([['op' => 'set_native', 'ref' => 'aaaaaaa', 'settings' => [$key => 'New title']],
                ['op' => 'add_native', 'into' => '2222222', 'widget' => $type, 'settings' => [$key => 'New title']],
                ['op' => 'duplicate', 'ref' => 'aaaaaaa', 'into' => '2222222'],
                ['op' => 'move', 'ref' => '1111111', 'after' => '2222222']] as $op) {
                $r = $this->apply([$op], $tree);
                $this->assertArrayHasKey('tree', $r, json_encode($r));
                if ($op['op'] === 'set_native' || $op['op'] === 'add_native') {
                    $this->assertSame([$key], array_keys($r['schemas'][$type]['controls']));
                }
            }
            $this->assertSame('Short title', $policy->summary($type, [$key => 'Short title']));
        }
        $tree = self::tree();
        $tree[0]['elements'][1] = self::widget('counter', ['ending_number' => '10'], 'bbbbbbb');
        $r = $this->apply([
            ['op' => 'set_native', 'ref' => 'aaaaaaa', 'settings' => ['title' => 'New']],
            ['op' => 'set_native', 'ref' => 'bbbbbbb', 'settings' => ['ending_number' => '20']],
        ], $tree);
        $this->assertSame(['ending_number', 'title'], array_keys($r['schemas']['counter']['controls']));
    }

    public function test_set_native_preserves_envelope_and_refuses_inner_flag(): void
    {
        $tree = self::tree();
        $tree[0]['elements'][0]['extra_stored_key'] = ['legacy' => true];
        $expected = $tree[0]['elements'][0];
        $expected['settings']['title'] = 'New';
        $op = ['op' => 'set_native', 'ref' => 'aaaaaaa', 'settings' => ['title' => 'New']];
        $r = $this->apply([$op], $tree);
        $this->assertSame($expected, $r['results'][0]['result_node'] ?? $r);
        foreach ([true, '1', 'false', 0] as $flag) {
            $tree[0]['elements'][0]['isInner'] = $flag;
            $this->assertSame('native_node_shape', $this->apply([$op], $tree)['code'] ?? null);
        }
        foreach ([false, ''] as $flag) {
            $tree[0]['elements'][0]['isInner'] = $flag;
            $this->assertArrayHasKey('tree', $this->apply([$op], $tree));
        }
    }

    public function test_native_html_id_controls_are_refused_by_key(): void
    {
        $this->api->widgetControls['counter']['_element_id'] = ['type' => 'text'];
        $this->api->widgetControls['counter']['button_css_id'] = ['type' => 'text'];
        foreach (['_element_id', 'button_css_id'] as $key) {
            foreach ([['op' => 'set_native', 'ref' => 'aaaaaaa', 'settings' => [$key => 'arbitrary id']],
                ['op' => 'add_native', 'into' => '2222222', 'widget' => 'counter', 'settings' => [$key => 'arbitrary id']]] as $op) {
                $r = $this->apply([$op]);
                $this->assertSame('native_settings_invalid', $r['code'] ?? null, json_encode($r));
                $this->assertSame($key, $r['failures'][0]['key']);
                $this->assertArrayHasKey($key, $r['schema']['controls']);
            }
        }
        $this->assertArrayHasKey('tree', $this->apply([['op' => 'set_native', 'ref' => 'aaaaaaa', 'settings' => ['title' => 'A title']]]));
    }

    public function test_column_moves_keep_destination_section_inner_flag(): void
    {
        foreach ([[false, true], ['', '1']] as [$outer, $inner]) {
            $tree = [
                ['id' => '1111111', 'elType' => 'section', 'isInner' => $outer, 'settings' => [], 'elements' => [
                    ['id' => 'aaaaaaa', 'elType' => 'column', 'isInner' => $outer, 'settings' => [], 'elements' => []]]],
                ['id' => '2222222', 'elType' => 'section', 'isInner' => $outer, 'settings' => [], 'elements' => [
                    ['id' => 'bbbbbbb', 'elType' => 'column', 'isInner' => $outer, 'settings' => [], 'elements' => [
                        ['id' => 'ccccccc', 'elType' => 'section', 'isInner' => $inner, 'settings' => [], 'elements' => [
                            ['id' => 'ddddddd', 'elType' => 'column', 'isInner' => $inner, 'settings' => [], 'elements' => []]]]]]]],
            ];
            foreach ([['op' => 'move', 'ref' => 'aaaaaaa', 'before' => 'ddddddd'],
                ['op' => 'move', 'ref' => 'ddddddd', 'before' => 'aaaaaaa']] as $op) {
                $this->assertSame('layout_invalid', $this->apply([$op], $tree)['code'] ?? null);
            }
            $r = $this->apply([['op' => 'move', 'ref' => 'aaaaaaa', 'before' => 'bbbbbbb']], $tree);
            $this->assertArrayHasKey('tree', $r, json_encode($r));
            $this->assertSame($tree[0]['elements'][0], $r['tree'][1]['elements'][0]);
        }
    }

    public function test_native_anchors_can_repeat_and_duplicate_can_anchor_itself(): void
    {
        $ops = [
            ['op' => 'duplicate', 'ref' => 'aaaaaaa', 'after' => 'aaaaaaa'],
            ['op' => 'add_native', 'into' => '2222222', 'widget' => 'counter', 'settings' => ['title' => 'First']],
            ['op' => 'add_native', 'into' => '2222222', 'widget' => 'counter', 'settings' => ['title' => 'Second']],
            ['op' => 'duplicate', 'ref' => 'bbbbbbb', 'into' => '2222222'],
        ];
        $r = $this->apply($ops);
        $this->assertArrayHasKey('tree', $r, json_encode($r));
        $this->assertSame(['First', 'Second', 'Heading'], array_column(array_column($r['tree'][1]['elements'], 'settings'), 'title'));
        $this->assertSame($r['results'][0]['new_ids'][0], $r['tree'][0]['elements'][1]['id']);
        foreach ([
            [['op' => 'set_native', 'ref' => 'aaaaaaa', 'settings' => []], ['op' => 'duplicate', 'ref' => 'aaaaaaa', 'into' => '2222222']],
            [['op' => 'duplicate', 'ref' => 'aaaaaaa', 'into' => '2222222'], ['op' => 'remove', 'ref' => 'aaaaaaa']],
        ] as $bad) {
            $this->assertSame('ops_invalid', self::parse($bad)['code'] ?? null);
        }
        $after = $this->apply([
            ['op' => 'add_native', 'after' => 'aaaaaaa', 'widget' => 'counter', 'settings' => ['title' => 'First']],
            ['op' => 'add_native', 'after' => 'aaaaaaa', 'widget' => 'counter', 'settings' => ['title' => 'Second']],
        ]);
        $this->assertSame(['Hello', 'Second', 'First', 'Heading'], array_column(array_column($after['tree'][0]['elements'], 'settings'), 'title'));
    }

    public function test_move_into_change_includes_position(): void
    {
        $tree = self::tree();
        $tree[1]['elements'][] = self::widget('counter', ['title' => 'Last'], 'ccccccc');
        $r = $this->apply([['op' => 'move', 'ref' => 'aaaaaaa', 'into' => '2222222', 'position' => 'first']], $tree);
        $this->assertSame('aaaaaaa', $r['tree'][1]['elements'][0]['id']);
        $this->assertSame('first', $r['changes'][0]['anchor']['position'] ?? null);
    }

    public function test_repeated_ids_under_native_children_have_distinct_detail(): void
    {
        $tree = self::tree();
        $tree[0]['elements'][0]['elements'][] = self::widget('heading', [], 'bbbbbbb');
        $r = LayoutOps::apply($tree, [['op' => 'remove', 'ref' => 'aaaaaaa']], new IdSeed('duplicates'), [], true, null, new NativeWidgetPolicy($this->api), ['mobile']);
        $this->assertSame('data_unreadable', $r['code'] ?? null);
        $this->assertSame('an element id appears more than once on the page', $r['detail'] ?? null);
    }

    public function test_new_operations_reject_each_invalid_grammar_shape(): void
    {
        $set = ['op' => 'set_native', 'ref' => 'aaaaaaa', 'settings' => []];
        $add = ['op' => 'add_native', 'after' => 'aaaaaaa', 'widget' => 'counter', 'settings' => []];
        $duplicate = ['op' => 'duplicate', 'ref' => 'aaaaaaa', 'after' => 'bbbbbbb'];
        $bad = [];
        foreach ([$set, $add, $duplicate] as $op) {
            $bad[] = $op + ['extra' => 'no'];
            $bad[] = $op + ['position' => 'first'];
            if ($op['op'] !== 'set_native') {
                $bad[] = $op + ['before' => '2222222'];
                $bad[] = $op + ['into' => '2222222'];
            } else {
                $bad[] = $op + ['after' => 'bbbbbbb'];
            }
            foreach ($op['op'] === 'add_native' ? ['widget', 'settings'] : ($op['op'] === 'set_native' ? ['ref', 'settings'] : ['ref']) as $missing) {
                $copy = $op; unset($copy[$missing]); $bad[] = $copy;
            }
            if ($op['op'] !== 'duplicate') {
                foreach (['text', ['nonempty']] as $settings) {
                    $copy = $op; $copy['settings'] = $settings; $bad[] = $copy;
                }
            } else {
                $bad[] = $op + ['settings' => []];
                $bad[] = $op + ['widget' => 'counter'];
            }
        }
        foreach ($bad as $op) { $this->assertSame('bad_input', self::parse([$op])['code'] ?? null, json_encode($op)); }
    }

    public function test_native_moves_refuse_incompatible_hosts_and_duplicate_svg(): void
    {
        $tree = self::tree();
        $tree[] = ['id' => '3333333', 'elType' => 'section', 'isInner' => false, 'settings' => [], 'elements' => [
            ['id' => '4444444', 'elType' => 'column', 'isInner' => false, 'settings' => [], 'elements' => []]]];
        $tree[] = ['id' => '5555555', 'elType' => 'container', 'settings' => ['__dynamic__' => ['background_image' => 'bound']], 'elements' => []];
        foreach (['bbbbbbb', '3333333', '5555555'] as $host) {
            $r = $this->apply([['op' => 'move', 'ref' => 'aaaaaaa', 'into' => $host]], $tree);
            $this->assertArrayNotHasKey('tree', $r);
            $this->assertContains($r['code'], ['layout_invalid', 'node_not_editable']);
        }
        $tree[0]['elements'][0]['settings']['icon'] = ['library' => 'svg', 'value' => ['id' => 7, 'url' => 'https://example.com/a.svg']];
        $this->assertSame('native_duplicate_refused', $this->apply([['op' => 'duplicate', 'ref' => 'aaaaaaa', 'into' => '2222222']], $tree)['code'] ?? null);
    }

    public function test_core_atomic_without_prefix_is_locked_and_short_summary_exact(): void
    {
        $policy = new NativeWidgetPolicy($this->api);
        $this->assertSame('A short title', $policy->summary('counter', ['title' => '<b>A short title</b>', 'number' => '99']));
        $this->api->widgetCatalogue[0]['atomic'] = true;
        $nodes = ElementorClassicMapper::project(self::tree(), $policy)->nodes();
        $this->assertSame('locked', $nodes[1]['kind']);
        $this->assertSame('widget_not_usable', $this->apply([['op' => 'add_native', 'into' => '2222222', 'widget' => 'counter', 'settings' => []]])['code'] ?? null);
    }

    public function test_cut_schema_keeps_only_member_and_activator_unless_root_written(): void
    {
        $this->api->widgetControls['counter'] += [
            'typography_typography' => ['type' => 'select', 'options' => ['' => 'Default', 'custom' => 'Custom'], 'groupType' => 'typography', 'groupPrefix' => 'typography_'],
            'typography_font_weight' => ['type' => 'number', 'groupType' => 'typography', 'groupPrefix' => 'typography_', 'condition' => ['typography_typography' => 'custom']],
            'typography_font_family' => ['type' => 'font', 'groupType' => 'typography', 'groupPrefix' => 'typography_', 'condition' => ['typography_typography' => 'custom']],
        ];
        $tree = self::tree();
        $tree[0]['elements'][0]['settings']['typography_typography'] = 'custom';
        $r = $this->apply([['op' => 'set_native', 'ref' => 'aaaaaaa', 'settings' => ['typography_font_weight' => '700']]], $tree);
        $this->assertArrayHasKey('tree', $r, json_encode($r));
        $this->assertSame(['typography_font_weight', 'typography_typography'], array_keys($r['schemas']['counter']['controls']));
        $r = $this->apply([['op' => 'set_native', 'ref' => 'aaaaaaa', 'settings' => ['typography_typography' => 'custom']]], $tree);
        $this->assertSame(['typography_font_family', 'typography_font_weight', 'typography_typography'], array_keys($r['schemas']['counter']['controls']));
    }

    public function test_repeater_group_activator_uses_live_option_keys(): void
    {
        $this->api->widgetControls['counter']['rows']['fields'] = [
            ['name' => 'typography_typography', 'type' => 'select', 'options' => ['' => 'Default', 'custom' => 'Custom'], 'groupType' => 'typography', 'groupPrefix' => 'typography_'],
            ['name' => 'typography_font_weight', 'type' => 'number', 'groupType' => 'typography', 'groupPrefix' => 'typography_', 'condition' => ['typography_typography' => 'custom']],
        ];
        $r = $this->apply([['op' => 'add_native', 'into' => '2222222', 'widget' => 'counter', 'settings' => ['rows' => [
            ['_id' => 'ccccccc', 'typography_typography' => 'custom', 'typography_font_weight' => '700']]]]]);
        $this->assertArrayHasKey('tree', $r, json_encode($r));
        $this->assertSame(['custom'], $r['schemas']['counter']['controls']['rows']['fields']['typography_typography']['activator']['values']);
    }

    public function test_native_grammar_and_nested_duplicate_keys(): void
    {
        foreach ([['op' => 'set_native', 'ref' => 'aaaaaaa', 'settings' => ['title' => 'New']],
            ['op' => 'add_native', 'into' => '1111111', 'widget' => 'counter', 'settings' => []],
            ['op' => 'duplicate', 'ref' => 'aaaaaaa', 'into' => '2222222', 'position' => 'first'],
            ['op' => 'move', 'ref' => 'aaaaaaa', 'into' => '2222222']] as $op) {
            $this->assertArrayHasKey('input', self::parse([$op]));
        }
        $raw = '{"post_id":41,"base_fingerprint":"' . str_repeat('a', 64) . '","operations":[{"op":"set_native","ref":"aaaaaaa","settings":{"image":{"id":"7","url":"a","u\\u0072l":"b"}}}]}';
        $r = PageEditValidator::parse($raw);
        $this->assertSame('native_settings_invalid', $r['code'] ?? null);
        $this->assertNotEmpty($r['failures']);
        $this->assertSame('bad_input', self::parse([['op' => 'add_native', 'into' => '1111111', 'widget' => 'bad_name', 'settings' => []]])['code'] ?? null);
        foreach (['{"rows":{"0":{"_id":"aaaaaaa","text":"x"}}}', '{"rows":[{}]}'] as $settings) {
            $raw = '{"post_id":41,"base_fingerprint":"' . str_repeat('a', 64) . '","operations":[{"op":"set_native","ref":"aaaaaaa","settings":' . $settings . '}]}';
            $this->assertSame('native_settings_invalid', PageEditValidator::parse($raw)['code'] ?? null);
        }
    }

    public function test_projection_core_only_summary_and_opaque_children(): void
    {
        $tree = self::tree();
        $tree[0]['elements'][0]['settings'] = ['title' => '<b>' . str_repeat('é', 150) . '</b>', 'number' => '123', 'color' => '#ABCDEF'];
        $tree[0]['elements'][0]['elements'] = [self::widget('heading', ['title' => 'Hidden'], 'ccccccc')];
        foreach (['vendor-widget', 'html', 'e-heading', 'premium', 'missing'] as $i => $type) {
            $tree[1]['elements'][] = self::widget($type, [], 'dddddd' . $i);
        }
        $nodes = ElementorClassicMapper::project($tree, new NativeWidgetPolicy($this->api))->toArray(500)['nodes'];
        $this->assertSame('native', $nodes[1]['kind']);
        $this->assertSame('counter', $nodes[1]['widget']);
        $this->assertSame(str_repeat('é', 120), $nodes[1]['summary']);
        $this->assertNotContains('ccccccc', array_column($nodes, 'ref'));
        foreach (array_slice($nodes, 4) as $node) {
            $this->assertSame('locked', $node['kind']);
            $this->assertArrayNotHasKey('widget', $node);
        }
    }

    public function test_set_native_carries_leaves_and_edits_classified_widget(): void
    {
        $tree = self::tree();
        $tree[0]['elements'][0]['settings']['legacy'] = ['enabled' => true, 'count' => 3, 'text' => '<p>Existing</p>'];
        $r = $this->apply([['op' => 'set_native', 'ref' => 'aaaaaaa', 'settings' => ['title' => 'Updated']], ['op' => 'set_native', 'ref' => 'bbbbbbb', 'settings' => ['title' => 'Updated heading']]], $tree);
        $this->assertArrayHasKey('tree', $r, json_encode($r));
        $this->assertSame($tree[0]['elements'][0]['settings']['legacy'], $r['tree'][0]['elements'][0]['settings']['legacy']);
        $this->assertSame('Updated heading', $r['tree'][0]['elements'][1]['settings']['title']);
        $this->assertSame($tree[0]['elements'][0]['settings'], $r['results'][0]['before_settings']);
        $this->assertSame($r['tree'][0]['elements'][0], $r['results'][0]['result_node']);
        $deleted = $this->apply([['op' => 'set_native', 'ref' => 'aaaaaaa', 'settings' => ['title' => null]]]);
        $this->assertSame([], $deleted['tree'][0]['elements'][0]['settings'] ?? null);
    }

    public function test_settings_refusals_preserve_sp14(): void
    {
        $scalar = self::parse([['op' => 'set_native', 'ref' => 'aaaaaaa', 'settings' => ['number' => 123]]]);
        $this->assertSame('native_settings_invalid', $scalar['code'] ?? null);
        $this->assertNotEmpty($scalar['failures']);
        $this->assertArrayHasKey('schema', $scalar);
        foreach ([['title' => '<script>x</script>'], ['missing' => 'x'], ['color_laptop' => '#ABCDEF'], ['icon' => ['library' => 'svg', 'value' => 'x']]] as $settings) {
            $r = $this->apply([['op' => 'set_native', 'ref' => 'aaaaaaa', 'settings' => $settings]]);
            $this->assertSame('native_settings_invalid', $r['code'] ?? null, json_encode($r));
            $this->assertNotEmpty($r['failures']);
            $this->assertArrayHasKey('schema', $r);
        }
        $tree = self::tree();
        $tree[0]['elements'][0]['settings']['__globals__'] = ['title' => 'globals/title'];
        $r = $this->apply([['op' => 'set_native', 'ref' => 'aaaaaaa', 'settings' => ['title' => 'Updated']]], $tree);
        $this->assertSame('native_settings_invalid', $r['code'] ?? null);
    }

    public function test_designed_and_locked_sources_refused(): void
    {
        foreach (['set_native', 'duplicate'] as $op) {
            $input = ['op' => $op, 'ref' => 'aaaaaaa'] + ($op === 'set_native' ? ['settings' => ['title' => 'New']] : ['into' => '2222222']);
            $tree = self::tree();
            $tree[0]['elements'][0]['settings'][LeafPolicy::LABEL_KEY] = LeafPolicy::LABEL_PREFIX . 'Heading';
            $this->assertSame('native_designed_node', $this->apply([$input], $tree)['code'] ?? null);
            $tree[0]['elements'][0]['widgetType'] = 'vendor-widget';
            $tree[0]['elements'][0]['settings'] = [];
            $this->assertSame('node_not_editable', $this->apply([$input], $tree)['code'] ?? null);
        }
    }

    public function test_add_shape_unique_ids_and_placement(): void
    {
        $ops = [['op' => 'add_native', 'into' => '1111111', 'position' => 'first', 'widget' => 'counter', 'settings' => ['number' => '123']],
            ['op' => 'add_native', 'into' => '2222222', 'widget' => 'counter', 'settings' => []]];
        $r = $this->apply($ops);
        $this->assertArrayHasKey('tree', $r, json_encode($r));
        $node = $r['tree'][0]['elements'][0];
        $this->assertSame(['id', 'elType', 'settings', 'elements', 'widgetType'], array_keys($node));
        $this->assertSame($node, $r['results'][0]['result_node']);
        $this->assertMatchesRegularExpression('/^[0-9a-f]{7}$/', $node['id']);
        $this->assertNotSame($node['id'], $r['tree'][1]['elements'][0]['id']);
        $this->assertSame([$node['id']], $r['results'][0]['new_ids']);
        $this->assertSame($r['tree'], $this->apply($ops)['tree']);
        foreach (['1111111', 'aaaaaaa'] as $ref) {
            $this->assertArrayNotHasKey('tree', $this->apply([['op' => 'add_native', 'after' => $ref, 'widget' => 'counter', 'settings' => []]], $ref === 'aaaaaaa' ? [self::widget()] : null));
        }
        foreach (['vendor-widget', 'html', 'e-heading', 'premium', 'missing'] as $type) {
            $this->assertSame('widget_not_usable', $this->apply([['op' => 'add_native', 'into' => '1111111', 'widget' => $type, 'settings' => []]])['code'] ?? null);
        }
    }

    public function test_duplicate_changes_only_ids_and_refuses_denied_subtrees(): void
    {
        $op = ['op' => 'duplicate', 'ref' => 'aaaaaaa', 'into' => '2222222'];
        $r = $this->apply([$op]);
        $copy = $r['tree'][1]['elements'][0] ?? [];
        $id = $copy['id'] ?? '';
        $copy['id'] = 'aaaaaaa';
        $this->assertSame(self::tree()[0]['elements'][0], $copy);
        $this->assertSame([$id], $r['results'][0]['new_ids']);
        foreach (['_element_id', 'custom_css', '__dynamic__', 'motion_fx_x', 'sticky_x'] as $key) {
            $tree = self::tree();
            $tree[0]['elements'][0]['elements'] = [self::widget('heading', ['nested' => [$key => 'x']], 'ccccccc')];
            $this->assertArrayNotHasKey('tree', $this->apply([$op], $tree), $key);
        }
    }

    public function test_native_remove_move_and_ref_lifetime(): void
    {
        $r = $this->apply([['op' => 'move', 'ref' => 'aaaaaaa', 'into' => '2222222']]);
        $this->assertSame(self::tree()[0]['elements'][0], $r['tree'][1]['elements'][0] ?? null);
        $r = $this->apply([['op' => 'remove', 'ref' => 'aaaaaaa']]);
        $this->assertSame(['bbbbbbb'], array_column($r['tree'][0]['elements'] ?? [], 'id'));
        $this->assertSame('node_not_editable', $this->apply([['op' => 'move', 'ref' => '1111111', 'into' => 'aaaaaaa']])['code'] ?? null);
        $parsed = self::parse([['op' => 'remove', 'ref' => '1111111'], ['op' => 'set_native', 'ref' => 'aaaaaaa', 'settings' => []]]);
        $rule = PageEditValidator::againstPage($parsed['input'], ElementorClassicMapper::project(self::tree(), new NativeWidgetPolicy($this->api))->nodes(), ['operations' => BuilderContract::OPS]);
        $this->assertSame('ref_gone', $rule['detail'] ?? null);
        $tree = self::tree();
        $tree[1]['elements'] = [self::widget()];
        $this->assertSame('data_unreadable', LayoutOps::apply($tree, [], new IdSeed('native-edit'), [], true, null, new NativeWidgetPolicy($this->api))['code'] ?? null);
    }

    public function test_live_groups_responsive_windows_lists_and_carry_identity(): void
    {
        $this->api->widgetControls['counter']['typography_typography'] = ['type' => 'select', 'options' => ['' => 'Default', 'custom' => 'Custom'], 'groupType' => 'typography', 'groupPrefix' => 'typography_'];
        $this->api->widgetControls['counter']['typography_font_size'] = ['type' => 'slider', 'size_units' => ['px'], 'is_responsive' => true, 'responsive' => ['devices' => ['tablet', 'mobile']], 'groupType' => 'typography', 'groupPrefix' => 'typography_', 'condition' => ['typography_typography' => 'custom']];
        $settings = ['typography_typography' => 'custom', 'typography_font_size_mobile' => ['unit' => 'px', 'size' => '24']];
        $r = $this->apply([['op' => 'set_native', 'ref' => 'aaaaaaa', 'settings' => $settings]]);
        $this->assertArrayHasKey('tree', $r, json_encode($r));
        unset($settings['typography_typography']);
        $this->assertSame('native_settings_invalid', $this->apply([['op' => 'set_native', 'ref' => 'aaaaaaa', 'settings' => $settings]])['code'] ?? null);
        $settings['typography_typography'] = 'custom';
        $settings['typography_font_size_laptop'] = $settings['typography_font_size_mobile'];
        $this->assertSame('native_settings_invalid', $this->apply([['op' => 'set_native', 'ref' => 'aaaaaaa', 'settings' => $settings]])['code'] ?? null);
        $tree = self::tree();
        $tree[0]['elements'][0]['settings']['rows'] = [['_id' => 'ccccccc', 'text' => 'Old'], ['_id' => 'ddddddd', 'text' => 'Removed']];
        $patch = ['rows' => [['_id' => 'eeeeeee', 'text' => 'New']]];
        $r = $this->apply([['op' => 'set_native', 'ref' => 'aaaaaaa', 'settings' => $patch]], $tree);
        $this->assertSame($patch['rows'], $r['tree'][0]['elements'][0]['settings']['rows'] ?? null);
        $schema = (new NativeWidgetPolicy($this->api))->schema('counter', [], null, ['mobile', 'tablet']);
        $before = ['title' => 'Before', 'unsupported' => ['n' => 3]];
        $written = ['title' => 'After'];
        $merged = NativeSettingsValidator::mergeSettings($before, $written);
        $this->assertTrue(NativeSettingsValidator::validateWritten($schema, $before, $written, $merged, ['mobile', 'tablet'])['ok']);
        $merged['unsupported']['n'] = '3';
        $this->assertFalse(NativeSettingsValidator::validateWritten($schema, $before, $written, $merged, ['mobile', 'tablet'])['ok']);
    }

    public function test_duplicate_subtree_and_every_new_ref_unavailable_until_next_call(): void
    {
        $tree = self::tree();
        $tree[0]['elements'][0]['elements'] = [self::widget('heading', ['title' => 'Child'], 'ccccccc')];
        $r = $this->apply([['op' => 'duplicate', 'ref' => 'aaaaaaa', 'into' => '2222222']], $tree);
        $this->assertArrayHasKey('tree', $r, json_encode($r));
        $copy = $r['tree'][1]['elements'][0];
        $newIds = [$copy['id'], $copy['elements'][0]['id']];
        $this->assertSame($newIds, $r['results'][0]['new_ids']);
        $copy['id'] = 'aaaaaaa';
        $copy['elements'][0]['id'] = 'ccccccc';
        $this->assertSame($tree[0]['elements'][0], $copy);
        $add = ['op' => 'add_native', 'into' => '1111111', 'widget' => 'counter', 'settings' => []];
        $r = $this->apply([$add]);
        $newId = $r['results'][0]['new_ids'][0];
        $r = $this->apply([$add, ['op' => 'set_native', 'ref' => $newId, 'settings' => ['title' => 'New']]]);
        $this->assertSame('node_not_found', $r['code'] ?? null);
        $this->assertSame('widget_not_usable', $this->apply([['op' => 'set_native', 'ref' => 'bbbbbbb', 'settings' => ['title' => 'New']]], $this->classifiedReplacement())['code'] ?? null);
    }

    private function classifiedReplacement(): array
    {
        $this->api->coreWidgets = ['counter'];
        return self::tree();
    }

    public function test_legacy_columns_accept_native_widgets_and_sections_refuse_them(): void
    {
        $tree = [['id' => '1111111', 'elType' => 'section', 'isInner' => false, 'settings' => [], 'elements' => [
            ['id' => '2222222', 'elType' => 'column', 'isInner' => false, 'settings' => [], 'elements' => []]]]];
        $op = ['op' => 'add_native', 'into' => '2222222', 'widget' => 'counter', 'settings' => ['title' => 'Column']];
        $r = $this->apply([$op], $tree);
        $this->assertSame('Column', $r['tree'][0]['elements'][0]['elements'][0]['settings']['title'] ?? null);
        $op['into'] = '1111111';
        $this->assertSame('layout_invalid', $this->apply([$op], $tree)['code'] ?? null);
        $tree = self::tree();
        $tree[0]['elements'][0]['elements'] = [self::widget('html', [], 'ccccccc')];
        $this->assertSame('native_duplicate_refused', $this->apply([['op' => 'duplicate', 'ref' => 'aaaaaaa', 'into' => '2222222']], $tree)['code'] ?? null);
        $op = ['op' => 'add_native', 'into' => '1111111', 'widget' => 'counter', 'settings' => ['title' => null]];
        $this->assertSame('native_settings_invalid', $this->apply([$op])['code'] ?? null);
    }

    public function test_native_widgets_fit_row_containers_without_changing_inner_flag(): void
    {
        $tree = self::tree();
        $tree[1]['settings']['flex_direction'] = 'row';
        $r = $this->apply([['op' => 'add_native', 'into' => '2222222', 'widget' => 'counter', 'settings' => []]], $tree);
        $this->assertArrayHasKey('tree', $r, json_encode($r));
        $r = $this->apply([['op' => 'move', 'ref' => 'aaaaaaa', 'into' => '2222222']], $tree);
        $this->assertSame($tree[0]['elements'][0], $r['tree'][1]['elements'][0] ?? null);
        $tree[0]['elements'][0]['isInner'] = true;
        $this->assertSame('layout_invalid', $this->apply([['op' => 'move', 'ref' => 'aaaaaaa', 'into' => '2222222']], $tree)['code'] ?? null);
    }

    public function test_native_reads_older_stored_inner_flags_without_changing_move_bytes(): void
    {
        $tree = self::tree();
        $tree[0]['elements'][0]['isInner'] = '';
        $move = $this->apply([['op' => 'move', 'ref' => 'aaaaaaa', 'into' => '2222222']], $tree);
        $this->assertSame($tree[0]['elements'][0], $move['tree'][1]['elements'][0] ?? null);
        $set = $this->apply([['op' => 'set_native', 'ref' => 'aaaaaaa', 'settings' => ['title' => 'New']]], $tree);
        $this->assertArrayHasKey('tree', $set, json_encode($set));
        $this->assertSame('', $set['results'][0]['result_node']['isInner']);
    }
}
