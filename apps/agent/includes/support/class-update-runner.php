<?php
/**
 * UpdateRunner: resolves installed versions and applies plugin/theme/core
 * updates, preferring WP-CLI when available and falling back to the WordPress
 * upgrader APIs.
 *
 * This class is the single seam between the command logic and the WordPress /
 * shell runtime, which keeps the command itself unit-testable.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

/**
 * Executes updates via WP-CLI or the WP upgrader APIs.
 */
class UpdateRunner
{
    /**
     * The reason the most recent isComplete() call returned false, or '' when
     * that call returned true (or isComplete() has not been called yet).
     * Reset to '' at the START of every isComplete() call (see that method)
     * so a caller can never read a stale reason left over from an earlier
     * item in the same batch/request. Populated only from the same concise,
     * non-secret (slug/version/WP-error-message) text already written to
     * DebugLog by isPluginComplete()/isThemeComplete() — safe to surface
     * directly in the CP-visible per-item result log (agent-only, 0.61.19,
     * GitHub issue #182's sibling visibility fix).
     */
    private string $lastIncompleteReason = '';

    /**
     * Validate an untrusted version string before it reaches WP-CLI argv or an
     * upgrader offer URL. Accepts only the literal "latest" or a token that
     * starts with a digit and contains only [0-9A-Za-z.-] (no spaces, no flag
     * separators), which blocks argument injection such as "latest --activate"
     * or "1.0 --activate".
     *
     * @param string $version Raw requested version.
     * @return bool True when safe to use.
     */
    public static function isValidVersion(string $version): bool
    {
        if ($version === 'latest') {
            return true;
        }

        return preg_match('#^[0-9][0-9A-Za-z.\-]*$#', $version) === 1;
    }

    /**
     * Resolve the currently installed version of an item.
     *
     * @param string $type plugin|theme|core.
     * @param string $slug Sanitized slug (ignored for core).
     * @return string Installed version, or '' when unknown.
     */
    public function currentVersion(string $type, string $slug): string
    {
        switch ($type) {
            case 'core':
                if (function_exists('get_bloginfo')) {
                    $v = get_bloginfo('version');
                    if (is_string($v) && $v !== '') {
                        return $v;
                    }
                }

                return isset($GLOBALS['wp_version']) && is_scalar($GLOBALS['wp_version'])
                    ? (string) $GLOBALS['wp_version']
                    : '';

            case 'plugin':
                return $this->pluginVersion($slug);

            case 'theme':
                return $this->themeVersion($slug);
        }

        return '';
    }

    /**
     * Is this item actually present on the site?
     *
     * Used to distinguish a genuinely not-applicable update target (plugin/
     * theme never installed here, e.g. a stale or mistargeted control-plane
     * task) from a real upgrade failure. Core is always "installed". When the
     * detection API itself is unavailable (e.g. `get_plugins()` not loaded in
     * a constrained runtime), this fails OPEN — returns true — so we never
     * misreport a genuine target as not-installed just because we couldn't
     * check; a real failure downstream still surfaces as 'failed'.
     *
     * @param string $type plugin|theme|core.
     * @param string $slug Sanitized slug (ignored for core).
     * @return bool
     */
    public function isInstalled(string $type, string $slug): bool
    {
        switch ($type) {
            case 'core':
                return true;

            case 'plugin':
                $this->loadPluginApi();
                if (!function_exists('get_plugins')) {
                    return true;
                }
                $all = get_plugins();
                if (!is_array($all)) {
                    return true;
                }
                if (isset($all[$slug])) {
                    return true;
                }
                // Allow a folder-only slug — or a full "folder/main-file.php"
                // basename whose main file was since renamed (S6, issue
                // #131) — to match by FOLDER. See pluginFolder()'s doc.
                $folder = self::pluginFolder($slug);
                foreach (array_keys($all) as $file) {
                    if (is_string($file) && str_starts_with($file, $folder . '/')) {
                        return true;
                    }
                }

                return false;

            case 'theme':
                if (!function_exists('wp_get_themes')) {
                    return true;
                }
                $themes = wp_get_themes();
                if (!is_array($themes)) {
                    return true;
                }

                return isset($themes[$slug]);
        }

        return true;
    }

    /**
     * Resolve the version that an update would move the item to.
     *
     * Used only by dry-run. For an explicit version request we return it as-is.
     * For 'latest' we consult the WordPress update transients / version check.
     *
     * @param string $type      plugin|theme|core.
     * @param string $slug      Sanitized slug.
     * @param string $requested 'latest' or an explicit x.y.z.
     * @return string Target version, or '' when none is available.
     */
    public function availableVersion(string $type, string $slug, string $requested): string
    {
        if ($requested !== 'latest') {
            return $requested;
        }

        switch ($type) {
            case 'plugin':
                return $this->pluginUpdateVersion($slug);
            case 'theme':
                return $this->themeUpdateVersion($slug);
            case 'core':
                return $this->coreUpdateVersion();
        }

        return '';
    }

    /**
     * Apply the update. Prefers WP-CLI; falls back to the upgrader APIs.
     *
     * @param string $type    plugin|theme|core.
     * @param string $slug    Sanitized slug.
     * @param string $version 'latest' or an explicit x.y.z.
     * @return array{ok:bool,log:string}
     */
    public function apply(string $type, string $slug, string $version): array
    {
        if (!self::isValidVersion($version)) {
            return ['ok' => false, 'log' => 'Rejected unsafe version string.'];
        }

        if ($this->wpCliAvailable()) {
            return $this->applyViaWpCli($type, $slug, $version);
        }

        return $this->applyViaUpgrader($type, $slug, $version);
    }

