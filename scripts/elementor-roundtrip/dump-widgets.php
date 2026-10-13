<?php
declare(strict_types=1);

/** Select editor registration before WordPress can load Elementor or build a stack. */
function rt_widget_dump_boot_context(): array
{
    if (class_exists('Elementor\\Plugin', false)
        || (function_exists('did_action') && did_action('elementor/loaded') > 0)) {
        throw new RuntimeException('widget dump editor context must precede Elementor boot');
    }
    if (defined('WP_ADMIN') && WP_ADMIN !== true) {
        throw new RuntimeException('widget dump requires WP_ADMIN before wp-load.php');
    }
    if (!defined('WP_ADMIN')) {
        define('WP_ADMIN', true);
    }
    return ['method' => 'wp_admin_before_wp_load', 'wp_admin' => true];
}

/** Prepare the throwaway site's kit before a fresh request registers responsive controls. */
function rt_widget_dump_prepare($plugin): void
{
    $breakpoints = $plugin->breakpoints->get_breakpoints();
    $kitId = $plugin->kits_manager->get_active_id();
    if (!is_array($breakpoints) || $breakpoints === [] || !is_numeric($kitId) || (int) $kitId < 1) {
        throw new RuntimeException('widget dump cannot prepare the active kit');
    }
    $settings = get_post_meta((int) $kitId, '_elementor_page_settings', true);
    if ($settings === '') {
        $settings = [];
    }
    if (!is_array($settings)) {
        throw new RuntimeException('widget dump kit settings are not an array');
    }
    $names = array_values(array_diff(array_keys($breakpoints), ['desktop']));
    $settings['active_breakpoints'] = array_map(static fn (string $name): string => 'viewport_' . $name, $names);
    update_post_meta((int) $kitId, '_elementor_page_settings', $settings);
    if (get_post_meta((int) $kitId, '_elementor_page_settings', true) !== $settings) {
        throw new RuntimeException('widget dump could not activate all breakpoints');
    }
}

/** Keep only values that have a plain JSON representation. */
function rt_widget_plain($value)
{
    if ($value === null || is_string($value) || is_bool($value) || is_int($value)
        || (is_float($value) && is_finite($value))) {
        return $value;
    }
    if (is_array($value) || $value instanceof stdClass) {
        $out = $value instanceof stdClass ? new stdClass() : [];
        foreach ($value as $key => $child) {
            if (is_object($out)) {
                $out->{$key} = rt_widget_plain($child);
            } else {
                $out[$key] = rt_widget_plain($child);
            }
        }
        return $out;
    }
    throw new RuntimeException('a widget control contains a non-data default');
}

/** The group activator is its popover starter or its selector used by other group fields. */
function rt_widget_group_activator(string $family, string $prefix, array $controls, $manager): ?string
{
    $group = $manager->get_control_groups($family);
    if (!is_object($group)) {
        throw new RuntimeException('unregistered control group ' . $family);
    }
    if (is_callable([$group, 'get_options'])) {
        $options = $group->get_options();
        $starter = $options['popover']['starter_name'] ?? null;
        if (is_string($starter) && isset($controls[$prefix . $starter])) {
            return $prefix . $starter;
        }
    }
    $references = [];
    $walk = static function ($conditions) use (&$walk, &$references, $prefix): void {
        if (!is_array($conditions)) {
            return;
        }
        foreach ($conditions as $key => $value) {
            // Simple conditions use keys; compound conditions use terms with a name.
            $name = $key === 'name' && is_string($value) ? $value : (string) $key;
            $name = rtrim($name, '!');
            $name = (string) preg_replace('/\[.*$/', '', $name);
            if (strpos($name, $prefix) === 0) {
                $references[$name] = ($references[$name] ?? 0) + 1;
            }
            if (is_array($value)) {
                $walk($value);
            }
        }
    };
    foreach ($controls as $control) {
        if (($control['groupPrefix'] ?? null) === $prefix && ($control['groupType'] ?? null) === $family) {
            $walk($control['condition'] ?? []);
            $walk($control['conditions'] ?? []);
        }
    }
    $candidates = [];
    foreach ($controls as $id => $control) {
        if (($control['groupPrefix'] ?? null) !== $prefix || ($control['groupType'] ?? null) !== $family) {
            continue;
        }
        if (($control['type'] ?? null) === 'popover_toggle') {
            return (string) $id;
        }
        if (isset($references[$id]) && in_array($control['type'] ?? null, ['select', 'choose', 'switcher'], true)) {
            $candidates[$id] = $references[$id];
        }
    }
    arsort($candidates, SORT_NUMERIC);
    return $candidates === [] ? null : (string) array_key_first($candidates);
}

