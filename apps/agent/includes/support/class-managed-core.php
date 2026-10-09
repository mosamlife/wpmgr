<?php
/**
 * ManagedCore: whether WordPress core on this site is owned by something other
 * than WordPress's own updater (GitHub issue #367).
 *
 * The full rationale sits below the direct-access guard: Plugin Check looks
 * for that guard only in a file's first 50 lines.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

use WPMgr\Agent\Cache\WpConfigEditor;

if (!defined('ABSPATH')) {
    exit; // No direct access.
}

/*
 * WHY THIS EXISTS. On a site whose core is installed by Composer (a
 * Roots/Bedrock project, or any project requiring a WordPress core package),
 * a core update applied from inside WordPress cannot be right. Either it fails
 * part way, as in #367, where the content directory is not writable by PHP and
 * core stops while unpacking, or it succeeds and the next Composer deploy
 * quietly puts the old core back. The remedy is the project's composer.json
 * and a redeploy, never an in-place update.
 *
 * The same answer applies when file changes are disallowed on the site
 * (DISALLOW_FILE_MODS, or the `file_mod_allowed` filter). That is not an
 * agent invention: WordPress gates its own `update_core` capability on
 * wp_is_file_mod_allowed('capability_update_core') and turns its background
 * updater off on wp_is_file_mod_allowed('automatic_updater'), so with file
 * changes disallowed WordPress itself never updates core. This class asks
 * exactly the capability question core asks, so a site that re-allows admin
 * core updates through that filter re-allows ours too.
 *
 * WHAT IT READS, and nothing else:
 *   - whether the Roots\WPConfig\Config class is already loaded (no I/O);
 *   - the wp-config.php WordPress itself loaded, located by wp-load.php's own
 *     rule, matched as a string through WpConfigEditor::isManagedConfigContent()
 *     with its comments left out, and never included or evaluated;
 *   - a file named composer.json in the WordPress directory and in at most
 *     PARENT_LEVELS directories above it, decoded as JSON and inspected only
 *     for the core packages below and the directory it installs core into,
 *     which must be this WordPress directory.
 * Every read is bounded in size, every probe is @-suppressed and skipped when
 * it would fall outside open_basedir, and every failure reads as "no
 * evidence". A missing or unreadable signal therefore leaves the core update
 * path exactly as it was before this class existed. Nothing here writes.
 *
 * Read lazily: constructing this class does no I/O, because the update command
 * is constructed on every request that registers routes.
 */

/**
 * Detects a Composer-owned or file-change-locked WordPress core.
 */
class ManagedCore
{
    /** Core is installed or owned by Composer. */
    public const REASON_COMPOSER = 'composer';

    /** File changes are disallowed on this site, so WordPress does not update core either. */
    public const REASON_FILE_MODS = 'file_mods';

    /**
     * Composer packages whose only job is to install WordPress core itself.
     * Composer package names are case-insensitive, so they are compared
     * lowercased.
     *
     * @var array<int,string>
     */
    public const CORE_PACKAGES = [
        'roots/wordpress',
        'roots/wordpress-no-content',
        'roots/wordpress-full',
        'johnpbloch/wordpress',
        'johnpbloch/wordpress-core',
    ];

    /**
     * How far above the WordPress directory a composer.json is looked for. A
     * Bedrock project keeps it two levels up (web/wp), a plain installer
     * layout one level up (wordpress/). Bounded so a manifest belonging to an
     * unrelated project higher up the tree is never read.
     */
    private const PARENT_LEVELS = 3;

    /** The largest composer.json or wp-config.php this class reads, in bytes. */
    private const MAX_READ_BYTES = 1048576;

    /** WordPress directory to inspect. */
    private string $abspath;

    /**
     * Test seam for the file-changes question. Null means "ask WordPress".
     *
     * @var (callable():bool)|null
     */
    private $fileModsAllowed;

    /**
     * Memoized verdict; null until detect() first runs.
     *
     * @var array{reason:string,evidence:string}|null
     */
    private ?array $verdict = null;

    /**
     * @param string|null             $abspath         WordPress directory to inspect. Null reads ABSPATH.
     * @param (callable():bool)|null  $fileModsAllowed Replaces the file-changes question (tests only).
     */
    public function __construct(?string $abspath = null, ?callable $fileModsAllowed = null)
    {
        if ($abspath === null) {
            $abspath = defined('ABSPATH') ? (string) constant('ABSPATH') : '';
        }

        $this->abspath         = $abspath;
        $this->fileModsAllowed = $fileModsAllowed;
    }