    /**
     * Verify that a just-applied plugin/theme update produced a complete,
     * loadable artifact rather than a half-written directory left by an
     * aborted copy (GitHub issue #131).
     *
     * A version-header bump alone does not prove an apply finished: WordPress
     * core's install_package()/copy_dir() is a non-atomic recursive copy that
     * clears the destination directory FIRST, then copies files in; if the
     * PHP process is killed mid-copy (max_execution_time, PHP-FPM
     * request_terminate_timeout) the main plugin/theme file — which core
     * copies early — can already be in place and report a new version while
     * the rest of the tree (e.g. a `vendor/` autoloader) is still missing.
     *
     * Deliberately MINIMAL: this reuses WordPress's own definition of "valid"
     * rather than inventing content hashing, file-count comparisons, or any
     * other heuristic that could false-fail a legitimately unusual (but
     * complete) plugin/theme layout.
     *   - plugin: validate_plugin() — the same check core itself runs before
     *     activating a plugin. It resolves the header via get_plugins() and
     *     confirms the main file exists on disk; a WP_Error return means the
     *     header could not be re-read (the half-write symptom).
     *   - theme: the style.css `Name` header must still be present and
     *     non-empty after the apply — the same signal WordPress treats as
     *     "this is not a valid theme".
     *   - core: always true. Core updates have no directory-level rollback
     *     here by design (see UpdateCommand's class doc, D3) — recovery for
     *     core relies on UpdateCommand::execute()'s resource guard (B1) plus
     *     WordPress's own Core_Upgrader temp-backup mechanism.
     *
     * BLOCKER (issue #131 final-hardening review) — an earlier revision of
     * this method ALSO flagged a plugin incomplete when its main file's
     * source merely referenced the literal string `vendor/autoload.php` and
     * that exact path was missing on disk (S3, original adversarial
     * review). That check was too eager: a plain `str_contains()` over the
     * first 8 KB of the main file cannot tell the near-universal DEFENSIVE
     * idiom — `if ( file_exists( __DIR__ . '/vendor/autoload.php' ) ) {
     * require ...; }`, or even a bare comment mentioning the path — apart
     * from an unconditional `require`. A legitimate plugin that references
     * the path but genuinely ships WITHOUT a `vendor/` directory (a
     * dev-only Composer dependency, a "lite" build, or a
     * graceful-degradation pattern) would fail this check after a
     * perfectly GOOD apply, triggering an automatic revert that reports the
     * update as `failed` — and STICKILY so: every subsequent run would
     * re-apply and immediately re-revert the same update, permanently
     * un-updatable via the agent. Deleted entirely rather than trying to
     * special-case the idiom (parsing PHP conditionals reliably is exactly
     * the kind of heuristic this method's doc already warns against). The
     * half-write symptom this was meant to catch — a good main file with a
     * missing `vendor/` tree — is still caught by the control plane's
     * post-update health probe (issue #132's wp_fatal detection), which was
     * always the backstop for anything this method's necessarily-narrow
     * checks miss.
     *
     * Net effect: isComplete() == validate_plugin() for plugin, the
     * style.css `Name` re-read for theme, always true for core.
     *
     * Fails OPEN (returns true) whenever the detection API itself is
     * unavailable, mirroring isInstalled()'s stance — a constrained runtime
     * must never cause a false rollback of a genuinely good update; a real
     * problem still surfaces through the normal apply()/currentVersion() path.
     *
     * Agent-only regression fix (0.61.18) — a GOOD apply was being
     * auto-reverted on an open_basedir/RunCloud-style host: the update files
     * DID apply (apply() returned ok:true, no mid-copy crash), but
     * get_plugins()'s / validate_plugin()'s internal file_exists()/
     * is_readable() calls — and wp_get_theme()'s style.css re-read — went
     * through a PHP stat/realpath cache entry populated BEFORE this
     * request's non-atomic directory copy, and reported the just-swapped
     * main file/style.css as absent even though wp_clean_plugins_cache(true)/
     * wp_clean_themes_cache() below had already busted WordPress's OWN
     * metadata cache — that call never touches the separate OS-level stat
     * cache. isPluginComplete()/isThemeComplete() now call
     * clearstatcache(true) BEFORE that re-read (see each method's own doc).
     * This can only ever turn a false "missing" into an accurate "present";
     * a genuinely missing/truncated file still reads missing afterwards, so
     * it cannot mask a real half-write, and the removed vendor/autoload.php
     * sub-check above stays removed — no narrowing of half-write detection.
     * Every `false` verdict is now also instrumented via DebugLog with the
     * concrete failure reason plus a raw, cache-bypassing on-disk version
     * read, so a future occurrence is diagnosable from one log line instead
     * of guessed at again.
     *
     * @param string $type            plugin|theme|core.
     * @param string $slug            Sanitized slug (ignored for core).
     * @param string $expectedVersion Version the apply targeted, used only to
     *                                enrich the DebugLog diagnostic on a
     *                                `false` verdict ('' when unknown); never
     *                                affects the return value.
     * @return bool
     */
    public function isComplete(string $type, string $slug, string $expectedVersion = ''): bool
    {
        // Reset FIRST, unconditionally — so a caller that skips reading
        // lastIncompleteReason() after a TRUE verdict (the common case) never
        // leaves a prior item's reason lingering for the next isComplete()
        // call in the same batch/request to accidentally inherit.
        $this->lastIncompleteReason = '';

        return match ($type) {
            'plugin' => $this->isPluginComplete($slug, $expectedVersion),
            'theme'  => $this->isThemeComplete($slug, $expectedVersion),
            default  => true,
        };
    }

