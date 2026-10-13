<?php
declare(strict_types=1);

namespace Elementor\Modules\NestedElements\Base {
    class Widget_Nested_Base extends \N12Widget {}
}
namespace Elementor\Modules\AtomicWidgets {
    class Widget extends \N12Widget {}
}
namespace Elementor\Modules\Promotions\Widgets {
    class Pro_Widget_Promotion extends \N12Widget
    {
        public function show_in_panel(): bool { return false; }
    }
}
namespace {
    require_once __DIR__ . '/dump-widgets.php';
    $kitSettings = ['site_title' => 'Example'];
    function get_post_meta(int $id, string $key, bool $single) { return $GLOBALS['kitSettings']; }
    function update_post_meta(int $id, string $key, array $settings): void { $GLOBALS['kitSettings'] = $settings; }

    class N12Widget
    {
        public function __construct(private string $name = 'heading') {}
        public function get_name(): string { return $this->name; }
        public function get_title(): string { return 'Heading'; }
        public static function get_type(): string { return 'widget'; }
        public function show_in_panel(): bool { return true; }
        public function get_categories(): array { return ['basic']; }
        public function get_controls(): array
        {
            return [
                'section' => ['type' => 'section'],
                'title' => ['type' => 'text', 'default' => 'Heading'],
                'align' => ['type' => 'choose', 'options' => ['left' => ['title' => 'Left'], 'right' => ['title' => 'Right']]],
                'enabled' => ['type' => 'switcher', 'return_value' => 'enabled'],
                'default_switch' => ['type' => 'switcher'],
                'tag' => ['type' => 'select', 'options' => ['h2' => 'H2', 'h3' => 'H3'], 'default' => 'h2'],
                'tags' => ['type' => 'select2', 'options' => [1 => 'First', 'other' => 'Other'], 'default' => []],
                'title_typography' => ['type' => 'popover_toggle', 'groupPrefix' => 'title_', 'return_value' => 'custom', 'default' => ''],
                'title_font_size' => ['type' => 'slider', 'groupType' => 'typography', 'groupPrefix' => 'title_',
                    'responsive' => ['max' => 'desktop'], 'condition' => ['title_typography' => 'custom']],
                'title_font_size_mobile' => ['type' => 'slider', 'groupType' => 'typography', 'groupPrefix' => 'title_',
                    'responsive' => ['max' => 'mobile'], 'parent' => 'title_font_size'],
                'title_font_size_tablet' => ['type' => 'slider', 'groupType' => 'typography', 'groupPrefix' => 'title_',
                    'responsive' => ['min' => 'mobile', 'max' => 'tablet'], 'parent' => 'title_font_size'],
                'border_border' => ['type' => 'select', 'options' => ['none' => 'None', 'solid' => 'Solid'], 'groupType' => 'border', 'groupPrefix' => 'border_'],
                'border_width' => ['type' => 'dimensions', 'groupType' => 'border', 'groupPrefix' => 'border_', 'condition' => ['border_border!' => 'none']],
                'shadow_box_shadow_type' => ['type' => 'popover_toggle', 'groupPrefix' => 'shadow_', 'return_value' => 'yes'],
                'shadow_box_shadow' => ['type' => 'box_shadow', 'groupType' => 'box-shadow', 'groupPrefix' => 'shadow_',
                    'conditions' => ['terms' => [['name' => 'shadow_box_shadow_type', 'value' => 'yes']]]],
                '_box_shadow_box_shadow_type' => ['type' => 'popover_toggle', 'groupPrefix' => '_box_shadow_'],
                'image_size_size' => ['type' => 'select', 'options' => ['medium' => 'Medium'], 'groupType' => 'image-size', 'groupPrefix' => 'image_size_'],
                'items' => ['type' => 'repeater', 'default' => [], 'fields' => [
                    ['name' => 'caption', 'type' => 'text', 'default' => ''],
                    ['name' => 'url', 'type' => 'url', 'default' => ['url' => '', 'is_external' => false]],
                    ['name' => 'details', 'type' => 'repeater', 'fields' => [['name' => 'text', 'type' => 'textarea']]],
                ]],
            ];
        }
    }
    class N12Nested extends \Elementor\Modules\NestedElements\Base\Widget_Nested_Base {}
    class N12Atomic extends \Elementor\Modules\AtomicWidgets\Widget {}
    class N12Section extends N12Widget
    {
        public static function get_type(): string { return 'section'; }
    }
    class N12Hidden extends N12Widget
    {
        public function show_in_panel(): bool { return false; }
    }
    class N12SpecialDocument extends N12Widget
    {
        public function get_categories(): array { return ['link-in-bio']; }
    }
    class N12General extends N12Widget
    {
        public function get_categories(): array { return ['general']; }
    }
    class N12Untitled extends N12Widget
    {
        public function get_title(): string { return ''; }
    }
    class N12NamedInternal extends N12Widget
    {
        public function get_controls(): array { throw new LogicException('internal controls must not be inspected'); }
    }
    class N12MalformedControl extends N12Widget
    {
        public function get_controls(): array { return ['title' => ['default' => 'Heading']]; }
    }
    class N12ControlProbe extends N12Widget
    {
        public function __construct(private array $controls) { parent::__construct(); }
        public function get_controls(): array { return $this->controls; }
    }
    class N12BadDefault extends N12Widget
    {
        public function get_controls(): array { return ['title' => ['type' => 'text', 'default' => static fn () => 'x']]; }
    }
    class N12Group
    {
        public function __construct(private string $family) {}
        public function get_options(): array
        {
            return $this->family === 'typography' ? ['popover' => ['starter_name' => 'typography']] : [];
        }
    }
    $manager = new class {
        public function get_control_groups(string $family): N12Group { return new N12Group($family); }
        public function get_control(string $type): object
        {
            return new class($type) {
                public function __construct(private string $type) {}
                public function get_default_value()
                {
                    return $this->type === 'slider' ? ['unit' => 'px', 'size' => ''] : '';
                }
            };
        }
    };
    $registry = [new N12Widget(), new N12Widget('html'), new N12Widget('e-heading'),
        new N12Widget('nested-tabs'), new N12Nested('accordion'), new N12Atomic('atomic-title'), new stdClass(),
        new N12Untitled('common-base'), new N12NamedInternal('common'), new N12NamedInternal('common-optimized'),
        new N12Untitled('untitled-core'), new \Elementor\Modules\Promotions\Widgets\Pro_Widget_Promotion('form'),
        new N12Section('inner-section'), new N12Hidden('contact-buttons'), new N12SpecialDocument('link-in-bio'),
        new N12General('image-box')];
    $plugin = (object) [
        'widgets_manager' => new class($registry) {
            public function __construct(public array $types) {}
            public function get_widget_types(): array { return $this->types; }
        },
        'controls_manager' => $manager,
        'kits_manager' => new class {
            public function get_active_id(): int { return 123; }
        },
        'breakpoints' => new class {
            public function get_breakpoints(): array { return ['mobile' => (object) [], 'tablet' => (object) [], 'laptop' => (object) []]; }
            public function get_active_breakpoints(): array { return $this->get_breakpoints(); }
        },
    ];
    $passed = 0;
    $check = static function (bool $ok, string $name) use (&$passed): void {
        if (!$ok) { throw new RuntimeException($name); }
        ++$passed;
        echo 'ok   ' . $name . "\n";
    };
    try {
        rt_widget_dump_prepare($plugin);
        foreach (['select', 'select2'] as $type) {
            $slides = rt_widget_dump_controls(['slides_to_show' => ['type' => $type, 'options' => [
                '' => 'Default', 1 => 1, 2 => 2.5, 3 => false, 4 => true,
            ]]], $manager, ['desktop', 'tablet', 'mobile']);
            $check($slides['controls'][0]['options'] === ['', '1', '2', '3', '4'], $type . ' exports numeric option keys as strings and accepts scalar labels');
        }
        $nonChoices = rt_widget_dump_controls([
            'external_url' => ['type' => 'url', 'options' => false],
            'link' => ['type' => 'url', 'options' => ['is_external', 'nofollow', 'custom_attributes']],
            'decoration' => ['type' => 'text', 'options' => ['feature' => 'Feature']],
        ], $manager, ['desktop', 'tablet', 'mobile']);
        $check(array_column($nonChoices['controls'], 'options') === [[], [], []], 'URL feature lists and non-choice options are omitted');
        $mask = rt_widget_dump_controls(['_mask_shape' => ['type' => 'visual_choice', 'options' => [
            'blob' => ['title' => 'Blob', 'image' => 'blob.svg'], 'circle' => ['title' => 'Circle'], 'custom' => ['title' => 'Custom'],
        ]]], $manager, ['desktop', 'tablet', 'mobile']);
        $check($mask['controls'][0]['options'] === ['blob', 'circle', 'custom'], 'custom choice types retain their option keys in the oracle');
        foreach (['select', 'select2', 'choose', 'visual_choice'] as $type) {
            $invalid = [false, 'invalid', ['bad' => null], ['bad' => new stdClass()], ['bad' => static fn () => 'label']];
            if (in_array($type, ['select', 'select2'], true)) $invalid[] = ['bad' => ['label']];
            foreach ($invalid as $options) {
                try {
                    rt_widget_dump_controls(['bad' => ['type' => $type, 'options' => $options]], $manager, ['desktop', 'tablet', 'mobile']);
                    throw new LogicException('malformed ' . $type . ' options accepted');
                } catch (RuntimeException $error) {
                    $check(str_contains($error->getMessage(), 'non-data options'), 'malformed ' . $type . ' option map fails');
                }
            }
        }
        $check($kitSettings === ['site_title' => 'Example', 'active_breakpoints' => ['viewport_mobile', 'viewport_tablet', 'viewport_laptop']],
            'dump preparation activates all offered breakpoints and preserves kit settings');
        $dump = rt_widget_dump_inventory($plugin, __DIR__, '3.20.4');
        $reasons = array_column($dump['skipped'], 'reason', 'name');
        foreach (['form' => 'pro_placeholder', 'inner-section' => 'not_a_widget',
            'contact-buttons' => 'hidden_from_panel', 'link-in-bio' => 'special_document_category'] as $name => $reason) {
            $check(($reasons[$name] ?? null) === $reason, $name . ' is skipped as ' . $reason);
        }
        $check(array_column($dump['widgets'], 'name') === ['heading', 'image-box'], 'only usable core classic panel widgets in basic or general are dumped');
        $check(($reasons['html'] ?? null) === 'denied_widget' && ($reasons['e-heading'] ?? null) === 'atomic_widget'
            && ($reasons['nested-tabs'] ?? null) === 'nested_widget', 'denied atomic and nested widget names remain excluded and audited');
        $internalSkipped = array_values(array_filter($dump['skipped'], static fn (array $entry): bool =>
            in_array($entry['reason'], ['internal_base_type', 'empty_title'], true)));
        $check($internalSkipped === [
            ['name' => 'common', 'reason' => 'internal_base_type'],
            ['name' => 'common-base', 'reason' => 'internal_base_type'],
            ['name' => 'common-optimized', 'reason' => 'internal_base_type'],
            ['name' => 'untitled-core', 'reason' => 'empty_title'],
        ],
            'untitled internal widgets are skipped with their names and reasons');
        $check($dump['elementor_version'] === '3.20.4', 'dump records the running version');
        $controls = array_column($dump['widgets'][0]['controls'], null, 'id');
        $check(!isset($controls['_box_shadow_box_shadow_type']) && $dump['widgets'][0]['controls_skipped'] === [
            ['id' => '_box_shadow_box_shadow_type', 'reason' => 'incomplete_group_metadata'],
            ['id' => 'section', 'reason' => 'ui_only_type'],
        ], 'incomplete group metadata is omitted and recorded without inference');
        $check(($controls['title_typography']['return_value'] ?? null) === 'custom' && ($controls['title_typography']['group']['activator'] ?? null) === 'title_typography', 'popover starter resolves its sole member family and return value');
        $check(($controls['enabled']['return_value'] ?? null) === 'enabled' && ($controls['default_switch']['return_value'] ?? null) === 'yes', 'switchers export custom and default return values');
        $check($controls['title']['type'] === 'text' && !isset($controls['section']) && !array_key_exists('default', $controls['title']), 'stored-value controls remain while UI and defaults are omitted');
        $check($controls['align']['options'] === ['left', 'right'] && $controls['tag']['options'] === ['h2', 'h3']
            && $controls['tags']['options'] === ['1', 'other'], 'select choose and select2 report only option keys');
        $check(!array_key_exists('default', $controls['title_font_size']), 'effective defaults are not exported');
        $check($controls['title_font_size']['responsive']['devices'] === ['desktop', 'mobile', 'tablet']
            && !isset($controls['title_font_size_tablet'], $controls['title_font_size_mobile']),
            'responsive windows fold registered override devices into the base');
        $check($controls['title_font_size']['group'] === ['family' => 'typography', 'prefix' => 'title_', 'activator' => 'title_typography'],
            'group popover starter is recorded on responsive members');
        $check($controls['border_width']['group']['activator'] === 'border_border', 'selector conditions name the group activator');
        $check($controls['shadow_box_shadow']['group']['activator'] === 'shadow_box_shadow_type', 'popover controls name the group activator');
        $check($controls['image_size_size']['group']['activator'] === null, 'unconditional groups have no activator');
        $fields = array_column($controls['items']['fields'], null, 'id');
        $check($fields['url']['type'] === 'url' && $fields['details']['fields'][0]['type'] === 'textarea', 'repeater fields retain types at every depth');
        $check(json_decode(json_encode($dump, JSON_THROW_ON_ERROR), true, 512, JSON_THROW_ON_ERROR) === $dump, 'dump contains plain JSON data');
        $partialGroups = [
            'family_only' => ['type' => 'text', 'groupType' => 'typography'],
            'null_prefix' => ['type' => 'text', 'groupType' => 'typography', 'groupPrefix' => null],
            'empty_family' => ['type' => 'text', 'groupType' => '', 'groupPrefix' => 'style_'],
        ];
        $plugin->widgets_manager->types = [new N12ControlProbe($partialGroups)];
        $partialDump = rt_widget_dump_inventory($plugin, __DIR__, '3.20.4');
        $check($partialDump['widgets'][0]['controls'] === [] && array_column($partialDump['widgets'][0]['controls_skipped'], 'id')
            === ['empty_family', 'family_only', 'null_prefix'], 'widgets with only incomplete group controls retain omission records without allowed controls');
        $plugin->widgets_manager->types = [new N12ControlProbe(['items' => ['type' => 'repeater', 'fields' => [
            ['name' => 'caption', 'type' => 'text'],
            ['name' => 'font', 'type' => 'font', 'groupType' => 'typography'],
        ]]])];
        $repeaterDump = rt_widget_dump_inventory($plugin, __DIR__, '3.20.4');
        $check($repeaterDump['widgets'][0]['controls'] === [] && $repeaterDump['widgets'][0]['controls_skipped']
            === [['id' => 'items', 'reason' => 'incomplete_group_metadata']], 'a repeater with incomplete field metadata is omitted as a whole');
        $invalidPartialGroups = [
            ['non-data default', ['type' => 'text', 'groupPrefix' => 'style_', 'default' => static fn () => 'x'], 'non-data default'],
            ['malformed options', ['type' => 'select', 'groupPrefix' => 'style_', 'options' => 'invalid'], 'non-data options'],
            ['malformed responsive window', ['type' => 'text', 'groupPrefix' => 'style_', 'responsive' => true], 'invalid responsive window'],
            ['malformed repeater fields', ['type' => 'repeater', 'groupPrefix' => 'style_'], 'has no fields'],
            ['malformed group value', ['type' => 'text', 'groupType' => [], 'groupPrefix' => 'style_'], 'invalid group metadata'],
        ];
        foreach ($invalidPartialGroups as [$label, $control, $message]) {
            $plugin->widgets_manager->types = [new N12ControlProbe(['invalid' => $control])];
            try {
                rt_widget_dump_inventory($plugin, __DIR__, '3.20.4');
                throw new LogicException($label . ' was accepted');
            } catch (RuntimeException $error) {
                $check(strpos($error->getMessage(), $message) !== false, 'incomplete group metadata does not hide a ' . $label);
            }
        }
        $plugin->widgets_manager->types = [new N12MalformedControl()];
        try {
            rt_widget_dump_inventory($plugin, __DIR__, '3.20.4');
            throw new LogicException('malformed widget control was accepted');
        } catch (RuntimeException $error) {
            $check($error->getMessage() === 'control title has no type', 'a real widget with a malformed control fails loudly');
        }
        $plugin->widgets_manager->types = [new N12Widget(), new N12Untitled()];
        try {
            rt_widget_dump_inventory($plugin, __DIR__, '3.20.4');
            throw new LogicException('duplicate widget name was accepted');
        } catch (RuntimeException $error) {
            $check($error->getMessage() === 'duplicate registered widget heading', 'skipping untitled types does not hide duplicate widget names');
        }
        $plugin->widgets_manager->types = [new N12BadDefault()];
        try {
            rt_widget_dump_inventory($plugin, __DIR__, '3.20.4');
            throw new LogicException('non-data default was accepted');
        } catch (RuntimeException $error) {
            $check(strpos($error->getMessage(), 'non-data default') !== false, 'non-data defaults fail loudly');
        }
        $plugin->widgets_manager->types = [];
        try {
            rt_widget_dump_inventory($plugin, __DIR__, '3.20.4');
            throw new LogicException('empty widget registry was accepted');
        } catch (RuntimeException $error) {
            $check(strpos($error->getMessage(), 'no widgets') !== false, 'empty widget registry fails loudly');
        }
        echo 'widget-dump_test: ' . $passed . " passed\n";
    } catch (Throwable $error) {
        fwrite(STDERR, 'FAIL widget-dump_test: ' . $error->getMessage() . "\n");
        exit(1);
    }
}
