<?php

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\Builders\ElementorFacts;
use WPMgr\Agent\Abilities\Builders\ElementorWidgetDiscovery;
use WPMgr\Agent\Abilities\Builders\NativeWidgetPolicy;
use WPMgr\Agent\Abilities\Builders\NativeSettingsValidator;
use WPMgr\Agent\Abilities\OwnAbilities;
use WPMgr\Agent\Autoloader;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/** @covers \WPMgr\Agent\Abilities\Builders\ElementorWidgetDiscovery */
final class ElementorWidgetDiscoveryTest extends TestCase
{
    public function test_only_stored_choice_values_are_exported_as_option_keys(): void
    {
        $this->api->widgetControls['heading'] = [
            'tag' => ['type' => 'select', 'options' => ['h2' => 'Heading']],
            'external_url' => ['type' => 'url', 'options' => false],
            'link' => ['type' => 'url', 'options' => ['is_external', 'nofollow']],
            'decoration' => ['type' => 'text', 'options' => ['feature' => 'Feature']],
            '_mask_shape' => ['type' => 'visual_choice', 'options' => ['blob' => ['title' => 'Blob'], 'custom' => ['title' => 'Custom']]],
        ];
        $answer = (new ElementorWidgetDiscovery($this->api))->schema((object) [
            'widget' => 'heading', 'controls' => array_keys($this->api->widgetControls['heading']),
        ]);
        self::assertArrayHasKey('output', $answer);
        $controls = (array) $answer['output']['controls'];
        self::assertSame(['h2'], $controls['tag']['options']);
        self::assertSame(['blob', 'custom'], $controls['_mask_shape']['options']);
        foreach (['external_url', 'link', 'decoration'] as $id) {
            self::assertSame([], $controls[$id]['options'], $id);
        }
    }

    private const CATALOGUE = 'wpmgr/elementor-widgets';
    private const SCHEMA = 'wpmgr/elementor-widget-schema';

    private FakeElementorApi $api;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        $this->api = new FakeElementorApi();
        $this->api->widgetCatalogue = [
            ['name' => 'heading', 'title' => 'Heading', 'categories' => ['basic'], 'pro' => false, 'atomic' => false],
            ['name' => 'gallery', 'title' => 'Photo Gallery', 'categories' => ['pro', 'media'], 'pro' => true, 'atomic' => false],
            ['name' => 'e-heading', 'title' => 'Atomic Heading', 'categories' => ['basic'], 'pro' => false, 'atomic' => true],
            ['name' => 'html', 'title' => 'HTML', 'categories' => ['basic'], 'pro' => false, 'atomic' => false],
        ];
        $this->api->widgetControls = ['heading' => self::controls()];
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_catalogue_is_live_filtered_and_marks_only_allowed_classic_widgets_usable(): void
    {
        $out = $this->catalogue([]);
        $this->assertSame('3.35.9', $out['builder_version']);
        $this->assertSame(['heading', 'gallery', 'e-heading', 'html'], array_column($out['widgets'], 'name'));
        $this->assertSame([true, true, false, false], array_column($out['widgets'], 'usable'));
        $this->assertSame([false, true, false, false], array_column($out['widgets'], 'pro'));
        $this->assertSame(['Photo Gallery'], array_column($this->catalogue(['category' => 'media', 'search' => 'PHOTO'])['widgets'], 'title'));
        $this->assertSame([], $this->catalogue(['category' => 'basic', 'search' => 'gallery'])['widgets']);
        $this->api->widgetCatalogue = [];
        $this->assertSame([], $this->catalogue([])['widgets']);
        $this->assertCount(4, $this->api->callsTo('widgetCatalogue'));
        $this->assertSame([], $this->api->callsTo('document'));
        $this->assertSame([], $this->api->callsTo('regeneratePostCss'));
    }

    public function test_schema_skeleton_has_tabs_sections_ids_and_types_without_ui_or_denied_controls(): void
    {
        $out = $this->schema(['widget' => 'heading']);
        $this->assertSame('skeleton', $out['mode']);
        $this->assertFalse($out['truncated']);
        $this->assertSame([
            ['id' => 'content', 'sections' => [['id' => 'section_title', 'controls' => [
                ['id' => 'title', 'type' => 'text'],
                ['id' => 'size', 'type' => 'select'],
                ['id' => 'items', 'type' => 'repeater'],
            ]]]],
            ['id' => 'style', 'sections' => [['id' => 'section_style', 'controls' => [
                ['id' => 'typography_typography', 'type' => 'select'],
                ['id' => 'typography_font_size', 'type' => 'slider'],
                ['id' => 'align', 'type' => 'choose'],
                ['id' => 'align_tablet', 'type' => 'choose'],
            ]]]],
        ], $out['tabs']);
        $this->assertArrayNotHasKey('controls', $out);
    }

