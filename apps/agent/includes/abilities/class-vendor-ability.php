<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * A registered ability that is not WPMgr's own: a vendor's or core's.
 *
 * In this agent version such an ability can only be read, and only as the
 * service principal. Before it runs, the entry must be a principal-mode read
 * entry whose source matches the ability's namespace, and the live ability
 * must still be the one that was reviewed: a plain WP_Ability, owned by the
 * entry's owner (both callbacks), at an owner version inside the entry's
 * range, with the same structural input schema. While it runs, the
 * interception guards and the side-effect recorder are armed; any violation
 * or any recorded side effect refuses the call and withholds the output.
 * The output is projected onto the entry's pinned output shape.
 *
 * Every method returns data or a refusal; none of them prints.
 */
final class VendorAbility
{
    public const SOURCE_VENDOR = 'vendor';
    public const SOURCE_CORE   = 'core';

    /** Owner kinds a vendor entry may name. */
    private const VENDOR_KINDS = [
        AbilityOwnership::KIND_PLUGIN,
        AbilityOwnership::KIND_THEME,
        AbilityOwnership::KIND_MU_PLUGIN,
    ];

    private const RE_DIR = '/^[A-Za-z0-9][A-Za-z0-9._-]*$/';

    /** A live owner version is reported back, so it must be plain. */
    private const RE_VERSION = '/^[0-9A-Za-z._+~-]{1,64}$/';

    /** Deepest output shape followed. */
    private const MAX_SHAPE_DEPTH = 16;

    /** Core error codes mapped to the engine's refusal codes. */
    private const ERROR_MAP = [
        'ability_invalid_permissions'          => 'ability_permission_denied',
        'ability_invalid_permission_callback'  => 'ability_permission_denied',
        'ability_invalid_input'                => 'ability_input_invalid',
        'ability_missing_input_schema'         => 'ability_input_invalid',
        'ability_invalid_output'               => 'ability_output_invalid',
    ];

    /** Guard violations that mean a nested ability was refused. */
    private const NESTED = ['nested_ability_refused' => true, 'nested_reentry' => true];

    /**
     * Refusal for an entry that is not a runnable vendor or core read, or
     * null when it is.
     *
     * @param object $entry Decoded entry.
     * @param string $name  Ability name.
     * @param string $mode  Requested mode.
     * @return array{code:string,detail:string}|null
     */
    public static function entryRefusal(object $entry, string $name, string $mode): ?array
    {
        $namespace = (string) strstr($name, '/', true);
        $source    = $entry->source ?? null;
        $want      = $namespace === 'core' ? self::SOURCE_CORE : self::SOURCE_VENDOR;
        if ($namespace === 'wpmgr' || $source !== $want) {
            return self::refuse('entry_source_mismatch', 'the entry source does not match the ability namespace');
        }

        $class = $entry->class ?? null;
        if ($class === 'write' && in_array($entry->snapshot ?? 'none', ['none', null, ''], true)) {
            return self::refuse('snapshot_strategy_invalid', 'a write entry needs a snapshot strategy');
        }
        if ($class !== 'read' || $mode !== 'read') {
            return self::refuse('vendor_writes_not_in_this_version', 'only reads of this ability run on this agent version');
        }
        if (($entry->permission_mode ?? null) !== 'principal') {
            return self::refuse('permission_mode_not_assertable', 'this ability runs only under the service principal\'s own permissions');
        }
        $shape = $entry->output_fields ?? null;
        if (!is_object($shape) && !is_string($shape)) {
            return self::refuse('bad_entry', 'a read entry needs a pinned output shape');
        }

        return null;
    }

    /**
     * The registered ability, or null when the site has none by that name.
     *
     * @param string $name Ability name.
     * @return \WP_Ability|null
     */
    public static function resolve(string $name): ?\WP_Ability
    {
        if (!function_exists('wp_get_ability') || !class_exists('WP_Ability', false)) {
            return null;
        }
        $ability = wp_get_ability($name);

        return $ability instanceof \WP_Ability ? $ability : null;
    }

