<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Support\ArrayShape;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * Live, read-only widget discovery. A schema without control names is a
 * skeleton of every value control, grouped by tab and section. Named reads
 * return whole compact descriptors, never a slice of an option or default.
 * Both forms are capped by their JSON-encoded size and report truncation.
 * Atomic and hard-denied widgets are discoverable but are not usable.
 */
final class ElementorWidgetDiscovery
{
    public const MAX_BYTES = 32768;
    public const CODE_CONTROLS_UNAVAILABLE = 'elementor_controls_unavailable';
    public const DETAIL_CONTROLS_UNAVAILABLE = 'this Elementor version builds widget settings only in the editor; native edits need a newer Elementor';
    private const MAX_CONTROLS = 128;
    private const MAX_DEPTH = 8;
    private const RE_ID = '/^[a-zA-Z_][a-zA-Z0-9_-]{0,127}$/D';
    private const DEVICES = ['widescreen', 'desktop', 'laptop', 'tablet_extra', 'tablet', 'mobile_extra', 'mobile'];
    private const UI_TYPES = ['section', 'divider', 'heading', 'raw_html', 'deprecated_notice', 'tab', 'tabs', 'notice', 'alert'];

    public function __construct(private readonly ElementorApi $api = new ElementorRuntime())
    {
    }

    /**
     * @param bool $schema Whether this is the per-widget schema input.
     * @return array<string, mixed>
     */
    public static function inputSchema(bool $schema): array
    {
        $id = ['type' => 'string', 'pattern' => '^[a-zA-Z_][a-zA-Z0-9_-]{0,127}$', 'maxLength' => 128];
        if ($schema) {
            return [
                'type' => 'object',
                'properties' => [
                    'widget' => $id,
                    'controls' => ['type' => 'array', 'maxItems' => self::MAX_CONTROLS, 'uniqueItems' => true, 'items' => $id],
                ],
                'required' => ['widget'],
                'additionalProperties' => false,
            ];
        }

        return [
            'type' => 'object',
            'properties' => ['category' => $id, 'search' => ['type' => 'string', 'maxLength' => 128]],
            'additionalProperties' => false,
        ];
    }

    /**
     * @param bool $schema Whether this is the per-widget schema output.
     * @return array<string, mixed>
     */
    public static function outputSchema(bool $schema): array
    {
        $string = ['type' => 'string'];
        $bool   = ['type' => 'boolean'];
        $head   = ['builder' => ['type' => 'string', 'enum' => ['elementor']], 'builder_version' => $string, 'truncated' => $bool];
        if (!$schema) {
            $row = [
                'type' => 'object',
                'properties' => ['name' => $string, 'title' => $string, 'categories' => ['type' => 'array', 'items' => $string], 'pro' => $bool, 'atomic' => $bool, 'usable' => $bool],
                'required' => ['name', 'title', 'categories', 'pro', 'atomic', 'usable'],
                'additionalProperties' => false,
            ];

            return ['type' => 'object', 'properties' => $head + ['widgets' => ['type' => 'array', 'items' => $row]], 'required' => array_merge(array_keys($head), ['widgets']), 'additionalProperties' => false];
        }

        $control = ['type' => 'object', 'properties' => ['id' => $string, 'type' => $string], 'required' => ['id', 'type'], 'additionalProperties' => false];
        $section = ['type' => 'object', 'properties' => ['id' => $string, 'controls' => ['type' => 'array', 'items' => $control]], 'required' => ['id', 'controls'], 'additionalProperties' => false];
        $tab     = ['type' => 'object', 'properties' => ['id' => $string, 'sections' => ['type' => 'array', 'items' => $section]], 'required' => ['id', 'sections'], 'additionalProperties' => false];
        $props   = $head + ['widget' => $string, 'atomic' => $bool, 'usable' => $bool, 'mode' => ['type' => 'string', 'enum' => ['skeleton', 'controls']], 'tabs' => ['type' => 'array', 'items' => $tab], 'controls' => ['type' => 'object', 'additionalProperties' => self::controlOutputSchema()]];

        return [
            'type' => 'object', 'properties' => $props,
            'required' => array_merge(array_keys($head), ['widget', 'atomic', 'usable', 'mode']),
            'oneOf' => [
                ['type' => 'object', 'properties' => ['mode' => ['type' => 'string', 'enum' => ['skeleton']]], 'required' => ['tabs']],
                ['type' => 'object', 'properties' => ['mode' => ['type' => 'string', 'enum' => ['controls']]], 'required' => ['controls']],
            ],
            'additionalProperties' => false,
        ];
    }

