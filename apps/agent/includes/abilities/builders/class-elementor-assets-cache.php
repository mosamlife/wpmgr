<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Abilities\AbilityWriteScope;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * The save may add or refresh entries in svg/font-icon, css/widgets and
 * json/widgets of the assets option. Values must be decoded arrays of plain
 * values, identical outside those sections after empty content-type containers
 * are dropped. A missing old option is an empty array; deletion is refused.
 * Existing entry keys must remain. Each added or changed entry has exactly
 * content and version: content is a string or array of plain values, and
 * version is a string. Unchanged entries retain their shape.
 */
final class ElementorAssetsCache
{
    /** The exact option name, checked even when the scope matches a prefix. */
    public const OPTION = '_elementor_assets_data';

    /** Content type and category pairs whose entries the save may cache. */
    private const SECTIONS = [['svg', 'font-icon'], ['css', 'widgets'], ['json', 'widgets']];

    /**
     * Whether one option write only adds or refreshes valid asset entries.
     *
     * @param mixed  $before   Old value, decoded by core, or AbilityWriteScope::ABSENT.
     * @param mixed  $after    New value, decoded by core, or AbilityWriteScope::ABSENT.
     * @param int    $targetId The post being saved.
     * @param string $name     The full option name.
     * @return bool
     */
    public static function ownWrite(mixed $before, mixed $after, int $targetId, string $name): bool
    {
        if ($before === AbilityWriteScope::ABSENT) {
            $before = [];
        }
        if ($targetId < 1 || $name !== self::OPTION || !is_array($before) || !is_array($after)) {
            return false;
        }
        if (!self::plainValues($before) || !self::plainValues($after)) {
            return false;
        }
        foreach (self::SECTIONS as [$type, $category]) {
            $was = self::section($before, $type, $category);
            $now = self::section($after, $type, $category);
            if ($was === null || $now === null || array_diff_key($was, $now) !== []) {
                return false;
            }
            foreach ($now as $key => $entry) {
                if (array_key_exists($key, $was) && $was[$key] === $entry) {
                    continue;
                }
                if (!is_array($entry) || count($entry) !== 2 || !is_string($entry['version'] ?? null) || (!is_string($entry['content'] ?? null) && !is_array($entry['content'] ?? null))) {
                    return false;
                }
            }
            unset($before[$type][$category], $after[$type][$category]);
            if (($before[$type] ?? null) === []) {
                unset($before[$type]);
            }
            if (($after[$type] ?? null) === []) {
                unset($after[$type]);
            }
        }

        return $before === $after;
    }

    /**
     * Section entries, empty when absent; null for a malformed container.
     *
     * @param array<mixed> $value    Decoded option value.
     * @param string       $type     Content type.
     * @param string       $category Asset category.
     * @return array<mixed>|null
     */
    private static function section(array $value, string $type, string $category): ?array
    {
        if (!array_key_exists($type, $value)) {
            return [];
        }
        if (!is_array($value[$type])) {
            return null;
        }
        if (!array_key_exists($category, $value[$type])) {
            return [];
        }

        return is_array($value[$type][$category]) ? $value[$type][$category] : null;
    }

    /**
     * Whether every value is a string, int, float, bool, null or array of
     * those. Referenced arrays are visited once, including shared subtrees.
     *
     * @param array<mixed> $values Decoded values.
     * @return bool
     */
    private static function plainValues(array $values): bool
    {
        $pending = [$values];
        $seen = [];
        while ($pending !== []) {
            $current = array_pop($pending);
            foreach ($current as $key => $value) {
                if (is_array($value)) {
                    $reference = \ReflectionReference::fromArrayElement($current, $key);
                    if ($reference !== null) {
                        $id = $reference->getId();
                        if (isset($seen[$id])) {
                            continue;
                        }
                        $seen[$id] = true;
                    }
                    $pending[] = $value;
                } elseif ($value !== null && !is_scalar($value)) {
                    return false;
                }
            }
        }

        return true;
    }
}
