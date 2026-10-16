<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Support\ArrayShape;

if (!defined('ABSPATH')) {
    exit;
}

/** Live classic widget eligibility and compact schemas for one native edit plan. */
final class NativeWidgetPolicy
{
    public const THEME_PRO_WIDGETS = ['theme-site-logo', 'theme-site-title', 'nav-menu', 'search-form'];

    public function __construct(private ElementorApi $api, private ?string $templateType = null)
    {
        if ($templateType !== null && !in_array($templateType, ThemeTemplateCreate::TYPES, true)) {
            throw new \InvalidArgumentException('unsupported template type');
        }
    }

    /** Explicit native operations, native targets and removals/replacements covering native nodes use the native contract. */
    public static function isNativeCall(array $ops, array $nodes): bool
    {
        $parents = [];
        $native = [];
        foreach ($nodes as $node) {
            $parents[$node['ref']] = $node['parent'];
            if ($node['kind'] === 'native') {
                $native[] = $node['ref'];
            }
        }
        foreach ($ops as $op) {
            if (in_array($op['op'] ?? '', ['set_native', 'add_native', 'duplicate'], true)) {
                return true;
            }
            if (in_array($op['ref'] ?? null, $native, true)) {
                return true;
            }
            if (!in_array($op['op'] ?? '', ['remove', 'replace'], true)) {
                continue;
            }
            foreach ($native as $ref) {
                for ($depth = 0; $depth <= ElementorDocument::MAX_DEPTH && isset($parents[$ref]); $depth++) {
                    if ($ref === ($op['ref'] ?? null)) {
                        return true;
                    }
                    $ref = $parents[$ref];
                }
            }
        }
        return false;
    }

    public static function denied(string $name, array $names, array $prefixes): bool
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

    public function eligible(string $type): bool
    {
        if (preg_match('/^[a-z][a-z0-9_-]{0,63}$/D', $type) !== 1 || str_starts_with($type, 'e-')
            || str_starts_with($type, 'nested-') || self::denied($type, LeafPolicy::DENIED_TYPES, LeafPolicy::DENIED_TYPE_PREFIXES)
            || !$this->api->widgetTypeExists($type)) {
            return false;
        }
        $core = $this->api->widgetIsCore($type);
        $pro = $this->templateType !== null && in_array($type, self::THEME_PRO_WIDGETS, true)
            && $this->api->proActive() && preg_match('/^[0-9]+\.[0-9]+\.[0-9]+$/D', $this->api->proVersion() ?? '') === 1
            && $this->api->widgetIsPro($type);
        if (!$core && !$pro) {
            return false;
        }
        $matches = [];
        foreach ($this->api->widgetCatalogue() ?? [] as $row) {
            if (($row['name'] ?? null) === $type) {
                $matches[] = $row;
            }
        }
        return count($matches) === 1 && ($matches[0]['atomic'] ?? null) === false
            && (($core && ($matches[0]['pro'] ?? null) === false) || ($pro && ($matches[0]['pro'] ?? null) === true));
    }

    /**
     * Select written base controls, their group dependencies and full repeater fields.
     * A null key list is a trusted full document; public schema reads have their own cap.
     * Breakpoints supplied by the plan retain the API's order and may be unavailable.
     */
    public function schema(string $type, array $libraryImages = [], ?array $keys = null, ?array $active = null): ?NativeControlSchema
    {
        if (!$this->eligible($type)) {
            return null;
        }
        $raw = $this->api->widgetControls($type);
        if ($raw === null || ElementorWidgetDiscovery::availabilityProblem($raw) !== null) {
            return null;
        }
        try {
            $raw = ElementorWidgetDiscovery::baseControls($raw);
            $selected = $keys === null ? array_fill_keys(array_keys($raw), true) : [];
            foreach ($keys ?? [] as $key) {
                $base = array_key_exists($key, $raw) ? $key : self::baseControl($key);
                $selected[$base] = true;
            }
            // A member needs its activator; a written activator also needs its governed members.
            foreach (array_keys($selected) as $id) {
                $control = $raw[$id] ?? [];
                if (!ElementorWidgetDiscovery::valueControl($id, $control)) {
                    continue;
                }
                $compact = ElementorWidgetDiscovery::compact($control, $raw, $active, 0);
                $group = $compact['group'] ?? null;
                if (is_array($group)) {
                    if (is_string($group['activator'])) {
                        $selected[$group['activator']] = true;
                    }
                    if ($id === $group['activator']) {
                        foreach ($raw as $memberId => $member) {
                            if (($member['groupType'] ?? null) === $group['family'] && ($member['groupPrefix'] ?? null) === $group['prefix']) {
                                $selected[$memberId] = true;
                            }
                        }
                    }
                }
                // Activators can omit group metadata; their members name the root in conditions.
                foreach ($raw as $memberId => $member) {
                    if (isset($member['groupType'], $member['groupPrefix']) && array_key_exists($id, $member['condition'] ?? [])) {
                        $selected[$memberId] = true;
                    }
                }
            }
            $controls = [];
            foreach (array_keys($selected) as $id) {
                $control = $raw[$id] ?? [];
                if (ElementorWidgetDiscovery::valueControl($id, $control)) {
                    $compact = ElementorWidgetDiscovery::compact($control, $raw, $active, 0);
                    $controls[$id] = self::validationControl($compact, $raw, $control);
                    if (is_string($control['label'] ?? null)) {
                        $controls[$id]['label'] = $control['label'];
                    }
                }
            }
            return new NativeControlSchema(self::canonical(['widget' => $type, 'atomic' => false, 'controls' => $controls]), $libraryImages);
        } catch (\Throwable $e) {
            return null;
        }
    }