    /**
     * The reason the most recent isComplete() call returned false. See the
     * $lastIncompleteReason property doc for the reset/lifetime contract.
     * UpdateCommand reads this ONLY immediately after an isComplete() call
     * that itself returned false, so a caller must never treat a non-empty
     * value here as meaningful on its own without having just observed that.
     *
     * @return string Concise, non-secret reason, or '' when the most recent
     *                 isComplete() call returned true (or was never called).
     */
    public function lastIncompleteReason(): string
    {
        return $this->lastIncompleteReason;
    }

    /**
     * Plugin half of isComplete(). See isComplete()'s doc for the rationale.
     *
     * Agent-only regression fix (0.61.18) — clearstatcache(true) runs FIRST,
     * before wp_clean_plugins_cache()/get_plugins()/validate_plugin() re-read
     * the just-swapped directory. wp_clean_plugins_cache() only busts
     * WordPress's OWN metadata cache; get_plugins()'s and validate_plugin()'s
     * underlying file_exists()/is_readable() calls still go through PHP's
     * separate OS-level stat/realpath cache, which install_package()'s
     * non-atomic copy can leave stale for the exact file it just wrote — the
     * false "incomplete" symptom reported on open_basedir/RunCloud-style
     * hosts. No filename is passed to clearstatcache() here because the
     * exact swapped path isn't known until get_plugins() resolves
     * `$basename` a few lines down (a renamed main file resolves only by
     * FOLDER — see S6 below); a full clear is a single cheap op run once per
     * applied item, not a hot loop.
     *
     * @param string $slug            Sanitized plugin basename or folder.
     * @param string $expectedVersion Version the apply targeted, for the
     *                                DebugLog diagnostic only ('' when
     *                                unknown); never affects the verdict.
     * @return bool
     */
    private function isPluginComplete(string $slug, string $expectedVersion = ''): bool
    {
        $this->loadPluginApi();
        if (!function_exists('get_plugins') || !function_exists('validate_plugin')) {
            return true;
        }

        // Agent-only regression fix (0.61.18) — see this method's class doc.
        clearstatcache(true);

        if (function_exists('wp_clean_plugins_cache')) {
            // The main plugin file may have just been rewritten; force a
            // fresh header read rather than trusting a pre-apply cache.
            wp_clean_plugins_cache(true);
        }

        $all = get_plugins();
        if (!is_array($all)) {
            return true;
        }

        $basename = isset($all[$slug]) ? $slug : '';
        if ($basename === '') {
            // S6 (issue #131 adversarial review) — resolve by FOLDER, not by
            // re-appending '/' to $slug directly. The CP sends the FULL
            // "folder/main-file.php" basename as the slug; when an update
            // legitimately renames the plugin's bootstrap file, get_plugins()
            // no longer has a "folder/oldmain.php" key at all, and the old
            // fallback (`str_starts_with($file, $slug . '/')`) could never
            // match anything either — $slug already ends in ".php", so it
            // was checking for a key literally starting with
            // "folder/oldmain.php/", which no installed plugin basename can
            // ever be. That made a GOOD, differently-named update
            // indistinguishable from a real half-write and triggered a
            // false auto-rollback. Deriving the folder first lets a
            // renamed-main-file plugin still resolve correctly.
            $folder = self::pluginFolder($slug);
            foreach (array_keys($all) as $file) {
                if (is_string($file) && str_starts_with($file, $folder . '/')) {
                    $basename = $file;
                    break;
                }
            }
        }

        if ($basename === '') {
            // get_plugins() no longer resolves this slug to any header at
            // all EVEN AFTER the clearstatcache()+wp_clean_plugins_cache()
            // above — the genuine half-written-main-file symptom, not a
            // stale-cache artifact.
            $diagnostic = $this->pluginOnDiskDiagnostic($slug, $expectedVersion);
            DebugLog::write(
                'WPMgr Agent: isComplete(plugin) => INCOMPLETE for "' . $slug . '"'
                . ' | reason: basename-unresolved (no get_plugins() entry under'
                . ' this slug or its folder, even after clearstatcache()+wp_clean_plugins_cache())'
                . ' | ' . $diagnostic
            );
            // Surfaced to the CP-visible item log (agent-only, 0.61.19) — same
            // facts as the DebugLog line above, condensed for a one-line
            // display; never depends on WPMGR_DEBUG.
            $this->lastIncompleteReason = 'basename-unresolved (no installed plugin found for this slug); ' . $diagnostic;
            return false;
        }

        if (function_exists('opcache_invalidate')) {
            // Guarded: not every host runs OPcache. Harmless for the header
            // re-read above (get_plugins()/validate_plugin() parse the file
            // directly; they never `include`/`require` it) and correct for
            // the reactivation include_once() a successful apply performs
            // afterward — an in-place-overwritten main file could otherwise
            // still execute stale cached bytecode there.
            $mainFilePath = $this->pluginMainFilePath($basename);
            if ($mainFilePath !== '') {
                @opcache_invalidate($mainFilePath, true); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- best-effort opcode-cache bust; @-guarded so an open_basedir host can never warn/fatal here
            }
        }

        $result = validate_plugin($basename);
        if (is_wp_error($result)) {
            $diagnostic = $this->pluginOnDiskDiagnostic($slug, $expectedVersion);
            DebugLog::write(
                'WPMgr Agent: isComplete(plugin) => INCOMPLETE for "' . $basename . '"'
                . ' | reason: validate_plugin(): ' . $result->get_error_message()
                . ' (after clearstatcache()+wp_clean_plugins_cache())'
                . ' | ' . $diagnostic
            );
            // Surfaced to the CP-visible item log (agent-only, 0.61.19) — same
            // facts as the DebugLog line above, condensed for a one-line
            // display; never depends on WPMGR_DEBUG.
            $this->lastIncompleteReason = 'validate_plugin: ' . $result->get_error_message() . '; ' . $diagnostic;
            return false;
        }

        // BLOCKER (issue #131 final-hardening review) — the S3
        // vendor/autoload.php sub-check that previously lived here has been
        // REMOVED entirely; see this method's class doc (isComplete()) for
        // why. validate_plugin() succeeding is the whole signal.
        return true;
    }