/** Responsive windows come from duplicated device controls or one stored responsive control. */
function rt_widget_responsive_window(string $id, array $control, array $devices): ?array
{
    if (isset($control['is_responsive']) && !is_bool($control['is_responsive'])) {
        throw new RuntimeException('control ' . $id . ' has an invalid responsive flag');
    }
    $single = ($control['is_responsive'] ?? false) === true;
    $window = $control['responsive'] ?? null;
    if (!$single && ($window === null || $window === false)) {
        return null;
    }
    if ($single && $window === null) {
        $window = [];
    }
    if (!is_array($window)) {
        throw new RuntimeException('control ' . $id . ' has an invalid responsive window');
    }
    foreach (['min', 'max'] as $bound) {
        if (isset($window[$bound]) && (!is_string($window[$bound]) || !in_array($window[$bound], $devices, true))) {
            throw new RuntimeException('control ' . $id . ' names an unknown breakpoint');
        }
    }
    $declared = $window['devices'] ?? null;
    if (array_key_exists('devices', $window)) {
        if (!is_array($declared) || $declared === []) {
            throw new RuntimeException('control ' . $id . ' has invalid responsive devices');
        }
        foreach ($declared as $device) {
            if (!is_string($device) || !in_array($device, $devices, true)) {
                throw new RuntimeException('control ' . $id . ' names an unknown breakpoint');
            }
        }
    }
    if ($single) {
        $allowed = $declared ?? $devices;
    } else {
        $device = $window['max'] ?? ($window['min'] ?? null);
        if ($device === null || ($declared !== null && !in_array($device, $declared, true))) {
            throw new RuntimeException('control ' . $id . ' has an invalid responsive window');
        }
        $allowed = [$device];
    }
    return ['min' => $window['min'] ?? null, 'max' => $window['max'] ?? null,
        'devices' => $allowed, 'source' => $single ? 'single_control' : 'duplicates'];
}

/** Resolve the raw parent chain independently of control order and projected omissions. */
function rt_widget_responsive_root(string $id, array $named, array $windows): string
{
    $root = $id;
    $path = [];
    while (($parent = $named[$root]['parent'] ?? null) !== null) {
        if (isset($path[$root]) || !is_string($parent) || $parent === '' || !isset($named[$parent])) {
            throw new RuntimeException('control ' . $id . ' has an invalid responsive parent');
        }
        if (count($path) >= 64) {
            throw new RuntimeException('control ' . $id . ' exceeds the responsive parent chain limit');
        }
        $path[$root] = true;
        $root = $parent;
    }
    if (($windows[$root]['source'] ?? null) !== 'duplicates') {
        throw new RuntimeException('control ' . $id . ' has an invalid responsive parent');
    }
    foreach ($path as $variant => $_) {
        if (($windows[$variant]['source'] ?? null) !== 'duplicates'
            || $variant !== $root . '_' . $windows[$variant]['devices'][0]) {
            throw new RuntimeException('control ' . $id . ' has an invalid responsive parent');
        }
    }
    return $root;
}