    public function test_stripped_stacks_refuse_schema_reads_without_version_gating(): void
    {
        foreach (['3.20.4', '3.35.9', '4.3.4'] as $version) {
            $this->api->version = $version;
            $controls = self::controls();
            foreach ($controls as &$control) {
                unset($control['options'], $control['size_units'], $control['range'], $control['label']);
            }
            unset($control);
            $this->api->widgetControls['heading'] = $controls;
            foreach ([[], ['title'], ['typography_font_size'], ['align']] as $ids) {
                $result = (new ElementorWidgetDiscovery($this->api))->schema((object) (['widget' => 'heading'] + ($ids === [] ? [] : ['controls' => $ids])));
                $this->assertSame('elementor_controls_unavailable', $result['refusal']['code'] ?? null, $version . ' ' . json_encode($ids));
                $this->assertSame('this Elementor version builds widget settings only in the editor; native edits need a newer Elementor', $result['refusal']['detail']);
            }
            $this->api->widgetControls['heading'] = self::controls();
            $this->assertArrayHasKey('output', (new ElementorWidgetDiscovery($this->api))->schema((object) ['widget' => 'heading', 'controls' => ['title']]));
        }
    }

    public function test_named_schema_keeps_options_defaults_devices_groups_and_repeater_fields(): void
    {
        $out = $this->schema(['widget' => 'heading', 'controls' => ['title', 'size', 'typography_font_size', 'align', 'align_tablet', 'items']]);
        $this->assertSame('controls', $out['mode']);
        $controls = (array) $out['controls'];
        $this->assertSame('', $controls['title']['default']);
        $this->assertSame(['h1', 'h2', '0'], $controls['size']['options']);
        $this->assertSame('h2', $controls['size']['default']);
        $this->assertSame(['desktop', 'tablet', 'mobile'], $controls['typography_font_size']['responsive']['devices']);
        $this->assertSame(['tablet', 'mobile'], $controls['align']['responsive']['devices']);
        $this->assertSame(['tablet'], $controls['align_tablet']['responsive']['devices']);
        $this->assertSame(['family' => 'typography', 'prefix' => 'typography_', 'activator' => 'typography_typography'], $controls['typography_font_size']['group']);
        $this->assertSame(['typography_typography' => 'custom'], $controls['typography_font_size']['condition']);
        $this->assertSame(['px', 'em'], $controls['typography_font_size']['size_units']);
        $this->assertSame(['px' => ['min' => 1, 'max' => 100]], $controls['typography_font_size']['range']);
        $fields = (array) $controls['items']['fields'];
        $this->assertSame(['text', 'link', 'kind'], array_keys($fields));
        $this->assertSame('url', $fields['link']['type']);
        $this->assertSame(['a', 'b'], $fields['kind']['options']);
        $this->assertStringNotContainsString('Human option label', json_encode($out, JSON_THROW_ON_ERROR));
        $this->assertArrayNotHasKey('tabs', $out);
    }