    /**
     * Absolute path to a resolved plugin basename's main file, for the
     * opcode-cache bust in isPluginComplete(). Returns '' when WP_PLUGIN_DIR
     * isn't defined (never the case in a real WordPress load).
     *
     * @param string $basename Resolved "folder/main-file.php" plugin basename.
     * @return string
     */
    private function pluginMainFilePath(string $basename): string
    {
        $pluginDir = defined('WP_PLUGIN_DIR') ? (string) constant('WP_PLUGIN_DIR') : '';
        return $pluginDir === '' ? '' : rtrim($pluginDir, '/\\') . '/' . $basename;
    }

    /**
     * Diagnostic-only, best-effort RAW on-disk read of a plugin main file's
     * Version header, entirely independent of get_plugins()/validate_plugin()
     * — and therefore of whatever cache state THEY just saw — so a single
     * DebugLog line recording an isComplete()=false verdict is enough to tell
     * a stale-cache false negative (file present, at the expected version,
     * on disk) apart from a genuine half-write (file missing or short)
     * without guessing. Never throws and never affects the return value
     * above; every filesystem touch is @-suppressed so an
     * open_basedir-restricted probe can never warn/fatal here.
     *
     * @param string $slug            Sanitized plugin basename or folder.
     * @param string $expectedVersion Version the apply targeted ('' when unknown).
     * @return string Diagnostic fragment (never empty).
     */
    private function pluginOnDiskDiagnostic(string $slug, string $expectedVersion): string
    {
        $expected = $expectedVersion !== '' ? $expectedVersion : 'unknown';

        $pluginDir = defined('WP_PLUGIN_DIR') ? (string) constant('WP_PLUGIN_DIR') : '';
        if ($pluginDir === '' || !function_exists('get_plugin_data')) {
            return 'on-disk read unavailable (expected version ' . $expected . ')';
        }

        // Best-effort candidate: the slug as given when it already looks
        // like a "folder/file.php" basename, else guess the conventional
        // "folder/folder.php" main-file name. Diagnostic-only — a wrong
        // guess just yields "not found", itself a true (if less specific)
        // signal; it never flips the actual isComplete() verdict above.
        $candidate = str_contains($slug, '/') ? $slug : ($slug . '/' . $slug . '.php');
        $path      = rtrim($pluginDir, '/\\') . '/' . $candidate;

        clearstatcache(true, $path);

        if (!@is_readable($path)) { // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- best-effort diagnostic probe on a guessed candidate path that may legitimately not exist; must never warn/fatal under open_basedir
            return 'on-disk main file NOT found at "' . $candidate . '" (expected version ' . $expected . ')';
        }

        $data   = @get_plugin_data($path, false, false); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- best-effort raw header parse for a diagnostic log line only; must never warn/fatal under open_basedir
        $onDisk = is_array($data) && isset($data['Version']) && $data['Version'] !== ''
            ? (string) $data['Version']
            : 'unreadable';

        return 'on-disk main file present at "' . $candidate . '", raw Version header: ' . $onDisk
            . ' (expected ' . $expected . ')';
    }

    /**
     * Extract the plugin FOLDER component from a slug that may be a bare
     * folder name ("akismet") or a full "folder/main-file.php" basename.
     * Shared by the folder-prefix fallback in isInstalled(), pluginVersion(),
     * and isPluginComplete() so a plugin whose main file was renamed by an
     * update — the CP-supplied slug's exact basename no longer matching any
     * installed key — still resolves by folder rather than failing to match
     * at all (S6, GitHub issue #131).
     *
     * @param string $slug Sanitized plugin slug/basename.
     * @return string Folder name (never contains a '/').
     */
    private static function pluginFolder(string $slug): string
    {
        $pos = strpos($slug, '/');

        return $pos === false ? $slug : substr($slug, 0, $pos);
    }

