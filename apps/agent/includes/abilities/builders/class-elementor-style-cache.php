<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Abilities\AbilityWriteScope;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * Elementor's style-cache validity options, as the save of one page may
 * write them.
 *
 * Every option whose name starts with PREFIX holds one tree:
 * {state: bool, meta?: mixed, children?: {key: tree or bool}}. A bare bool
 * stands for {state: that bool} and a missing option for {state: false}, as
 * Elementor reads them. The children of the stored tree are keyed by post
 * id, so children[<post id>] is that post's entry.
 *
 * ownWrite() admits one write of such an option during the save of a target
 * post only when it changes nothing but the target's own entry: the value
 * before and the value after are identical, keys in order and types
 * included, once children[<target id>] is taken out of both and a children
 * list left empty is dropped. Anything else is not admitted: another
 * post's entry or the tree's own state changed, added or removed; a whole
 * option added or deleted that holds more than the target's entry; a value
 * of any other shape, or children that are not a list of entries.
 */
final class ElementorStyleCache
{
    /** Name prefix of the options, matched at the start of the name. */
    public const PREFIX = 'elementor_atomic_cache_validity__';

    /**
     * Whether one write of a style-cache option changes only the target's
     * entry. A value check for AbilityWriteScope.
     *
     * @param mixed $before   The value before the write, or AbilityWriteScope::ABSENT.
     * @param mixed $after    The value after the write, or AbilityWriteScope::ABSENT.
     * @param int   $targetId The post being saved.
     * @return bool
     */
    public static function ownWrite(mixed $before, mixed $after, int $targetId): bool
    {
        if ($targetId < 1) {
            return false;
        }
        $was = self::withoutTarget($before, $targetId);
        $now = self::withoutTarget($after, $targetId);

        return $was !== null && $now !== null && $was === $now;
    }

    /**
     * The stored tree with the target's entry taken out and an empty
     * children list dropped; null for a value of any other shape.
     *
     * @param mixed $value    Stored value, or AbilityWriteScope::ABSENT.
     * @param int   $targetId The post being saved.
     * @return array<mixed>|null
     */
    private static function withoutTarget(mixed $value, int $targetId): ?array
    {
        if ($value === AbilityWriteScope::ABSENT) {
            $value = ['state' => false];
        } elseif (is_bool($value)) {
            $value = ['state' => $value];
        }
        if (!is_array($value)) {
            return null;
        }
        if (array_key_exists('children', $value)) {
            if (!is_array($value['children'])) {
                return null;
            }
            unset($value['children'][$targetId]);
            if ($value['children'] === []) {
                unset($value['children']);
            }
        }

        return $value;
    }
}