    /**
     * Why WordPress core on this site must not be updated from inside
     * WordPress, if it must not.
     *
     * `reason` is '' when core is updatable here, otherwise REASON_COMPOSER or
     * REASON_FILE_MODS. Composer wins when both apply, because its remedy
     * (composer.json and a redeploy) is the one that actually updates core.
     * `evidence` is a short, operator-facing description of what was found;
     * it never contains file content.
     *
     * @return array{reason:string,evidence:string}
     */
    public function detect(): array
    {
        if ($this->verdict !== null) {
            return $this->verdict;
        }

        $evidence = $this->composerEvidence();
        if ($evidence !== '') {
            return $this->verdict = ['reason' => self::REASON_COMPOSER, 'evidence' => $evidence];
        }

        $evidence = $this->fileModsEvidence();
        if ($evidence !== '') {
            return $this->verdict = ['reason' => self::REASON_FILE_MODS, 'evidence' => $evidence];
        }

        return $this->verdict = ['reason' => '', 'evidence' => ''];
    }

    /**
     * Whether $dir lies inside one of the directories an open_basedir value
     * allows. An empty value allows everything.
     *
     * Anchored on a path-segment boundary: "/srv/www" allows "/srv/www" and
     * "/srv/www/site" but never "/srv/www2". That is stricter than PHP's own
     * prefix semantics on purpose, because the only cost of being stricter is
     * skipping a probe. A "." entry (the working directory) is skipped for the
     * same reason: this class never probes relative to the working directory.
     *
     * @param string $dir         Absolute directory.
     * @param string $openBasedir The open_basedir value.
     * @return bool
     */
    public static function pathAllowedBy(string $dir, string $openBasedir): bool
    {
        if (trim($openBasedir) === '') {
            return true;
        }

        $dir = rtrim(str_replace('\\', '/', $dir), '/') . '/';

        foreach (explode(PATH_SEPARATOR, $openBasedir) as $base) {
            $base = trim($base);
            if ($base === '' || $base === '.') {
                continue;
            }

            $base = rtrim(str_replace('\\', '/', $base), '/') . '/';
            if (strncmp($dir, $base, strlen($base)) === 0) {
                return true;
            }
        }

        return false;
    }

    /**
     * The Composer signals, cheapest first.
     *
     * @return string Evidence, or '' when none was found.
     */
    private function composerEvidence(): string
    {
        try {
            // Bedrock's config/application.php runs Roots\WPConfig\Config before
            // WordPress loads, so on such a site the class is already in memory.
            // `false`: never trigger an autoloader for it.
            if (class_exists('Roots\\WPConfig\\Config', false)) {
                return 'the Roots\\WPConfig\\Config class is loaded';
            }

            $evidence = $this->configEvidence();
            if ($evidence !== '') {
                return $evidence;
            }

            return $this->manifestEvidence();
        } catch (\Throwable $e) {
            return '';
        }
    }

    /**
     * The wp-config.php signal. Locates the file by wp-load.php's own rule:
     * the WordPress directory first, then one level up unless a wp-settings.php
     * there says that directory is another installation.
     *
     * @return string Evidence, or ''.
     */
    private function configEvidence(): string
    {
        $dir = $this->wordPressDir();
        if ($dir === '') {
            return '';
        }

        $candidates = [$dir . '/wp-config.php'];
        $parent     = self::parentOf($dir);
        if ($parent !== '' && !$this->exists(self::join($parent, 'wp-settings.php'))) {
            $candidates[] = self::join($parent, 'wp-config.php');
        }

        foreach ($candidates as $path) {
            if (!$this->isFile($path)) {
                continue;
            }

            // The first existing candidate is the file WordPress loaded; stop
            // there whatever it says, exactly as wp-load.php does. A comment
            // is not configuration, so it is left out. The plain string test
            // runs first, so only a file that already looks managed is lexed.
            $content = $this->boundedRead($path);
            $managed = $content !== ''
                && WpConfigEditor::isManagedConfigContent($content)
                && WpConfigEditor::isManagedConfigContent(self::withoutComments($content));

            return $managed
                ? $path . ' loads a framework-managed config (Roots\\WPConfig\\Config or config/application.php)'
                : '';
        }

        return '';
    }

