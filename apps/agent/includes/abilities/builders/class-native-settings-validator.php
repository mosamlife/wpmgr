<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Abilities\PageCreateBuilder;
use WPMgr\Agent\Support\ArrayShape;

if (!defined('ABSPATH')) {
    exit;
}

/** Pure validation of stored shapes and independently fenced AI-written leaves. */
final class NativeSettingsValidator
{
    public const CODE = 'native_settings_invalid';

    /** Breakpoints ordered from the smallest viewport to the largest. */
    public const BREAKPOINTS = ['mobile', 'mobile_extra', 'tablet', 'tablet_extra', 'laptop', 'desktop', 'widescreen'];

    private const MAX_DEPTH = 8;
    private const MAX_SETTINGS_BYTES = 65536;
    private const GROUPS = ['typography', 'background', 'border', 'box_shadow', 'box-shadow', 'text-shadow', 'text-stroke', 'css-filter'];
    private const CONTROL_TYPES = ['text', 'textarea', 'wysiwyg', 'number', 'slider', 'dimensions', 'color',
        'select', 'select2', 'choose', 'switcher', 'popover_toggle', 'url', 'media', 'icons', 'font', 'box_shadow', 'text_shadow', 'repeater'];

    /** Font icon libraries and their exact CSS class shapes; SVG is not a font icon. */
    private const ICONS = [
        'fa-solid'   => '/^fas fa-[a-z0-9]+(?:-[a-z0-9]+)*$/D',
        'fa-regular' => '/^far fa-[a-z0-9]+(?:-[a-z0-9]+)*$/D',
        'fa-brands'  => '/^fab fa-[a-z0-9]+(?:-[a-z0-9]+)*$/D',
        'eicons'     => '/^eicon-[a-z0-9]+(?:-[a-z0-9]+)*$/D',
    ];

    /**
     * Check stored-value shapes. AI input enters validateRaw or
     * validateWritten for the independent parsed string and carry-over policy.
     * Null is not a stored control value. No defaults are inserted.
     *
     * @param NativeControlSchema $schema      Live schema and trusted media inventory.
     * @param mixed               $settings    Complete candidate settings.
     * @param mixed               $breakpoints Site's active breakpoint names.
     * @return array<string,mixed> {ok:true}, or Refusal::command() with a failing-key schema.
     */
    public static function validate(NativeControlSchema $schema, $settings, $breakpoints): array
    {
        $doc = $schema->document;
        $widget = $doc['widget'] ?? null;
        $failures = [];
        if (!is_string($widget) || preg_match('/^[a-z][a-z0-9_-]{0,63}$/D', $widget) !== 1
            || self::denied($widget, LeafPolicy::DENIED_TYPES, LeafPolicy::DENIED_TYPE_PREFIXES)
            || ($doc['atomic'] ?? null) !== false) {
            self::failure($failures, 'schema', 'widget', 'use a known classic widget from the live catalogue');
        } elseif (!self::map($doc['controls'] ?? null)) {
            self::failure($failures, 'schema', 'controls', 'supply a live control map');
        } elseif (!self::strings($breakpoints) || array_diff($breakpoints, self::BREAKPOINTS) !== []) {
            self::failure($failures, 'site', 'breakpoints', 'supply the site active breakpoint names', self::BREAKPOINTS);
        } elseif (!self::map($settings) || !self::bounded($settings, self::MAX_SETTINGS_BYTES)) {
            self::failure($failures, 'input', 'settings', 'settings must be a map within the input limit');
        } else {
            self::settings($doc['controls'], $settings, $breakpoints, $schema, 'settings', 0, $failures);
        }
        return self::answer($schema, $failures);
    }

    /**
     * AI entry point: parse once with duplicate detection before PHP maps
     * discard property names. A set operation also supplies its before state.
     *
     * @param NativeControlSchema $schema Live schema.
     * @param string $json Raw AI settings JSON.
     * @param mixed $breakpoints Active breakpoints.
     * @param array<string,mixed> $before Stored settings before the edit.
     * @return array<string,mixed>
     */
    public static function validateRaw(NativeControlSchema $schema, string $json, $breakpoints, array $before = []): array
    {
        $parsed = NativeSettingsJson::parse($json);
        if (!$parsed['ok']) {
            $failure = $parsed['failure'];
            $failures = [];
            self::failure($failures, $failure['path'], $failure['key'], $failure['reason']);
            return self::answer($schema, $failures);
        }
        $written = $parsed['settings'];

        return self::validateWritten($schema, $before, $written, self::mergeSettings($before, $written), $breakpoints);
    }