    /** @return array<string, mixed> */
    private static function controlOutputSchema(): array
    {
        $string   = ['type' => 'string'];
        $nullable = ['type' => ['string', 'null']];
        $list     = ['type' => 'array', 'items' => $string];
        $props = [
            'type' => $string,
            'options' => $list,
            'default' => ['type' => ['string', 'number', 'boolean', 'array', 'object', 'null']],
            'responsive' => [
                'type' => ['object', 'null'],
                'properties' => ['devices' => $list, 'min' => $nullable, 'max' => $nullable],
                'required' => ['devices', 'min', 'max'], 'additionalProperties' => false,
            ],
            'group' => [
                'type' => ['object', 'null'],
                'properties' => ['family' => $string, 'prefix' => $string, 'activator' => $nullable],
                'required' => ['family', 'prefix', 'activator'], 'additionalProperties' => false,
            ],
            'condition' => ['type' => 'object'],
            'conditions' => ['type' => 'object'],
            'return_value' => $string,
            'raw_css_value' => ['type' => 'boolean'],
            'size_units' => $list,
            'range' => ['type' => 'object'],
            'multiple' => ['type' => 'boolean'],
            'fields' => ['type' => 'object', 'additionalProperties' => ['type' => 'object', 'required' => ['type', 'options', 'default', 'responsive', 'group']]],
        ];

        return ['type' => 'object', 'properties' => $props, 'required' => ['type', 'options', 'default', 'responsive', 'group'], 'additionalProperties' => false];
    }

    /**
     * @param bool                 $schema Whether this is a schema input.
     * @param array<string, mixed> $props  Input members.
     * @return string|null
     */
    public static function inputProblem(bool $schema, array $props): ?string
    {
        $allowed = array_keys(self::inputSchema($schema)['properties']);
        if (array_diff(array_keys($props), $allowed) !== []) {
            return 'unknown input field';
        }
        if ($schema && (!isset($props['widget']) || !is_string($props['widget']) || preg_match(self::RE_ID, $props['widget']) !== 1)) {
            return 'widget must be a widget name';
        }
        if (array_key_exists('category', $props) && (!is_string($props['category']) || preg_match(self::RE_ID, $props['category']) !== 1)) {
            return 'category must be a category name';
        }
        if (array_key_exists('search', $props) && (!is_string($props['search']) || strlen($props['search']) > 128 || preg_match('//u', $props['search']) !== 1)) {
            return 'search must be UTF-8 text of at most 128 bytes';
        }
        if (array_key_exists('controls', $props)) {
            $ids = $props['controls'];
            if (!is_array($ids) || !ArrayShape::isList($ids) || count($ids) > self::MAX_CONTROLS) {
                return 'controls must be a list of control ids';
            }
            $seen = [];
            foreach ($ids as $id) {
                if (!is_string($id) || preg_match(self::RE_ID, $id) !== 1 || isset($seen[$id])) {
                    return 'controls must hold distinct control ids';
                }
                $seen[$id] = true;
            }
        }

        return null;
    }

    /**
     * @param object $input Optional category and search filters, combined with AND.
     * @return array{output?: array<string, mixed>, refusal?: array{code: string, detail: string}}
     */
    public function catalogue(object $input): array
    {
        $props = get_object_vars($input);
        $bad   = self::inputProblem(false, $props);
        if ($bad !== null) {
            return self::refuse('bad_input', $bad);
        }
        $gate = $this->gate();
        if ($gate !== null) {
            return ['refusal' => $gate];
        }
        $rows = $this->api->widgetCatalogue();
        if ($rows === null) {
            return self::refuse('widget_registry_unreadable', 'the widget registry could not be read');
        }
        $out = $this->head() + ['widgets' => []];
        try {
            foreach ($rows as $row) {
                if (isset($props['category']) && !in_array($props['category'], $row['categories'], true)) {
                    continue;
                }
                $search = $props['search'] ?? '';
                if ($search !== '' && stripos($row['name'], $search) === false && stripos($row['title'], $search) === false) {
                    continue;
                }
                $row['usable'] = !$row['atomic'] && !self::denied($row['name'], LeafPolicy::DENIED_TYPES, LeafPolicy::DENIED_TYPE_PREFIXES);
                $candidate = $out;
                $candidate['widgets'][] = $row;
                if (!self::fits($candidate)) {
                    $out['truncated'] = true;
                    continue;
                }
                $out = $candidate;
            }
        } catch (\JsonException $e) {
            return self::refuse('widget_registry_unreadable', 'the widget registry could not be encoded');
        }

        return ['output' => $out];
    }

