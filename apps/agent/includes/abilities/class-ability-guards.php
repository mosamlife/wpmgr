<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * Interception and nested-ability guards for a call the engine makes through
 * the Abilities API.
 *
 * On WordPress 7.1+ other plugins can rewrite an ability's input, short-circuit
 * its execution, or change its input validation, permission result, output or
 * output validation through six filters. While armed, this class records each
 * value at the earliest priority and compares it at the latest one, pairing
 * the two by nesting depth; any difference is a violation, and the caller
 * refuses the call. It also refuses every ability other than the one outer
 * call (and its declared nested allow-list), and refuses re-entry.
 *
 * Each recorder must be the first callback and each guard the last one on
 * its filter, every time either runs, and the two must run the same number of
 * times. A filter that runs before the execute callback (input, input
 * validation, permission) aborts the call by throwing AbilityInterception on
 * any violation, so the callback does not run; the later filters record the
 * violation and finish() refuses the result.
 *
 * The short-circuit filter's default is a per-call sentinel object, not null:
 * pass-through means the value at the latest priority is the very instance
 * recorded at the earliest one, and that instance is a sentinel.
 *
 * Armed only between arm() and disarm(), and only for the outer call. The
 * engine's own wpmgr/* handlers do not go through WP_Ability::execute, so in
 * practice this guards the vendor or core calls a handler makes.
 */
final class AbilityGuards
{
    public const FILTER_INPUT      = 'wp_ability_normalize_input';
    public const FILTER_PRE        = 'wp_pre_execute_ability';
    public const FILTER_PERMISSION = 'wp_ability_permission_result';
    public const FILTER_RESULT     = 'wp_ability_execute_result';
    public const FILTER_VALIDATE_INPUT  = 'wp_ability_validate_input';
    public const FILTER_VALIDATE_OUTPUT = 'wp_ability_validate_output';

    private const SENTINEL_CLASS = 'WP_Filter_Sentinel';

    private bool $armed = false;

    private string $outer = '';

    /** @var list<string> */
    private array $nestedAllow = [];

    /** Labels whose violations abort the call before the execute callback. */
    private const ABORTING = ['input' => true, 'validate_input' => true, 'permission' => true];

    /** @var array<string,list<mixed>> Values recorded at the earliest priority, innermost last, per label. */
    private array $stacks = [];

    /** @var array<string,int> Recorder runs per label. */
    private array $recorderRuns = [];

    /** @var array<string,int> Guard runs per label. */
    private array $guardRuns = [];

    /** @var list<string> */
    private array $violations = [];

    /** @var array<string,string> Violation label stem per guarded filter. */
    private array $edgeKeys = [];

    /** @var array<string,int> Entries per ability name. */
    private array $entered = [];

    /** @var list<array{0:string,1:callable,2:int}> Installed hooks, for removal. */
    private array $hooks = [];

    /**
     * Do the guarded filters exist on this WordPress? They arrive in 7.1.
     *
     * @return bool
     */
    public static function supported(): bool
    {
        if (!function_exists('add_filter') || !function_exists('remove_filter')) {
            return false;
        }

        return version_compare(self::wpVersion(), '7.1', '>=');
    }

    /**
     * The running WordPress version, from core's own global. Empty when unknown.
     *
     * @return string
     */
    public static function wpVersion(): string
    {
        $version = $GLOBALS['wp_version'] ?? '';

        return is_string($version) ? $version : '';
    }