/** Dump complete control definitions and separately report omitted control ids. */
function rt_widget_dump_controls(array $controls, $manager, array $devices): array
{
    $named = [];
    foreach ($controls as $key => $control) {
        if (!is_array($control)) {
            throw new RuntimeException('a widget control is not an array');
        }
        $id = is_string($key) ? $key : ($control['name'] ?? null);
        if (!is_string($id) || $id === '' || isset($named[$id])) {
            throw new RuntimeException('a widget control has a missing or duplicate id');
        }
        $named[$id] = $control;
    }
    $groups = [];
    $out = [];
    $skipped = [];
    $windows = [];
    foreach ($named as $id => $control) {
        $type = $control['type'] ?? null;
        if (!is_string($type) || $type === '') {
            throw new RuntimeException('control ' . $id . ' has no type');
        }
        if (in_array($type, ['section', 'tab', 'tabs', 'heading', 'divider', 'raw_html', 'alert', 'notice', 'deprecated_notice', 'button'], true)) {
            $skipped[] = ['id' => $id, 'reason' => 'ui_only_type'];
            continue;
        }
        $options = [];
        if (in_array($type, ['select', 'select2', 'choose'], true)) {
            if (!is_array($control['options'] ?? [])) {
                throw new RuntimeException('control ' . $id . ' has non-data options');
            }
            $options = array_map('strval', array_keys($control['options'] ?? []));
            sort($options, SORT_STRING);
        }
        if (array_key_exists('default', $control)) {
            rt_widget_plain($control['default']);
        }
        $responsive = rt_widget_responsive_window($id, $control, $devices);
        $windows[$id] = $responsive;
        $fields = [];
        $incompleteFields = false;
        if ($type === 'repeater') {
            if (!isset($control['fields']) || !is_array($control['fields'])) {
                throw new RuntimeException('repeater ' . $id . ' has no fields');
            }
            $fieldSchema = rt_widget_dump_controls($control['fields'], $manager, $devices);
            $fields = $fieldSchema['controls'];
            $incompleteFields = array_filter($fieldSchema['controls_skipped'], static fn (array $entry): bool =>
                $entry['reason'] === 'incomplete_group_metadata') !== [];
        }
        $group = null;
        $incompleteGroup = false;
        if (array_key_exists('groupType', $control) || array_key_exists('groupPrefix', $control)) {
            $family = $control['groupType'] ?? null;
            $prefix = $control['groupPrefix'] ?? null;
            if (($family !== null && !is_string($family)) || ($prefix !== null && !is_string($prefix))) {
                throw new RuntimeException('control ' . $id . ' has invalid group metadata');
            }
            $incompleteGroup = $family === null || $prefix === null || $family === '' || $prefix === '';
            if (!$incompleteGroup) {
                $key = $family . ':' . $prefix;
                if (!array_key_exists($key, $groups)) {
                    $groups[$key] = rt_widget_group_activator($family, $prefix, $named, $manager);
                }
                $group = ['family' => $family, 'prefix' => $prefix, 'activator' => $groups[$key]];
            }
        }
        // Omit a repeater as a whole when any field lacks a complete definition.
        // Other malformed data is validated before recording a metadata omission.
        if ($incompleteGroup || $incompleteFields) {
            $skipped[] = ['id' => $id, 'reason' => 'incomplete_group_metadata'];
            continue;
        }
        $out[$id] = ['id' => $id, 'type' => $type, 'options' => $options,
            'responsive' => $responsive, 'group' => $group, 'fields' => $fields];
    }
    $roots = [];
    foreach ($named as $id => $control) {
        if (($control['parent'] ?? null) === null) {
            continue;
        }
        $roots[$id] = rt_widget_responsive_root($id, $named, $windows);
    }
    foreach ($roots as $id => $root) {
        if (!isset($out[$id])) {
            continue;
        }
        if (!isset($out[$root])) {
            $skipped[] = ['id' => $id, 'reason' => 'incomplete_group_metadata'];
            unset($out[$id]);
            continue;
        }
        if ($out[$id]['type'] !== $out[$root]['type']
            || $out[$id]['options'] !== $out[$root]['options'] || $out[$id]['group'] !== $out[$root]['group']
            || $out[$id]['fields'] !== $out[$root]['fields']) {
            throw new RuntimeException('control ' . $id . ' differs from its responsive base');
        }
        $out[$root]['responsive']['devices'][] = $windows[$id]['devices'][0];
        unset($out[$id]);
    }
    foreach ($out as &$control) {
        if ($control['responsive'] !== null) {
            $control['responsive']['devices'] = array_values(array_unique($control['responsive']['devices']));
            sort($control['responsive']['devices'], SORT_STRING);
        }
    }
    unset($control);
    $out = array_values($out);
    usort($out, static fn (array $a, array $b): int => strcmp($a['id'], $b['id']));
    usort($skipped, static fn (array $a, array $b): int => strcmp($a['id'], $b['id']));
    return ['controls' => $out, 'controls_skipped' => $skipped];
}