    /**
     * @param object $input Widget name, and optionally the control ids to drill into.
     * @return array{output?: array<string, mixed>, refusal?: array{code: string, detail: string}}
     */
    public function schema(object $input): array
    {
        $props = get_object_vars($input);
        $bad   = self::inputProblem(true, $props);
        if ($bad !== null) {
            return self::refuse('bad_input', $bad);
        }
        $gate = $this->gate();
        if ($gate !== null) {
            return ['refusal' => $gate];
        }
        $rows = $this->api->widgetCatalogue();
        if ($rows === null) {
            return self::refuse('widget_registry_unreadable', 'the widget registry could not be read');
        }
        $widget = null;
        foreach ($rows as $row) {
            if ($row['name'] === $props['widget']) {
                $widget = $row;
                break;
            }
        }
        if ($widget === null) {
            return self::refuse('widget_not_found', 'no registered widget has that name');
        }
        if ($widget['atomic']) {
            return self::refuse('atomic_not_supported', 'atomic widget schemas are not supported in this phase');
        }
        if (self::denied($widget['name'], LeafPolicy::DENIED_TYPES, LeafPolicy::DENIED_TYPE_PREFIXES)) {
            return self::refuse('widget_not_usable', 'this widget is not usable for native edits');
        }
        $raw = $this->api->widgetControls($widget['name']);
        if ($raw === null) {
            return self::refuse('widget_schema_unreadable', 'the widget controls could not be read');
        }
        $unavailable = self::availabilityProblem($raw);
        if ($unavailable !== null) {
            return ['refusal' => $unavailable];
        }
        try {
            $raw = self::baseControls($raw);
        } catch (\UnexpectedValueException $e) {
            return self::refuse('widget_schema_unreadable', 'the widget control schema could not be read');
        }
        $ids   = $props['controls'] ?? [];
        $named = $ids !== [];
        $out   = $this->head() + ['widget' => $widget['name'], 'atomic' => false, 'usable' => true, 'mode' => $named ? 'controls' : 'skeleton'];
        $out  += $named ? ['controls' => new \stdClass()] : ['tabs' => []];
        foreach ($ids as $id) {
            if (!isset($raw[$id]) || !self::valueControl($id, $raw[$id])) {
                return self::refuse('control_not_found', 'a requested control is not an available value control');
            }
        }
        try {
            $active = $named ? $this->api->activeBreakpoints() : null;
            foreach ($named ? $ids : array_keys($raw) as $id) {
                $control = $raw[$id];
                if (!self::valueControl($id, $control)) {
                    continue;
                }
                $candidate = $out;
                if ($named) {
                    $controls = (array) $out['controls'];
                    $controls[$id] = self::compact($control, $raw, $active, 0);
                    $candidate['controls'] = (object) $controls;
                } else {
                    self::addSkeletonControl($candidate['tabs'], $id, $control, $raw);
                }
                if (!self::fits($candidate)) {
                    $out['truncated'] = true;
                    continue;
                }
                $out = $candidate;
            }
        } catch (\JsonException | \UnexpectedValueException $e) {
            return self::refuse('widget_schema_unreadable', 'the widget control schema could not be read');
        }

        return ['output' => $out];
    }

    /** @return array{code: string, detail: string}|null */
    private function gate(): ?array
    {
        $active = $this->api->loaded();
        $status = new AdapterStatus($active, $this->api->version(), ElementorFacts::CLASSIC_MIN, ElementorFacts::CLASSIC_MAX, $active ? [] : ['elementor_missing']);

        return $status->refusal();
    }

    /** @return array<string, mixed> */
    private function head(): array
    {
        return ['builder' => 'elementor', 'builder_version' => $this->api->version(), 'truncated' => false];
    }

