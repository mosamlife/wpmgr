<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * Who owns a registered ability, read from the code that would actually run.
 *
 * An ability's name says nothing about who registered it, and its callbacks
 * can be replaced at registration time by any active plugin. Ownership is
 * therefore resolved from the source file of each callback: the execute
 * callback and the permission callback are located by Reflection, the file is
 * canonicalised with realpath(), and the path is matched against the canonical
 * WordPress roots, each carrying a trailing separator so a sibling directory
 * whose name merely starts with the owner's name never matches.
 *
 * Kinds: core, plugin, theme, mu-plugin, unknown. Unknown covers an internal
 * function, a callback whose source file cannot be canonicalised (eval'd code
 * included) and any path outside the roots. Both callbacks must resolve to the
 * same kind and directory; otherwise the ability is "split" and is never
 * attributed to either owner.
 *
 * The class also reports whether the ability object is a plain WP_Ability as
 * far as execution is concerned. A subclass that overrides any method on the
 * execution path runs without the filters the interception guards rely on, so
 * such an ability is refused with ability_class_overridden.
 *
 * Read-only: nothing here calls a callback or the ability.
 */
final class AbilityOwnership
{
    public const KIND_CORE      = 'core';
    public const KIND_PLUGIN    = 'plugin';
    public const KIND_THEME     = 'theme';
    public const KIND_MU_PLUGIN = 'mu-plugin';
    public const KIND_UNKNOWN   = 'unknown';

    public const REFUSE_CLASS    = 'ability_class_overridden';
    public const REFUSE_SPLIT    = 'ability_owner_split';
    public const REFUSE_MISMATCH = 'ability_owner_mismatch';

    /**
     * Methods on the execution path. Each must be declared by WP_Ability
     * itself: an override bypasses the filters, runs code other than the two
     * callbacks this class attributes, or changes what validation checks.
     */
    public const EXECUTION_METHODS = [
        'execute',
        'check_permissions',
        'do_execute',
        'normalize_input',
        'validate_input',
        'validate_output',
        'invoke_callback',
        'get_input_schema',
        'get_output_schema',
    ];

    private const BASE_CLASS = 'WP_Ability';

    /** @var array<string,array<string,string>> Symlinked entries per root. */
    private static array $links = [];

    /**
     * Does the ability run exactly WP_Ability's execution path?
     *
     * @param object $ability A registered ability.
     * @return bool False for anything that is not a WP_Ability, or that
     *              overrides any of EXECUTION_METHODS.
     */
    public static function abilityClassOk(object $ability): bool
    {
        if (!class_exists(self::BASE_CLASS, false) || !is_a($ability, self::BASE_CLASS)) {
            return false;
        }
        if (get_class($ability) === self::BASE_CLASS) {
            return true;
        }
        try {
            foreach (self::EXECUTION_METHODS as $method) {
                $ref = new \ReflectionMethod($ability, $method);
                if ($ref->getDeclaringClass()->getName() !== self::BASE_CLASS) {
                    return false;
                }
            }
        } catch (\ReflectionException $e) {
            return false;
        }

        return true;
    }

    /**
     * Classify an ability's owner.
     *
     * @param object                    $ability A registered ability.
     * @param array<string,string>|null $roots   Canonical roots, from roots()
     *                                           when null.
     * @return array{owner_kind:string,owner_dir:string,owner_split:bool,ability_class_ok:bool}
     */
    public static function classify(object $ability, ?array $roots = null): array
    {
        $roots     = $roots ?? self::roots();
        $classOk   = self::abilityClassOk($ability);
        [$exec, $perm] = self::callbacks($ability);

        $a = self::classifyCallback($exec, $roots);
        $b = self::classifyCallback($perm, $roots);

        if ($a === null || $b === null || $a !== $b) {
            return [
                'owner_kind'       => self::KIND_UNKNOWN,
                'owner_dir'        => '',
                'owner_split'      => true,
                'ability_class_ok' => $classOk,
            ];
        }

        return [
            'owner_kind'       => $a['kind'],
            'owner_dir'        => $a['dir'],
            'owner_split'      => false,
            'ability_class_ok' => $classOk,
        ];
    }