    /** Native calls require available controls for the page's core widgets and new widget types. */
    public function availabilityProblem(array $tree, array $ops): ?array
    {
        $types = [];
        $walk = static function (array $nodes) use (&$walk, &$types): void {
            foreach ($nodes as $node) {
                if (($node['elType'] ?? null) === 'widget' && is_string($node['widgetType'] ?? null)) {
                    $types[$node['widgetType']] = true;
                }
                $walk($node['elements'] ?? []);
            }
        };
        $walk($tree);
        foreach ($ops as $op) {
            if (($op['op'] ?? null) === 'add_native' && is_string($op['widget'] ?? null)) {
                $types[$op['widget']] = true;
            }
        }
        foreach (array_keys($types) as $type) {
            if (!$this->eligible($type)) {
                continue;
            }
            $controls = $this->api->widgetControls($type);
            $problem = $controls === null ? null : ElementorWidgetDiscovery::availabilityProblem($controls);
            if ($problem !== null) {
                return $problem;
            }
        }

        return null;
    }

    private static function baseControl(string $key): string
    {
        foreach (['widescreen', 'desktop', 'laptop', 'tablet_extra', 'tablet', 'mobile_extra', 'mobile'] as $device) {
            if (str_ends_with($key, '_' . $device)) {
                return substr($key, 0, -strlen($device) - 1);
            }
        }
        return $key;
    }

    /** Canonical JSON sorts map keys recursively with SORT_STRING; list order is retained. */
    public static function canonical(array $value): array
    {
        if (!ArrayShape::isList($value)) {
            ksort($value, SORT_STRING);
        }
        foreach ($value as &$child) {
            if (is_array($child)) {
                $child = self::canonical($child);
            } elseif ($child instanceof \stdClass) {
                $child = (object) self::canonical((array) $child);
            }
        }
        return $value;
    }

    /** Union of a widget's per-operation documents, with deterministic map order. */
    public static function union(array $left, array $right): array
    {
        $right['controls'] = array_replace($left['controls'] ?? [], $right['controls']);
        return self::canonical($right);
    }

    /** Translate discovery's metadata into the validation contract without widening it. */
    private static function validationControl(array $control, array $raw, array $source): array
    {
        $window = $control['responsive'] ?? null;
        $control['responsive'] = is_array($window) ? ['devices' => $window['devices']] : false;
        if (isset($control['size_units'])) {
            $control['units'] = $control['size_units'];
        }
        $group = $control['group'] ?? null;
        if (is_array($group)) {
            $key = $group['activator'];
            $control['group'] = $group['family'];
            $root = is_string($key) ? ($raw[$key] ?? []) : [];
            $values = ($root['type'] ?? null) === 'popover_toggle' ? [$root['return_value'] ?? null] : (is_string($key) ? ($control['condition'][$key] ?? null) : null);
            if ($values === null && is_string($key)) {
                $root = $raw[$key] ?? [];
                $values = ($root['type'] ?? null) === 'switcher' ? [$root['return_value'] ?? 'yes'] : array_values(array_filter(array_map('strval', array_keys($root['options'] ?? [])), static fn ($v) => $v !== ''));
            }
            $control['activator'] = ['key' => $key, 'values' => is_array($values) ? array_values($values) : (is_string($values) ? [$values] : [])];
        }
        if (isset($control['fields'])) {
            $fields = (array) $control['fields'];
            $rawFields = [];
            foreach ($source['fields'] ?? [] as $key => $field) {
                $rawFields[$field['name'] ?? $key] = $field;
            }
            foreach ($fields as $key => $field) {
                $fields[$key] = self::validationControl($field, $rawFields, $rawFields[$key] ?? []);
            }
            $control['fields'] = $fields;
        }
        return $control;
    }