    /**
     * @param string               $id      Control id.
     * @param array<string, mixed> $control Control definition.
     * @return bool
     */
    public static function valueControl(string $id, array $control): bool
    {
        $type = $control['type'] ?? null;

        return is_string($type) && $type !== ''
            && !in_array($type, array_merge(self::UI_TYPES, ['code', 'html', 'custom_code']), true)
            && !self::denied($id, LeafPolicy::DENIED_KEYS, LeafPolicy::DENIED_KEY_PREFIXES);
    }

    /**
     * A stack with choice or slider controls needs their settings metadata.
     * No surviving choice or unit definitions means native controls are unavailable.
     * Text-only stacks and explicitly populated settings are independent of version.
     *
     * @param array<string,array<string,mixed>> $controls Live controls.
     * @return array{code:string,detail:string}|null
     */
    public static function availabilityProblem(array $controls): ?array
    {
        $needsMetadata = false;
        foreach ($controls as $control) {
            if (!is_array($control)) {
                continue;
            }
            $type = $control['type'] ?? null;
            $key = in_array($type, ['select', 'select2', 'choose'], true) ? 'options' : ($type === 'slider' ? 'size_units' : null);
            if ($key === null) {
                continue;
            }
            $needsMetadata = true;
            if (is_array($control[$key] ?? null) && $control[$key] !== []) {
                return null;
            }
        }

        return $needsMetadata ? ['code' => self::CODE_CONTROLS_UNAVAILABLE, 'detail' => self::DETAIL_CONTROLS_UNAVAILABLE] : null;
    }

    /**
     * @param string       $name     Widget or control id.
     * @param list<string> $names    Denied exact names.
     * @param list<string> $prefixes Denied prefixes.
     * @return bool
     */
    private static function denied(string $name, array $names, array $prefixes): bool
    {
        $name = strtolower($name);
        if (in_array($name, $names, true)) {
            return true;
        }
        foreach ($prefixes as $prefix) {
            if (str_starts_with($name, $prefix)) {
                return true;
            }
        }

        return false;
    }

    /**
     * @param array<mixed>                        $tabs    Skeleton tabs.
     * @param string                              $id      Control id.
     * @param array<string, mixed>                $control Control definition.
     * @param array<string, array<string, mixed>> $raw     Expanded registry.
     * @return void
     */
    private static function addSkeletonControl(array &$tabs, string $id, array $control, array $raw): void
    {
        $section = is_string($control['section'] ?? null) ? $control['section'] : '';
        $tab = $control['tab'] ?? ($raw[$section]['tab'] ?? 'content');
        if (!is_string($tab)) {
            throw new \UnexpectedValueException('invalid control tab');
        }
        $tabIndex = array_search($tab, array_column($tabs, 'id'), true);
        if ($tabIndex === false) {
            $tabIndex = count($tabs);
            $tabs[] = ['id' => $tab, 'sections' => []];
        }
        $sections = &$tabs[$tabIndex]['sections'];
        $sectionIndex = array_search($section, array_column($sections, 'id'), true);
        if ($sectionIndex === false) {
            $sectionIndex = count($sections);
            $sections[] = ['id' => $section, 'controls' => []];
        }
        $sections[$sectionIndex]['controls'][] = ['id' => $id, 'type' => $control['type']];
    }

    /**
     * @param array<string, mixed>                $control Control definition.
     * @param array<string, array<string, mixed>> $raw     Sibling controls, for group activators.
     * @param list<string>|null                   $active  Active breakpoint names.
     * @param int                                 $depth   Repeater nesting depth.
     * @return array<string, mixed>
     */
    public static function compact(array $control, array $raw, ?array $active, int $depth): array
    {
        if ($depth > self::MAX_DEPTH) {
            throw new \UnexpectedValueException('repeater nesting limit');
        }
        $out = [
            'type' => $control['type'],
            'options' => in_array($control['type'], ['select', 'select2', 'choose', 'visual_choice'], true)
                ? array_map('strval', array_keys(is_array($control['options'] ?? null) ? $control['options'] : [])) : [],
            'default' => $control['default'] ?? null,
            'responsive' => self::responsive($control['responsive'] ?? null, $active, ($control['is_responsive'] ?? false) === true),
            'group' => self::group($control, $raw),
        ];
        if (self::rawCssValue($control)) {
            $out['raw_css_value'] = true;
        }
        foreach (['condition', 'conditions', 'return_value', 'size_units', 'range', 'multiple'] as $key) {
            if (array_key_exists($key, $control)) {
                $out[$key] = $control[$key];
            }
        }
        if ($control['type'] === 'repeater') {
            $fields = [];
            foreach ($control['fields'] ?? [] as $key => $field) {
                if (!is_array($field)) {
                    throw new \UnexpectedValueException('invalid repeater field');
                }
                $id = $field['name'] ?? $key;
                if (!is_string($id) || isset($fields[$id])) {
                    throw new \UnexpectedValueException('invalid repeater field id');
                }
                $fields[$id] = $field;
            }
            $fields = self::baseControls($fields);
            $compact = [];
            foreach ($fields as $id => $field) {
                if (self::valueControl($id, $field)) {
                    $compact[$id] = self::compact($field, $fields, $active, $depth + 1);
                }
            }
            $out['fields'] = (object) $compact;
        }

        return $out;
    }