    /**
     * Theme half of isComplete(). See isComplete()'s doc for the rationale.
     *
     * Agent-only regression fix (0.61.18) — same stale-stat-cache false
     * negative as isPluginComplete(), same fix: clearstatcache(true) runs
     * BEFORE wp_clean_themes_cache()/wp_get_theme() re-read the just-swapped
     * style.css. See isPluginComplete()'s doc for the full rationale.
     *
     * @param string $slug            Sanitized theme stylesheet.
     * @param string $expectedVersion Version the apply targeted, for the
     *                                DebugLog diagnostic only ('' when
     *                                unknown); never affects the verdict.
     * @return bool
     */
    private function isThemeComplete(string $slug, string $expectedVersion = ''): bool
    {
        if (!function_exists('wp_get_theme')) {
            return true;
        }

        // Agent-only regression fix (0.61.18) — see this method's class doc.
        clearstatcache(true);

        if (function_exists('wp_clean_themes_cache')) {
            wp_clean_themes_cache();
        }

        $theme = wp_get_theme($slug);
        if (!is_object($theme) || !method_exists($theme, 'get')) {
            return true;
        }

        if (function_exists('opcache_invalidate') && function_exists('get_theme_root')) {
            // Guarded/best-effort, mirroring isPluginComplete(); style.css is
            // never `include`d so this is defensive rather than required.
            $styleCss = rtrim((string) get_theme_root($slug), '/\\') . '/' . $slug . '/style.css';
            @opcache_invalidate($styleCss, true); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- best-effort opcode-cache bust; @-guarded so an open_basedir host can never warn/fatal here
        }

        // A half-written theme (style.css truncated or its header block
        // corrupted mid-copy) reads back with an empty Name — the same
        // signal WordPress itself treats as "this is not a valid theme".
        $name = (string) $theme->get('Name');
        if ($name === '') {
            $diagnostic = $this->themeOnDiskDiagnostic($slug, $expectedVersion);
            DebugLog::write(
                'WPMgr Agent: isComplete(theme) => INCOMPLETE for "' . $slug . '"'
                . ' | reason: empty-style-name (after clearstatcache()+wp_clean_themes_cache())'
                . ' | ' . $diagnostic
            );
            // Surfaced to the CP-visible item log (agent-only, 0.61.19) — same
            // facts as the DebugLog line above, condensed for a one-line
            // display; never depends on WPMGR_DEBUG.
            $this->lastIncompleteReason = 'empty-style-name (style.css header did not re-read); ' . $diagnostic;
            return false;
        }

        return true;
    }

    /**
     * Diagnostic-only, best-effort RAW on-disk read of a theme's style.css
     * headers, independent of wp_get_theme()'s cache. See
     * pluginOnDiskDiagnostic()'s doc for the full rationale — same purpose,
     * theme-side. Never throws and never affects the return value above;
     * every filesystem touch is @-suppressed so an open_basedir-restricted
     * probe can never warn/fatal here.
     *
     * @param string $slug            Sanitized theme stylesheet.
     * @param string $expectedVersion Version the apply targeted ('' when unknown).
     * @return string Diagnostic fragment (never empty).
     */
    private function themeOnDiskDiagnostic(string $slug, string $expectedVersion): string
    {
        $expected = $expectedVersion !== '' ? $expectedVersion : 'unknown';

        if (!function_exists('get_theme_root') || !function_exists('get_file_data')) {
            return 'on-disk read unavailable (expected version ' . $expected . ')';
        }

        $path = rtrim((string) get_theme_root($slug), '/\\') . '/' . $slug . '/style.css';
        clearstatcache(true, $path);

        if (!@is_readable($path)) { // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- best-effort diagnostic probe; must never warn/fatal under open_basedir
            return 'on-disk style.css NOT found at "' . $slug . '/style.css" (expected version ' . $expected . ')';
        }

        $headers = @get_file_data($path, ['Version' => 'Version', 'Name' => 'Theme Name']); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- best-effort raw header parse for a diagnostic log line only; must never warn/fatal under open_basedir
        $onDiskVersion   = is_array($headers) && isset($headers['Version']) && $headers['Version'] !== ''
            ? (string) $headers['Version']
            : 'unreadable';
        $onDiskNameEmpty = !is_array($headers) || !isset($headers['Name']) || $headers['Name'] === '';

        return 'on-disk style.css present, raw Version header: ' . $onDiskVersion
            . ', raw Name header ' . ($onDiskNameEmpty ? 'EMPTY' : 'present')
            . ' (expected version ' . $expected . ')';
    }

    /**
     * Force a core downgrade/upgrade to an explicit version (rollback).
     *
     * Prefers WP-CLI `core update --version=<v> --force`; falls back to
     * Core_Upgrader with a forced offer for the requested version.
     *
     * @param string $version Explicit target version (x.y.z).
     * @return array{ok:bool,log:string}
     */
    public function forceCore(string $version): array
    {
        // forceCore always targets an explicit version; "latest" is not a valid
        // rollback target and the literal must never reach the offer URL / argv.
        if ($version === 'latest' || !self::isValidVersion($version)) {
            return ['ok' => false, 'log' => 'Rejected unsafe version string.'];
        }

        if ($this->wpCliAvailable()) {
            return $this->runWpCli([
                'core',
                'update',
                '--version=' . $version,
                '--force',
                '--skip-plugins',
                '--skip-themes',
            ]);
        }

        return $this->forceCoreViaUpgrader($version);
    }

