<?php
/**
 * DestinationVerifier: the restore-skip's OWN backstop (GitHub issue #328).
 *
 * Positively re-reads a plugin or theme destination after a failure that was
 * classified as "core never touched it", and answers true, false or null. Only
 * `true` may skip a restore.
 *
 * THE FULL RATIONALE IS BELOW THE DIRECT-ACCESS GUARD, not above it. Plugin
 * Check's direct-file-access detector only reads the first 50 raw lines of a
 * file when looking for the guard, so a long header comment above it silently
 * turns into a shipping-blocking ERROR. Keep the guard high and the reasoning
 * underneath.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

if (!defined('ABSPATH')) {
    exit; // No direct access.
}

/*
 * DestinationVerifier: the restore-skip's OWN backstop (GitHub issue #328).
 *
 * WHY IT IS NOT OPTIONAL. UpdateOutcome tells us whether core COULD have
 * touched the destination, reasoning purely from the error code core returned.
 * That is an inference about code paths, not an observation of the disk, and
 * skipping a restore is irreversible in the sense that matters: the moment we
 * decide not to restore, nothing else in the system will. Every backstop that
 * looks like it would cover the gap was checked and does not:
 *   - the control plane returns on a failed item before its health probe runs;
 *   - the agent's own isComplete() verification is gated on a SUCCESSFUL apply;
 *   - core's restore_temp_backup is registered only inside the
 *     `if ( is_wp_error( $result ) )` branch on install_package()'s return, so
 *     it does not exist at all on a download or unpack failure;
 *   - the UpdateInFlight marker is cleared unconditionally when the item ends.
 * So the skip has to carry its own positive verification, and this class is it.
 *
 * THE CONTRACT. verify() returns a THREE-valued verdict and the caller may skip
 * the restore on `true` ONLY:
 *   true   the directory was positively re-read and matches its pre-update
 *          state (its header parses, names the expected slug's main file and
 *          still reports the pre-update version).
 *   false  something is provably wrong (directory gone, empty, header
 *          unparseable, version already moved, a payload entry missing).
 *   null   this host could not answer (root not listable, main file could not
 *          be resolved honestly, the WordPress header reader is unavailable,
 *          anything threw).
 * NULL AND FALSE BOTH RESTORE. Absence of contradiction is never enough; a
 * clean bill of health requires the positive read (signal R4). That means an
 * unverifiable host keeps exactly the behaviour it had before this class
 * existed, so this is a strict narrowing of the restore set, never a widening.
 *
 * IT RESOLVES THE ROOT ITSELF rather than through SnapshotManager::liveDir().
 * Deliberate: liveDir()'s realpath()-based containment has a documented
 * false-negative class on open_basedir and relocated/symlinked wp-content (the
 * S8 regression recorded in UpdateCommand's class doc), and a false negative
 * here would produce a `null` verdict and a needless restore on exactly the
 * population that regression already hurt twice. Use the SAME expression core
 * used as the install destination, so if core could write there we can list it.
 *
 * NEVER get_plugins() / UpdateRunner::currentVersion(). Those go through
 * WordPress's cached, full-directory plugin scan, which can happily return the
 * PRE-apply version for a directory that has since been wiped. That is the one
 * false TRUE this backstop must never produce, and a test pins it. Read the
 * header off the file with get_file_data() after clearstatcache().
 *
 * COST. About six syscalls plus one 8KB header read, and only on a failure that
 * has ALREADY been classified as "core never touched the destination". Zero on
 * every success path.
 *
 * KNOWN LIMIT, named rather than fixed: R5 compares only the top-level entry
 * set and R6 only the main file's size, so a deep-file-only modification passes
 * as intact. Core has no mechanism that does that (every path it has is
 * whole-directory), but a third party hooked on `upgrader_pre_install` or
 * `upgrader_source_selection` could. A full recursive comparison is the
 * complete answer and costs a whole-tree walk on the failure path.
 */

/**
 * Positively re-reads a plugin/theme destination after a pre-install failure.
 */
final class DestinationVerifier
{
    /**
     * Above this many entries on either side, the top-level superset check (R5)
     * is skipped rather than paid for. A plugin with more than 2000 top-level
     * entries is not a shape this check adds confidence to.
     */
    private const MAX_LISTING_ENTRIES = 2000;