    /** Identify free-entry controls whose selectors use a raw value token. */
    public static function rawCssValue(array $control): bool
    {
        if (!in_array($control['type'] ?? null, ['text', 'textarea', 'number', 'slider', 'dimensions'], true)) {
            return false;
        }
        $flag = array_key_exists('raw_css_value', $control) ? $control['raw_css_value'] : false;
        if (!is_bool($flag) || (isset($control['selectors']) && !is_array($control['selectors']))) {
            throw new \UnexpectedValueException('invalid CSS selector metadata');
        }
        foreach ($control['selectors'] ?? [] as $selector => $rule) {
            if (!is_string($selector) || !is_string($rule)) {
                throw new \UnexpectedValueException('invalid CSS selector metadata');
            }
            if (str_contains($selector . $rule, '{{VALUE}}') || str_contains($selector . $rule, '{{value}}')) {
                $flag = true;
            }
        }
        return $flag;
    }

    /** Fold device variants into their root; stored overrides use root_device keys. */
    public static function baseControls(array $raw): array
    {
        $out = $raw;
        $devices = [];
        foreach ($raw as $id => $control) {
            if (!isset($control['parent'])) {
                continue;
            }
            $root = $id;
            $path = [];
            while (isset($raw[$root]['parent'])) {
                $parent = $raw[$root]['parent'];
                if (isset($path[$root]) || count($path) >= 64 || !is_string($parent) || !isset($raw[$parent])) {
                    throw new \UnexpectedValueException('invalid responsive parent');
                }
                $path[$root] = true;
                $root = $parent;
            }
            if (($raw[$root]['is_responsive'] ?? false) === true || ($raw[$root]['responsive']['max'] ?? null) !== 'desktop') {
                throw new \UnexpectedValueException('invalid responsive root');
            }
            $devices[$root]['desktop'] = true;
            foreach ($path as $variant => $_) {
                $window = $raw[$variant]['responsive'] ?? null;
                $device = is_array($window) ? ($window['max'] ?? $window['min'] ?? null) : null;
                if (!is_string($device) || !in_array($device, self::DEVICES, true) || $variant !== $root . '_' . $device
                    || ($raw[$variant]['type'] ?? null) !== ($raw[$root]['type'] ?? null)
                    || ($raw[$variant]['is_responsive'] ?? false) === true) {
                    throw new \UnexpectedValueException('invalid responsive variant');
                }
                foreach (['options', 'return_value', 'groupType', 'groupPrefix', 'fields'] as $key) {
                    if (($raw[$variant][$key] ?? null) !== ($raw[$root][$key] ?? null)) {
                        throw new \UnexpectedValueException('responsive variant differs from root');
                    }
                }
                if (self::rawCssValue($raw[$variant]) || self::rawCssValue($out[$root])) {
                    $out[$root]['raw_css_value'] = true;
                }
                $devices[$root][$device] = true;
            }
            unset($out[$id]);
        }
        foreach ($devices as $root => $window) {
            $out[$root]['is_responsive'] = true;
            $out[$root]['responsive'] = ['devices' => array_keys($window)];
        }
        return $out;
    }