    /**
     * Force a core version via Core_Upgrader using a synthetic offer URL.
     *
     * @param string $version Explicit target version.
     * @return array{ok:bool,log:string}
     */
    protected function forceCoreViaUpgrader(string $version): array
    {
        $this->loadUpgraderApi();

        if (!class_exists('\Core_Upgrader') || !class_exists('\WP_Ajax_Upgrader_Skin')) {
            return ['ok' => false, 'log' => 'Core upgrader API unavailable.'];
        }

        $locale = function_exists('get_locale') ? (string) get_locale() : 'en_US';

        $offer                  = new \stdClass();
        $offer->response        = 'upgrade';
        $offer->current         = $version;
        $offer->version         = $version;
        $offer->download        = 'https://downloads.wordpress.org/release/wordpress-' . $version . '.zip';
        $offer->locale          = $locale;
        $offer->packages        = (object) [
            'full'        => $offer->download,
            'no_content'  => false,
            'new_bundled' => false,
            'partial'     => false,
            'rollback'    => false,
        ];
        $offer->php_version     = '5.6.20';
        $offer->mysql_version   = '5.0';
        $offer->new_bundled     = '';
        $offer->partial_version = '';

        $upgrader = new \Core_Upgrader(new \WP_Ajax_Upgrader_Skin());
        try {
            $result = $upgrader->upgrade($offer);
        } finally {
            // GUARANTEE: whatever core's update_core()/maintenance_mode() did
            // or failed to do internally, this run's maintenance flag is
            // cleared on every terminal path — success, WP_Error, or a thrown
            // exception. See Maintenance class doc for why core alone cannot
            // be trusted to always reach its own cleanup line.
            Maintenance::clear($upgrader);
        }

        return $this->upgraderOutcome($result);
    }

    // ---------------------------------------------------------------------
    // WP-CLI path
    // ---------------------------------------------------------------------

    /**
     * Is a WP-CLI execution context available?
     *
     * @return bool
     */
    public function wpCliAvailable(): bool
    {
        return defined('WP_CLI') && WP_CLI;
    }

    /**
     * Apply an update through the WP-CLI runner.
     *
     * @param string $type    plugin|theme|core.
     * @param string $slug    Sanitized slug.
     * @param string $version 'latest' or x.y.z.
     * @return array{ok:bool,log:string}
     */
    private function applyViaWpCli(string $type, string $slug, string $version): array
    {
        $args = match ($type) {
            'plugin' => ['plugin', 'update', $slug, '--skip-themes'],
            'theme'  => ['theme', 'update', $slug, '--skip-plugins'],
            'core'   => array_merge(
                ['core', 'update', '--skip-plugins', '--skip-themes'],
                $version !== 'latest' ? ['--version=' . $version] : []
            ),
            default  => [],
        };

        if ($args === []) {
            return ['ok' => false, 'log' => 'Unsupported type for WP-CLI update.'];
        }

        if ($type !== 'core' && $version !== 'latest') {
            $args[] = '--version=' . $version;
        }

        return $this->runWpCli($args);
    }

    /**
     * Invoke WP-CLI's runcommand and capture its output. Isolated so tests can
     * stub it.
     *
     * @param array<int,string> $args Command argument vector.
     * @return array{ok:bool,log:string}
     */
    protected function runWpCli(array $args): array
    {
        if (!class_exists('\WP_CLI')) {
            return ['ok' => false, 'log' => 'WP-CLI not loadable.'];
        }

        $command = implode(' ', $args);

        try {
            /** @var array{stdout?:string,stderr?:string,return_code?:int} $res */
            $res = \WP_CLI::runcommand(
                $command,
                ['return' => 'all', 'exit_error' => false, 'launch' => false]
            );

            $code   = isset($res['return_code']) ? (int) $res['return_code'] : 0;
            $stdout = isset($res['stdout']) ? (string) $res['stdout'] : '';
            $stderr = isset($res['stderr']) ? (string) $res['stderr'] : '';

            return [
                'ok'  => $code === 0,
                'log' => trim($stdout . "\n" . $stderr),
            ];
        } catch (\Throwable $e) {
            return ['ok' => false, 'log' => 'WP-CLI execution error.'];
        }
    }

    // ---------------------------------------------------------------------
    // Upgrader (PHP fallback) path
    // ---------------------------------------------------------------------