    /**
     * Native edit merge: array_replace(before, AI), then top-level null
     * deletions. Arrays replace in full, with PHP's key order retained.
     * Authorization and policy are checked by validateWritten, not this helper.
     *
     * @param array<string,mixed> $before Stored settings.
     * @param array<string,mixed> $written AI patch.
     * @return array<string,mixed>
     */
    public static function mergeSettings(array $before, array $written): array
    {
        $merged = array_replace($before, $written);
        foreach ($written as $key => $value) {
            if ($value === null) {
                unset($merged[$key]);
            }
        }

        return $merged;
    }

    /** @param array<string,mixed> $settings Stored settings. Empty settings encode as []. */
    public static function encodeSettings(array $settings): string
    {
        return json_encode($settings, JSON_THROW_ON_ERROR);
    }

    /**
     * Check precisely the AI's writes. Untouched settings, including existing
     * bindings and unsupported controls, are carried byte-identically. A
     * replaced array is wholly AI-written and is checked row by row.
     *
     * @param NativeControlSchema $schema Live schema and verified media.
     * @param mixed $before Stored settings before the edit.
     * @param mixed $written AI patch, with null only for top-level deletions.
     * @param mixed $merged Proposed resulting settings.
     * @param mixed $breakpoints Active breakpoints.
     * @return array<string,mixed>
     */
    public static function validateWritten(NativeControlSchema $schema, $before, $written, $merged, $breakpoints): array
    {
        $header = self::validate($schema, [], $breakpoints);
        if (!$header['ok']) {
            return $header;
        }
        $failures = [];
        if (!self::map($before) || !self::map($written) || !self::map($merged) || !self::bounded($before, self::MAX_SETTINGS_BYTES)
            || !self::bounded($written, self::MAX_SETTINGS_BYTES) || !self::bounded($merged, self::MAX_SETTINGS_BYTES)) {
            self::failure($failures, 'input', 'settings', 'before, AI settings and result must be bounded maps');
            return self::answer($schema, $failures);
        }
        $expected = self::mergeSettings($before, $written);
        if (serialize($expected) !== serialize($merged)) {
            foreach (array_unique(array_merge(array_keys($expected), array_keys($merged))) as $key) {
                if (!array_key_exists($key, $expected) || !array_key_exists($key, $merged)
                    || serialize($expected[$key]) !== serialize($merged[$key])) {
                    self::failure($failures, 'settings', $key, 'the result must equal the exact replace-and-delete merge; carried leaves cannot change');
                }
            }
            if ($failures === []) {
                self::failure($failures, 'input', 'settings', 'the result must retain the PHP merge key order');
            }
            return self::answer($schema, $failures);
        }
        $accepted = [];
        foreach ($written as $key => $value) {
            if (self::deniedKey($key) || self::containsDenied($before[$key] ?? null)) {
                self::failure($failures, 'settings', $key, 'a denied key or a value containing one cannot be deleted or replaced');
            } elseif (self::overridden($key, $before, $schema->document['controls'])) {
                self::failure($failures, 'settings', $key, 'an existing dynamic or global binding would override this write');
            } elseif ($value !== null) {
                $accepted[$key] = $value;
            } else {
                [$base, $breakpoint] = self::split($key);
                $control = $schema->document['controls'][$base] ?? null;
                [, $baseBreakpoint] = self::split($base);
                if (!is_array($control) || !in_array($control['type'] ?? null, self::CONTROL_TYPES, true)
                    || ($breakpoint !== null && ($baseBreakpoint !== null || !self::responsive($control['responsive'] ?? false, $breakpoint, $breakpoints)))
                    || ($breakpoint === null && self::unknownOverride($key, $schema->document['controls']))) {
                    self::failure($failures, 'settings', $key, 'only a supported known control at an active responsive breakpoint may be deleted');
                }
            }
        }
        self::settings($schema->document['controls'], $accepted, $breakpoints, $schema, 'settings', 0, $failures, true, $merged);

        return self::answer($schema, $failures);
    }

