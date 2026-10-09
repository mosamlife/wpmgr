<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * The closed set of page builders the agent can build for.
 *
 * A page-create editor value "builder:<id>" resolves to an adapter only when
 * all three hold, checked in this order:
 *
 *   1. <id> is one of IDS (else bad_input). Data cannot add an id.
 *   2. An adapter for <id> is compiled into this agent (else
 *      builder_not_available, detail not_compiled).
 *   3. The catalogue entry's limits.builders_enabled is a list of strings
 *      that names <id> (else builder_not_enabled). Anything else in that
 *      place, including an object or a missing key, enables nothing.
 *
 * Refusal details are WPMgr's own words; none echoes the input.
 */
final class BuilderRegistry
{
    /** Every builder id the agent knows. */
    public const IDS = ['elementor', 'beaver', 'wpbakery', 'divi5', 'bricks', 'breakdance', 'oxygen6'];

    /** A builder editor value (the $ matches only at the very end). */
    public const RE_EDITOR = '/^builder:([a-z0-9]+)$/D';

    /**
     * The adapters compiled into this agent, id => class.
     *
     * @var array<string, class-string<BuilderAdapter>>
     */
    private const COMPILED = [
        'elementor' => ElementorAdapter::class,
    ];

    /**
     * Resolve a page-create editor value to the adapter that builds it.
     *
     * @param string                             $editor       The editor value from the input.
     * @param mixed                              $limits       The catalogue entry's limits, as decoded with objects.
     * @param array<string, BuilderAdapter>|null $compiledSeam Tests only: stands in for the compiled set. Production passes none.
     * @return array{adapter?: BuilderAdapter, code?: string, detail?: string}
     */
    public static function resolve(string $editor, mixed $limits, ?array $compiledSeam = null): array
    {
        if (preg_match(self::RE_EDITOR, $editor, $m) !== 1 || !in_array($m[1], self::IDS, true)) {
            return ['code' => 'bad_input', 'detail' => 'editor must name a page builder WPMgr knows'];
        }
        $id      = $m[1];
        $adapter = $compiledSeam === null ? self::compiled($id) : ($compiledSeam[$id] ?? null);
        if (!$adapter instanceof BuilderAdapter || $adapter->id() !== $id) {
            return ['code' => 'builder_not_available', 'detail' => 'not_compiled'];
        }
        if (!self::enabled($limits, $id)) {
            return ['code' => 'builder_not_enabled', 'detail' => 'the catalogue entry does not enable this page builder'];
        }

        return ['adapter' => $adapter];
    }

    /**
     * The compiled adapter for $id, or null.
     *
     * @param string $id Adapter id.
     * @return BuilderAdapter|null
     */
    private static function compiled(string $id): ?BuilderAdapter
    {
        $class = self::COMPILED[$id] ?? null;

        return $class === null ? null : new $class();
    }

    /**
     * Does the entry's limits.builders_enabled, a list of strings, name $id?
     *
     * @param mixed  $limits Entry limits.
     * @param string $id     Adapter id.
     * @return bool
     */
    private static function enabled(mixed $limits, string $id): bool
    {
        if (!$limits instanceof \stdClass) {
            return false;
        }
        $list = get_object_vars($limits)['builders_enabled'] ?? null;
        if (!is_array($list) || !array_is_list($list)) {
            return false;
        }
        foreach ($list as $name) {
            if (!is_string($name)) {
                return false;
            }
        }

        return in_array($id, $list, true);
    }
}