    /**
     * Apply an update through the WordPress upgrader APIs under a quiet skin.
     *
     * @param string $type    plugin|theme|core.
     * @param string $slug    Sanitized slug.
     * @param string $version 'latest' or x.y.z.
     * @return array{ok:bool,log:string}
     */
    protected function applyViaUpgrader(string $type, string $slug, string $version): array
    {
        $this->loadUpgraderApi();

        // B3 (GitHub issue #131) — harden the unpack directory. WordPress's
        // own temp-directory resolution (get_temp_dir(), used by
        // WP_Filesystem_*::unzip_file()/wp_tempnam() when WP_TEMP_DIR is
        // undefined) falls through to sys_get_temp_dir() / upload_tmp_dir on
        // a locked-down host, which can sit OUTSIDE open_basedir and fail
        // silently there. wp-content/upgrade is always inside the site's own
        // writable tree, so pin WP_TEMP_DIR to it explicitly rather than
        // trusting that fallback.
        $upgradeDir = defined('WP_CONTENT_DIR') ? rtrim((string) WP_CONTENT_DIR, '/\\') . '/upgrade' : '';
        if ($upgradeDir !== '' && !is_dir($upgradeDir) && function_exists('wp_mkdir_p')) {
            wp_mkdir_p($upgradeDir);
        }
        if ($upgradeDir !== '' && is_dir($upgradeDir) && !defined('WP_TEMP_DIR')) {
            // phpcs:ignore WordPress.NamingConventions.PrefixAllGlobals.NonPrefixedConstantFound -- WP_TEMP_DIR is a documented, overridable WordPress core constant (wp-admin/includes/file.php get_temp_dir()), not a constant of our own; it is simply missing from this sniff's hardcoded allowed_core_constants list
            define('WP_TEMP_DIR', $upgradeDir);
        }

        if (function_exists('wp_update_plugins') && $type === 'plugin') {
            wp_update_plugins();
        }
        if (function_exists('wp_update_themes') && $type === 'theme') {
            wp_update_themes();
        }

        // Force a clean working directory for THIS upgrade only. A prior
        // interrupted apply can leave a stale wp-content/upgrade/<slug>.<ver>/
        // extraction directory behind (the reported orphaned
        // upgrade/latepoint.5.6.4/); Plugin_Upgrader::install_package()
        // otherwise reuses whatever it finds there instead of a fresh unpack.
        // Added immediately before, and removed immediately after, this
        // single upgrade() call so it never leaks into an unrelated
        // concurrent upgrade running in a different request.
        $forceCleanWorking = static function (array $options): array {
            $options['clear_working'] = true;
            return $options;
        };
        add_filter('upgrader_package_options', $forceCleanWorking);

        try {
            switch ($type) {
                case 'plugin':
                    if (!class_exists('\Plugin_Upgrader') || !class_exists('\WP_Ajax_Upgrader_Skin')) {
                        return ['ok' => false, 'log' => 'Upgrader API unavailable.'];
                    }

                    // Capture active state BEFORE upgrade. WordPress's
                    // Plugin_Upgrader::upgrade() registers an upgrader_pre_install
                    // hook that calls deactivate_plugins($plugin, silent=true) and
                    // does NOT re-activate after the upgrade finishes. WP-CLI's
                    // `wp plugin update` preserves active state; we mirror that
                    // behaviour here so the PHP-fallback path doesn't strand an
                    // active plugin inactive after a successful upgrade.
                    $pluginsFilePath  = WPMGR_AGENT_DIR; // unused — silence linters about the var
                    $wasActive        = function_exists('is_plugin_active')             ? \is_plugin_active($slug) : false;
                    $wasNetworkActive = function_exists('is_plugin_active_for_network') ? \is_plugin_active_for_network($slug) : false;

                    $upgrader = new \Plugin_Upgrader(new \WP_Ajax_Upgrader_Skin());
                    try {
                        $result = $upgrader->upgrade($slug);
                    } finally {
                        // GUARANTEE: clear maintenance mode on every terminal
                        // path of THIS upgrade() call, independent of whether it
                        // succeeded, returned a WP_Error, or threw.
                        Maintenance::clear($upgrader);
                    }
                    $outcome  = $this->upgraderOutcome($result);

                    if ($outcome['ok'] && ($wasActive || $wasNetworkActive) && function_exists('activate_plugin')) {
                        // Refresh plugin caches before reactivating: the upgrade
                        // may have changed the main plugin file (slug stays the
                        // same but the metadata cache is stale).
                        if (function_exists('wp_clean_plugins_cache')) {
                            \wp_clean_plugins_cache(true);
                        }
                        $activated = \activate_plugin($slug, '', $wasNetworkActive, true);
                        if (\is_wp_error($activated)) {
                            $outcome['log'] .= "\n[wpmgr] upgrade succeeded but reactivation failed: "
                                . $activated->get_error_message();
                            \WPMgr\Agent\Support\DebugLog::write('WPMgr Agent: post-upgrade reactivation failed for '
                                . $slug . ': ' . $activated->get_error_message());
                        }
                    }

                    return $outcome;

                case 'theme':
                    if (!class_exists('\Theme_Upgrader') || !class_exists('\WP_Ajax_Upgrader_Skin')) {
                        return ['ok' => false, 'log' => 'Upgrader API unavailable.'];
                    }
                    $upgrader = new \Theme_Upgrader(new \WP_Ajax_Upgrader_Skin());
                    try {
                        $result = $upgrader->upgrade($slug);
                    } finally {
                        Maintenance::clear($upgrader);
                    }

                    return $this->upgraderOutcome($result);

                case 'core':
                    return $this->applyCoreUpgrade($version);
            }

            return ['ok' => false, 'log' => 'Unsupported type.'];
        } finally {
            remove_filter('upgrader_package_options', $forceCleanWorking);
        }
    }

    /**
     * Run a core upgrade via Core_Upgrader to the requested (or latest) version.
     *
     * @param string $version 'latest' or x.y.z.
     * @return array{ok:bool,log:string}
     */
    private function applyCoreUpgrade(string $version): array
    {
        if (function_exists('wp_version_check')) {
            wp_version_check([], true);
        }

        if (!class_exists('\Core_Upgrader') || !function_exists('get_core_updates')) {
            return ['ok' => false, 'log' => 'Core upgrader API unavailable.'];
        }

        $updates = get_core_updates();
        if (!is_array($updates) || $updates === []) {
            return ['ok' => false, 'log' => 'No core update offer available.'];
        }

        $offer = null;
        foreach ($updates as $candidate) {
            if (!is_object($candidate)) {
                continue;
            }
            $candidateVersion = isset($candidate->version) ? (string) $candidate->version : '';
            if ($version === 'latest' || $candidateVersion === $version) {
                $offer = $candidate;
                break;
            }
        }

        if ($offer === null) {
            return ['ok' => false, 'log' => 'Requested core version not offered.'];
        }

        if (!class_exists('\WP_Ajax_Upgrader_Skin')) {
            return ['ok' => false, 'log' => 'Upgrader skin unavailable.'];
        }

        $upgrader = new \Core_Upgrader(new \WP_Ajax_Upgrader_Skin());
        try {
            $result = $upgrader->upgrade($offer);
        } finally {
            Maintenance::clear($upgrader);
        }

        return $this->upgraderOutcome($result);
    }

