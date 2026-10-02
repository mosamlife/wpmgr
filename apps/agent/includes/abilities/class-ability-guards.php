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
 * value at the earliest priority and compares it at the latest one; any
 * difference is a violation, and the caller refuses the call. It also refuses
 * every ability other than the one outer call (and its declared nested
 * allow-list), and refuses re-entry.
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

    /** @var array<string,mixed> Recorded values by filter. */
    private array $recorded = [];

    /** @var list<mixed> Short-circuit defaults seen at the earliest priority, innermost last. */
    private array $preStack = [];

    /** @var list<string> */
    private array $violations = [];

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
        $this->recorded   = [];
        $this->violations = [];
        $this->entered    = [];
        $this->preStack   = [];
        $this->armed      = true;

        $record = function (string $key): callable {
            return function (...$args) use ($key) {
                if ($this->armed) {
                    $this->recorded[$key] = $args[0] ?? null;
                }

                return $args[0] ?? null;
            };
        };
        $guard = function (string $key, string $label): callable {
            return function (...$args) use ($key, $label) {
                $value = $args[0] ?? null;
                if ($this->armed && array_key_exists($key, $this->recorded) && $this->recorded[$key] !== $value) {
                    $this->violations[] = $label;

                    return $this->recorded[$key];
                }

                return $value;
            };
        };

        $preRecord = function (...$args) {
            $value = $args[0] ?? null;
            if ($this->armed) {
                $this->preStack[] = $value;
            }

            return $value;
        };
        $pre = function (...$args) {
            $value = $args[0] ?? null;
            $name  = isset($args[1]) && is_string($args[1]) ? $args[1] : '';
            if (!$this->armed) {
                return $value;
            }
            $hadRecord = $this->preStack !== [];
            $recorded  = array_pop($this->preStack);
            if (!$hadRecord || $value !== $recorded || !self::isSentinel($recorded)) {
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

        $this->hook(self::FILTER_INPUT, $record('input'), PHP_INT_MIN);
        $this->hook(self::FILTER_INPUT, $guard('input', 'input'), PHP_INT_MAX);
        $this->hook(self::FILTER_PERMISSION, $record('permission'), PHP_INT_MIN);
        $this->hook(self::FILTER_PERMISSION, $guard('permission', 'permission'), PHP_INT_MAX);
        $this->hook(self::FILTER_RESULT, $record('result'), PHP_INT_MIN);
        $this->hook(self::FILTER_RESULT, $guard('result', 'result'), PHP_INT_MAX);
        $this->hook(self::FILTER_VALIDATE_INPUT, $record('validate_input'), PHP_INT_MIN);
        $this->hook(self::FILTER_VALIDATE_INPUT, $guard('validate_input', 'validate_input'), PHP_INT_MAX);
        $this->hook(self::FILTER_VALIDATE_OUTPUT, $record('validate_output'), PHP_INT_MIN);
        $this->hook(self::FILTER_VALIDATE_OUTPUT, $guard('validate_output', 'validate_output'), PHP_INT_MAX);
        $this->hook(self::FILTER_PRE, $preRecord, PHP_INT_MIN);
        $this->hook(self::FILTER_PRE, $pre, PHP_INT_MAX);
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
        $this->armed    = false;
        $this->preStack = [];
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
