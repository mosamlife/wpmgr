<?php
/**
 * Autoloader: resolves a WPMgr\Agent\* fully-qualified class name to its
 * WordPress-style file path (class-*.php / interface-*.php) under includes/,
 * with no dependency on Composer's classmap autoloader.
 *
 * This exists because the plugin's own spl_autoload_register() closure (in
 * the main plugin file) previously assumed a purely mechanical StudlyCase ->
 * kebab-case mapping for both the class short-name and its containing
 * directory. That assumption breaks for:
 *   - interface files that drop the trailing "Interface" per WordPress
 *     convention (e.g. EmailKeystoreInterface -> interface-email-keystore.php);
 *   - a namespace segment whose kebab form differs from a bare lowercase
 *     (ObjectCache -> the real "object-cache" directory, not "objectcache");
 *   - brand/acronym-heavy class names whose filenames were hand-picked
 *     rather than mechanically derived (CloudPanel, WPEngine, IFrame, ...).
 *
 * On installs that ship without a Composer vendor/ directory (a git clone or
 * a GitHub source ZIP without `composer install`), the Composer classmap
 * that normally masks this bug is absent, and the plugin's own autoloader is
 * the sole resolver -- so it must resolve every one of the plugin's own
 * symbols by itself.
 *
 * resolve() is a pure function: given a class name it returns a file path or
 * null, with no require/include and no other side effects. That keeps it
 * unit-testable in isolation (see tests/AutoloaderTest.php) and keeps the
 * spl_autoload_register() closure in the main plugin file a thin wrapper
 * around it.
 *
 * @package WPMgr\Agent
 */

declare(strict_types=1);

namespace WPMgr\Agent;

/**
 * Pure WPMgr\Agent\* class-name to file-path resolver.
 */
final class Autoloader
{
    /**
     * Per-directory normalized-filename cache, keyed by absolute directory
     * path. Populated lazily by scanDirectory() so a given directory is
     * scanned at most once per request no matter how many misses land there.
     *
     * @var array<string,array<string,array<int,string>>>
     */
    private static array $dirScanCache = [];

    /**
     * Resolve a fully-qualified WPMgr\Agent\* class/interface/trait name to
     * the includes/ file that declares it.
     *
     * Resolution order:
     *   1. Fast path: exact class-<slug>.php / interface-<slug>.php, where
     *      <slug> is the kebab-cased class short-name and the containing
     *      directory is independently kebab-cased (not merely lowercased).
     *   2. Fast path: when the short-name ends in "Interface", also try
     *      interface-<slug-without-the-trailing-"Interface">.php, covering
     *      WordPress's convention of dropping the "Interface" suffix from
     *      interface filenames. The full-slug candidate from step 1 is
     *      checked first, so an interface file that keeps the full suffix
     *      (e.g. CommandInterface -> interface-command-interface.php) is
     *      unaffected.
     *   3. Fallback: scan the target directory once, normalize every
     *      class- (or interface-) prefixed filename (strip that prefix and
     *      the .php extension, lowercase, drop all non-alphanumerics) and
     *      look up the similarly-normalized short-name. A directory-wide
     *      collision on the needed key is treated as unresolvable rather
     *      than guessed.
     *
     * @param string $class Fully-qualified class name (as passed to a
     *                       spl_autoload_register() callback).
     * @return string|null Absolute file path, or null when unresolved or the
     *                      class is outside the WPMgr\Agent\ namespace.
     */
    public static function resolve(string $class): ?string
    {
        $prefix = 'WPMgr\\Agent\\';
        if (strpos($class, $prefix) !== 0) {
            return null;
        }

        $relative = str_replace('\\', '/', substr($class, strlen($prefix)));

        $dir  = dirname($relative);
        $base = basename($relative);

        $slug    = self::kebabCase($base);
        $subdir  = self::kebabCaseDir($dir);
        $baseDir = rtrim((string) WPMGR_AGENT_DIR, '/\\') . '/includes/' . $subdir;

        $hasInterfaceSuffix = strlen($base) > 9 && substr($base, -9) === 'Interface';

        $candidates = [
            $baseDir . 'class-' . $slug . '.php',
            $baseDir . 'interface-' . $slug . '.php',
        ];
        if ($hasInterfaceSuffix) {
            $trimmedSlug  = self::kebabCase(substr($base, 0, -9));
            $candidates[] = $baseDir . 'interface-' . $trimmedSlug . '.php';
        }

        foreach ($candidates as $file) {
            if (is_file($file)) {
                return $file;
            }
        }

        return self::resolveByScan(rtrim($baseDir, '/'), $base, $hasInterfaceSuffix);
    }