    /**
     * The C1 checks against the live ability: class, owner, owner version
     * and schema.
     *
     * @param object                    $ability Registered ability.
     * @param object                    $entry   Decoded entry.
     * @param array<string,string>|null $roots   Canonical roots, from core when null.
     * @param array<string,mixed>|null  $plugins Installed plugins, from core when null.
     * @return array{refusal?:array{code:string,detail:string},owner?:array{kind:string,dir:string,version:string}}
     */
    public static function verify(object $ability, object $entry, ?array $roots = null, ?array $plugins = null): array
    {
        $source = $entry->source ?? null;
        $dir    = $entry->owner_dir ?? null;
        if ($source === self::SOURCE_CORE) {
            if ($dir !== null && $dir !== '') {
                return ['refusal' => self::refuse('bad_entry', 'a core entry names no owner directory')];
            }
            $kind = AbilityOwnership::KIND_CORE;
            $dir  = '';
        } else {
            $kind = $entry->owner_kind ?? AbilityOwnership::KIND_PLUGIN;
            if (!in_array($kind, self::VENDOR_KINDS, true) || !is_string($dir) || preg_match(self::RE_DIR, $dir) !== 1) {
                return ['refusal' => self::refuse('bad_entry', 'a vendor entry needs an owner kind and directory')];
            }
        }

        $owner = AbilityOwnership::refusal($ability, $kind, $dir, $roots);
        if ($owner !== null) {
            return ['refusal' => self::refuse($owner, 'the ability on this site is not the reviewed one')];
        }

        // A must-use plugin carries no version header core reads, so its
        // version can never be checked against a range.
        if ($kind === AbilityOwnership::KIND_MU_PLUGIN) {
            return ['refusal' => self::refuse('builder_version_unverified', 'a must-use plugin has no version to check')];
        }
        $live = AbilityOwnership::ownerVersion($kind, $dir, $plugins);
        $min  = $entry->version_min ?? null;
        $max  = $entry->version_max_tested ?? null;
        if (!is_string($live) || preg_match(self::RE_VERSION, $live) !== 1 || !is_string($min) || $min === '' || !is_string($max) || $max === ''
            || !VersionCompare::inRange($live, $min, $max)) {
            return ['refusal' => self::refuse('builder_version_unverified', 'the installed version is outside the reviewed range')];
        }

        $pinned  = $entry->schema_struct_sha256 ?? null;
        $dynamic = [];
        foreach ((array) ($entry->dynamic_enum_paths ?? []) as $path) {
            if (is_string($path)) {
                $dynamic[] = $path;
            }
        }
        $actual = AbilitySchema::hashOf($ability, $dynamic);
        if (!is_string($pinned) || $pinned === '' || !is_string($actual) || !hash_equals($pinned, $actual)) {
            return ['refusal' => self::refuse('ability_schema_changed', 'the ability\'s input schema differs from the reviewed one')];
        }

        return ['owner' => ['kind' => $kind, 'dir' => $dir, 'version' => $live]];
    }

    /**
     * Validate the exact input text against the live input schema and return
     * the value to pass to the ability.
     *
     * @param object $ability   Registered ability.
     * @param string $inputText Input JSON text, already known to be an object.
     * @return array{refusal?:array{code:string,detail:string},input?:mixed}
     */
    public static function prepareInput(object $ability, string $inputText): array
    {
        $asObject = json_decode($inputText, false, 32);
        $asArray  = json_decode($inputText, true, 32);
        if (!is_object($asObject) || !is_array($asArray)) {
            return ['refusal' => self::refuse('ability_input_invalid', 'input must be a JSON object')];
        }
        $schema = method_exists($ability, 'get_input_schema') ? $ability->get_input_schema() : null;
        if (!is_array($schema)) {
            return ['refusal' => self::refuse('ability_input_invalid', 'the ability\'s input schema could not be read')];
        }
        if ($schema === []) {
            // Core accepts no input for an ability without an input schema.
            return get_object_vars($asObject) === []
                ? ['input' => null]
                : ['refusal' => self::refuse('ability_input_invalid', 'this ability takes no input')];
        }
        if (!function_exists('rest_validate_value_from_schema')) {
            return ['refusal' => self::refuse('ability_input_invalid', 'the input could not be validated on this site')];
        }
        $valid = rest_validate_value_from_schema($asArray, $schema, 'input');
        if ($valid !== true) {
            return ['refusal' => self::refuse('ability_input_invalid', 'the input does not match the ability\'s schema')];
        }

        return ['input' => $asArray];
    }