/** Registered classic widgets whose concrete implementation belongs to the pinned core. */
function rt_widget_dump_inventory($plugin, string $corePath, string $version, ?callable $registrationCheck = null): array
{
    $root = realpath($corePath);
    if ($root === false) {
        throw new RuntimeException('Elementor core path is unreadable');
    }
    $types = $plugin->widgets_manager->get_widget_types();
    if (!is_array($types) || $types === []) {
        throw new RuntimeException('Elementor registered no widgets');
    }
    $breakpoints = $plugin->breakpoints->get_breakpoints();
    $activeBreakpoints = $plugin->breakpoints->get_active_breakpoints();
    if (!is_array($breakpoints) || $breakpoints === [] || !is_array($activeBreakpoints) || $activeBreakpoints === []
        || array_diff(array_keys($activeBreakpoints), array_keys($breakpoints)) !== []) {
        throw new RuntimeException('Elementor registered no breakpoints');
    }
    $devices = array_values(array_unique(array_merge(['desktop'], array_keys($activeBreakpoints))));
    $widgets = [];
    $skipped = [];
    $seen = [];
    foreach ($types as $widget) {
        $class = new ReflectionClass($widget);
        $file = $class->getFileName();
        $file = is_string($file) ? realpath($file) : false;
        if ($file === false || strpos($file, $root . DIRECTORY_SEPARATOR) !== 0) {
            if (is_callable([$widget, 'get_name'])) {
                $outsideName = $widget->get_name();
                if (is_string($outsideName) && $outsideName !== '') {
                    $skipped[] = ['name' => $outsideName, 'reason' => 'not_core'];
                }
            }
            continue;
        }
        $name = $widget->get_name();
        if (!is_string($name) || $name === '') {
            throw new RuntimeException('a registered widget has no name');
        }
        if (isset($seen[$name])) {
            throw new RuntimeException('duplicate registered widget ' . $name);
        }
        $seen[$name] = true;
        $atomic = strpos($name, 'e-') === 0;
        $nested = strpos($name, 'nested-') === 0;
        do {
            $nested = $nested || strpos($class->getName(), 'Elementor\\Modules\\Nested') === 0;
            $atomic = $atomic || strpos($class->getName(), 'Elementor\\Modules\\AtomicWidgets\\') === 0;
        } while ($class = $class->getParentClass());
        if ($atomic || $nested) {
            $skipped[] = ['name' => $name, 'reason' => $atomic ? 'atomic_widget' : 'nested_widget'];
            continue;
        }
        if (in_array($name, ['html', 'shortcode', 'template', 'text-path', 'menu-anchor', 'sidebar'], true)
            || strpos($name, 'wp-widget-') === 0) {
            $skipped[] = ['name' => $name, 'reason' => 'denied_widget'];
            continue;
        }
        // Shared base types and untitled types are not page widgets.
        if (in_array($name, ['common', 'common-base', 'common-optimized'], true)) {
            $skipped[] = ['name' => $name, 'reason' => 'internal_base_type'];
            continue;
        }
        $promotionClass = 'Elementor\\Modules\\Promotions\\Widgets\\Pro_Widget_Promotion';
        if (class_exists($promotionClass) && is_a($widget, $promotionClass)) {
            $skipped[] = ['name' => $name, 'reason' => 'pro_placeholder'];
            continue;
        }
        if ($widget->get_type() !== 'widget') {
            $skipped[] = ['name' => $name, 'reason' => 'not_a_widget'];
            continue;
        }
        if ($widget->show_in_panel() !== true) {
            $skipped[] = ['name' => $name, 'reason' => 'hidden_from_panel'];
            continue;
        }
        $categories = $widget->get_categories();
        if (!is_array($categories) || array_filter($categories, static fn ($category): bool => !is_string($category)) !== []) {
            throw new RuntimeException('widget ' . $name . ' has invalid categories');
        }
        if (array_intersect($categories, ['basic', 'general']) === []) {
            $skipped[] = ['name' => $name, 'reason' => 'special_document_category'];
            continue;
        }
        $title = $widget->get_title();
        if (!is_string($title)) {
            throw new RuntimeException('widget ' . $name . ' has no title');
        }
        if (trim($title) === '') {
            $skipped[] = ['name' => $name, 'reason' => 'empty_title'];
            continue;
        }
        if ($registrationCheck !== null) $registrationCheck();
        $controls = $widget->get_controls();
        if ($registrationCheck !== null) $registrationCheck();
        if (!is_array($controls) || $controls === []) {
            throw new RuntimeException('widget ' . $name . ' registered no controls');
        }
        $schema = rt_widget_dump_controls($controls, $plugin->controls_manager, $devices);
        $widgets[$name] = ['name' => $name, 'title' => $title,
            'controls' => $schema['controls'], 'controls_skipped' => $schema['controls_skipped']];
    }
    if ($widgets === []) {
        throw new RuntimeException('Elementor registered no core classic widgets');
    }
    ksort($widgets, SORT_STRING);
    usort($skipped, static fn (array $a, array $b): int => strcmp($a['name'], $b['name']));
    return ['schema_version' => 2, 'elementor_version' => $version, 'widgets' => array_values($widgets), 'skipped' => $skipped];
}