    public function test_single_control_and_parent_chain_windows(): void
    {
        $this->api->activeBreakpoints = ['widescreen', 'laptop', 'tablet', 'mobile'];
        $this->api->widgetControls['heading'] = [
            'size' => ['type' => 'slider', 'size_units' => ['px'], 'is_responsive' => true, 'responsive' => []],
            'desktop_only' => ['type' => 'slider', 'responsive' => ['max' => 'desktop']],
            'limited' => ['type' => 'slider', 'is_responsive' => true, 'responsive' => ['devices' => ['tablet', 'mobile_extra']]],
            'align' => ['type' => 'choose', 'responsive' => ['max' => 'desktop']],
            'align_widescreen' => ['type' => 'choose', 'responsive' => ['min' => 'widescreen'], 'parent' => 'align'],
            'align_tablet' => ['type' => 'choose', 'responsive' => ['max' => 'tablet'], 'parent' => 'align'],
            'align_mobile' => ['type' => 'choose', 'responsive' => ['max' => 'mobile'], 'parent' => 'align_tablet'],
        ];
        $out = $this->schema(['widget' => 'heading', 'controls' => ['size', 'limited', 'align', 'desktop_only']]);
        $controls = (array) $out['controls'];
        $this->assertSame(['widescreen', 'desktop', 'laptop', 'tablet', 'mobile'], $controls['size']['responsive']['devices']);
        $this->assertSame(['desktop'], $controls['desktop_only']['responsive']['devices']);
        $this->assertSame(['tablet'], $controls['limited']['responsive']['devices']);
        $this->assertSame(['widescreen', 'desktop', 'tablet', 'mobile'], $controls['align']['responsive']['devices']);
        $skeleton = $this->schema(['widget' => 'heading']);
        $this->assertSame(['size', 'desktop_only', 'limited', 'align'], array_column($skeleton['tabs'][0]['sections'][0]['controls'], 'id'));
        $r = (new ElementorWidgetDiscovery($this->api))->schema((object) ['widget' => 'heading', 'controls' => ['align_mobile']]);
        $this->assertSame('control_not_found', $r['refusal']['code']);
        $this->api->widgetControls['heading']['align_mobile']['parent'] = 'align_mobile';
        $r = (new ElementorWidgetDiscovery($this->api))->schema((object) ['widget' => 'heading']);
        $this->assertSame('widget_schema_unreadable', $r['refusal']['code']);
    }

    public function test_popover_starter_resolves_one_member_family(): void
    {
        $this->api->widgetControls['heading'] = [
            'title_typography' => ['type' => 'popover_toggle', 'groupPrefix' => 'title_', 'return_value' => 'custom'],
            'title_font_size' => ['type' => 'slider', 'size_units' => ['px'], 'groupPrefix' => 'title_', 'groupType' => 'typography', 'condition' => ['title_typography!' => '']],
        ];
        $out = $this->schema(['widget' => 'heading', 'controls' => ['title_typography', 'title_font_size']]);
        $controls = (array) $out['controls'];
        $this->assertSame('custom', $controls['title_typography']['return_value']);
        $this->assertSame($controls['title_font_size']['group'], $controls['title_typography']['group']);
        $this->assertSame('title_typography', $controls['title_typography']['group']['activator']);
        $this->api->widgetControls['heading']['title_other'] = ['type' => 'slider', 'groupPrefix' => 'title_', 'groupType' => 'border'];
        $out = $this->schema(['widget' => 'heading', 'controls' => ['title_typography']]);
        $this->assertNull(((array) $out['controls'])['title_typography']['group']);
    }

    public function test_cut_schema_preserves_popover_activation_and_device_list(): void
    {
        $this->api->widgetControls['heading'] = [
            'title_typography' => ['type' => 'popover_toggle', 'groupPrefix' => 'title_', 'return_value' => 'custom'],
            'title_font_size' => ['type' => 'slider', 'groupPrefix' => 'title_', 'groupType' => 'typography', 'condition' => ['title_typography!' => ''], 'size_units' => ['px'], 'is_responsive' => true, 'responsive' => ['devices' => ['mobile']]],
        ];
        $this->api->widgetTypes = ['heading'];
        $this->api->coreWidgets = ['heading'];
        $policy = new NativeWidgetPolicy($this->api);
        $schema = $policy->schema('heading', [], ['title_font_size_mobile'], ['tablet', 'mobile']);
        $this->assertNotNull($schema);
        $this->assertSame(['custom'], $schema->document['controls']['title_font_size']['activator']['values']);
        $this->assertSame(['devices' => ['mobile']], $schema->document['controls']['title_font_size']['responsive']);
        $patch = ['title_typography' => 'custom', 'title_font_size_mobile' => ['unit' => 'px', 'size' => '24']];
        $this->assertTrue(NativeSettingsValidator::validateWritten($schema, [], $patch, $patch, ['tablet', 'mobile'])['ok']);
        $patch['title_typography'] = '';
        $this->assertFalse(NativeSettingsValidator::validateWritten($schema, [], $patch, $patch, ['tablet', 'mobile'])['ok']);
        $schema = $policy->schema('heading', [], ['title_typography'], ['mobile']);
        $this->assertArrayHasKey('title_font_size', $schema->document['controls']);
    }

