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
     * Above this many entries, WordPress's own default temp directory
     * (get_temp_dir()) is treated as pathologically populated rather than
     * healthy, even when it is genuinely writable — see
     * pinTempDirForUnpack()'s doc (GitHub issue #216). Well above sane
     * transient temp-file use, well below the 10,000+ stale `sess_*`
     * entries observed on the reporting host's GC-disabled session-save-path
     * default.
     */
    private const MAX_HEALTHY_DEFAULT_TEMP_ENTRIES = 2000;

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
     * Which component types (plugin|theme|core) have already had their
     * forced fresh update-check run during THIS UpdateRunner instance's
     * lifetime (== this run — one CP `update` command constructs exactly one
     * UpdateRunner; see UpdateCommand's class doc). Keyed by type, value
     * always true when present. See forceFreshCheckOncePerRun()'s doc
     * (GitHub issue #218).
     *
     * @var array<string,bool>
     */
    private array $freshCheckedTypes = [];

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
     * Used by dry-run AND as the pre-apply "is anything even pending" check
     * in UpdateCommand::processItem(). For an explicit version request we
     * return it as-is (no WordPress check involved — the caller already
     * knows the target). For 'latest' we consult the WordPress update
     * transients, but ONLY after forcing a fresh check first (GitHub issue
     * #208): see pluginUpdateVersion()/themeUpdateVersion()/
     * coreUpdateVersion() for why. Before that fix, a momentarily
     * stale/expired/never-yet-populated transient was read as-is and
     * indistinguishable from "genuinely no update available", so a real
     * pending update could be silently reported as up_to_date here while
     * WordPress's own background auto-updater applied it moments later.
     * That forced check is now memoized once per run per component-type
     * (GitHub issue #218) via forceFreshCheckOncePerRun() rather than once
     * per call — see its doc for the bulk-run regression that fixed.
     *
     * @param string $type      plugin|theme|core.
     * @param string $slug      Sanitized slug.
     * @param string $requested 'latest' or an explicit x.y.z.
     * @return string|null Target version, '' when a forced fresh check
     *                confirms none is available (genuinely up to date), or
     *                null when availability could not be determined AT ALL
     *                even after that forced check (the update transient is
     *                still missing/malformed, or the relevant WordPress
     *                update-check function itself is unavailable in this
     *                runtime). The caller (UpdateCommand) MUST treat null
     *                distinctly from '' — never fold "couldn't tell" into
     *                "nothing to do" (see UpdateCommand::isUpdatable()'s
     *                doc and its two call sites).
     */
    public function availableVersion(string $type, string $slug, string $requested): ?string
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

        return $this->upgraderOutcome($result, $upgrader);
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

        // B3 (GitHub issue #131), superseded (agent-only regression fix,
        // 0.61.21 — GitHub issue #131 second follow-up): see
        // pinTempDirForUnpack()'s doc for why the pin is now a FALLBACK only,
        // applied when WordPress's own default temp dir is not usable, rather
        // than something attempted unconditionally.
        $tempDirNote = $this->pinTempDirForUnpack();
        DebugLog::write('WPMgr Agent: applyViaUpgrader() ' . $tempDirNote);

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
            // Set by whichever case below actually ran an upgrade; used after
            // the switch to prefix the CP-visible log with $tempDirNote
            // (agent-only, 0.61.20) and as the switch's fallthrough guard —
            // stays null only when $type matched none of the cases.
            $outcome = null;

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
                    $outcome  = $this->upgraderOutcome($result, $upgrader);

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

                    break;

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

                    $outcome = $this->upgraderOutcome($result, $upgrader);
                    break;

                case 'core':
                    $outcome = $this->applyCoreUpgrade($version);
                    break;
            }

            if ($outcome === null) {
                return ['ok' => false, 'log' => 'Unsupported type.'];
            }

            // Agent-only, 0.61.20 — surface the temp-dir decision in the
            // SAME log the CP shows for this item, not just DebugLog (which
            // is silent unless WPMGR_DEBUG/WP_DEBUG_LOG is on). Prefixed so
            // it reads first, ahead of whatever the upgrade itself logged.
            if ($tempDirNote !== '') {
                $outcome['log'] = '[wpmgr] ' . $tempDirNote
                    . ($outcome['log'] !== '' ? "\n" . $outcome['log'] : '');
            }

            return $outcome;
        } finally {
            remove_filter('upgrader_package_options', $forceCleanWorking);
        }
    }

    /**
     * Decide whether WP_TEMP_DIR needs to be pinned for THIS apply, and if
     * so, to WHERE — preferring to leave it undefined entirely (WordPress's
     * own default) whenever that default already works in this request's
     * execution context.
     *
     * HISTORY — B3 (GitHub issue #131) pinned WP_TEMP_DIR to
     * wp-content/upgrade unconditionally (guarded only by is_dir()) so that
     * WordPress's own temp-directory resolution (get_temp_dir(), used by
     * download_url()/wp_tempnam() when downloading the update package) could
     * never fall through to sys_get_temp_dir()/upload_tmp_dir — a location
     * that can sit OUTSIDE open_basedir on a locked-down host (RunCloud-style)
     * and fail there. 0.61.20 added a writability check (is_dir() +
     * is_writable()) on top of that, since wp-content/upgrade merely
     * EXISTING was not enough evidence it was writable in this request's
     * context — but the pin itself was still attempted FIRST, unconditionally,
     * on every host where that check passed.
     *
     * REGRESSION (agent-only, 0.61.21 — GitHub issue #131 second follow-up):
     * pinning WP_TEMP_DIR to wp-content/upgrade breaks updates on a STANDARD
     * host where wp-content/upgrade IS writable, because that directory is
     * not just a convenient writable location — it is the EXACT working
     * directory WP_Upgrader::unpack_package() itself uses
     * (wp-admin/includes/class-wp-upgrader.php), and that method
     * unconditionally DELETES every existing entry directly under
     * wp-content/upgrade/ as its very first step, before unzipping:
     *
     *     $upgrade_folder = $wp_filesystem->wp_content_dir() . 'upgrade/';
     *     $upgrade_files = $wp_filesystem->dirlist( $upgrade_folder );
     *     foreach ( $upgrade_files as $file ) {
     *         $wp_filesystem->delete( $upgrade_folder . $file['name'], true );
     *     }
     *     ...
     *     $result = unzip_file( $package, $working_dir );
     *
     * With WP_TEMP_DIR pinned to wp-content/upgrade, the package
     * download_package()/wp_tempnam() wrote a few lines earlier in the SAME
     * upgrade() call is itself a file sitting directly inside
     * wp-content/upgrade/ — so this cleanup step deletes the just-downloaded
     * package (recursively, for ANY path underneath wp-content/upgrade/ too —
     * a dedicated subdirectory nested under it does not avoid the collision)
     * before unzip_file() ever runs. unzip_file() then fails against a source
     * file that no longer exists, upgrade() returns false/a WP_Error with no
     * directory copy ever having started, and the #131 isComplete() guard
     * correctly (but unhelpfully) rolls the never-applied "apply" back —
     * exactly the reported bare "Update failed." with no downstream Reason,
     * on a host where the writability check itself passed.
     *
     * THE FIX: never pin unconditionally. First check whether WordPress's own
     * DEFAULT temp dir (what get_temp_dir() resolves to while WP_TEMP_DIR is
     * undefined — normally sys_get_temp_dir(), falling back through
     * upload_tmp_dir/WP_CONTENT_DIR per WP core) is ACTUALLY usable in this
     * execution context, proven with a real create+write+delete of a small
     * file rather than trusting is_dir()/is_writable() alone. If it is, do
     * NOT define WP_TEMP_DIR at all: WordPress's own default temp file
     * location is completely disjoint from wp-content/upgrade, so no
     * collision with unpack_package()'s cleanup is possible — this is exactly
     * the pre-#131 behaviour that worked on this class of (standard) host.
     * Only when the default is NOT usable (the #131 RunCloud/open_basedir
     * case) do we pin — and even then, to a dedicated directory that is NOT
     * wp-content/upgrade or any path underneath it: see
     * fallbackTempDirCandidate()'s doc. This preserves the #131 intent (a
     * writable, open_basedir-safe temp location) while eliminating the
     * collision that caused this regression.
     *
     * BROADENED (GitHub issue #216): "usable" no longer means merely
     * writable. A directory can pass the writability probe above and STILL
     * be a bad place to unpack an update — a reported host's get_temp_dir()
     * resolved to its PHP session save path with garbage collection
     * effectively disabled, which had accumulated 10,000+ stale `sess_*`
     * files; heavy per-file I/O against a directory that populated failed
     * intermittently with "Could not copy file." tempDirEntryCountExceeds()
     * cheaply (streaming, bounded) tests whether the default holds more than
     * self::MAX_HEALTHY_DEFAULT_TEMP_ENTRIES entries; when it does, the
     * default is treated exactly like an unwritable one and this method
     * falls through to the SAME conditional last-resort fallback+create+
     * write-probe pin path below — this constant's doc, not a new pin
     * location, is the change. The pin location itself is unchanged and
     * remains a CONDITIONAL last-resort (never unconditional): this is
     * exactly the posture defended to the wp.org reviewer for this
     * `define(WP_TEMP_DIR)` call.
     *
     * Never redefines an already-defined WP_TEMP_DIR (an operator's
     * wp-config.php override, or an earlier call in this same request) —
     * that would be a fatal error, and it is also never our place to
     * second-guess an explicit choice made elsewhere.
     *
     * All filesystem probes (resolveDefaultTempDir(), isDirWritableByProbe(),
     * tempDirEntryCountExceeds(), fallbackTempDirCandidate()) are
     * @-suppressed: on an open_basedir-restricted host, touching a
     * disallowed path can emit a PHP warning (never a fatal), and none of
     * this decision logic may let that noise surface — "can't tell" is
     * treated the same as "not usable" by construction.
     *
     * @return string A short, log-safe (no secrets) description of the
     *                decision, suitable for both DebugLog and the
     *                CP-visible item log.
     */
    private function pinTempDirForUnpack(): string
    {
        if (defined('WP_TEMP_DIR')) {
            // Respect whatever already defined it (an operator's
            // wp-config.php override, or an earlier call in this same
            // request) — never redefine a PHP constant (fatal) and never
            // second-guess an explicit choice made elsewhere.
            return 'temp dir: WP_TEMP_DIR already defined; leaving as-is.';
        }

        $default = $this->resolveDefaultTempDir();

        // "Usable" (GitHub issue #216) requires BOTH a real writability
        // proof AND that the default isn't pathologically populated — see
        // this method's class doc. $defaultUnusableReason records WHICH
        // condition failed, for the log strings below, replacing the
        // previously-hardcoded 'not writable' wording.
        $defaultUsable         = false;
        $defaultUnusableReason = 'not writable';
        if ($default !== '' && $this->isDirWritableByProbe($default)) {
            if ($this->tempDirEntryCountExceeds($default, self::MAX_HEALTHY_DEFAULT_TEMP_ENTRIES)) {
                $defaultUnusableReason = 'shared temp dir has ' . self::MAX_HEALTHY_DEFAULT_TEMP_ENTRIES . '+ entries';
            } else {
                $defaultUsable = true;
            }
        }

        if ($defaultUsable) {
            // The fix: WordPress's own default already works here — never
            // define WP_TEMP_DIR, so it stays completely disjoint from
            // wp-content/upgrade and the collision described in this
            // method's doc can never occur. This is the pre-#131 behaviour.
            return 'temp dir: WordPress\'s own default (' . $default . ') is writable here; leaving WP_TEMP_DIR unset.';
        }

        [$fallbackDir, $fallbackLabel] = $this->fallbackTempDirCandidate();
        if ($fallbackDir === '') {
            return 'temp dir: WordPress\'s own default (' . ($default !== '' ? $default : 'unresolved')
                . ') is unusable here (' . $defaultUnusableReason
                . ') and no fallback directory is available; leaving WP_TEMP_DIR unset.';
        }

        if (!@is_dir($fallbackDir) && function_exists('wp_mkdir_p')) { // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- best-effort existence probe; must never warn/fatal under open_basedir, see method doc
            @wp_mkdir_p($fallbackDir); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- best-effort directory creation; a failure here is caught by the write-probe right below, not by this return value
        }

        if (!$this->isDirWritableByProbe($fallbackDir)) {
            return 'temp dir: WordPress\'s own default (' . ($default !== '' ? $default : 'unresolved')
                . ') is unusable here (' . $defaultUnusableReason . '), and the fallback directory ' . $fallbackDir
                . ' could not be created/written either; leaving WP_TEMP_DIR unset.';
        }

        // phpcs:ignore WordPress.NamingConventions.PrefixAllGlobals.NonPrefixedConstantFound -- WP_TEMP_DIR is a documented, overridable WordPress core constant (wp-admin/includes/file.php get_temp_dir()), not a constant of our own; it is simply missing from this sniff's hardcoded allowed_core_constants list
        define('WP_TEMP_DIR', $fallbackDir);

        return 'temp dir: WordPress\'s own default was unusable here (' . $defaultUnusableReason
            . '); pinned to a dedicated fallback dir ' . $fallbackDir . ' (' . $fallbackLabel . ').';
    }

    /**
     * Bounded, STREAMING check for whether $dir contains more than $limit
     * entries (GitHub issue #216) — used to decide whether WordPress's own
     * default temp dir is pathologically populated (see
     * pinTempDirForUnpack()'s doc) rather than merely writable.
     *
     * Deliberately never builds a full listing (no scandir()/glob() over a
     * directory that may hold tens of thousands of entries): opendir()/
     * readdir() are used directly so this can return true as soon as the
     * count exceeds $limit, costing at most $limit+1 readdir() calls even
     * against a pathologically large directory.
     *
     * Fails CLOSED to "not exceeded" (returns false) whenever the directory
     * cannot be opened at all — an open_basedir restriction, or a race where
     * it was removed between the writability probe and this call — so this
     * check can only ever ADD a fallback pin on top of a genuinely
     * enumerable, pathologically populated directory; it can never itself
     * cause a pin to be skipped by guessing. Every filesystem touch is
     * @-suppressed, mirroring isDirWritableByProbe()'s posture: "can't tell"
     * must never surface as a warning under open_basedir.
     *
     * @param string $dir   Absolute directory path to inspect.
     * @param int    $limit Entry-count threshold; exceeded means strictly
     *                      more than this many non-dot entries were found.
     * @return bool True when $dir's entry count (excluding `.`/`..`) is
     *              strictly greater than $limit. False when at or below
     *              $limit, or when $dir could not be opened for reading.
     */
    private function tempDirEntryCountExceeds(string $dir, int $limit): bool
    {
        $handle = @opendir($dir); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- best-effort bounded probe; must never warn/fatal under open_basedir or a directory removed mid-race, see method doc
        if ($handle === false) {
            return false;
        }

        try {
            $count = 0;
            while (($entry = readdir($handle)) !== false) {
                if ($entry === '.' || $entry === '..') {
                    continue;
                }
                ++$count;
                if ($count > $limit) {
                    return true;
                }
            }
        } finally {
            @closedir($handle); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- best-effort cleanup; must never warn/fatal under open_basedir
        }

        return false;
    }

    /**
     * Resolve the directory WordPress's OWN get_temp_dir() would choose right
     * now, given the caller has already confirmed WP_TEMP_DIR is undefined.
     * Falls back to sys_get_temp_dir() directly on the — never expected in a
     * real WordPress load, where get_temp_dir() is always available — chance
     * get_temp_dir() itself is not.
     *
     * @return string Absolute path with no trailing slash, or '' when
     *                undeterminable.
     */
    private function resolveDefaultTempDir(): string
    {
        if (function_exists('get_temp_dir')) {
            $dir = (string) get_temp_dir();
            if ($dir !== '') {
                return rtrim($dir, '/\\');
            }
        }

        return rtrim(sys_get_temp_dir(), '/\\');
    }

    /**
     * Prove a directory is genuinely writable in THIS execution context by
     * actually creating, writing to, and deleting a small probe file inside
     * it — rather than trusting is_dir()/is_writable() alone. is_writable()
     * can disagree with what an actual write syscall does, and
     * get_temp_dir()'s own hard-coded last-resort return is never checked for
     * writability at all by WordPress itself. This is the same headless,
     * direct-I/O posture the rest of this class uses: WP_Filesystem is never
     * initialized in this runtime (no interactive/FTP context to prompt for
     * credentials in), so a real, contained file write is the only reliable
     * signal available.
     *
     * Every filesystem touch is @-suppressed: on an open_basedir-restricted
     * host, touching a disallowed path can emit a PHP warning (never a
     * fatal), and this probe must never let that warning surface — "can't
     * tell" is treated identically to "not writable" by construction.
     *
     * @param string $dir Absolute directory path to probe.
     * @return bool True only when a real file could be created, written to,
     *              and removed inside $dir.
     */
    private function isDirWritableByProbe(string $dir): bool
    {
        if ($dir === '' || !@is_dir($dir)) { // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- best-effort existence probe; must never warn/fatal under open_basedir, see method doc
            return false;
        }

        $probe = rtrim($dir, '/\\') . '/.wpmgr-tempdir-probe-' . bin2hex(random_bytes(6));

        $written = @file_put_contents($probe, '.'); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- must PROVE writability with a real write rather than trust is_writable(), must never warn/fatal under open_basedir; file_put_contents() itself is allowed by Plugin Check
        if ($written === false) {
            return false;
        }

        if (function_exists('wp_delete_file')) {
            wp_delete_file($probe);
        } else {
            @unlink($probe); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged,WordPress.WP.AlternativeFunctions.unlink_unlink -- headless-agent probe-file cleanup; wp_delete_file() is unavailable only in a constrained runtime that never loaded wp-includes/functions.php, must never warn/fatal under open_basedir
        }

        return true;
    }

    /**
     * The dedicated fallback temp directory to pin WP_TEMP_DIR to when
     * WordPress's own default is not usable — deliberately NOT
     * wp-content/upgrade or any path underneath it (see
     * pinTempDirForUnpack()'s doc for why that collides with
     * WP_Upgrader::unpack_package()'s own cleanup of that exact directory).
     * wp_upload_dir()'s basedir is a WordPress-blessed writable location that
     * sits entirely outside wp-content/upgrade, so WP_Upgrader never touches
     * it — this also keeps the write inside wp_upload_dir(), matching Plugin
     * Check's write-location expectations for this plugin's own writes.
     *
     * @return array{0:string,1:string} Tuple of [absolute directory path (''
     *                when undeterminable), a short human label for the log].
     */
    private function fallbackTempDirCandidate(): array
    {
        if (function_exists('wp_upload_dir')) {
            $uploads = wp_upload_dir();
            if (
                is_array($uploads)
                && empty($uploads['error'])
                && isset($uploads['basedir'])
                && is_string($uploads['basedir'])
                && $uploads['basedir'] !== ''
            ) {
                return [rtrim($uploads['basedir'], '/\\') . '/.wpmgr-tmp', 'uploads-dir fallback'];
            }
        }

        // wp_upload_dir() unavailable/erroring — fall back to a subdirectory
        // of WP_CONTENT_DIR ITSELF (a SIBLING of wp-content/upgrade, never
        // underneath it, so unpack_package()'s cleanup of wp-content/upgrade/
        // still never touches it). Guarded against an empty base: never use
        // an empty WP_CONTENT_DIR, which would resolve to a bare
        // '/.wpmgr-tmp' at the filesystem root.
        if (defined('WP_CONTENT_DIR')) {
            $contentDir = rtrim((string) WP_CONTENT_DIR, '/\\');
            if ($contentDir !== '') {
                return [$contentDir . '/.wpmgr-tmp', 'wp-content fallback (wp_upload_dir() unavailable)'];
            }
        }

        return ['', ''];
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

        return $this->upgraderOutcome($result, $upgrader);
    }

    /**
     * Normalize an upgrader return value into the ok/log shape, enriched
     * (agent-only, 0.61.21) with WHY a false/null result failed.
     *
     * Before this fix, a false or null upgrade() return produced only the
     * bare "Update failed." — accurate but useless for diagnosing the real
     * cause (a download failure, a corrupt/incompatible archive, a
     * directory-creation failure, ...). That bare message is exactly what a
     * real production failure looked like (GitHub issue #131 second
     * follow-up): a false/null result with no downstream isComplete()
     * "Reason:" suffix, meaning the failure happened INSIDE the upgrader
     * itself — before any file was ever copied into place — with nothing in
     * the CP-visible log to say why.
     *
     * Two independent sources are now captured, both best-effort and never
     * fatal if the shape is unexpected:
     *   - $result itself, when it is a WP_Error: its code + message (as
     *     before, now also code-prefixed).
     *   - $upgrader->skin's collected messages
     *     (Automatic_Upgrader_Skin::get_upgrade_messages(), inherited by the
     *     WP_Ajax_Upgrader_Skin used throughout this class — see
     *     applyViaUpgrader()/forceCoreViaUpgrader()/applyCoreUpgrade()) — the
     *     exact strings WP core's own upgrade UI would have shown (e.g.
     *     "Downloading update from …", "Unpacking the update…", "The package
     *     could not be installed.", a PCLZIP error). Captured for BOTH a
     *     WP_Error result and a bare false/null result, since either can hide
     *     more specific detail than the WP_Error's own get_error_message()
     *     alone (which is not always populated — see collectSkinMessages()'s
     *     doc for why the skin's own messages are the richer source).
     *
     * @param mixed       $result   Upgrader result (bool|array|WP_Error|null).
     * @param object|null $upgrader The WP_Upgrader instance that produced
     *                              $result, when available, so its skin's
     *                              collected messages can be captured. Never
     *                              required — a null value degrades to using
     *                              $result alone (the pre-0.61.21 behaviour).
     * @return array{ok:bool,log:string}
     */
    private function upgraderOutcome($result, ?object $upgrader = null): array
    {
        $skinMessages = $this->collectSkinMessages($upgrader);

        if (is_object($result) && method_exists($result, 'get_error_message')) {
            /** @var \WP_Error $result */
            $code = method_exists($result, 'get_error_code') ? (string) $result->get_error_code() : '';
            $log  = 'WP_Error' . ($code !== '' ? ' (' . $code . ')' : '') . ': ' . $result->get_error_message();
            if ($skinMessages !== '') {
                $log .= "\n" . $skinMessages;
            }

            return ['ok' => false, 'log' => $log];
        }

        if ($result === false || $result === null) {
            $log = $skinMessages !== '' ? 'Update failed: ' . $skinMessages : 'Update failed.';

            return ['ok' => false, 'log' => $log];
        }

        return ['ok' => true, 'log' => 'Update applied via upgrader.'];
    }

    /**
     * Best-effort capture of the messages a WP_Ajax_Upgrader_Skin (or any
     * Automatic_Upgrader_Skin descendant) collected during this upgrade()
     * call — WordPress's own progress/error strings, the same ones its admin
     * UI would render. Never throws: an unexpected shape (a skin without
     * get_upgrade_messages(), or a non-array return) degrades to an empty
     * string rather than affecting the outcome above.
     *
     * @param object|null $upgrader The WP_Upgrader instance, or null.
     * @return string A single, condensed, log-safe line with any HTML tags
     *                 stripped (the skin's own messages allow basic markup —
     *                 a/br/em/strong — which has no place in a plain-text CP
     *                 log line), or '' when nothing could be collected.
     */
    private function collectSkinMessages(?object $upgrader): string
    {
        if ($upgrader === null || !isset($upgrader->skin) || !is_object($upgrader->skin)) {
            return '';
        }

        $skin = $upgrader->skin;
        if (!method_exists($skin, 'get_upgrade_messages')) {
            return '';
        }

        $messages = $skin->get_upgrade_messages();
        if (!is_array($messages) || $messages === []) {
            return '';
        }

        $clean = [];
        foreach ($messages as $message) {
            if (!is_string($message) || $message === '') {
                continue;
            }
            $text = $message;
            if (function_exists('wp_strip_all_tags')) {
                $text = wp_strip_all_tags($text);
            }
            $text = trim($text);
            if ($text !== '') {
                $clean[] = $text;
            }
        }

        if ($clean === []) {
            return '';
        }

        // Cap to the last 5 lines — the failure context is almost always at
        // or near the end of the collected messages; earlier entries are
        // routine "Downloading…"/"Unpacking…" progress noise that would
        // otherwise dominate a short CP-visible log line.
        return implode(' | ', array_slice($clean, -5));
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
     * Force exactly one fresh update-check per component-type for the
     * lifetime of this UpdateRunner instance — i.e. once per RUN, not once
     * per item (GitHub issue #218, a regression from #208/#212).
     *
     * #208/#212 made pluginUpdateVersion()/themeUpdateVersion()/
     * coreUpdateVersion() each force a fresh check (delete the relevant
     * site transient, then re-run wp_update_plugins()/wp_update_themes()/
     * wp_version_check([], true)) before reading the transient, so a
     * stale/expired transient could never masquerade as "genuinely up to
     * date". But UpdateCommand::execute() calls availableVersion() — and
     * therefore this force-fresh-check block — ONCE PER ITEM in its foreach
     * over a bulk `update` task. With N `latest` items of the same type in
     * one run, that meant N full wp.org catalog round-trips in a single
     * request, each one discarding the fresh transient the PRIOR item's
     * call had just populated; a mid-batch network failure could leave a
     * later item reading a freshly-emptied/incomplete transient and
     * misreport a genuinely pending update as up_to_date.
     *
     * The fix: memoize by type. The flag for $type is set FIRST, before the
     * force block runs (re-entrancy safe — nothing inside wp_update_plugins()
     * et al. can trigger a second round-trip or recursion for the same
     * type), so every subsequent item of the SAME type in the same run reads
     * the transient this first call already populated — one shared fresh
     * transient covers every per-slug lookup, matching the apply path's own
     * single forced check per run (see applyViaUpgrader()/
     * applyCoreUpgrade()).
     *
     * VERIFIED SAFE: the apply path never calls this method or
     * availableVersion() — applyViaUpgrader()/applyCoreUpgrade() call
     * wp_update_plugins()/wp_update_themes()/wp_version_check() directly —
     * so this memo cannot affect it. RollbackCommand builds its own
     * UpdateRunner and never calls availableVersion() either.
     *
     * core intentionally has no delete_site_transient() call, matching
     * coreUpdateVersion()'s pre-existing (unchanged) behaviour — core has no
     * separate transient-deletion step; wp_version_check()'s own `true`
     * second argument is WordPress's "ignore the 12-hour throttle" flag and
     * was always sufficient on its own.
     *
     * @param string $type plugin|theme|core.
     * @return void
     */
    private function forceFreshCheckOncePerRun(string $type): void
    {
        if (isset($this->freshCheckedTypes[$type])) {
            return;
        }
        $this->freshCheckedTypes[$type] = true;

        switch ($type) {
            case 'plugin':
                if (function_exists('delete_site_transient')) {
                    delete_site_transient('update_plugins');
                }
                if (function_exists('wp_update_plugins')) {
                    wp_update_plugins();
                }
                break;

            case 'theme':
                if (function_exists('delete_site_transient')) {
                    delete_site_transient('update_themes');
                }
                if (function_exists('wp_update_themes')) {
                    wp_update_themes();
                }
                break;

            case 'core':
                if (function_exists('wp_version_check')) {
                    wp_version_check([], true);
                }
                break;
        }
    }

    /**
     * Pending update version for a plugin from the update transient.
     *
     * Forces a fresh check via forceFreshCheckOncePerRun('plugin') before
     * reading the transient (GitHub issue #208) — the identical,
     * unconditional call applyViaUpgrader() already makes before a real
     * plugin apply (see below). This closes the gap where this READ-ONLY
     * resolution path trusted whatever was already cached while the APPLY
     * path would have refreshed it first: a stale/expired/never-yet-populated
     * transient can no longer make a real pending update look like "up to
     * date" here. That forced check now runs at most ONCE PER RUN per
     * component-type (GitHub issue #218), not once per item — see
     * forceFreshCheckOncePerRun()'s doc for the bulk-run regression this
     * fixes and why that is still safe.
     *
     * GH #212 residual gap: wp_update_plugins() alone stays throttled by
     * WordPress core's own ~12h `update_plugins` transient window — off-cron
     * (i.e. any caller reaching this method outside the 30-min metadata cron,
     * which already force-refreshed via Scheduler::refreshUpdateTransients())
     * a "forced" check here could still read a stale cached response. Delete
     * the transient FIRST so wp_update_plugins() always performs a real
     * re-check, matching applyViaUpgrader()'s equivalent forced pre-apply
     * check and coreUpdateVersion()'s `wp_version_check([], true)` below.
     *
     * @param string $slug Plugin basename.
     * @return string|null The pending version, '' when a forced fresh check
     *                confirms none is available, or null when availability
     *                could not be determined even after that check
     *                (get_site_transient()/wp_update_plugins() unavailable
     *                in this runtime, or the transient is not the
     *                well-formed object WordPress itself always produces
     *                once a check has actually completed).
     */
    private function pluginUpdateVersion(string $slug): ?string
    {
        $this->forceFreshCheckOncePerRun('plugin');

        if (!function_exists('get_site_transient')) {
            return null;
        }
        $transient = get_site_transient('update_plugins');
        if (!is_object($transient) || !isset($transient->response) || !is_array($transient->response)) {
            return null;
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
     * Forces a fresh check via forceFreshCheckOncePerRun('theme') before
     * reading the transient (GitHub issue #208) — same rationale and shape
     * as pluginUpdateVersion(); see its doc. That forced check now runs at
     * most ONCE PER RUN per component-type (GitHub issue #218), not once
     * per item — see forceFreshCheckOncePerRun()'s doc.
     *
     * GH #212 residual gap: see pluginUpdateVersion()'s doc — the same
     * off-cron core-throttle gap applies here, so the transient is deleted
     * before wp_update_themes() forces a real re-check.
     *
     * @param string $slug Theme stylesheet.
     * @return string|null The pending version, '' when a forced fresh check
     *                confirms none is available, or null when availability
     *                could not be determined even after that check
     *                (get_site_transient()/wp_update_themes() unavailable
     *                in this runtime, or the transient is not the
     *                well-formed object WordPress itself always produces
     *                once a check has actually completed).
     */
    private function themeUpdateVersion(string $slug): ?string
    {
        $this->forceFreshCheckOncePerRun('theme');

        if (!function_exists('get_site_transient')) {
            return null;
        }
        $transient = get_site_transient('update_themes');
        if (!is_object($transient) || !isset($transient->response) || !is_array($transient->response)) {
            return null;
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
     * Forces a fresh check via forceFreshCheckOncePerRun('core') before
     * reading the transient (GitHub issue #208) — the identical forced,
     * cache-bypassing call applyCoreUpgrade() already makes before a real
     * core apply (the `true` second argument is WordPress's own "ignore the
     * 12-hour throttle" flag). Same rationale as pluginUpdateVersion(); see
     * its doc. That forced check now runs at most ONCE PER RUN per
     * component-type (GitHub issue #218), not once per item — see
     * forceFreshCheckOncePerRun()'s doc.
     *
     * @return string|null The pending core version, '' when a forced fresh
     *                check confirms none is available, or null when
     *                availability could not be determined even after that
     *                check (get_site_transient()/wp_version_check()
     *                unavailable in this runtime, or the transient is not
     *                the well-formed object WordPress itself always
     *                produces once a check has actually completed).
     */
    private function coreUpdateVersion(): ?string
    {
        $this->forceFreshCheckOncePerRun('core');

        if (!function_exists('get_site_transient')) {
            return null;
        }
        $transient = get_site_transient('update_core');
        if (!is_object($transient) || !isset($transient->updates) || !is_array($transient->updates)) {
            return null;
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