    /**
     * Build a complete classic widget result, then check its exact envelope
     * and AI settings through the same path as an externally supplied node.
     *
     * @param NativeControlSchema $schema Live widget schema.
     * @param mixed $id New or retained element id.
     * @param mixed $isInner Inner-widget flag, a node field rather than an AI setting.
     * @param mixed $settings Complete new widget settings.
     * @param mixed $breakpoints Active breakpoints.
     * @return array<string,mixed>
     */
    public static function buildResultNode(NativeControlSchema $schema, $id, $isInner, $settings, $breakpoints): array
    {
        $node = ['id' => $id, 'elType' => 'widget', 'widgetType' => $schema->document['widget'] ?? '',
            'isInner' => $isInner, 'settings' => $settings, 'elements' => []];
        $checked = self::checkResultNode($schema, $node, $breakpoints);

        return $checked['ok'] ? ['ok' => true, 'node' => $node] : $checked;
    }

    /**
     * @param NativeControlSchema $schema Live schema.
     * @param mixed $node Proposed widget result.
     * @param mixed $breakpoints Active breakpoints.
     * @param array<string,mixed> $before Previous settings for a set operation.
     * @param array<string,mixed>|null $written AI patch; null means this is an add.
     * @return array<string,mixed>
     */
    public static function checkResultNode(NativeControlSchema $schema, $node, $breakpoints, array $before = [], ?array $written = null): array
    {
        $failures = [];
        if (!self::members($node, ['id', 'elType', 'widgetType', 'isInner', 'settings', 'elements'])) {
            self::failure($failures, 'result', 'node', 'a widget result must hold exactly id, elType, widgetType, isInner, settings and elements');
            return self::answer($schema, $failures);
        }
        if (!LeafPolicy::shapeMatches($node['id'], 'repeater-id')) {
            self::failure($failures, 'result', 'id', 'use a seven-character lower-case hex element id');
        }
        if ($node['elType'] !== 'widget') {
            self::failure($failures, 'result', 'elType', 'the result element type must be widget');
        }
        if ($node['widgetType'] !== ($schema->document['widget'] ?? null)) {
            self::failure($failures, 'result', 'widgetType', 'the result must use the requested schema widget');
        }
        if (!is_bool($node['isInner'])) {
            self::failure($failures, 'result', 'isInner', 'the node inner flag must be a boolean');
        }
        if ($node['elements'] !== []) {
            self::failure($failures, 'result', 'elements', 'a widget cannot hold children');
        }
        if ($failures !== []) {
            return self::answer($schema, $failures);
        }

        return self::validateWritten($schema, $before, $written ?? $node['settings'], $node['settings'], $breakpoints);
    }

    /** @param list<array<string,mixed>> $failures Collected failures. @return array<string,mixed> */
    private static function answer(NativeControlSchema $schema, array $failures): array
    {
        if ($failures === []) {
            return ['ok' => true];
        }
        $doc = $schema->document;
        $widget = $doc['widget'] ?? null;
        $refusal = Refusal::command(Refusal::make(self::CODE, 'Native settings were refused.', $failures));
        $controls = [];
        $known = self::map($doc['controls'] ?? null) ? $doc['controls'] : [];
        foreach ($failures as $failure) {
            $key = $failure['key'];
            if (str_starts_with($failure['path'], 'settings.')) {
                $key = explode('.', substr($failure['path'], strlen('settings.')))[0];
            }
            [$base] = self::split($key);
            foreach (array_unique([$base, $key]) as $candidate) {
                if (isset($known[$candidate]) && is_array($known[$candidate])) {
                    $controls[$candidate] = $known[$candidate];
                    $activation = $controls[$candidate]['activator'] ?? null;
                    $activator = is_array($activation) ? ($activation['key'] ?? null) : null;
                    if (is_string($activator) && isset($known[$activator])) {
                        $controls[$activator] = $known[$activator];
                    }
                }
            }
        }
        $compact = ['widget' => is_string($widget) ? $widget : '', 'atomic' => false, 'controls' => $controls];
        $refusal['schema'] = self::bounded($compact, NativeControlSchema::MAX_BYTES)
            ? $compact : ['widget' => '', 'atomic' => false, 'controls' => []];

        return $refusal;
    }