    /**
     * Install the guards for one outer call.
     *
     * @param string       $outer       The one ability name that may run.
     * @param list<string> $nestedAllow Names that may also run; each is
     *                                  denylist-checked here.
     * @return void
     */
    public function arm(string $outer, array $nestedAllow = []): void
    {
        $this->disarm();
        $this->outer       = $outer;
        $this->nestedAllow = array_values(array_filter(
            $nestedAllow,
            static fn ($n) => is_string($n) && !AbilityDenylist::denies($n)
        ));
        $this->stacks       = [];
        $this->recorderRuns = [];
        $this->guardRuns    = [];
        $this->violations   = [];
        $this->entered      = [];
        $this->armed        = true;

        $record = function (string $filter, string $key): callable {
            $self = null;
            $self = function (...$args) use ($filter, $key, &$self) {
                $value = $args[0] ?? null;
                if ($this->armed) {
                    $this->stacks[$key][]      = $value;
                    $this->recorderRuns[$key] = ($this->recorderRuns[$key] ?? 0) + 1;
                    $ok = $this->assertEdge($filter, $self, true, $key);
                    $ok = $this->assertAllEdges() && $ok;
                    if (!$ok) {
                        $this->abortIfEarly($key);
                    }
                }

                return $value;
            };

            return $self;
        };
        $guard = function (string $filter, string $key): callable {
            $self = null;
            $self = function (...$args) use ($filter, $key, &$self) {
                $value = $args[0] ?? null;
                if (!$this->armed) {
                    return $value;
                }
                [$ok, $recorded] = $this->closePair($filter, $self, $key);
                if ($ok && $recorded !== $value) {
                    $this->violations[] = $key;
                    $ok                 = false;
                }
                if (!$ok) {
                    $this->abortIfEarly($key);

                    return $recorded;
                }

                return $value;
            };

            return $self;
        };

        $preRecord = null;
        $preRecord = function (...$args) use (&$preRecord) {
            $value = $args[0] ?? null;
            if ($this->armed) {
                $this->stacks['short_circuit'][]      = $value;
                $this->recorderRuns['short_circuit'] = ($this->recorderRuns['short_circuit'] ?? 0) + 1;
                $this->assertEdge(self::FILTER_PRE, $preRecord, true, 'short_circuit');
            }

            return $value;
        };
        $pre = null;
        $pre = function (...$args) use (&$pre) {
            $value = $args[0] ?? null;
            $name  = isset($args[1]) && is_string($args[1]) ? $args[1] : '';
            if (!$this->armed) {
                return $value;
            }
            // Before anything runs, every guarded filter must still be
            // bracketed by our recorder and guard, so a callback staged
            // outside them is refused before the callbacks execute.
            [$ok, $recorded] = $this->closePair(self::FILTER_PRE, $pre, 'short_circuit');
            if (!$ok || $value !== $recorded || !self::isSentinel($recorded)) {
                $this->violations[] = 'short_circuit';

                return new \WP_Error('wpmgr_ability_intercepted', 'ability_intercepted/short_circuit');
            }
            if ($name !== $this->outer && !in_array($name, $this->nestedAllow, true)) {
                $this->violations[] = 'nested_ability_refused';

                return new \WP_Error('wpmgr_nested_ability_refused', 'nested_ability_refused');
            }
            $this->entered[$name] = ($this->entered[$name] ?? 0) + 1;
            if ($this->entered[$name] > 1) {
                $this->violations[] = 'nested_reentry';

                return new \WP_Error('wpmgr_nested_ability_refused', 'nested_ability_refused');
            }

            return $value;
        };

        $pairs = [
            self::FILTER_INPUT           => 'input',
            self::FILTER_PERMISSION      => 'permission',
            self::FILTER_RESULT          => 'result',
            self::FILTER_VALIDATE_INPUT  => 'validate_input',
            self::FILTER_VALIDATE_OUTPUT => 'validate_output',
        ];
        $this->edgeKeys = $pairs + [self::FILTER_PRE => 'short_circuit'];
        foreach ($pairs as $filter => $key) {
            $this->hook($filter, $record($filter, $key), PHP_INT_MIN);
            $this->hook($filter, $guard($filter, $key), PHP_INT_MAX);
        }
        $this->hook(self::FILTER_PRE, $preRecord, PHP_INT_MIN);
        $this->hook(self::FILTER_PRE, $pre, PHP_INT_MAX);
    }

    /**
     * The guard half of a pair: count the run, check every edge, and pop the
     * value its recorder pushed at the same depth.
     *
     * @param string   $filter   Filter name.
     * @param callable $callback The guard itself.
     * @param string   $key      Violation label.
     * @return array{0:bool,1:mixed} Whether all is well, and the recorded value.
     */
    private function closePair(string $filter, $callback, string $key): array
    {
        $this->guardRuns[$key] = ($this->guardRuns[$key] ?? 0) + 1;
        $ok = $this->assertEdge($filter, $callback, false, $key);
        $ok = $this->assertAllEdges() && $ok;
        if (($this->stacks[$key] ?? []) === []) {
            $this->violations[] = $key . '_unpaired';

            return [false, null];
        }
        $recorded = array_pop($this->stacks[$key]);

        return [$ok, $recorded];
    }

    /**
     * Abort the ability call when $key is a filter that runs before the
     * execute callback.
     *
     * @param string $key Violation label.
     * @return void
     * @throws AbilityInterception For an early filter.
     */
    private function abortIfEarly(string $key): void
    {
        if (isset(self::ABORTING[$key])) {
            throw new AbilityInterception('ability_intercepted/' . $key); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- a fixed internal label, caught by the engine and never printed.
        }
    }