    /**
     * The composer.json signal: the WordPress directory and up to PARENT_LEVELS
     * directories above it. Depth alone is not ownership: a manifest counts
     * only when it installs core into this WordPress directory.
     *
     * @return string Evidence, or ''.
     */
    private function manifestEvidence(): string
    {
        $dir = $this->wordPressDir();
        if ($dir === '') {
            return '';
        }

        for ($level = 0; $level <= self::PARENT_LEVELS; $level++) {
            $evidence = $this->manifestEvidenceIn($dir, $level === 0);
            if ($evidence !== '') {
                return $evidence;
            }

            $dir = self::parentOf($dir);
            if ($dir === '') {
                break;
            }
        }

        return '';
    }

    /**
     * Inspect one directory's composer.json.
     *
     * @param string $dir            Absolute directory.
     * @param bool   $isWordPressDir Whether $dir is the WordPress directory itself.
     * @return string Evidence, or ''.
     */
    private function manifestEvidenceIn(string $dir, bool $isWordPressDir): string
    {
        $file = self::join($dir, 'composer.json');
        if (!$this->isFile($file)) {
            return '';
        }

        $raw = $this->boundedRead($file);
        if ($raw === '') {
            return '';
        }

        $manifest = json_decode($raw, true);
        if (!is_array($manifest)) {
            return '';
        }

        // A core package installed by Composer ships its own manifest inside
        // the WordPress directory, typed for the core installers.
        if ($isWordPressDir) {
            $type = $manifest['type'] ?? null;
            $name = $manifest['name'] ?? null;
            if (is_string($type) && strtolower(trim($type)) === 'wordpress-core') {
                return $file . ' marks this WordPress directory as a Composer-installed core package';
            }
            if (is_string($name) && in_array(strtolower(trim($name)), self::CORE_PACKAGES, true)) {
                return $file . ' marks this WordPress directory as the Composer package ' . strtolower(trim($name));
            }
        }

        // `require` only. A core package under `require-dev` is a development
        // or test dependency, which is not what serves this site.
        $require = $manifest['require'] ?? null;
        if (!is_array($require)) {
            return '';
        }

        foreach (array_keys($require) as $package) {
            $package = strtolower(trim((string) $package));
            if (in_array($package, self::CORE_PACKAGES, true)) {
                return $this->installsCoreHere($manifest, $dir) ? $file . ' requires ' . $package : '';
            }
        }

        return '';
    }

    /**
     * Whether a manifest installs core into this WordPress directory. The core
     * installers read `extra.wordpress-install-dir`: one directory, or a map
     * from core package to directory, relative to the manifest unless
     * absolute, and "wordpress" when it names none.
     *
     * @param array<mixed,mixed> $manifest    Decoded composer.json.
     * @param string             $manifestDir Directory holding it.
     * @return bool
     */
    private function installsCoreHere(array $manifest, string $manifestDir): bool
    {
        $extra   = $manifest['extra'] ?? null;
        $setting = is_array($extra) ? ($extra['wordpress-install-dir'] ?? null) : null;

        $named = is_string($setting) ? [$setting] : [];
        if (is_array($setting)) {
            foreach ($setting as $package => $dir) {
                if (in_array(strtolower(trim((string) $package)), self::CORE_PACKAGES, true)) {
                    $named[] = $dir;
                }
            }
        }

        $dirs = [];
        foreach ($named as $dir) {
            if (is_string($dir) && trim($dir) !== '') {
                $dirs[] = str_replace('\\', '/', trim($dir));
            }
        }

        $here = self::normalize($this->wordPressDir());
        foreach ($dirs !== [] ? $dirs : ['wordpress'] as $dir) {
            if (self::normalize(self::isAbsolute($dir) ? $dir : $manifestDir . '/' . $dir) === $here) {
                return true;
            }
        }

        return false;
    }

    /**
     * A path with its "." and ".." segments and repeated separators resolved
     * as text, without touching the filesystem.
     *
     * @param string $path Absolute path with forward slashes.
     * @return string
     */
    private static function normalize(string $path): string
    {
        $drive = preg_match('#^[A-Za-z]:#', $path) === 1 ? substr($path, 0, 2) : '';
        $parts = [];
        foreach (explode('/', substr($path, strlen($drive))) as $part) {
            if ($part === '..') {
                array_pop($parts);
            } elseif ($part !== '' && $part !== '.') {
                $parts[] = $part;
            }
        }

        return $drive . '/' . implode('/', $parts);
    }