    /**
     * @param array<string,mixed> $controls Control map.
     * @param array<string,mixed> $settings Candidate settings at this level.
     * @param list<string> $active Active breakpoint names.
     * @param NativeControlSchema $schema Trusted site inputs.
     * @param string $path Dotted parent path.
     * @param int $depth Repeater nesting depth.
     * @param list<array<string,mixed>> $failures Collected failures.
     * @param bool $written Whether to apply the independent AI string/scalar policy.
     * @param array<string,mixed>|null $context Merged settings for group activator checks.
     */
    private static function settings(array $controls, array $settings, array $active, NativeControlSchema $schema, string $path, int $depth, array &$failures, bool $written = false, ?array $context = null): void
    {
        foreach ($settings as $key => $value) {
            if (count($failures) >= Refusal::MAX_FAILURES) {
                return;
            }
            $key = (string) $key;
            [$base, $breakpoint] = self::split($key);
            if (self::deniedKey($key) || self::deniedKey($base)) {
                self::failure($failures, $path, $key, 'this control is never written');
                continue;
            }
            $control = $controls[$base] ?? null;
            if (!is_array($control) || !is_string($control['type'] ?? null)) {
                self::failure($failures, $path, $key, 'use a known control from the compact widget schema');
                continue;
            }
            [, $baseBreakpoint] = self::split($base);
            if ($breakpoint !== null && ($baseBreakpoint !== null || !self::responsive($control['responsive'] ?? false, $breakpoint, $active))) {
                self::failure($failures, $path, $key, 'use an active breakpoint inside this control responsive window');
                continue;
            }
            if ($breakpoint === null && self::unknownOverride($key, $controls)) {
                self::failure($failures, $path, $key, 'use a recognised active responsive breakpoint suffix');
                continue;
            }
            $group = $control['group'] ?? LeafPolicy::groupOf($base);
            if ($control['type'] === 'box_shadow' && $group === null) {
                $group = 'box_shadow';
            }
            if ($control['type'] === 'text_shadow' && $group === null) {
                $group = 'text-shadow';
            }
            if ($group !== null) {
                $activator = $control['activator'] ?? null;
                $root = is_array($activator) && is_string($activator['key'] ?? null) ? ($controls[$activator['key']] ?? []) : [];
                $popover = ($root['type'] ?? null) === 'popover_toggle';
                $return = $root['return_value'] ?? null;
                if (!in_array($group, self::GROUPS, true) || !self::map($activator)
                    || !is_string($activator['key'] ?? null) || !self::strings($activator['values'] ?? null, false)
                    || !isset($controls[$activator['key']])
                    || ($popover && (!is_string($return) || $return === '' || !in_array($return, $activator['values'], true)))
                    || ($base !== $activator['key'] && ($popover
                        ? (($context ?? $settings)[$activator['key']] ?? null) !== $return
                        : !in_array(($context ?? $settings)[$activator['key']] ?? null, $activator['values'], true)))) {
                    self::failure($failures, $path, $key, 'supply this group activator with one of its activating values');
                    continue;
                }
            }
            if (str_ends_with($base, LeafPolicy::FONT_FAMILY_SUFFIX)
                && (!is_string($value) || preg_match('/' . OutlineV3Schema::FONT_PATTERN . '/D', $value) !== 1)) {
                self::failure($failures, $path, $key, 'use a font family of letters, digits and spaces');
                continue;
            }
            if ($control['type'] === 'repeater') {
                self::repeater($control, $value, $active, $schema, $path, $key, $depth, $failures, $written);
                continue;
            }
            $rich = NativeStringPolicy::richKey($schema->document['widget'], $base, $path);
            $why = $written ? NativeStringPolicy::leafProblem($value, $control['type'], $rich, $base) : null;
            if ($why === null) {
                $why = self::valueProblem($control, $value, $schema, $written, $rich);
            }
            if ($why !== null) {
                $allowed = in_array($control['type'], ['select', 'select2', 'choose'], true)
                    && self::strings($control['options'] ?? null) ? $control['options'] : null;
                self::failure($failures, $path, $key, $why, $allowed);
            }
        }
    }