    /**
     * Verify that a plugin/theme directory still looks exactly as it did before
     * the update that just failed.
     *
     * @param string $type        'plugin'|'theme'.
     * @param string $slug        Sanitized slug ('folder' or 'folder/main.php').
     * @param string $fromVersion The version installed BEFORE the apply. A true
     *                             verdict is impossible without it (see R4).
     * @param string $payloadDir  Absolute path to the snapshot payload (a copy
     *                             of the live directory taken before the apply),
     *                             or '' when there is no snapshot.
     * @return array{verdict:bool|null,detail:string,signals:array<int,string>}
     */
    public static function verify(string $type, string $slug, string $fromVersion, string $payloadDir): array
    {
        try {
            return self::run($type, $slug, $fromVersion, $payloadDir);
        } catch (\Throwable $e) {
            DebugLog::write('WPMgr Agent: destination verification threw: ' . $e->getMessage());

            return self::answer(null, 'verification could not run (' . $e->getMessage() . ')', ['threw']);
        }
    }

    /**
     * The verification proper; verify() is the never-throwing wrapper.
     *
     * @param string $type        'plugin'|'theme'.
     * @param string $slug        Sanitized slug.
     * @param string $fromVersion Pre-update version.
     * @param string $payloadDir  Snapshot payload directory, or ''.
     * @return array{verdict:bool|null,detail:string,signals:array<int,string>}
     */
    private static function run(string $type, string $slug, string $fromVersion, string $payloadDir): array
    {
        $signals = [];

        if ($type !== 'plugin' && $type !== 'theme') {
            return self::answer(null, 'only plugin and theme destinations can be verified', ['unsupported-type']);
        }

        $folder = self::folderOf($slug);
        if ($folder === '') {
            return self::answer(null, 'the target folder could not be derived from the slug', ['no-folder']);
        }

        // --- R0 ROOT LISTABLE -------------------------------------------
        // Once this succeeds, open_basedir is no longer a candidate
        // explanation for a child being unreadable, because open_basedir is
        // prefix-based. That is precisely what lets R1 say "absent" rather
        // than "unknown".
        $root = self::rootFor($type, $slug);
        if ($root === '') {
            return self::answer(null, 'the ' . $type . ' root directory could not be resolved', ['R0-no-root']);
        }
        $entries = self::listDir($root);
        if ($entries === null) {
            return self::answer(null, 'the ' . $type . ' root directory is not listable', ['R0-root-unlistable']);
        }
        $signals[] = 'R0-root-listable';

        // --- R1 DIRECTORY PRESENT ---------------------------------------
        // The destroyed-destination catch.
        if (!in_array($folder, $entries, true)) {
            if ($type === 'theme' && self::foundInAnotherThemeRoot($folder, $root)) {
                return self::answer(
                    null,
                    'the theme directory is not in the expected root but exists under another registered theme root',
                    array_merge($signals, ['R1-other-theme-root'])
                );
            }

            return self::answer(
                false,
                'the live directory is absent from ' . $root,
                array_merge($signals, ['R1-absent'])
            );
        }
        $signals[] = 'R1-present';

        // --- R2 LISTABLE AND NON-EMPTY ----------------------------------
        // Presence in the parent listing is positive evidence of existence;
        // permissions are not evidence of destruction, so an unlistable child
        // is null and never false.
        $live      = $root . '/' . $folder;
        $liveNames = self::listDir($live);
        if ($liveNames === null) {
            return self::answer(
                null,
                'the live directory is present but not listable',
                array_merge($signals, ['R2-unlistable'])
            );
        }
        if ($liveNames === []) {
            return self::answer(
                false,
                'the live directory is present but empty',
                array_merge($signals, ['R2-empty'])
            );
        }
        $signals[] = 'R2-non-empty';

        // --- R3 MAIN FILE RESOLUTION, HONEST ----------------------------
        // A GUESSED main file name that misses must never produce FALSE, or
        // every plugin with an unconventional main file becomes a permanent
        // spurious-restore machine.
        $mainRelative = self::mainFileRelative($type, $slug, $folder, $payloadDir, $liveNames);
        if ($mainRelative === '') {
            return self::answer(
                null,
                'the main ' . $type . ' file could not be resolved for this target',
                array_merge($signals, ['R3-unresolved'])
            );
        }
        $signals[]  = 'R3-main-file';
        $mainPath   = $live . '/' . $mainRelative;

        // --- R4 HEADER RE-READ, THE POSITIVE SIGNAL ---------------------
        $header = self::readHeader($type, $mainPath);
        if ($header === null) {
            return self::answer(
                null,
                'this host could not read the ' . $type . ' header after the failure',
                array_merge($signals, ['R4-unreadable'])
            );
        }
        if ($header['Name'] === '') {
            // The truncated half-write.
            return self::answer(
                false,
                'the main file is present but its header did not parse',
                array_merge($signals, ['R4-header-unparseable'])
            );
        }
        if ($fromVersion === '') {
            // Without a pre-update version there is nothing to match the
            // on-disk version against, so the positive signal cannot complete.
            return self::answer(
                null,
                'the pre-update version is unknown, so the header could not be matched against it',
                array_merge($signals, ['R4-no-from-version'])
            );
        }
        if ($header['Version'] !== $fromVersion) {
            // A swap that COMPLETED while reporting a pre-boundary code.
            // Exact string identity, never version_compare: any movement at
            // all means core did reach the destination.
            return self::answer(
                false,
                'the on-disk version is ' . self::clip($header['Version']) . ', not the pre-update '
                . self::clip($fromVersion),
                array_merge($signals, ['R4-version-moved'])
            );
        }
        $signals[] = 'R4-header-matches';

        // --- R5 TOP-LEVEL SUPERSET --------------------------------------
        // Every payload top-level entry must still be present live. EXTRA live
        // entries are NOT evidence: core has no mechanism that adds a single
        // top-level file (every path it has is whole-directory), whereas a
        // plugin writing its own cache file between capture and check is
        // completely normal.
        $payloadNames = $payloadDir !== '' ? self::listDir($payloadDir) : null;
        if ($payloadNames !== null && $payloadNames !== []) {
            if (count($payloadNames) > self::MAX_LISTING_ENTRIES || count($liveNames) > self::MAX_LISTING_ENTRIES) {
                $signals[] = 'R5-skipped-oversized';
            } else {
                $liveIndex = array_flip($liveNames);
                foreach ($payloadNames as $name) {
                    if (!isset($liveIndex[$name])) {
                        return self::answer(
                            false,
                            'a top-level entry present before the update is missing now (' . self::clip($name) . ')',
                            array_merge($signals, ['R5-entry-missing'])
                        );
                    }
                }
                $signals[] = 'R5-superset';
            }
        } else {
            $signals[] = 'R5-skipped-no-payload';
        }

        // --- R6 MAIN FILE SIZE ------------------------------------------
        // SIZE ONLY, NEVER mtime: the snapshot copy does not preserve mtimes,
        // so an mtime comparison is a false positive on every single capture.
        // A payload with no copy of this relative path yields NULL for this
        // signal and never FALSE: symlinks are skipped when the payload is
        // built, and on a Bedrock or Composer layout a plain size inequality
        // against a missing payload file would be a spurious restore on every
        // such host.
        if ($payloadDir !== '') {
            $liveSize    = self::sizeOf($mainPath);
            $payloadSize = self::sizeOf($payloadDir . '/' . $mainRelative);
            if ($liveSize !== null && $payloadSize !== null) {
                if ($liveSize !== $payloadSize) {
                    return self::answer(
                        false,
                        'the main file size changed since the pre-update snapshot',
                        array_merge($signals, ['R6-size-differs'])
                    );
                }
                $signals[] = 'R6-size-matches';
            } else {
                $signals[] = 'R6-skipped-no-payload-copy';
            }
        }

        return self::answer(
            true,
            'the ' . $type . ' header still reports ' . self::clip($fromVersion)
            . ' and the directory contents are intact',
            $signals
        );
    }