    /**
     * The end-of-call check, run before the result is accepted and while still
     * armed: every edge is still ours, every recorder run was paired with a
     * guard run, and nothing is left on a stack.
     *
     * @return bool True when the call may be accepted.
     */
    public function finish(): bool
    {
        $this->assertAllEdges();
        foreach ($this->edgeKeys as $key) {
            if (($this->recorderRuns[$key] ?? 0) !== ($this->guardRuns[$key] ?? 0) || ($this->stacks[$key] ?? []) !== []) {
                $this->violations[] = $key . '_unpaired';
            }
        }

        return $this->violations === [];
    }

    /**
     * Record a violation unless $callback is, at this moment, the first entry
     * of the lowest-priority bucket ($first) or the last entry of the
     * highest-priority bucket (!$first) registered for $filter. A callback
     * that sits outside ours, whenever and however it was added, could change
     * the value after our comparison or before our recording; so could one we
     * cannot see. Either is a violation labelled "<key>_order".
     *
     * @param string   $filter   Filter name.
     * @param callable $callback Our own callback for this edge.
     * @param bool     $first    True for the earliest edge, false for the latest.
     * @param string   $key      Violation label stem.
     * @return bool True when our callback holds the edge.
     */
    private function assertEdge(string $filter, $callback, bool $first, string $key): bool
    {
        if (!self::holdsEdge($filter, $callback, $first)) {
            $this->violations[] = $key . '_order';

            return false;
        }

        return true;
    }

    /**
     * assertEdge() for every hook this instance installed.
     *
     * @return bool True when every edge is held.
     */
    private function assertAllEdges(): bool
    {
        $ok = true;
        foreach ($this->hooks as [$filter, $callback, $priority]) {
            $key = $this->edgeKeys[$filter] ?? 'hook';
            if (!$this->assertEdge($filter, $callback, $priority === PHP_INT_MIN, $key)) {
                $ok = false;
            }
        }

        return $ok;
    }

    /**
     * @param string   $filter   Filter name.
     * @param callable $callback Callback.
     * @param bool     $first    Earliest edge when true, latest when false.
     * @return bool
     */
    private static function holdsEdge(string $filter, $callback, bool $first): bool
    {
        $registry = $GLOBALS['wp_filter'] ?? null;
        $hook     = is_array($registry) ? ($registry[$filter] ?? null) : null;
        if (!is_object($hook) || !property_exists($hook, 'callbacks') || !is_array($hook->callbacks) || $hook->callbacks === []) {
            return false;
        }
        if (class_exists('WP_Hook', false) && !($hook instanceof \WP_Hook)) {
            return false;
        }
        $priorities = array_keys($hook->callbacks);
        $bucket     = $hook->callbacks[$first ? min($priorities) : max($priorities)];
        if (!is_array($bucket) || $bucket === []) {
            return false;
        }
        $entry = $first ? reset($bucket) : end($bucket);

        return is_array($entry) && array_key_exists('function', $entry) && $entry['function'] === $callback;
    }

    /**
     * Refuse a value that reached the caller as core's short-circuit sentinel:
     * a pass-through default must never be the result of a call.
     *
     * @param mixed $result The value the call returned.
     * @return bool True when $result is acceptable.
     */
    public function checkResult($result): bool
    {
        if (is_object($result) && is_a($result, self::SENTINEL_CLASS)) {
            $this->violations[] = 'sentinel_result';

            return false;
        }

        return true;
    }

    /**
     * Is $value core's short-circuit default? On a WordPress that defines the
     * sentinel class only an instance of it qualifies; without the class, the
     * historical null default does.
     *
     * @param mixed $value Value recorded at the earliest priority.
     * @return bool
     */
    private static function isSentinel($value): bool
    {
        if (class_exists(self::SENTINEL_CLASS, false)) {
            return is_object($value) && is_a($value, self::SENTINEL_CLASS);
        }

        return $value === null;
    }

    /**
     * Remove every hook and stop guarding.
     *
     * @return void
     */
    public function disarm(): void
    {
        $this->armed  = false;
        $this->stacks = [];
        foreach ($this->hooks as [$filter, $callback, $priority]) {
            remove_filter($filter, $callback, $priority);
        }
        $this->hooks = [];
    }

    /**
     * Violations seen since arm(), each a short stable label.
     *
     * @return list<string>
     */
    public function violations(): array
    {
        return array_values(array_unique($this->violations));
    }

    /**
     * Ability names admitted past the short-circuit guard since arm(), in
     * the order they first ran.
     *
     * @return list<string>
     */
    public function invoked(): array
    {
        return array_map('strval', array_keys($this->entered));
    }

    /**
     * @param string   $filter   Filter name.
     * @param callable $callback Callback.
     * @param int      $priority Priority.
     * @return void
     */
    private function hook(string $filter, callable $callback, int $priority): void
    {
        add_filter($filter, $callback, $priority, 4);
        $this->hooks[] = [$filter, $callback, $priority];
    }
}