    public function test_big_widget_skeleton_can_be_drilled_down_without_shipping_all_defaults(): void
    {
        $controls = [];
        for ($i = 0; $i < 300; ++$i) {
            $controls['field_' . $i] = ['type' => 'text', 'tab' => 'content', 'section' => 'main', 'default' => str_repeat('x', 2048)];
        }
        $this->api->widgetControls['heading'] = $controls;
        $skeleton = $this->schema(['widget' => 'heading']);
        $this->assertFalse($skeleton['truncated']);
        $this->assertCount(count($controls), $skeleton['tabs'][0]['sections'][0]['controls']);
        $detail = $this->schema(['widget' => 'heading', 'controls' => ['field_299']]);
        $this->assertSame(['field_299'], array_keys((array) $detail['controls']));
        $this->assertSame($controls['field_299']['default'], ((array) $detail['controls'])['field_299']['default']);
    }

    public function test_cap_applies_to_encoded_skeleton_and_named_controls_without_cutting_values(): void
    {
        $controls = [];
        for ($i = 0; $i < 2000; ++$i) {
            $controls['field_' . $i] = ['type' => 'text', 'default' => str_repeat('é"\\', 2000)];
        }
        $this->api->widgetControls['heading'] = $controls;
        foreach ([['widget' => 'heading'], ['widget' => 'heading', 'controls' => ['field_0', 'field_1', 'field_2']]] as $input) {
            $out = $this->schema($input);
            $this->assertTrue($out['truncated']);
            $this->assertLessThanOrEqual(32768, strlen(json_encode($out, JSON_THROW_ON_ERROR)));
            foreach ((array) ($out['controls'] ?? []) as $control) {
                $this->assertSame($controls['field_0']['default'], $control['default']);
            }
        }
        $this->api->widgetControls['heading'] = ['huge' => ['type' => 'text', 'default' => str_repeat('x', 40000)], 'small' => ['type' => 'text', 'default' => 'ok']];
        $out = $this->schema(['widget' => 'heading', 'controls' => ['huge', 'small']]);
        $this->assertTrue($out['truncated']);
        $this->assertSame(['small'], array_keys((array) $out['controls']));
    }

    public function test_missing_widget_control_and_atomic_widget_have_clear_refusals(): void
    {
        $discovery = new ElementorWidgetDiscovery($this->api);
        $this->assertSame('widget_not_found', $discovery->schema((object) ['widget' => 'missing'])['refusal']['code']);
        $this->assertSame('control_not_found', $discovery->schema((object) ['widget' => 'heading', 'controls' => ['missing']])['refusal']['code']);
        $this->assertSame('atomic_not_supported', $discovery->schema((object) ['widget' => 'e-heading'])['refusal']['code']);
        $this->assertSame('widget_not_usable', $discovery->schema((object) ['widget' => 'html'])['refusal']['code']);
        $this->assertSame('control_not_found', $discovery->schema((object) ['widget' => 'heading', 'controls' => ['custom_css']])['refusal']['code']);
    }

    public function test_elementor_absent_and_versions_outside_supported_range_are_refused_before_discovery(): void
    {
        $discovery = new ElementorWidgetDiscovery($this->api);
        $this->api->loaded = false;
        foreach ([$discovery->catalogue((object) []), $discovery->schema((object) ['widget' => 'heading'])] as $r) {
            $this->assertSame(['code' => 'builder_not_available', 'detail' => 'elementor_missing'], $r['refusal']);
        }
        $this->api->loaded = true;
        foreach ([null, '3.19.9', '4.4.0'] as $version) {
            $this->api->version = $version;
            $this->assertSame('version_unverified', $discovery->catalogue((object) [])['refusal']['detail']);
            $this->assertSame('version_unverified', $discovery->schema((object) ['widget' => 'heading'])['refusal']['detail']);
        }
        $this->assertSame([], $this->api->callsTo('widgetCatalogue'));
        foreach ([ElementorFacts::CLASSIC_MIN, ElementorFacts::CLASSIC_MAX] as $version) {
            $this->api->version = $version;
            $this->assertArrayHasKey('output', $discovery->catalogue((object) []));
        }
    }

    public function test_registry_failure_and_controls_failure_do_not_look_like_empty_success(): void
    {
        $discovery = new ElementorWidgetDiscovery($this->api);
        $this->api->widgetCatalogue = null;
        $this->assertSame('widget_registry_unreadable', $discovery->catalogue((object) [])['refusal']['code']);
        $this->api->widgetCatalogue = [['name' => 'heading', 'title' => 'Heading', 'categories' => [], 'pro' => false, 'atomic' => false]];
        $this->api->widgetControls['heading'] = null;
        $this->assertSame('widget_schema_unreadable', $discovery->schema((object) ['widget' => 'heading'])['refusal']['code']);
    }