    /**
     * Run the ability once, guarded and recorded. Call while the service
     * principal is the current user.
     *
     * The result is JSON-encoded and released while the recorder is still
     * armed, so code that runs during serialisation or destruction of the
     * result is recorded too. Any throw from the call is caught, recorded as
     * a violation, and never reported with its text; the hook stacks the
     * throw left open are closed back to their state before the call.
     *
     * @param \WP_Ability        $ability     Registered ability.
     * @param string             $name        Ability name.
     * @param mixed              $input       Prepared input.
     * @param list<mixed>        $nestedAllow Entry's nested allow-list.
     * @param AbilitySideEffects $effects     Side-effect recorder.
     * @return array{json:string|null,error_code:string|null,violations:list<string>,invoked:list<string>}
     */
    public static function call(\WP_Ability $ability, string $name, $input, array $nestedAllow, AbilitySideEffects $effects): array
    {
        $allow = [];
        foreach ($nestedAllow as $n) {
            if (is_string($n)) {
                $allow[] = $n;
            }
        }
        $guards     = new AbilityGuards();
        $extra      = [];
        $json       = null;
        $errorCode  = null;
        $stack      = self::hookState();
        try {
            $guards->arm($name, $allow);
            try {
                $effects->arm();
                try {
                    $result = $ability->execute($input);
                    $guards->checkResult($result);
                    if ($result instanceof \WP_Error) {
                        $errorCode = self::errorCode($result);
                    } else {
                        $encoded = json_encode($result);
                        $json    = is_string($encoded) ? $encoded : null;
                    }
                    $result = null;
                } catch (AbilityInterception $e) {
                    // The guards recorded why; the caller refuses.
                    $result = null;
                    self::restoreHookState($stack);
                } catch (\Throwable $e) {
                    $result  = null;
                    $extra[] = 'uncaught_exception';
                    self::restoreHookState($stack);
                }
            } finally {
                $effects->disarm();
            }
            // Before disarm: the end-of-call check needs the hooks in place.
            $guards->finish();
        } finally {
            $guards->disarm();
        }

        return [
            'json'       => $json,
            'error_code' => $errorCode,
            'violations' => array_values(array_unique(array_merge($guards->violations(), $extra))),
            'invoked'    => $guards->invoked(),
        ];
    }

    /**
     * The refusal for a finished call, or null when its output may be used.
     * Side effects first, then a switched user, then violations, then the
     * ability's own error.
     *
     * @param array{json:string|null,error_code:string|null,violations:list<string>,invoked:list<string>} $call        From call().
     * @param AbilitySideEffects                                                                        $effects     Recorder.
     * @param bool                                                                                      $principalOk Whether the principal is still the current user.
     * @return array{code:string,detail:string,extra:array<string,mixed>}|null
     */
    public static function outcomeRefusal(array $call, AbilitySideEffects $effects, bool $principalOk = true): ?array
    {
        $violations = $call['violations'];
        $with       = $violations !== [] ? ['violations' => $violations] : [];
        if ($effects->detected()) {
            return ['code' => 'read_side_effect_detected', 'detail' => 'the read changed the site or called out; its output was withheld', 'extra' => ['side_effects' => $effects->details()] + $with];
        }
        if (!$principalOk) {
            return ['code' => 'principal_switched', 'detail' => 'the current user changed during the call; its output was withheld', 'extra' => $with];
        }
        if ($violations !== []) {
            $nestedOnly = array_diff_key(array_flip($violations), self::NESTED) === [];

            return $nestedOnly
                ? ['code' => 'nested_ability_refused', 'detail' => 'the ability called another ability that is not allowed', 'extra' => $with]
                : ['code' => 'ability_intercepted', 'detail' => 'the call was interfered with or did not finish', 'extra' => $with];
        }
        $core = $call['error_code'];
        if ($core !== null) {
            if (isset(self::ERROR_MAP[$core])) {
                return ['code' => self::ERROR_MAP[$core], 'detail' => 'the ability refused the call', 'extra' => []];
            }

            return ['code' => 'ability_failed', 'detail' => 'the ability returned an error', 'extra' => ['error_code' => $core]];
        }
        if ($call['json'] === null) {
            return ['code' => 'ability_output_invalid', 'detail' => 'the ability output is not JSON-encodable', 'extra' => []];
        }

        return null;
    }

    /**
     * An error's code, reduced to [a-z0-9_-] and 64 bytes.
     *
     * @param \WP_Error $error Error.
     * @return string
     */
    private static function errorCode(\WP_Error $error): string
    {
        $raw = $error->get_error_code();

        return is_string($raw) ? substr((string) preg_replace('/[^a-z0-9_-]/', '', strtolower($raw)), 0, 64) : '';
    }

    /**
     * The hook-execution state a throw can leave open: the current-filter
     * stack depth and, per hook object, its nesting level and action flag.
     *
     * @return array{depth:int,hooks:array<string,array{0:object,1:int,2:bool}>}
     */
    private static function hookState(): array
    {
        $current = $GLOBALS['wp_current_filter'] ?? [];
        $state   = ['depth' => is_array($current) ? count($current) : 0, 'hooks' => []];
        $props   = self::hookProps();
        if ($props === null || !is_array($GLOBALS['wp_filter'] ?? null)) {
            return $state;
        }
        foreach ($GLOBALS['wp_filter'] as $tag => $hook) {
            if ($hook instanceof \WP_Hook) {
                $state['hooks'][(string) $tag] = [$hook, (int) $props['nesting_level']->getValue($hook), (bool) $props['doing_action']->getValue($hook)];
            }
        }

        return $state;
    }