    /**
     * The refusal for running $ability as an ability owned by $kind/$dir, or
     * null when it may run as that owner.
     *
     * @param object                    $ability A registered ability.
     * @param string                    $kind    Expected owner kind.
     * @param string                    $dir     Expected owner directory.
     * @param array<string,string>|null $roots   Canonical roots.
     * @return string|null One of the REFUSE_* codes, or null.
     */
    public static function refusal(object $ability, string $kind, string $dir, ?array $roots = null): ?string
    {
        $c = self::classify($ability, $roots);
        if (!$c['ability_class_ok']) {
            return self::REFUSE_CLASS;
        }
        if ($c['owner_split']) {
            return self::REFUSE_SPLIT;
        }
        if ($c['owner_kind'] === self::KIND_UNKNOWN || $c['owner_kind'] !== $kind || $c['owner_dir'] !== $dir) {
            return self::REFUSE_MISMATCH;
        }

        return null;
    }

    /**
     * The canonical roots of this install, each ending in '/'. A root that
     * cannot be canonicalised is left out, so nothing classifies under it.
     *
     * @return array<string,string> Keys: core_inc, core_admin, plugin,
     *                              mu-plugin, theme.
     */
    public static function roots(): array
    {
        $raw = [];
        if (defined('ABSPATH')) {
            $abs = (string) constant('ABSPATH');
            $inc = defined('WPINC') ? (string) constant('WPINC') : 'wp-includes';
            $raw['core_inc']   = $abs . $inc;
            $raw['core_admin'] = $abs . 'wp-admin';
        }
        if (defined('WP_PLUGIN_DIR')) {
            $raw[self::KIND_PLUGIN] = (string) constant('WP_PLUGIN_DIR');
        }
        if (defined('WPMU_PLUGIN_DIR')) {
            $raw[self::KIND_MU_PLUGIN] = (string) constant('WPMU_PLUGIN_DIR');
        }
        if (function_exists('get_theme_root')) {
            $raw[self::KIND_THEME] = (string) get_theme_root();
        }

        return self::canonicalRoots($raw);
    }

    /**
     * Canonicalise raw root paths: realpath(), then exactly one trailing '/'.
     *
     * @param array<string,string> $raw Root key to path.
     * @return array<string,string>
     */
    public static function canonicalRoots(array $raw): array
    {
        $out = [];
        foreach ($raw as $key => $path) {
            if ($path === '') {
                continue;
            }
            $real = @realpath($path);
            if (!is_string($real) || $real === '/') {
                continue;
            }
            $out[$key] = rtrim(str_replace('\\', '/', $real), '/') . '/';
        }

        return $out;
    }

    /**
     * Classify one canonical source file against canonical roots.
     *
     * @param string|null          $file  realpath() of the source, or null.
     * @param array<string,string> $roots Canonical roots, each ending in '/'.
     * @return array{kind:string,dir:string}
     */
    public static function classifyPath(?string $file, array $roots): array
    {
        $unknown = ['kind' => self::KIND_UNKNOWN, 'dir' => ''];
        if ($file === null || $file === '') {
            return $unknown;
        }
        $file = str_replace('\\', '/', $file);

        $kinds = [self::KIND_MU_PLUGIN, self::KIND_PLUGIN, self::KIND_THEME];
        foreach ($kinds as $kind) {
            $root = $roots[$kind] ?? '';
            $dir  = $root === '' ? null : self::firstSegmentUnder($file, $root);
            if ($dir !== null) {
                return ['kind' => $kind, 'dir' => $dir];
            }
        }
        foreach (['core_inc', 'core_admin'] as $key) {
            $root = $roots[$key] ?? '';
            if ($root !== '' && strncmp($file, $root, strlen($root)) === 0) {
                return ['kind' => self::KIND_CORE, 'dir' => ''];
            }
        }
        // A symlinked plugin or theme directory: the source canonicalises to
        // the link target, so match it against each linked entry of a root.
        foreach ($kinds as $kind) {
            $root = $roots[$kind] ?? '';
            $dir  = $root === '' ? null : self::linkedEntryFor($file, $root);
            if ($dir !== null) {
                return ['kind' => $kind, 'dir' => $dir];
            }
        }

        return $unknown;
    }