    /** Plain text summary uses text controls only, including repeater text fields. */
    public function summary(string $type, array $settings): string
    {
        $controls = $this->api->widgetControls($type) ?? [];
        $parts = [];
        self::textParts($controls, $settings, $parts, 0);
        $text = wp_strip_all_tags(html_entity_decode(implode(' ', $parts), ENT_QUOTES | ENT_HTML5, 'UTF-8'));
        $text = trim((string) preg_replace('/[\x00-\x20\x7F]+/u', ' ', $text));
        $text = str_replace(['<', '>'], '', $text);
        return preg_match('/^.{0,120}/us', $text, $matches) === 1 ? $matches[0] : '';
    }

    private static function textParts(array $controls, array $settings, array &$parts, int $depth): void
    {
        if ($depth > 8) {
            return;
        }
        foreach ($controls as $key => $control) {
            $key = is_string($control['name'] ?? null) ? $control['name'] : $key;
            $value = $settings[$key] ?? null;
            if (in_array($control['type'] ?? '', ['text', 'textarea', 'wysiwyg'], true) && is_string($value)) {
                $parts[] = $value;
            } elseif (($control['type'] ?? '') === 'repeater' && is_array($value)) {
                foreach ($value as $row) {
                    if (is_array($row)) {
                        self::textParts((array) ($control['fields'] ?? []), $row, $parts, $depth + 1);
                    }
                }
            }
        }
    }

    /** Reserved grid controls and every device variant cannot be written natively. */
    public static function deniedCssKey(string $key): bool
    {
        $key = strtolower($key);
        foreach (['_grid_column_custom', '_grid_row_custom'] as $base) {
            if ($key === $base || str_starts_with($key, $base . '_')) {
                return true;
            }
        }
        return false;
    }

    /** HTML-id controls and their responsive variants are reserved for designed anchors. */
    public static function htmlIdKey(string $key): bool
    {
        $key = strtolower($key);
        $base = self::baseControl($key);
        return $key === LeafPolicy::ANCHOR_KEY || $base === LeafPolicy::ANCHOR_KEY
            || str_ends_with($key, '_css_id') || str_ends_with($base, '_css_id');
    }

    /** Reserved controls cannot be written through native settings. */
    public static function settingsProblem(NativeControlSchema $schema, array $written): ?array
    {
        $failures = [];
        $controls = [];
        foreach ($written as $key => $value) {
            $base = self::baseControl($key);
            if (self::htmlIdKey($key) || self::deniedCssKey($key)) {
                $failures[] = ['path' => 'settings', 'key' => $key, 'reason' => self::htmlIdKey($key) ? 'HTML id controls cannot be written natively' : 'reserved grid controls cannot be written natively'];
                if (isset($schema->document['controls'][$base])) {
                    $controls[$base] = $schema->document['controls'][$base];
                }
            }
        }
        if ($failures === []) {
            return null;
        }
        $refusal = Refusal::command(Refusal::make(NativeSettingsValidator::CODE, 'Native settings were refused.', $failures));
        $refusal['schema'] = ['widget' => $schema->document['widget'], 'atomic' => false, 'controls' => $controls];
        return $refusal;
    }

    /** A designed mark is reserved for the designed editing path. */
    public static function designed(array $node): bool
    {
        $label = $node['settings'][LeafPolicy::LABEL_KEY] ?? null;
        return is_string($label) && str_starts_with($label, LeafPolicy::LABEL_PREFIX);
    }