    /**
     * Close what a throw left open: trim the current-filter stack back to its
     * depth before the call, and return each hook object to the nesting level
     * and action flag it had then, dropping its iteration state for the
     * levels the throw abandoned. Safe because the throw has unwound every
     * frame that ran at those levels. A hook first registered during the call
     * returns to level zero.
     *
     * @param array{depth:int,hooks:array<string,array{0:object,1:int,2:bool}>} $state From hookState().
     * @return void
     */
    private static function restoreHookState(array $state): void
    {
        // Popped one entry at a time, the way core closes each level.
        while (isset($GLOBALS['wp_current_filter']) && is_array($GLOBALS['wp_current_filter'])
            && count($GLOBALS['wp_current_filter']) > $state['depth']) {
            array_pop($GLOBALS['wp_current_filter']);
        }
        $props = self::hookProps();
        if ($props === null || !is_array($GLOBALS['wp_filter'] ?? null)) {
            return;
        }
        foreach ($GLOBALS['wp_filter'] as $tag => $hook) {
            if (!$hook instanceof \WP_Hook) {
                continue;
            }
            $saved = $state['hooks'][(string) $tag] ?? null;
            [$level, $doing] = $saved !== null && $saved[0] === $hook ? [$saved[1], $saved[2]] : [0, false];
            try {
                if ((int) $props['nesting_level']->getValue($hook) <= $level) {
                    continue;
                }
                foreach (['iterations', 'current_priority'] as $name) {
                    $value = $props[$name]->getValue($hook);
                    if (is_array($value)) {
                        $props[$name]->setValue($hook, array_filter($value, static fn ($k) => (int) $k < $level, ARRAY_FILTER_USE_KEY));
                    }
                }
                $props['nesting_level']->setValue($hook, $level);
                $props['doing_action']->setValue($hook, $doing);
            } catch (\Throwable $e) {
                continue;
            }
        }
    }

    /**
     * Reflection handles on WP_Hook's execution state, or null when this
     * WordPress does not have them.
     *
     * @return array<string,\ReflectionProperty>|null
     */
    private static function hookProps(): ?array
    {
        if (!class_exists('WP_Hook', false)) {
            return null;
        }
        try {
            $out = [];
            foreach (['nesting_level', 'doing_action', 'iterations', 'current_priority'] as $name) {
                $out[$name] = new \ReflectionProperty(\WP_Hook::class, $name);
            }

            return $out;
        } catch (\ReflectionException $e) {
            return null;
        }
    }

    /**
     * Project a value onto a pinned output shape: an object with an
     * allow-list of keys ({"fields":{key:shape}}), a list of one shape
     * ({"items":shape}), or a scalar leaf ("string", "int", "bool"). A value
     * of the wrong kind becomes null, and an unknown shape yields null.
     *
     * @param mixed $value Decoded output (arrays, never objects).
     * @param mixed $shape Shape, decoded to arrays.
     * @param int   $depth Current depth.
     * @return mixed
     */
    public static function project($value, $shape, int $depth = 0)
    {
        if ($depth > self::MAX_SHAPE_DEPTH) {
            return null;
        }
        if (is_string($shape)) {
            switch ($shape) {
                case 'string':
                    return is_string($value) ? $value : null;
                case 'int':
                    return is_int($value) ? $value : null;
                case 'bool':
                    return is_bool($value) ? $value : null;
                default:
                    return null;
            }
        }
        if (!is_array($shape)) {
            return null;
        }
        if (isset($shape['fields']) && is_array($shape['fields']) && count($shape) === 1) {
            if (!is_array($value) || ($value !== [] && array_is_list($value))) {
                return null;
            }
            $out = new \stdClass();
            foreach ($shape['fields'] as $key => $sub) {
                $key = (string) $key;
                if (array_key_exists($key, $value)) {
                    $out->{$key} = self::project($value[$key], $sub, $depth + 1);
                }
            }

            return $out;
        }
        if (array_key_exists('items', $shape) && count($shape) === 1) {
            if (!is_array($value) || !array_is_list($value)) {
                return null;
            }
            $out = [];
            foreach ($value as $item) {
                $out[] = self::project($item, $shape['items'], $depth + 1);
            }

            return $out;
        }

        return null;
    }

    /**
     * @param string $code   Refusal code.
     * @param string $detail Reason.
     * @return array{code:string,detail:string}
     */
    private static function refuse(string $code, string $detail): array
    {
        return ['code' => $code, 'detail' => $detail];
    }
}