    /**
     * Version of the owner, or null when it cannot be read.
     *
     * @param string                   $kind    Owner kind.
     * @param string                   $dir     Owner directory.
     * @param array<string,mixed>|null $plugins Installed plugins keyed by
     *                                          plugin file; read from core
     *                                          when null.
     * @return string|null
     */
    public static function ownerVersion(string $kind, string $dir, ?array $plugins = null): ?string
    {
        if ($kind === self::KIND_CORE) {
            $v = AbilityGuards::wpVersion();

            return $v === '' ? null : $v;
        }
        if ($dir === '') {
            return null;
        }
        if ($kind === self::KIND_PLUGIN) {
            foreach ($plugins ?? self::installedPlugins() as $file => $data) {
                if (!is_string($file) || !is_array($data)) {
                    continue;
                }
                if ($file === $dir || strncmp($file, $dir . '/', strlen($dir) + 1) === 0) {
                    $v = $data['Version'] ?? '';

                    return is_string($v) && $v !== '' ? $v : null;
                }
            }

            return null;
        }
        if ($kind === self::KIND_THEME && function_exists('wp_get_theme')) {
            try {
                $theme = wp_get_theme($dir);
                if (is_object($theme) && method_exists($theme, 'exists') && $theme->exists()) {
                    $v = $theme->get('Version');

                    return is_string($v) && $v !== '' ? $v : null;
                }
            } catch (\Throwable $e) {
                return null;
            }
        }

        return null;
    }

    /**
     * The additive inventory fields for one registered ability.
     *
     * @param object                    $ability A registered ability.
     * @param array<string,string>|null $roots   Canonical roots.
     * @param array<string,mixed>|null  $plugins Installed plugins; read from
     *                                           core when null.
     * @return array{owner_kind:string,owner_dir:string,owner_version:string|null,owner_split:bool,ability_class_ok:bool}
     */
    public static function inventoryFields(object $ability, ?array $roots = null, ?array $plugins = null): array
    {
        $c = self::classify($ability, $roots);

        return [
            'owner_kind'       => $c['owner_kind'],
            'owner_dir'        => $c['owner_dir'],
            'owner_version'    => $c['owner_split'] ? null : self::ownerVersion($c['owner_kind'], $c['owner_dir'], $plugins),
            'owner_split'      => $c['owner_split'],
            'ability_class_ok' => $c['ability_class_ok'],
        ];
    }

    /**
     * The two callbacks, read from WP_Ability's protected properties.
     *
     * @param object $ability A registered ability.
     * @return array{0:mixed,1:mixed}
     */
    private static function callbacks(object $ability): array
    {
        if (!class_exists(self::BASE_CLASS, false) || !is_a($ability, self::BASE_CLASS)) {
            return [null, null];
        }
        $out = [];
        foreach (['execute_callback', 'permission_callback'] as $prop) {
            try {
                $ref   = new \ReflectionProperty(self::BASE_CLASS, $prop);
                $out[] = $ref->getValue($ability);
            } catch (\Throwable $e) {
                $out[] = null;
            }
        }

        return [$out[0], $out[1]];
    }

    /**
     * One owner for everything a callback runs, or null when its files span
     * more than one owner.
     *
     * @param mixed                $cb    A callback as stored on the ability.
     * @param array<string,string> $roots Canonical roots.
     * @return array{kind:string,dir:string}|null
     */
    private static function classifyCallback($cb, array $roots): ?array
    {
        $files = self::sourceFiles($cb);
        if ($files === null) {
            return ['kind' => self::KIND_UNKNOWN, 'dir' => ''];
        }
        $owner = null;
        foreach ($files as $file) {
            $c = self::classifyPath($file, $roots);
            if ($owner !== null && $c !== $owner) {
                return null;
            }
            $owner = $c;
        }

        return $owner ?? ['kind' => self::KIND_UNKNOWN, 'dir' => ''];
    }

    /**
     * Canonical source file of a callable's own body, or null when it has none.
     *
     * @param mixed $cb A callback as stored on the ability.
     * @return string|null
     */
    public static function sourceFile($cb): ?string
    {
        $files = self::sourceFiles($cb);

        return $files === null ? null : $files[0];
    }

