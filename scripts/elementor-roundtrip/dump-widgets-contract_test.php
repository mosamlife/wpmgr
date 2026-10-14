<?php
declare(strict_types=1);

namespace Elementor\Core\Frontend {
    class Performance
    {
        public static bool $optimized = false;
        public static bool $styles = false;
        public static array $styleCalls = [];
        public static function should_optimize_controls(): bool { return self::$optimized; }
        public static function set_use_style_controls(bool $value): void
        {
            self::$styles = $value;
            self::$styleCalls[] = $value;
        }
    }
}
namespace {
    require_once __DIR__ . '/dump-widgets.php';
    use Elementor\Core\Frontend\Performance;
    $moduleLoaded = false;
    function did_action(string $hook): int { return $GLOBALS['moduleLoaded'] ? 1 : 0; }

    class N12ContractWidget
    {
        public $onRead = null;
        public function __construct(private string $name, public array $controls) {}
        public function get_name(): string { return $this->name; }
        public function get_title(): string { return ucfirst($this->name); }
        public static function get_type(): string { return 'widget'; }
        public function show_in_panel(): bool { return true; }
        public function get_categories(): array { return ['basic']; }
        public function get_controls(): array
        {
            if (is_callable($this->onRead)) ($this->onRead)();
            if (!Performance::$styles) {
                return ['header_size' => ['type' => 'select', 'options' => []]];
            }
            return $this->controls;
        }
    }
    class N12ContractLegacyWidget extends N12ContractWidget
    {
        public function get_controls(): array { return $this->controls; }
    }
    $raw = [
        'header_size' => ['type' => 'select', 'options' => array_fill_keys(['h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'div', 'span', 'p'], 'Tag')],
        'title_color' => ['type' => 'color'],
        'typography_typography' => ['type' => 'popover_toggle', 'return_value' => 'custom', 'groupType' => 'typography', 'groupPrefix' => 'typography_'],
        'typography_font_family' => ['type' => 'font', 'groupType' => 'typography', 'groupPrefix' => 'typography_'],
        '_margin' => ['type' => 'dimensions', 'responsive' => ['max' => 'desktop']],
        '_margin_tablet' => ['type' => 'dimensions', 'parent' => '_margin', 'responsive' => ['min' => 'mobile', 'max' => 'tablet']],
        '_margin_mobile' => ['type' => 'dimensions', 'parent' => '_margin', 'responsive' => ['max' => 'mobile']],
        '_padding' => ['type' => 'dimensions', 'responsive' => ['max' => 'desktop']],
        '_padding_tablet' => ['type' => 'dimensions', 'parent' => '_padding', 'responsive' => ['max' => 'tablet']],
        '_padding_mobile' => ['type' => 'dimensions', 'parent' => '_padding', 'responsive' => ['max' => 'mobile']],
        'hidden_value' => ['type' => 'hidden'],
    ];
    $uiTypes = ['section', 'tab', 'tabs', 'heading', 'divider', 'raw_html', 'alert', 'notice', 'deprecated_notice', 'button'];
    foreach ($uiTypes as $type) $raw['ui_' . $type] = ['type' => $type];
    $choices = [];
    for ($i = 0; $i < 200; ++$i) $choices['choice_' . $i] = ['type' => 'choose', 'options' => ['yes' => 'Yes']];
    $heading = new N12ContractWidget('heading', $raw);
    $button = new N12ContractWidget('button', ['link' => ['type' => 'url']] + $choices);
    $image = new N12ContractWidget('image', ['image' => ['type' => 'media']]);
    $manager = new class {
        public function get_control(string $type): object { return new class { public function get_default_value(): string { return ''; } }; }
        public function get_control_groups(string $family): object
        {
            return new class { public function get_options(): array { return ['popover' => ['starter_name' => 'typography']]; } };
        }
    };
    $plugin = (object) [
        'widgets_manager' => new class([$heading, $button, $image]) {
            public function __construct(public array $types) {}
            public function get_widget_types(): array { return $this->types; }
        },
        'controls_manager' => $manager,
        'breakpoints' => new class {
            public function get_breakpoints(): array { return ['mobile' => [], 'tablet' => [], 'laptop' => []]; }
            public function get_active_breakpoints(): array { return ['mobile' => [], 'tablet' => []]; }
        },
    ];
    $passed = 0;
    $failed = 0;
    function n12_demand(bool $ok, string $message): void { if (!$ok) throw new RuntimeException($message); }
    function n12_reject(callable $run, string $message): void
    {
        try { $run(); } catch (RuntimeException $error) {
            n12_demand(strpos($error->getMessage(), $message) !== false, 'wrong failure: ' . $error->getMessage());
            return;
        }
        throw new RuntimeException('invalid dump was accepted');
    }
    $test = static function (string $name, callable $run) use (&$passed, &$failed): void {
        try { $run(); ++$passed; echo 'ok   ' . $name . "\n"; }
        catch (Throwable $error) { ++$failed; fwrite(STDERR, 'FAIL ' . $name . ': ' . $error->getMessage() . "\n"); }
    };
    $test('dump context is established before WordPress loads', static function (): void {
        n12_demand(function_exists('rt_widget_dump_boot_context'), 'missing early editor context');
        $context = rt_widget_dump_boot_context();
        n12_demand(defined('WP_ADMIN') && WP_ADMIN === true && $context['method'] === 'wp_admin_before_wp_load', 'wrong editor context');
    });
    $test('optimised control registration is refused', static function () use ($plugin): void {
        Performance::$optimized = true;
        try { n12_reject(static fn () => rt_widget_dump($plugin, __DIR__, '3.35.9'), 'optimised'); }
        finally { Performance::$optimized = false; }
    });
    $test('a previously booted Elementor cannot change registration context', static function (): void {
        $GLOBALS['moduleLoaded'] = true;
        try { n12_reject(static fn () => rt_widget_dump_boot_context(), 'precede Elementor boot'); }
        finally { $GLOBALS['moduleLoaded'] = false; }
    });
    $test('stripped positive-control fixture fails', static function () use ($plugin, $heading): void {
        $full = $heading->controls;
        $heading->controls = ['header_size' => ['type' => 'select', 'options' => []]];
        Performance::$styles = true;
        try { n12_reject(static fn () => rt_widget_dump($plugin, __DIR__, '3.35.9'), 'positive control heading.header_size'); }
        finally { $heading->controls = $full; Performance::$styles = false; }
    });
    $test('responsive variants are folded into their base control', static function () use ($raw, $manager): void {
        $schema = rt_widget_dump_controls($raw, $manager, ['desktop', 'mobile', 'tablet']);
        $controls = array_column($schema['controls'], null, 'id');
        n12_demand(!isset($controls['_margin_tablet']) && !isset($controls['_margin_mobile']), 'device variants remain as separate controls');
        n12_demand($controls['_margin']['responsive']['devices'] === ['desktop', 'mobile', 'tablet'], 'registered devices were lost');
    });
    $chain = [
        'width' => ['type' => 'slider', 'parent' => null, 'responsive' => ['max' => 'desktop']],
        'width_laptop' => ['type' => 'slider', 'parent' => 'width', 'responsive' => ['max' => 'laptop']],
        'width_tablet_extra' => ['type' => 'slider', 'parent' => 'width_laptop', 'responsive' => ['max' => 'tablet_extra']],
    ];
    $test('a three-step responsive parent chain folds to its root in any input order', static function () use ($chain, $manager): void {
        $schema = rt_widget_dump_controls(array_reverse($chain, true), $manager, ['desktop', 'laptop', 'tablet_extra']);
        n12_demand(count($schema['controls']) === 1 && $schema['controls'][0]['id'] === 'width', 'chain did not fold to root');
        n12_demand($schema['controls'][0]['responsive']['devices'] === ['desktop', 'laptop', 'tablet_extra'], 'chain devices were lost');
        n12_demand(($schema['controls'][0]['responsive']['source'] ?? null) === 'duplicates', 'duplicate window source missing');
    });
    $test('a widescreen min variant folds and the next device can point to the root', static function () use ($chain, $manager): void {
        $controls = array_slice($chain, 0, 2, true) + ['width_widescreen' => ['type' => 'slider', 'parent' => 'width', 'responsive' => ['min' => 'widescreen']]];
        $schema = rt_widget_dump_controls($controls, $manager, ['desktop', 'laptop', 'tablet_extra', 'widescreen']);
        n12_demand(count($schema['controls']) === 1, 'min variant remains separate');
        n12_demand($schema['controls'][0]['responsive']['devices'] === ['desktop', 'laptop', 'widescreen'], 'widescreen device missing');
    });
    $test('a single-control responsive window uses its declared devices', static function () use ($manager): void {
        $schema = rt_widget_dump_controls(['width' => ['type' => 'slider', 'is_responsive' => true,
            'responsive' => ['devices' => ['tablet_extra', 'desktop']]]], $manager, ['desktop', 'laptop', 'tablet_extra']);
        n12_demand($schema['controls'][0]['responsive']['devices'] === ['desktop', 'tablet_extra'], 'declared single-control window changed');
        n12_demand(($schema['controls'][0]['responsive']['source'] ?? null) === 'single_control', 'single-control window source missing');
    });
    $test('a single-control responsive window without devices uses every active device', static function () use ($manager): void {
        foreach ([[], ['responsive' => []]] as $window) {
            $schema = rt_widget_dump_controls(['width' => ['type' => 'slider', 'is_responsive' => true] + $window], $manager, ['desktop', 'laptop', 'tablet_extra']);
            n12_demand(($schema['controls'][0]['responsive']['devices'] ?? null) === ['desktop', 'laptop', 'tablet_extra'], 'active single-control window missing');
        }
    });
    $test('single-control inventory uses active breakpoints rather than every offered breakpoint', static function () use ($plugin, $heading): void {
        $original = $heading->controls;
        $heading->controls['single_width'] = ['type' => 'slider', 'is_responsive' => true];
        try {
            $dump = rt_widget_dump($plugin, __DIR__, '3.35.9');
            $controls = array_column(array_column($dump['widgets'], null, 'name')['heading']['controls'], null, 'id');
            n12_demand(($controls['single_width']['responsive']['devices'] ?? null) === ['desktop', 'mobile', 'tablet'], 'inactive breakpoint entered the window');
        } finally { $heading->controls = $original; }
    });
    foreach (['cycle', 'missing parent', 'wrong variant id'] as $fault) {
        $test('a responsive chain with ' . $fault . ' is fatal', static function () use ($chain, $manager, $fault): void {
            $broken = $chain;
            if ($fault === 'cycle') $broken['width_laptop']['parent'] = 'width_tablet_extra';
            if ($fault === 'missing parent') $broken['width_tablet_extra']['parent'] = 'absent';
            if ($fault === 'wrong variant id') {
                $broken['other_tablet_extra'] = $broken['width_tablet_extra'];
                unset($broken['width_tablet_extra']);
            }
            n12_reject(static fn () => rt_widget_dump_controls($broken, $manager, ['desktop', 'laptop', 'tablet_extra']), 'invalid responsive parent');
        });
    }
    $test('a parented control without a responsive device is fatal', static function () use ($manager): void {
        n12_reject(static fn () => rt_widget_dump_controls([
            'width' => ['type' => 'slider', 'responsive' => ['max' => 'desktop']],
            'width_mobile' => ['type' => 'slider', 'parent' => 'width'],
        ], $manager, ['desktop', 'mobile']), 'invalid responsive parent');
    });
    $test('a single-control root cannot also have duplicated variants', static function () use ($manager): void {
        n12_reject(static fn () => rt_widget_dump_controls([
            'width' => ['type' => 'slider', 'is_responsive' => true, 'responsive' => ['devices' => ['desktop', 'mobile']]],
            'width_mobile' => ['type' => 'slider', 'parent' => 'width', 'responsive' => ['max' => 'mobile']],
        ], $manager, ['desktop', 'mobile']), 'invalid responsive parent');
    });
    $test('responsive parent traversal has a step cap', static function () use ($manager): void {
        $controls = ['width' => ['type' => 'slider', 'parent' => null, 'responsive' => ['max' => 'desktop']]];
        $devices = ['desktop'];
        $parent = 'width';
        for ($i = 0; $i < 65; ++$i) {
            $device = 'device_' . $i;
            $id = 'width_' . $device;
            $controls[$id] = ['type' => 'slider', 'parent' => $parent, 'responsive' => ['max' => $device]];
            $devices[] = $device;
            $parent = $id;
        }
        n12_reject(static fn () => rt_widget_dump_controls(array_reverse($controls, true), $manager, $devices), 'responsive parent chain limit');
    });
    $test('UI controls are dropped while activators and hidden values remain', static function () use ($raw, $manager, $uiTypes): void {
        $schema = rt_widget_dump_controls($raw, $manager, ['desktop', 'mobile', 'tablet']);
        $controls = array_column($schema['controls'], null, 'id');
        foreach ($uiTypes as $type) n12_demand(!isset($controls['ui_' . $type]), 'UI type was retained: ' . $type);
        n12_demand(isset($controls['typography_typography'], $controls['hidden_value']), 'stored-value controls were dropped');
    });
    $test('style reads and registration checks are recorded and restored', static function () use ($plugin): void {
        Performance::$styleCalls = [];
        $dump = rt_widget_dump($plugin, __DIR__, '3.35.9');
        n12_demand(Performance::$styleCalls === [true, false] && !Performance::$styles, 'style-control read was not bracketed');
        n12_demand($dump['registration_context']['should_optimize_controls'] === false, 'non-optimised registration was not recorded');
        n12_demand($dump['positive_controls']['option_controls'] >= 200, 'options were not counted');
        $walk = static function (array $value) use (&$walk): void {
            n12_demand(!array_key_exists('default', $value), 'default leaked into output');
            foreach ($value as $child) if (is_array($child)) $walk($child);
        };
        $walk($dump);
        $header = array_column(array_column($dump['widgets'], null, 'name')['heading']['controls'], null, 'id')['header_size'];
        n12_demand($header['options'] === ['div', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'p', 'span'], 'option keys are not sorted');
    });
    $test('a flag that becomes true while a stack is read fails and restores style mode', static function () use ($plugin, $heading): void {
        $heading->onRead = static function (): void { Performance::$optimized = true; };
        try {
            n12_reject(static fn () => rt_widget_dump($plugin, __DIR__, '3.35.9'), 'optimised');
            n12_demand(!Performance::$styles, 'style mode was left active on failure');
        } finally { $heading->onRead = null; Performance::$optimized = false; }
    });
    $good = rt_widget_dump($plugin, __DIR__, '3.35.9');
    $without = static function (array $dump, string $widget, string $id): array {
        foreach ($dump['widgets'] as &$entry) {
            if ($entry['name'] === $widget) {
                $entry['controls'] = array_values(array_filter($entry['controls'], static fn (array $control): bool => $control['id'] !== $id));
            }
        }
        return $dump;
    };
    foreach (['heading' => ['header_size', 'title_color', '_margin', '_padding'], 'button' => ['link'], 'image' => ['image']] as $widget => $ids) {
        foreach ($ids as $id) {
            $test('missing ' . $widget . '.' . $id . ' fails its positive check', static function () use ($good, $without, $widget, $id): void {
                n12_reject(static fn () => rt_widget_dump_positive_controls($without($good, $widget, $id)), 'positive control ' . $widget . '.' . $id);
            });
        }
    }
    foreach (['_margin', '_padding'] as $id) {
        $test('missing ' . $id . ' responsive window fails', static function () use ($good, $id): void {
            $bad = $good;
            foreach ($bad['widgets'] as &$entry) {
                if ($entry['name'] === 'heading') {
                    foreach ($entry['controls'] as &$control) if ($control['id'] === $id) $control['responsive'] = null;
                    unset($control);
                }
            }
            unset($entry);
            n12_reject(static fn () => rt_widget_dump_positive_controls($bad), 'needs a responsive window');
        });
    }
    foreach (['typography_typography', 'typography_font_family'] as $id) {
        $test('missing typography member or activator fails: ' . $id, static function () use ($good, $without, $id): void {
            n12_reject(static fn () => rt_widget_dump_positive_controls($without($good, 'heading', $id)), 'typography needs its group activator');
        });
    }
    $test('option coverage floor rejects below the floor and accepts the exact floor', static function () use ($good, $without): void {
        $below = $without($without($good, 'button', 'choice_199'), 'button', 'choice_198');
        n12_reject(static fn () => rt_widget_dump_positive_controls($below), 'positive control options');
        $exact = rt_widget_dump_positive_controls($without($good, 'button', 'choice_199'));
        n12_demand($exact['option_controls'] === 200, 'exact option coverage floor was rejected');
    });
    $test('older pins without the optimisation API record its absence and validate full controls', static function () use ($plugin, $heading, $button, $image): void {
        $legacy = clone $plugin;
        $legacy->widgets_manager = clone $plugin->widgets_manager;
        $legacy->widgets_manager->types = array_map(static fn ($widget) => new N12ContractLegacyWidget($widget->get_name(), $widget->controls), [$heading, $button, $image]);
        $dump = rt_widget_dump($legacy, __DIR__, '3.20.4', stdClass::class);
        n12_demand($dump['registration_context']['optimization_check'] === 'api_not_available'
            && $dump['registration_context']['registration_checks'] === 0, 'legacy availability was not recorded accurately');
    });
    $test('an unavailable required optimisation API fails loudly', static function () use ($plugin): void {
        n12_reject(static fn () => rt_widget_dump($plugin, __DIR__, '3.35.9', stdClass::class), 'optimisation API is unavailable');
    });
    echo 'widget-dump-contract_test: ' . $passed . ' passed, ' . $failed . " failed\n";
    exit($failed === 0 ? 0 : 1);
}