    /** Designed sections and patterns reserve their entire descendant editing surface. */
    public static function designedProblem(array $tree, array $ops, array $projection): ?array
    {
        $nodes = [];
        $parents = [];
        self::storedIndex($tree, null, $nodes, $parents);
        $nativeRefs = [];
        $visibleRefs = [];
        foreach ($projection as $node) {
            $visibleRefs[$node['ref']] = true;
            if ($node['kind'] === 'native') {
                $nativeRefs[$node['ref']] = true;
            }
        }
        foreach ($ops as $i => $op) {
            $name = $op['op'];
            $ref = $op['ref'] ?? null;
            $checks = [];
            $nativeSource = isset($nativeRefs[$ref ?? '']) || (!isset($visibleRefs[$ref ?? ''])
                && ($nodes[$ref ?? '']['elType'] ?? null) === 'widget');
            if (in_array($name, ['set_native', 'duplicate'], true)
                || (in_array($name, ['move', 'remove'], true) && $nativeSource)) {
                $checks[] = $ref;
            }
            $nativeMove = $name === 'move' && ($nativeSource
                || (isset($op['into']) && ($nodes[$ref ?? '']['elType'] ?? null) === 'widget'));
            if (in_array($name, ['add_native', 'duplicate'], true) || $nativeMove) {
                $anchor = $op['into'] ?? $op['after'] ?? $op['before'] ?? null;
                $checks[] = isset($op['into']) ? $anchor : ($parents[$anchor ?? ''] ?? null);
            }
            foreach ($checks as $check) {
                while (is_string($check) && isset($nodes[$check])) {
                    if (self::designed($nodes[$check])) {
                        return ['code' => 'native_designed_node', 'detail' => 'use the designed edit operations inside this section or pattern', 'op_index' => $i];
                    }
                    $check = $parents[$check] ?? null;
                }
            }
        }
        return null;
    }

    private static function storedIndex(array $tree, ?string $parent, array &$nodes, array &$parents, int $depth = 0): void
    {
        if ($depth > ElementorDocument::MAX_DEPTH) {
            return;
        }
        foreach ($tree as $node) {
            if (!is_array($node) || !is_string($node['id'] ?? null)) {
                continue;
            }
            $nodes[$node['id']] = $node;
            $parents[$node['id']] = $parent;
            self::storedIndex($node['elements'] ?? [], $node['id'], $nodes, $parents, $depth + 1);
        }
    }

    /** Duplicate copies no anchors, denied keys, denied types or designed marks at any depth. */
    public static function duplicateProblem(array $node, int $depth = 0): bool
    {
        if ($depth > ElementorDocument::MAX_DEPTH || self::designed($node)) {
            return true;
        }
        foreach ($node as $key => $value) {
            $prefixes = array_merge(LeafPolicy::DENIED_KEY_PREFIXES, array_map(static fn ($denied) => $denied . '_', LeafPolicy::DENIED_KEYS), ['_motion_', 'motion_', '_sticky']);
            if (self::htmlIdKey((string) $key) || self::deniedCssKey((string) $key) || self::denied((string) $key, LeafPolicy::DENIED_KEYS, $prefixes)) {
                return true;
            }
            if (in_array($key, ['elType', 'widgetType'], true) && is_string($value)
                && (str_starts_with($value, 'e-') || self::denied($value, LeafPolicy::DENIED_TYPES, LeafPolicy::DENIED_TYPE_PREFIXES))) {
                return true;
            }
            if (is_array($value) && self::duplicateProblem($value, $depth + 1)) {
                return true;
            }
            if (is_string($value) && LeafPolicy::storedProblem($value, ElementorClassicMapper::LEAF_RULES) !== null) {
                return true;
            }
            if ($key === 'library' && $value === 'svg') {
                return true;
            }
        }
        return false;
    }

    /** Copied media keeps its original bytes and must still match verified site library facts. */
    public function duplicateMedia(NativeControlSchema $schema, array $settings, ?array $breakpoints): ?array
    {
        foreach ($settings as $key => $value) {
            if (!is_array($value)) {
                continue;
            }
            $bad = self::mediaValue($schema, $value, $breakpoints);
            if ($bad !== null) {
                $refusal = Refusal::command(Refusal::make(NativeSettingsValidator::CODE, 'Copied native media was refused.', [[
                    'path' => 'settings', 'key' => (string) $key, 'reason' => 'use a verified library image with its exact site URL',
                ]]));
                $failureSchema = $this->schema($schema->document['widget'], $schema->libraryImages, [(string) $key], $breakpoints);
                $refusal['schema'] = $failureSchema?->document ?? $schema->document;
                return $refusal;
            }
        }
        return null;
    }

    private static function mediaValue(NativeControlSchema $schema, array $value, ?array $breakpoints): ?array
    {
        if (array_key_exists('id', $value) && array_key_exists('url', $value)) {
            $id = $value['id'];
            if ((!is_int($id) && (!is_string($id) || preg_match('/^[0-9]+$/D', $id) !== 1))
                || (int) $id < 1 || !is_string($value['url'])) {
                return [];
            }
            foreach ($schema->libraryImages as $image) {
                if ((string) $image['id'] === (string) (int) $id && $image['url'] === $value['url']) {
                    return null;
                }
            }
            return [];
        }
        foreach ($value as $inner) {
            if (is_array($inner)) {
                $bad = self::mediaValue($schema, $inner, $breakpoints);
                if ($bad !== null) {
                    return $bad;
                }
            }
        }
        return null;
    }
}