    public function test_group_activator_is_read_from_live_conditions_including_nested_repeaters(): void
    {
        $group = ['groupType' => 'border', 'groupPrefix' => 'frame_'];
        $fields = [
            'frame_enable' => $group + ['type' => 'switcher', 'return_value' => 'yes'],
            'frame_width' => $group + ['type' => 'slider', 'size_units' => ['px'], 'condition' => ['frame_enable!' => '']],
        ];
        $this->api->widgetControls['heading'] = $fields + ['items' => ['type' => 'repeater', 'fields' => $fields]];
        $out = $this->schema(['widget' => 'heading', 'controls' => ['frame_enable', 'frame_width', 'items']]);
        $controls = (array) $out['controls'];
        $this->assertSame('frame_enable', $controls['frame_width']['group']['activator']);
        $this->assertSame('frame_enable', $controls['frame_enable']['group']['activator']);
        $this->assertSame('yes', $controls['frame_enable']['return_value']);
        $this->assertSame('frame_enable', ((array) $controls['items']['fields'])['frame_width']['group']['activator']);
        unset($this->api->widgetControls['heading']['frame_enable']);
        $out = $this->schema(['widget' => 'heading', 'controls' => ['frame_width']]);
        $this->assertNull(((array) $out['controls'])['frame_width']['group']['activator']);
    }

    public function test_responsive_devices_use_active_breakpoints_and_unknown_windows_refuse(): void
    {
        $this->api->activeBreakpoints = ['widescreen', 'laptop', 'tablet', 'mobile'];
        $out = $this->schema(['widget' => 'heading', 'controls' => ['typography_font_size', 'align']]);
        $controls = (array) $out['controls'];
        $this->assertSame(['widescreen', 'desktop', 'laptop', 'tablet', 'mobile'], $controls['typography_font_size']['responsive']['devices']);
        $this->assertSame(['tablet', 'mobile'], $controls['align']['responsive']['devices']);
        $this->api->activeBreakpoints = ['mobile'];
        $out = $this->schema(['widget' => 'heading', 'controls' => ['align']]);
        $this->assertSame(['mobile'], ((array) $out['controls'])['align']['responsive']['devices']);
        $discovery = new ElementorWidgetDiscovery($this->api);
        $this->api->activeBreakpoints = null;
        $this->assertSame('widget_schema_unreadable', $discovery->schema((object) ['widget' => 'heading', 'controls' => ['align']])['refusal']['code']);
        $this->assertArrayHasKey('output', $discovery->schema((object) ['widget' => 'heading']));
        $this->api->activeBreakpoints = ['mobile'];
        $this->api->widgetControls['heading']['align']['responsive'] = ['min' => 'unregistered'];
        $this->assertSame('widget_schema_unreadable', $discovery->schema((object) ['widget' => 'heading', 'controls' => ['align']])['refusal']['code']);
    }

    public function test_replies_validate_against_the_registered_output_schemas(): void
    {
        foreach ([
            [$this->catalogue([]), false],
            [$this->schema(['widget' => 'heading']), true],
            [$this->schema(['widget' => 'heading', 'controls' => ['title', 'items', 'typography_font_size']]), true],
        ] as [$out, $schema]) {
            $definition = ElementorWidgetDiscovery::outputSchema($schema);
            $this->assertSame('string', $definition['properties']['builder']['type']);
            $value = json_decode(json_encode($out, JSON_THROW_ON_ERROR), true, 32, JSON_THROW_ON_ERROR);
            $this->assertTrue(rest_validate_value_from_schema($value, $definition));
        }
    }