    /**
     * The destination root core itself would have installed into.
     *
     * @param string $type 'plugin'|'theme'.
     * @param string $slug Sanitized slug (themes may have per-theme roots).
     * @return string Absolute path without a trailing slash, or ''.
     */
    private static function rootFor(string $type, string $slug): string
    {
        if ($type === 'plugin') {
            // Core passes WP_PLUGIN_DIR as `destination` at
            // class-plugin-upgrader.php:227.
            if (defined('WP_PLUGIN_DIR')) {
                return rtrim((string) constant('WP_PLUGIN_DIR'), '/\\');
            }
            if (defined('WP_CONTENT_DIR')) {
                return rtrim((string) constant('WP_CONTENT_DIR'), '/\\') . '/plugins';
            }

            return '';
        }

        // Core passes get_theme_root( $theme ) at class-theme-upgrader.php:327.
        if (function_exists('get_theme_root')) {
            $root = get_theme_root($slug);
            if (is_string($root) && $root !== '') {
                return rtrim($root, '/\\');
            }
        }
        if (defined('WP_CONTENT_DIR')) {
            return rtrim((string) constant('WP_CONTENT_DIR'), '/\\') . '/themes';
        }

        return '';
    }

    /**
     * A theme that is absent from the expected root but present under another
     * registered root is an inconclusive reading, not a destroyed directory.
     *
     * @param string $folder   Theme stylesheet directory name.
     * @param string $expected The root already checked.
     * @return bool
     */
    private static function foundInAnotherThemeRoot(string $folder, string $expected): bool
    {
        $roots = [];
        if (function_exists('get_theme_roots')) {
            $registered = get_theme_roots();
            if (is_array($registered)) {
                foreach ($registered as $candidate) {
                    if (is_string($candidate) && $candidate !== '') {
                        $roots[] = $candidate;
                    }
                }
            }
        }
        if (isset($GLOBALS['wp_theme_directories']) && is_array($GLOBALS['wp_theme_directories'])) {
            foreach ($GLOBALS['wp_theme_directories'] as $candidate) {
                if (is_string($candidate) && $candidate !== '') {
                    $roots[] = $candidate;
                }
            }
        }

        foreach (array_unique($roots) as $candidate) {
            $candidate = rtrim($candidate, '/\\');
            if ($candidate === '' || $candidate === $expected) {
                continue;
            }
            // A theme root registered as a wp-content-relative path is not a
            // filesystem path; only absolute candidates are checkable here.
            if (@is_dir($candidate . '/' . $folder)) { // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- probing a possibly open_basedir-excluded alternate theme root; a warning here must not become output
                return true;
            }
        }

        return false;
    }