    /**
     * @param array<string,mixed> $control Control descriptor.
     * @param mixed $value Value to check.
     * @param NativeControlSchema $schema Trusted library inputs.
     */
    private static function valueProblem(array $control, $value, NativeControlSchema $schema, bool $written = false, bool $rich = false): ?string
    {
        switch ($control['type']) {
            case 'text':
            case 'textarea':
                if ($written) {
                    return is_string($value) ? NativeStringPolicy::problem($value, $rich) : 'write text as a string';
                }
                return is_string($value) ? LeafPolicy::plainTextProblem($value) : 'write plain text as a string';
            case 'wysiwyg':
                return $written && $rich && is_string($value) ? NativeStringPolicy::problem($value, true) : 'rich text is permitted only on owned rich-text keys';
            case 'number':
                return LeafPolicy::shapeMatches(['unit' => 'px', 'size' => $value], 'slider', ['px'])
                    ? null : 'write a decimal number as a numeric string';
            case 'slider':
            case 'dimensions':
                if ($written && $control['type'] === 'dimensions') {
                    return self::writtenDimensions($control, $value) ? null : 'write exact dimensions with numeric strings and yes or empty isLinked';
                }
                return self::strings($control['units'] ?? null, false)
                    && LeafPolicy::shapeMatches($value, $control['type'], $control['units'])
                    && in_array($value['unit'], $control['units'], true)
                    ? null : 'use the exact control shape, numeric strings and a unit offered by the control';
            case 'color':
                return LeafPolicy::shapeMatches($value, 'colour') ? null : 'use an upper-case hex colour, transparent or bounded rgba without spaces';
            case 'select':
            case 'select2':
            case 'choose':
                if (!self::strings($control['options'] ?? null)
                    || (isset($control['multiple']) && !is_bool($control['multiple']))) {
                    return 'the live control must list its option keys and a boolean multiple flag';
                }
                if ($control['type'] === 'select2' && ($control['multiple'] ?? false)) {
                    return self::strings($value) && array_diff($value, $control['options']) === []
                        ? null : 'write a list containing only offered option keys';
                }
                return LeafPolicy::shapeMatches($value, 'enum', $control['options']) ? null : 'choose one of the offered option keys';
            case 'switcher':
            case 'popover_toggle':
                $return = array_key_exists('return_value', $control) ? $control['return_value'] : ($control['type'] === 'switcher' ? 'yes' : null);
                return is_string($return) && $return !== '' && is_string($value) && ($value === '' || $value === $return)
                    ? null : 'write the control return value or the empty string';
            case 'url':
                if (!LeafPolicy::shapeMatches($value, 'link')) {
                    return 'write exactly url, is_external and nofollow with on or empty flags';
                }
                return PageCreateBuilder::linkProblem($value['url'], true);
            case 'media':
                if (!self::members($value, ['id', 'url']) || !LeafPolicy::shapeMatches($value, 'image')
                    || PageCreateBuilder::imageUrlProblem($value['url']) !== null) {
                    return 'write exactly a library image id as a numeric string and its URL';
                }
                foreach ($schema->libraryImages as $image) {
                    if (self::members($image, ['id', 'url']) && $image['id'] === $value['id'] && $image['url'] === $value['url']) {
                        return null;
                    }
                }
                return 'use a verified image from this site library with its canonical URL';
            case 'icons':
                return self::members($value, ['value', 'library']) && is_string($value['library'])
                    && isset(self::ICONS[$value['library']]) && is_string($value['value'])
                    && strlen($value['value']) <= 100 && preg_match(self::ICONS[$value['library']], $value['value']) === 1
                    ? null : 'write value and library for an Elementor font icon library with matching classes';
            case 'font':
                return is_string($value) && preg_match('/' . OutlineV3Schema::FONT_PATTERN . '/D', $value) === 1
                    ? null : 'use a font family of letters, digits and spaces';
            case 'box_shadow':
            case 'text_shadow':
                $offsets = $control['type'] === 'box_shadow' ? ['horizontal', 'vertical', 'blur', 'spread'] : ['horizontal', 'vertical', 'blur'];
                if (!self::members($value, array_merge($offsets, ['color']))) {
                    return 'write one shadow with exact offsets and color';
                }
                foreach ($offsets as $offset) {
                    if (!LeafPolicy::shapeMatches($value[$offset], 'shadow-offset')
                        || ($offset === 'blur' && (int) $value[$offset] < 0)) {
                        return 'write shadow offsets as bounded whole numeric strings and a nonnegative blur';
                    }
                }
                return LeafPolicy::shapeMatches($value['color'], 'colour') ? null : 'use a pinned shadow colour';
            default:
                return 'this control type has no supported safe shape';
        }
    }