    /**
     * Fallback: match by normalized (dash/case-insensitive) filename stem
     * within a single, already kebab-corrected directory.
     *
     * @param string $absDir             Absolute, trailing-slash-free directory to scan.
     * @param string $base               Original StudlyCase class short-name.
     * @param bool   $hasInterfaceSuffix Whether $base ends in "Interface".
     * @return string|null
     */
    private static function resolveByScan(string $absDir, string $base, bool $hasInterfaceSuffix): ?string
    {
        $map = self::scanDirectory($absDir);

        $targetKeys = [self::normalize($base)];
        if ($hasInterfaceSuffix) {
            $targetKeys[] = self::normalize(substr($base, 0, -9));
        }

        foreach ($targetKeys as $key) {
            if (!isset($map[$key])) {
                continue;
            }
            // A directory-wide collision on this key is unresolvable -- do
            // not guess which of two files the caller meant.
            return count($map[$key]) === 1 ? $map[$key][0] : null;
        }

        return null;
    }

    /**
     * Scan $absDir once for class-*.php / interface-*.php files, normalize
     * each filename stem, and cache the resulting normalizedStem => [file
     * paths] map for the remainder of the request.
     *
     * @param string $absDir Absolute directory path (no trailing slash).
     * @return array<string,array<int,string>>
     */
    private static function scanDirectory(string $absDir): array
    {
        if (isset(self::$dirScanCache[$absDir])) {
            return self::$dirScanCache[$absDir];
        }

        $map = [];

        $entries = is_dir($absDir) ? scandir($absDir) : false;
        if ($entries !== false) {
            foreach ($entries as $entry) {
                if (substr($entry, -4) !== '.php') {
                    continue;
                }

                $stem = substr($entry, 0, -4);
                if (str_starts_with($stem, 'class-')) {
                    $rest = substr($stem, strlen('class-'));
                } elseif (str_starts_with($stem, 'interface-')) {
                    $rest = substr($stem, strlen('interface-'));
                } else {
                    $rest = $stem;
                }

                $key         = self::normalize($rest);
                $map[$key][] = $absDir . '/' . $entry;
            }
        }

        self::$dirScanCache[$absDir] = $map;

        return $map;
    }

    /**
     * StudlyCase to kebab-case, e.g. "ObjectCacheConfig" -> "object-cache-config".
     *
     * @param string $segment Input segment.
     * @return string
     */
    private static function kebabCase(string $segment): string
    {
        return strtolower(preg_replace('/(?<!^)[A-Z]/', '-$0', $segment) ?? $segment);
    }

    /**
     * Kebab-case each '/'-separated segment of a class's containing
     * directory independently, rather than lowercasing the whole path, so a
     * multi-word namespace segment like ObjectCache maps to the real
     * "object-cache" directory instead of "objectcache". Returns '' for the
     * base namespace (no subdirectory), otherwise a trailing-slash-suffixed
     * relative path.
     *
     * @param string $dir dirname() of the class's namespace-relative path ('.' for none).
     * @return string
     */
    private static function kebabCaseDir(string $dir): string
    {
        if ($dir === '.' || $dir === '') {
            return '';
        }

        $segments = array_map(static fn (string $segment): string => self::kebabCase($segment), explode('/', $dir));

        return implode('/', $segments) . '/';
    }

    /**
     * Case/dash-insensitive comparison key: lowercase, alphanumerics only.
     *
     * @param string $segment Input.
     * @return string
     */
    private static function normalize(string $segment): string
    {
        return strtolower(preg_replace('/[^A-Za-z0-9]/', '', $segment) ?? $segment);
    }
}