    /**
     * The directory component of a slug that may be a bare folder or a full
     * "folder/main-file.php" plugin basename.
     *
     * @param string $slug Sanitized slug.
     * @return string
     */
    private static function folderOf(string $slug): string
    {
        $pos = strpos($slug, '/');

        return $pos === false ? $slug : substr($slug, 0, $pos);
    }

    /**
     * Resolve the main file's path RELATIVE to the target directory.
     *
     * Plugin, in order of trust:
     *   1. the control plane gave us the exact basename ("folder/main.php");
     *   2. the SNAPSHOT: the payload is a copy of the pre-update directory, so
     *      "<folder>.php" in it, or a lone *.php, is the real main file;
     *   3. the live listing's own "<folder>.php", or a lone *.php there.
     * Anything else gives up and returns '' (a NULL verdict), because guessing
     * and missing would report a healthy directory as modified.
     *
     * Theme: always style.css (core's own check_package requires it).
     *
     * @param string            $type       'plugin'|'theme'.
     * @param string            $slug       Sanitized slug.
     * @param string            $folder     Folder component of the slug.
     * @param string            $payloadDir Snapshot payload directory, or ''.
     * @param array<int,string> $liveNames  Top-level entries of the live directory.
     * @return string Relative path, or '' when it cannot be resolved honestly.
     */
    private static function mainFileRelative(
        string $type,
        string $slug,
        string $folder,
        string $payloadDir,
        array $liveNames
    ): string {
        if ($type === 'theme') {
            return 'style.css';
        }

        $pos = strpos($slug, '/');
        if ($pos !== false) {
            $basename = substr($slug, $pos + 1);

            return $basename !== '' ? $basename : '';
        }

        if ($payloadDir !== '') {
            $payloadNames = self::listDir($payloadDir);
            if ($payloadNames !== null) {
                $resolved = self::pickMainPhp($payloadNames, $folder);
                if ($resolved !== '') {
                    return $resolved;
                }
            }
        }

        return self::pickMainPhp($liveNames, $folder);
    }