    /**
     * @param array<string,mixed> $control Repeater descriptor.
     * @param mixed $value Rows.
     * @param list<string> $active Active breakpoint names.
     * @param NativeControlSchema $schema Trusted site inputs.
     * @param string $path Parent path.
     * @param string $key Control key.
     * @param int $depth Nesting depth.
     * @param list<array<string,mixed>> $failures Collected failures.
     */
    private static function repeater(array $control, $value, array $active, NativeControlSchema $schema, string $path, string $key, int $depth, array &$failures, bool $written = false): void
    {
        if ($depth >= self::MAX_DEPTH || !is_array($value) || !ArrayShape::isList($value) || !self::map($control['fields'] ?? null)) {
            self::failure($failures, $path, $key, 'write a bounded list of repeater rows described by the live fields');
            return;
        }
        $ids = [];
        foreach ($value as $index => $row) {
            if (count($failures) >= Refusal::MAX_FAILURES) {
                return;
            }
            $at = $path . '.' . $key . '.' . $index;
            if (!self::map($row)) {
                self::failure($failures, $path, $key, 'each repeater row must be a map');
                continue;
            }
            $id = $row['_id'] ?? null;
            if (!LeafPolicy::shapeMatches($id, 'repeater-id') || isset($ids[$id])) {
                self::failure($failures, $at, '_id', 'write a unique seven-character lower-case hex row id');
            } else {
                $ids[$id] = true;
            }
            unset($row['_id']);
            self::settings($control['fields'], $row, $active, $schema, $at, $depth + 1, $failures, $written);
        }
    }

    /** @param array<string,mixed> $control Dimensions schema. @param mixed $value Written dimensions. */
    private static function writtenDimensions(array $control, $value): bool
    {
        if (!self::members($value, ['unit', 'top', 'right', 'bottom', 'left', 'isLinked'])
            || !self::strings($control['units'] ?? null, false) || $value['unit'] !== 'px'
            || !in_array($value['unit'], $control['units'], true) || !LeafPolicy::shapeMatches($value['isLinked'], 'switcher')) {
            return false;
        }
        foreach (['top', 'right', 'bottom', 'left'] as $side) {
            if (!LeafPolicy::shapeMatches(['unit' => 'px', 'size' => $value[$side]], 'slider', ['px']) || str_starts_with($value[$side], '-')) {
                return false;
            }
        }

        return true;
    }

    /** @param mixed $value Existing value to be replaced or removed. */
    private static function containsDenied($value): bool
    {
        if (!is_array($value)) {
            return false;
        }
        foreach ($value as $key => $child) {
            if ((is_string($key) && self::deniedKey($key)) || self::containsDenied($child)) {
                return true;
            }
        }

        return false;
    }

    /** @param array<string,mixed> $before Previous settings. @param array<string,mixed> $controls Schema controls. */
    private static function overridden(string $key, array $before, array $controls): bool
    {
        [$base] = self::split($key);
        $control = $controls[$base] ?? null;
        $activation = is_array($control) ? ($control['activator'] ?? null) : null;
        $activator = is_array($activation) ? ($activation['key'] ?? null) : null;
        foreach (['__dynamic__', '__globals__'] as $binding) {
            $entries = $before[$binding] ?? [];
            if (!is_array($entries)) {
                return true;
            }
            foreach ($entries as $bound => $value) {
                if ($value === '' || $value === null || $value === []) {
                    continue;
                }
                [$boundBase] = self::split((string) $bound);
                if ($boundBase === $base || $boundBase === $activator) {
                    return true;
                }
            }
        }

        return false;
    }

    /** @return array{string,?string} Base control and known breakpoint suffix. */
    private static function split(string $key): array
    {
        foreach (self::BREAKPOINTS as $breakpoint) {
            $suffix = '_' . $breakpoint;
            if (str_ends_with($key, $suffix)) {
                return [substr($key, 0, -strlen($suffix)), $breakpoint];
            }
        }

        return [$key, null];
    }