    /**
     * Normalize an upgrader return value into the ok/log shape.
     *
     * @param mixed $result Upgrader result (bool|array|WP_Error|null).
     * @return array{ok:bool,log:string}
     */
    private function upgraderOutcome($result): array
    {
        if (is_object($result) && method_exists($result, 'get_error_message')) {
            /** @var \WP_Error $result */
            return ['ok' => false, 'log' => (string) $result->get_error_message()];
        }

        if ($result === false || $result === null) {
            return ['ok' => false, 'log' => 'Update failed.'];
        }

        return ['ok' => true, 'log' => 'Update applied via upgrader.'];
    }

    /**
     * Ensure the wp-admin upgrade/plugin/theme/file APIs are loaded.
     *
     * @return void
     */
    private function loadUpgraderApi(): void
    {
        if (!defined('ABSPATH')) {
            return;
        }
        $base = ABSPATH . 'wp-admin/includes/';
        foreach (['plugin.php', 'theme.php', 'file.php', 'misc.php', 'class-wp-upgrader.php', 'update.php'] as $file) {
            if (file_exists($base . $file)) {
                require_once $base . $file;
            }
        }
    }

    // ---------------------------------------------------------------------
    // Version helpers
    // ---------------------------------------------------------------------

    /**
     * Installed version of a plugin by its basename (folder/file.php) or folder.
     *
     * @param string $slug Plugin basename or folder.
     * @return string
     */
    private function pluginVersion(string $slug): string
    {
        $this->loadPluginApi();
        if (!function_exists('get_plugins')) {
            return '';
        }

        $all = get_plugins();
        if (!is_array($all)) {
            return '';
        }

        if (isset($all[$slug]) && is_array($all[$slug]) && isset($all[$slug]['Version'])) {
            return (string) $all[$slug]['Version'];
        }

        // Allow a folder-only slug — or a full "folder/main-file.php"
        // basename whose main file was since renamed (S6, issue #131) — to
        // match by FOLDER. See pluginFolder()'s doc.
        $folder = self::pluginFolder($slug);
        foreach ($all as $file => $meta) {
            if (!is_string($file) || !is_array($meta)) {
                continue;
            }
            if (str_starts_with($file, $folder . '/') && isset($meta['Version'])) {
                return (string) $meta['Version'];
            }
        }

        return '';
    }

    /**
     * Installed version of a theme by stylesheet.
     *
     * @param string $slug Theme stylesheet.
     * @return string
     */
    private function themeVersion(string $slug): string
    {
        if (!function_exists('wp_get_themes')) {
            return '';
        }

        $themes = wp_get_themes();
        if (!is_array($themes) || !isset($themes[$slug])) {
            return '';
        }

        $theme = $themes[$slug];
        if (is_object($theme) && method_exists($theme, 'get')) {
            return (string) $theme->get('Version');
        }

        return '';
    }

    /**
     * Pending update version for a plugin from the update transient.
     *
     * @param string $slug Plugin basename.
     * @return string
     */
    private function pluginUpdateVersion(string $slug): string
    {
        if (!function_exists('get_site_transient')) {
            return '';
        }
        $transient = get_site_transient('update_plugins');
        if (!is_object($transient) || !isset($transient->response) || !is_array($transient->response)) {
            return '';
        }
        $entry = $transient->response[$slug] ?? null;
        if (is_object($entry) && isset($entry->new_version)) {
            return (string) $entry->new_version;
        }

        return '';
    }

    /**
     * Pending update version for a theme from the update transient.
     *
     * @param string $slug Theme stylesheet.
     * @return string
     */
    private function themeUpdateVersion(string $slug): string
    {
        if (!function_exists('get_site_transient')) {
            return '';
        }
        $transient = get_site_transient('update_themes');
        if (!is_object($transient) || !isset($transient->response) || !is_array($transient->response)) {
            return '';
        }
        $entry = $transient->response[$slug] ?? null;
        if (is_array($entry) && isset($entry['new_version'])) {
            return (string) $entry['new_version'];
        }

        return '';
    }

    /**
     * Latest offered core version from the update transient.
     *
     * @return string
     */
    private function coreUpdateVersion(): string
    {
        if (!function_exists('get_site_transient')) {
            return '';
        }
        $transient = get_site_transient('update_core');
        if (!is_object($transient) || !isset($transient->updates) || !is_array($transient->updates)) {
            return '';
        }
        foreach ($transient->updates as $update) {
            if (is_object($update) && isset($update->response) && $update->response === 'upgrade' && isset($update->version)) {
                return (string) $update->version;
            }
        }

        return '';
    }

    /**
     * Load the wp-admin plugin helper so get_plugins() is available.
     *
     * @return void
     */
    private function loadPluginApi(): void
    {
        if (!function_exists('get_plugins') && defined('ABSPATH') && file_exists(ABSPATH . 'wp-admin/includes/plugin.php')) {
            require_once ABSPATH . 'wp-admin/includes/plugin.php';
        }
    }
}