    /**
     * "<folder>.php" when present, else the single *.php when there is exactly
     * one, else ''.
     *
     * @param array<int,string> $names  Top-level entry names.
     * @param string            $folder Folder name.
     * @return string
     */
    private static function pickMainPhp(array $names, string $folder): string
    {
        $preferred = $folder . '.php';
        if (in_array($preferred, $names, true)) {
            return $preferred;
        }

        $php = [];
        foreach ($names as $name) {
            if (strlen($name) > 4 && strtolower(substr($name, -4)) === '.php') {
                $php[] = $name;
            }
        }

        return count($php) === 1 ? $php[0] : '';
    }

    /**
     * Read Name/Version straight off the file, bypassing every WordPress cache.
     *
     * get_file_data() lives in wp-includes/functions.php, so it is present on
     * any booted site and does not require the wp-admin plugin API. It reads
     * the first 8KB of the file each time it is called; clearstatcache() first
     * so a stat cached before the failed apply cannot answer for a file that
     * has since changed on disk.
     *
     * @param string $type 'plugin'|'theme'.
     * @param string $path Absolute path to the main file.
     * @return array{Name:string,Version:string}|null Null when it could not run.
     */
    private static function readHeader(string $type, string $path): ?array
    {
        if (!function_exists('get_file_data')) {
            return null;
        }

        clearstatcache(true, $path);
        if (!@is_file($path) || !@is_readable($path)) { // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- the destination may be mid-failure or permission-restricted; a warning here must never reach the response body
            return null;
        }

        $map = $type === 'plugin'
            ? ['Name' => 'Plugin Name', 'Version' => 'Version']
            : ['Name' => 'Theme Name', 'Version' => 'Version'];

        $data = @get_file_data($path, $map, $type); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- a header read on a possibly half-written file must not emit into the response
        if (!is_array($data)) {
            return null;
        }

        return [
            'Name'    => isset($data['Name']) && is_string($data['Name']) ? trim($data['Name']) : '',
            'Version' => isset($data['Version']) && is_string($data['Version']) ? trim($data['Version']) : '',
        ];
    }

    /**
     * Top-level entry names of a directory, excluding . and .., or null when
     * the directory cannot be listed at all.
     *
     * @param string $dir Absolute directory path.
     * @return array<int,string>|null
     */
    private static function listDir(string $dir): ?array
    {
        if ($dir === '') {
            return null;
        }

        clearstatcache(true, $dir);
        $entries = @scandir($dir); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- an unlistable directory is an expected, meaningful answer here (see R0/R2); a warning must not reach the response
        if (!is_array($entries)) {
            return null;
        }

        $out = [];
        foreach ($entries as $entry) {
            if ($entry === '.' || $entry === '..') {
                continue;
            }
            $out[] = (string) $entry;
        }

        return $out;
    }

    /**
     * File size in bytes, or null when it cannot be determined.
     *
     * @param string $path Absolute file path.
     * @return int|null
     */
    private static function sizeOf(string $path): ?int
    {
        clearstatcache(true, $path);
        if (!@is_file($path)) { // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- a missing or unreadable payload copy is an expected answer (see R6); a warning must not reach the response
            return null;
        }
        $size = @filesize($path); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- same

        return is_int($size) ? $size : null;
    }

    /**
     * Bound a value before it reaches an operator-visible sentence.
     *
     * @param string $value Raw value.
     * @return string
     */
    private static function clip(string $value): string
    {
        $clean = (string) preg_replace('#[\x00-\x1F\x7F]+#', ' ', $value);
        $clean = trim($clean);

        return strlen($clean) > 64 ? substr($clean, 0, 64) . '...' : $clean;
    }

    /**
     * Build the return shape.
     *
     * @param bool|null         $verdict Verdict.
     * @param string            $detail  Human sentence.
     * @param array<int,string> $signals Signal trail, in evaluation order.
     * @return array{verdict:bool|null,detail:string,signals:array<int,string>}
     */
    private static function answer(?bool $verdict, string $detail, array $signals): array
    {
        return ['verdict' => $verdict, 'detail' => $detail, 'signals' => $signals];
    }
}