/** Require representative content, style and responsive controls before exporting a dump. */
function rt_widget_dump_positive_controls(array $dump): array
{
    $widgets = array_column($dump['widgets'], null, 'name');
    $controls = [];
    $optionControls = 0;
    foreach ($widgets as $name => $widget) {
        $controls[$name] = array_column($widget['controls'], null, 'id');
        foreach ($widget['controls'] as $control) {
            if ($control['options'] !== []) ++$optionControls;
        }
    }
    $required = ['heading' => ['header_size' => 'select', 'title_color' => 'color', '_margin' => 'dimensions', '_padding' => 'dimensions'],
        'button' => ['link' => 'url'], 'image' => ['image' => 'media']];
    foreach ($required as $widget => $keys) {
        foreach ($keys as $id => $type) {
            if (($controls[$widget][$id]['type'] ?? null) !== $type) {
                throw new RuntimeException('positive control ' . $widget . '.' . $id . ' must have type ' . $type);
            }
            if ($widget === 'heading' && $id === 'header_size'
                && array_diff(['h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'div', 'span', 'p'], $controls[$widget][$id]['options']) !== []) {
                throw new RuntimeException('positive control heading.header_size is missing tag options');
            }
            if (in_array($id, ['_margin', '_padding'], true)
                && (!is_array($controls[$widget][$id]['responsive']) || $controls[$widget][$id]['responsive']['devices'] === [])) {
                throw new RuntimeException('positive control heading.' . $id . ' needs a responsive window');
            }
        }
    }
    $typography = false;
    foreach ($controls['heading'] as $control) {
        $group = $control['group'];
        if (($group['family'] ?? null) === 'typography' && is_string($group['activator'] ?? null)
            && $control['id'] !== $group['activator']
            && in_array($controls['heading'][$group['activator']]['type'] ?? null, ['popover_toggle', 'hidden'], true)) {
            $typography = true;
        }
    }
    if (!$typography) {
        throw new RuntimeException('positive control heading typography needs its group activator');
    }
    if ($optionControls < 200) {
        throw new RuntimeException('positive control options: found ' . $optionControls . ', require at least 200');
    }
    return ['option_controls' => $optionControls];
}

/** Read full editor and style stacks, checking the optimisation flag around every registration. */
function rt_widget_dump($plugin, string $corePath, string $version, string $performanceClass = 'Elementor\\Core\\Frontend\\Performance'): array
{
    if (!defined('WP_ADMIN') || WP_ADMIN !== true || (function_exists('is_admin') && !is_admin())) {
        throw new RuntimeException('widget dump requires the early admin registration context');
    }
    $available = class_exists($performanceClass);
    $optimizationApi = $available && is_callable([$performanceClass, 'should_optimize_controls']);
    $styleApi = $available && is_callable([$performanceClass, 'set_use_style_controls']);
    if (!$optimizationApi && version_compare($version, '3.35.9', '>=')) {
        throw new RuntimeException('widget dump Performance optimisation API is unavailable');
    }
    if ($optimizationApi && !$styleApi) {
        throw new RuntimeException('widget dump Performance style-control API is unavailable');
    }
    $checks = 0;
    $check = static function () use ($performanceClass, $optimizationApi, &$checks): void {
        if ($optimizationApi) {
            if ($performanceClass::should_optimize_controls() !== false) {
                throw new RuntimeException('widget dump control registration is optimised');
            }
            ++$checks;
        }
    };
    try {
        if ($styleApi) $performanceClass::set_use_style_controls(true);
        $check();
        $dump = rt_widget_dump_inventory($plugin, $corePath, $version, $check);
        $dump['registration_context'] = ['method' => 'wp_admin_before_wp_load', 'wp_admin' => true,
            'optimization_check' => $optimizationApi ? 'performance_api' : 'api_not_available',
            'should_optimize_controls' => $optimizationApi ? false : null, 'registration_checks' => $checks,
            'style_controls_read' => $styleApi];
        $dump['positive_controls'] = rt_widget_dump_positive_controls($dump);
        return $dump;
    } finally {
        if ($styleApi) $performanceClass::set_use_style_controls(false);
    }
}