    /**
     * PHP source with its comments replaced by a space, so a commented-out
     * line never reads as configuration. token_get_all() only splits the text
     * into tokens and never runs it. '' without the tokenizer extension, which
     * makes the signal no evidence, like any other signal that cannot be read.
     *
     * @param string $source PHP source.
     * @return string
     */
    private static function withoutComments(string $source): string
    {
        if (!function_exists('token_get_all')) {
            return '';
        }

        $code = '';
        foreach (token_get_all($source) as $token) {
            if (!is_array($token)) {
                $code .= $token;
            } else {
                $code .= ($token[0] === T_COMMENT || $token[0] === T_DOC_COMMENT) ? ' ' : $token[1];
            }
        }

        return $code;
    }

    /**
     * The file-changes signal, asked the way WordPress asks it for its own
     * `update_core` capability.
     *
     * @return string Evidence, or ''.
     */
    private function fileModsEvidence(): string
    {
        if ($this->fileModsAllowed !== null) {
            try {
                return ($this->fileModsAllowed)() ? '' : 'file changes are disallowed on this site';
            } catch (\Throwable $e) {
                return '';
            }
        }

        $constant = defined('DISALLOW_FILE_MODS') && (bool) constant('DISALLOW_FILE_MODS');

        if (function_exists('wp_is_file_mod_allowed')) {
            try {
                if (wp_is_file_mod_allowed('capability_update_core')) {
                    return '';
                }

                return $constant
                    ? 'DISALLOW_FILE_MODS is set to true'
                    : 'the file_mod_allowed filter disallows file changes';
            } catch (\Throwable $e) {
                // A filter that throws is no answer; fall back to the constant.
            }
        }

        return $constant ? 'DISALLOW_FILE_MODS is set to true' : '';
    }

    /**
     * The WordPress directory, normalized, or '' when it is unusable. Never
     * the filesystem root and never a relative path: a search from either
     * would read files that have nothing to do with this site.
     *
     * @return string
     */
    private function wordPressDir(): string
    {
        $dir = rtrim(str_replace('\\', '/', trim($this->abspath)), '/');
        if ($dir === '') {
            return '';
        }

        return self::isAbsolute($dir) ? $dir : '';
    }

    /**
     * Whether a non-empty path with forward slashes is absolute.
     *
     * @param string $path Path.
     * @return bool
     */
    private static function isAbsolute(string $path): bool
    {
        return $path[0] === '/' || preg_match('#^[A-Za-z]:/#', $path) === 1;
    }

    /**
     * The parent of a normalized directory, or '' at the top of the tree.
     *
     * @param string $dir Normalized absolute directory ('/' for the root).
     * @return string
     */
    private static function parentOf(string $dir): string
    {
        if ($dir === '/' || preg_match('#^[A-Za-z]:/?$#', $dir) === 1) {
            return '';
        }

        $parent = str_replace('\\', '/', dirname($dir));
        if ($parent === '' || $parent === '.' || $parent === $dir) {
            return '';
        }

        return $parent;
    }

    /**
     * Join a directory and a file name without doubling the separator at the
     * filesystem root.
     *
     * @param string $dir  Directory.
     * @param string $name File name.
     * @return string
     */
    private static function join(string $dir, string $name): string
    {
        return rtrim($dir, '/') . '/' . $name;
    }

    /**
     * Whether $path is a readable regular file, probed only when its directory
     * is inside open_basedir.
     *
     * @param string $path Absolute file path.
     * @return bool
     */
    private function isFile(string $path): bool
    {
        if (!self::pathAllowedBy(dirname($path), (string) ini_get('open_basedir'))) {
            return false;
        }

        return @is_file($path) && @is_readable($path);
    }

    /**
     * Whether anything exists at $path, probed only when its directory is
     * inside open_basedir. wp-load.php's own test for the sibling
     * wp-settings.php.
     *
     * @param string $path Absolute path.
     * @return bool
     */
    private function exists(string $path): bool
    {
        if (!self::pathAllowedBy(dirname($path), (string) ini_get('open_basedir'))) {
            return false;
        }

        return @file_exists($path);
    }

    /**
     * Read a small file whole, or return '' when it is empty, too large or
     * unreadable.
     *
     * @param string $path Absolute file path, already checked by isFile().
     * @return string
     */
    private function boundedRead(string $path): string
    {
        $size = @filesize($path);
        if (!is_int($size) || $size <= 0 || $size > self::MAX_READ_BYTES) {
            return '';
        }

        $content = @file_get_contents($path, false, null, 0, self::MAX_READ_BYTES);

        return is_string($content) ? $content : '';
    }
}