    /**
     * Every canonical source file whose code a callable can run: its own body
     * first, then, for a method or a closure bound to an object or scoped to
     * a class, the file of that class, each ancestor and each trait they use.
     * A method body can call helpers resolved on the object, so all of them
     * count. Null when any user-defined part has no canonical file.
     *
     * @param mixed $cb A callback as stored on the ability.
     * @return non-empty-list<string>|null
     */
    public static function sourceFiles($cb): ?array
    {
        $classes = [];
        try {
            if ($cb instanceof \Closure) {
                $ref  = new \ReflectionFunction($cb);
                $bound = $ref->getClosureThis();
                if ($bound !== null) {
                    $classes[] = new \ReflectionClass($bound);
                }
                $scope = $ref->getClosureScopeClass();
                if ($scope !== null) {
                    $classes[] = $scope;
                }
            } elseif (is_string($cb) && $cb !== '') {
                if (strpos($cb, '::') !== false) {
                    [$cls, $method] = explode('::', $cb, 2);
                    $ref            = new \ReflectionMethod($cls, $method);
                    $classes[]      = new \ReflectionClass($cls);
                } elseif (function_exists($cb)) {
                    $ref = new \ReflectionFunction($cb);
                } else {
                    return null;
                }
            } elseif (is_array($cb) && count($cb) === 2 && isset($cb[0], $cb[1]) && is_string($cb[1])
                && (is_object($cb[0]) || is_string($cb[0]))
            ) {
                $ref       = new \ReflectionMethod($cb[0], $cb[1]);
                $classes[] = new \ReflectionClass($cb[0]);
            } elseif (is_object($cb) && method_exists($cb, '__invoke')) {
                $ref       = new \ReflectionMethod($cb, '__invoke');
                $classes[] = new \ReflectionClass($cb);
            } else {
                return null;
            }
        } catch (\Throwable $e) {
            return null;
        }

        $own = self::canonicalFile($ref->getFileName());
        if ($own === null) {
            return null;
        }
        $files = [$own];
        $seen  = [];
        while ($classes !== []) {
            $class = array_pop($classes);
            $name  = $class->getName();
            if (isset($seen[$name])) {
                continue;
            }
            $seen[$name] = true;
            if (!$class->isInternal()) {
                $file = self::canonicalFile($class->getFileName());
                if ($file === null) {
                    return null;
                }
                $files[] = $file;
            }
            foreach ($class->getTraits() as $trait) {
                $classes[] = $trait;
            }
            $parent = $class->getParentClass();
            if ($parent !== false) {
                $classes[] = $parent;
            }
        }

        return array_values(array_unique($files));
    }

    /**
     * @param string|false $file A reflected file name.
     * @return string|null realpath() of it, with '/' separators.
     */
    private static function canonicalFile($file): ?string
    {
        if (!is_string($file) || $file === '') {
            return null;
        }
        $real = @realpath($file);

        return is_string($real) ? str_replace('\\', '/', $real) : null;
    }

    /**
     * The first path segment of $file below $root, or null when $file is not
     * strictly under $root.
     *
     * @param string $file Canonical file.
     * @param string $root Canonical root ending in '/'.
     * @return string|null
     */
    private static function firstSegmentUnder(string $file, string $root): ?string
    {
        $len = strlen($root);
        if ($len < 2 || strncmp($file, $root, $len) !== 0) {
            return null;
        }
        $rest = substr($file, $len);
        $seg  = strstr($rest, '/', true);
        $seg  = $seg === false ? $rest : $seg;
        if ($seg === '' || $seg === '.' || $seg === '..') {
            return null;
        }

        return $seg;
    }

    /**
     * The entry of $root that is a symlink whose canonical target contains
     * $file, or null.
     *
     * @param string $file Canonical file.
     * @param string $root Canonical root ending in '/'.
     * @return string|null
     */
    private static function linkedEntryFor(string $file, string $root): ?string
    {
        foreach (self::linkedEntries($root) as $entry => $target) {
            if ($file === $target || strncmp($file, $target . '/', strlen($target) + 1) === 0) {
                return $entry;
            }
        }

        return null;
    }

    /**
     * Symlinked entries of a root, entry name to canonical target, read once
     * per request.
     *
     * @param string $root Canonical root ending in '/'.
     * @return array<string,string>
     */
    private static function linkedEntries(string $root): array
    {
        if (isset(self::$links[$root])) {
            return self::$links[$root];
        }
        $out     = [];
        $entries = @scandir($root);
        foreach (is_array($entries) ? $entries : [] as $entry) {
            if ($entry === '' || $entry === '.' || $entry === '..') {
                continue;
            }
            if (!@is_link($root . $entry)) {
                continue;
            }
            $target = @realpath($root . $entry);
            if (!is_string($target) || $target === '/') {
                continue;
            }
            $out[$entry] = rtrim(str_replace('\\', '/', $target), '/');
        }
        self::$links[$root] = $out;

        return $out;
    }

    /**
     * Installed plugins keyed by plugin file, from core's own header reader.
     *
     * @return array<string,mixed>
     */
    private static function installedPlugins(): array
    {
        if (!function_exists('get_plugins') && defined('ABSPATH')) {
            $file = (string) constant('ABSPATH') . 'wp-admin/includes/plugin.php';
            if (@is_file($file)) {
                require_once $file;
            }
        }
        if (!function_exists('get_plugins')) {
            return [];
        }
        try {
            $all = get_plugins();
        } catch (\Throwable $e) {
            return [];
        }

        return is_array($all) ? $all : [];
    }
}
