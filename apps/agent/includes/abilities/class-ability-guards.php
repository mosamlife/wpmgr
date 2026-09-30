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
 * its execution, or change its permission result or output through four
 * filters. While armed, this class records each value at the earliest priority
 * and compares it at the latest one; any difference is a violation, and the
 * caller refuses the call. It also refuses every ability other than the one
 * outer call (and its declared nested allow-list), and refuses re-entry.
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

    private bool $armed = false;

    private string $outer = '';

    /** @var list<string> */
    private array $nestedAllow = [];

    /** @var array<string,mixed> Recorded values by filter. */
    private array $recorded = [];

    /** @var list<string> */
    private array $violations = [];

    private int $entered = 0;

    /** @var list<array{0:string,1:callable,2:int}> Installed hooks, for removal. */
    private array $hooks = [];

    /**
     * Do the four filters exist on this WordPress? They arrive in 7.1.
     *
     * @return bool
     */
    public static function supported(): bool
    {
        if (!function_exists('add_filter') || !function_exists('remove_filter') || !function_exists('get_bloginfo')) {
            return false;
        }

        return version_compare((string) get_bloginfo('version'), '7.1', '>=');
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
        $this->entered    = 0;
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

        $pre = function (...$args) {
            $value = $args[0] ?? null;
            $name  = isset($args[1]) && is_string($args[1]) ? $args[1] : '';
            if (!$this->armed) {
                return $value;
            }
            if ($value !== null) {
                $this->violations[] = 'short_circuit';

                return new \WP_Error('wpmgr_ability_intercepted', 'ability_intercepted/short_circuit');
            }
            ++$this->entered;
            if ($this->entered > 1) {
                $this->violations[] = 'nested_reentry';

                return new \WP_Error('wpmgr_nested_ability_refused', 'nested_ability_refused');
            }
            if ($name !== $this->outer && !in_array($name, $this->nestedAllow, true)) {
                $this->violations[] = 'nested_ability_refused';

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
        $this->hook(self::FILTER_PRE, $pre, PHP_INT_MAX);
    }

    /**
     * Remove every hook and stop guarding.
     *
     * @return void
     */
    public function disarm(): void
    {
        $this->armed = false;
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