    public function test_read_abilities_are_registered_with_closed_inputs_and_output_schemas(): void
    {
        $registered = [];
        Functions\when('wp_register_ability')->alias(static function ($name, $definition) use (&$registered) {
            $registered[$name] = $definition;
        });
        OwnAbilities::registerAbilities();
        foreach ([self::CATALOGUE, self::SCHEMA] as $name) {
            $this->assertTrue(OwnAbilities::has($name));
            $this->assertSame('read', OwnAbilities::abilityClass($name));
            $this->assertContains($name, OwnAbilities::readNames());
            $this->assertSame(OwnAbilities::inputSchema($name), $registered[$name]['input_schema']);
            $this->assertSame('object', $registered[$name]['output_schema']['type']);
            $this->assertTrue($registered[$name]['meta']['annotations']['readonly']);
            $input = $name === self::SCHEMA ? (object) ['widget' => 'heading'] : (object) [];
            $this->assertSame('builder_not_available', OwnAbilities::run($name, $input)['refusal']['code']);
        }
        foreach ([[], ['category' => 'basic', 'search' => 'heading']] as $valid) {
            $this->assertNull(OwnAbilities::validate(self::CATALOGUE, (object) $valid));
        }
        foreach ([['category' => null], ['search' => []], ['unexpected' => true]] as $bad) {
            $this->assertNotNull(OwnAbilities::validate(self::CATALOGUE, (object) $bad));
        }
        foreach ([['widget' => 'heading'], ['widget' => 'heading', 'controls' => ['title']]] as $valid) {
            $this->assertNull(OwnAbilities::validate(self::SCHEMA, (object) $valid));
        }
        foreach ([[], ['widget' => ''], ['widget' => 'heading', 'controls' => 'title'], ['widget' => 'heading', 'controls' => ['title', 'title']], ['widget' => 'heading', 'controls' => [1 => 'title']], ['widget' => 'heading', 'controls' => [null]], ['widget' => 'heading', 'extra' => true]] as $bad) {
            $this->assertNotNull(OwnAbilities::validate(self::SCHEMA, (object) $bad));
        }
        $this->assertNotNull(Autoloader::resolve(ElementorWidgetDiscovery::class));
    }

    /** @return array<string, mixed> */
    private function catalogue(array $input): array
    {
        $r = (new ElementorWidgetDiscovery($this->api))->catalogue((object) $input);
        $this->assertArrayHasKey('output', $r, json_encode($r));
        return $r['output'];
    }

    /** @return array<string, mixed> */
    private function schema(array $input): array
    {
        $r = (new ElementorWidgetDiscovery($this->api))->schema((object) $input);
        $this->assertArrayHasKey('output', $r, json_encode($r));
        return $r['output'];
    }

    /** @return array<string, array<string, mixed>> */
    private static function controls(): array
    {
        $content = ['tab' => 'content', 'section' => 'section_title'];
        $style = ['tab' => 'style', 'section' => 'section_style'];
        $group = ['groupType' => 'typography', 'groupPrefix' => 'typography_'];
        return [
            'section_title' => ['type' => 'section', 'tab' => 'content'],
            'title' => $content + ['type' => 'text', 'default' => ''],
            'size' => $content + ['type' => 'select', 'options' => ['h1' => 'Human option label', 'h2' => 'Human option label', 0 => 'Human option label'], 'default' => 'h2'],
            'items' => $content + ['type' => 'repeater', 'fields' => [
                ['name' => 'text', 'type' => 'text', 'default' => ''],
                ['name' => 'link', 'type' => 'url', 'default' => ['url' => '']],
                ['name' => 'kind', 'type' => 'select', 'options' => ['a' => 'Human option label', 'b' => 'Human option label']],
                ['name' => 'notice', 'type' => 'raw_html'],
                ['name' => 'custom_css', 'type' => 'code'],
            ]],
            'section_style' => ['type' => 'section', 'tab' => 'style'],
            'typography_typography' => $style + $group + ['type' => 'select', 'options' => ['' => 'Default', 'custom' => 'Custom']],
            'typography_font_size' => $style + $group + ['type' => 'slider', 'is_responsive' => true, 'responsive' => [], 'condition' => ['typography_typography' => 'custom'], 'size_units' => ['px', 'em'], 'range' => ['px' => ['min' => 1, 'max' => 100]]],
            'align' => $style + ['type' => 'choose', 'is_responsive' => true, 'responsive' => ['devices' => ['tablet', 'mobile']]],
            'align_tablet' => $style + ['type' => 'choose', 'is_responsive' => true, 'responsive' => ['devices' => ['tablet']]],
            'ui_heading' => $style + ['type' => 'heading'],
            'custom_css' => $style + ['type' => 'code'],
            'motion_fx_translate' => $style + ['type' => 'slider'],
            'sticky' => $style + ['type' => 'select'],
        ];
    }
}