    /** @param mixed $window Responsive flag/window. @param list<string> $active Active names. */
    private static function responsive($window, string $breakpoint, array $active): bool
    {
        if (!in_array($breakpoint, $active, true) || $breakpoint === 'desktop') {
            return false;
        }
        if ($window === true) {
            return true;
        }
        if (!self::map($window) || $window === [] || array_diff(array_keys($window), ['min', 'max', 'devices']) !== []) {
            return false;
        }
        if (array_key_exists('devices', $window) && (!self::strings($window['devices'], true) || array_diff($window['devices'], self::BREAKPOINTS) !== [] || !in_array($breakpoint, $window['devices'], true))) {
            return false;
        }
        $min = $window['min'] ?? 'mobile';
        $max = $window['max'] ?? 'widescreen';
        if (!in_array($min, self::BREAKPOINTS, true) || !in_array($max, self::BREAKPOINTS, true)) {
            return false;
        }
        $index = array_search($breakpoint, self::BREAKPOINTS, true);

        return $index >= array_search($min, self::BREAKPOINTS, true) && $index <= array_search($max, self::BREAKPOINTS, true);
    }

    private static function deniedKey(string $key): bool
    {
        $suffixes = array_map(static fn(string $denied): string => $denied . '_', LeafPolicy::DENIED_KEYS);

        return NativeWidgetPolicy::htmlIdKey($key) || self::denied($key, LeafPolicy::DENIED_KEYS, array_merge(LeafPolicy::DENIED_KEY_PREFIXES, $suffixes, ['_motion_', 'motion_', '_sticky']));
    }

    /** @param array<string,mixed> $controls Known base controls. */
    private static function unknownOverride(string $key, array $controls): bool
    {
        foreach ($controls as $base => $control) {
            if (is_string($base) && is_array($control) && ($control['responsive'] ?? false) !== false
                && str_starts_with($key, $base . '_')) {
                return true;
            }
        }

        return false;
    }

    /** @param list<string> $exact Exact matches. @param list<string> $prefixes Denied prefixes. */
    private static function denied(string $key, array $exact, array $prefixes): bool
    {
        $key = strtolower($key);
        if (in_array($key, $exact, true)) {
            return true;
        }
        foreach ($prefixes as $prefix) {
            if (str_starts_with($key, $prefix)) {
                return true;
            }
        }

        return false;
    }

    /** @param mixed $value An associative map; an empty PHP array also represents an empty JSON object. */
    private static function map($value): bool
    {
        if (!is_array($value)) {
            return false;
        }
        foreach (array_keys($value) as $key) {
            if (!is_string($key) || $key === '') {
                return false;
            }
        }

        return true;
    }

    /** @param mixed $value List to check. @param bool $empty Whether an empty list is valid. */
    private static function strings($value, bool $empty = true): bool
    {
        return is_array($value) && ArrayShape::isList($value) && ($empty || $value !== [])
            && count(array_filter($value, 'is_string')) === count($value);
    }

    /** @param mixed $value Object. @param list<string> $keys Exact keys. */
    private static function members($value, array $keys): bool
    {
        return self::map($value) && count($value) === count($keys) && array_diff($keys, array_keys($value)) === [];
    }

    /** @param mixed $value JSON-compatible input. @param int $max Byte limit. */
    private static function bounded($value, int $max): bool
    {
        $encoded = json_encode($value, 0, 32);

        return $encoded !== false && strlen($encoded) <= $max;
    }

    /**
     * @param list<array<string,mixed>> $failures Collected failures.
     * @param string $path Parent path.
     * @param string $key Failing control.
     * @param string $reason Fix instruction.
     * @param list<string>|null $allowed Closed accepted set.
     */
    private static function failure(array &$failures, string $path, string $key, string $reason, ?array $allowed = null): void
    {
        if (count($failures) >= Refusal::MAX_FAILURES) {
            return;
        }
        // Failure keys remain nonempty after the envelope's line normalisation.
        if (trim((string) preg_replace('/[\x00-\x20\x7F]+/', ' ', $key)) === '') {
            $key = 'control';
        }
        $failure = ['path' => $path, 'key' => $key, 'reason' => $reason];
        if ($allowed !== null) {
            $failure['allowed'] = $allowed;
        }
        $failures[] = $failure;
    }
}