    /**
     * @param mixed             $window Control's responsive metadata.
     * @param list<string>|null $active Active breakpoint names.
     * @return array{devices: list<string>, min: string|null, max: string|null}|null
     */
    private static function responsive(mixed $window, ?array $active, bool $single): ?array
    {
        if (!$single && ($window === null || $window === false || $window === [])) {
            return null;
        }
        if ($single && $window === null) {
            $window = [];
        }
        if ($active === null || array_diff($active, self::DEVICES) !== [] || (!is_array($window) && $window !== true)) {
            throw new \UnexpectedValueException('responsive devices unavailable');
        }
        $min = is_array($window) ? ($window['min'] ?? null) : null;
        $max = is_array($window) ? ($window['max'] ?? null) : null;
        if (($min !== null && (!is_string($min) || !in_array($min, self::DEVICES, true)))
            || ($max !== null && (!is_string($max) || !in_array($max, self::DEVICES, true)))) {
            throw new \UnexpectedValueException('unknown responsive bound');
        }
        if ($single) {
            if (!is_array($window)) {
                throw new \UnexpectedValueException('invalid single-control responsive window');
            }
            $declared = array_key_exists('devices', $window) ? $window['devices'] : self::DEVICES;
        } elseif (is_array($window)) {
            $declared = [$max ?? $min];
        } else {
            $declared = self::DEVICES;
        }
        if (!is_array($declared) || $declared === [] || array_filter($declared, 'is_string') !== $declared || array_diff($declared, self::DEVICES) !== []) {
            throw new \UnexpectedValueException('unknown responsive devices');
        }
        $devices = array_values(array_filter(self::DEVICES, static fn ($device) => in_array($device, $declared, true) && ($device === 'desktop' || in_array($device, $active, true))));

        return ['devices' => $devices, 'min' => null, 'max' => null];
    }

    /**
     * The group's root dependency comes from its controls' condition maps.
     * No activator name is inferred from the family name. An ambiguous or
     * absent dependency is reported as null.
     *
     * @param array<string, mixed>                $control Control definition.
     * @param array<string, array<string, mixed>> $raw     Sibling controls.
     * @return array{family: string, prefix: string, activator: string|null}|null
     */
    private static function group(array $control, array $raw): ?array
    {
        $family = $control['groupType'] ?? null;
        $prefix = $control['groupPrefix'] ?? null;
        if (($control['type'] ?? null) === 'popover_toggle' && $family === null && is_string($prefix) && $prefix !== '') {
            $families = [];
            foreach ($raw as $member) {
                if (($member['groupPrefix'] ?? null) === $prefix && is_string($member['groupType'] ?? null) && $member['groupType'] !== '') {
                    $families[$member['groupType']] = true;
                }
            }
            $family = count($families) === 1 ? array_key_first($families) : null;
        }
        if (!is_string($family) || !is_string($prefix) || $prefix === '') {
            return null;
        }
        $popovers = [];
        foreach ($raw as $id => $member) {
            if (($member['groupPrefix'] ?? null) === $prefix && ($member['type'] ?? null) === 'popover_toggle' && self::valueControl($id, $member)) {
                $popovers[$id] = true;
            }
        }
        if ($popovers !== []) {
            return ['family' => $family, 'prefix' => $prefix, 'activator' => count($popovers) === 1 ? array_key_first($popovers) : null];
        }
        $roots = [];
        foreach ($raw as $member) {
            if (($member['groupType'] ?? null) !== $family || ($member['groupPrefix'] ?? null) !== $prefix) {
                continue;
            }
            foreach (array_keys(is_array($member['condition'] ?? null) ? $member['condition'] : []) as $key) {
                $key = rtrim((string) $key, '!');
                if (!str_starts_with($key, $prefix) || !isset($raw[$key]) || !self::valueControl($key, $raw[$key])) {
                    continue;
                }
                $root = true;
                foreach (array_keys(is_array($raw[$key]['condition'] ?? null) ? $raw[$key]['condition'] : []) as $dependency) {
                    if (str_starts_with((string) $dependency, $prefix)) {
                        $root = false;
                    }
                }
                if ($root) {
                    $roots[$key] = true;
                }
            }
        }

        return ['family' => $family, 'prefix' => $prefix, 'activator' => count($roots) === 1 ? array_key_first($roots) : null];
    }

    /**
     * @param array<string, mixed> $out Candidate answer.
     * @return bool
     */
    private static function fits(array $out): bool
    {
        return strlen(json_encode($out, JSON_THROW_ON_ERROR)) <= self::MAX_BYTES;
    }

    /**
     * @param string $code   Refusal code.
     * @param string $detail WPMgr's words, without input or site text.
     * @return array{refusal: array{code: string, detail: string}}
     */
    private static function refuse(string $code, string $detail): array
    {
        return ['refusal' => ['code' => $code, 'detail' => $detail]];
    }
}
