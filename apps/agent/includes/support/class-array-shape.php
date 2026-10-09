<?php

declare(strict_types=1);

namespace WPMgr\Agent\Support;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * Array shape tests the plugin needs on every supported WordPress version.
 *
 * WordPress only guarantees a global list test from 6.5, and the plugin
 * supports older cores, so the plugin carries its own.
 */
final class ArrayShape
{
    /**
     * True when the keys are exactly 0..n-1, in that order (an empty array
     * is a list). Same result as PHP's own list test.
     *
     * @param array<mixed> $value Array to test.
     * @return bool
     */
    public static function isList(array $value): bool
    {
        $expected = 0;
        foreach ($value as $key => $unused) {
            if ($key !== $expected) {
                return false;
            }
            ++$expected;
        }

        return true;
    }
}
