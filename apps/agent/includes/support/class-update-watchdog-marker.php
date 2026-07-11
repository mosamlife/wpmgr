<?php
/**
 * UpdateWatchdogMarker: writes/refreshes the small "watch marker" file that
 * the autoloader-free mu-plugin `mu-plugin-loader/a-wpmgr-update-watchdog.php`
 * reads during a fatal-triggered `register_shutdown_function()` callback
 * (GitHub issue #210). See the extended class doc below the direct-access
 * guard for the full design rationale.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

if (!defined('ABSPATH')) {
    exit; // No direct access.
}

/*
 * Background: when a plugin/theme update applies cleanly but then fatals on
 * EVERY WordPress bootstrap (e.g. `Class ... not found` in the updated
 * plugin's own loader), the site is down site-wide, and WPMgr's normal
 * recovery path — a signed REST call to RollbackCommand — can never be
 * reached, because `rest_api_init` fires AFTER all regular plugins have
 * already finished loading; the fatal happens before that point is ever
 * reached. `SnapshotManager::restore()` is a pure filesystem operation that
 * needs no plugin bootstrap at all, so a `register_shutdown_function()`
 * registered by an mu-plugin (which loads BEFORE any regular plugin) can
 * still invoke an equivalent restore even when the agent plugin itself never
 * finished loading on this request.
 *
 * This class is the ARM side of that mechanism, called from
 * `UpdateCommand::processItem()` immediately after a plugin/theme apply is
 * verified `succeeded` (never for `failed`/`skipped`/`up_to_date`, and never
 * for `core` — there is no directory-level snapshot for core; see
 * SnapshotManager's own class doc, D3). It persists a short-TTL, one-shot
 * marker as a FILE (not a wp-option) in the agent's own state directory —
 * see STORAGE LOCATION below for why — containing everything the mu-plugin
 * needs, with ABSOLUTE paths resolved and validated up front via
 * `SnapshotManager::resolvedRestorePaths()`: the mu-plugin never recomputes
 * WordPress-dependent paths from scratch and never trusts the marker's path
 * strings alone (see the mu-plugin's own doc for the independent
 * re-validation it still performs before ever touching disk).
 *
 * STORAGE LOCATION — deliberately WP_CONTENT_DIR-based, NOT the uploads-first
 * `StoragePaths::dataBase()` convention every other agent user-data store
 * uses. The mu-plugin's per-request "does a marker exist at all" check MUST
 * be a single, side-effect-free filesystem stat that runs on EVERY request
 * (see that file's doc) — `wp_upload_dir()` is unsafe for that hot path: its
 * default `$create_dir = true` argument can trigger an `apply_filters()` call
 * and a `wp_mkdir_p()` write on every invocation, and it depends on options
 * that may not yet be warm this early in bootstrap. `WP_CONTENT_DIR` is a
 * plain PHP constant, defined before mu-plugins load, with zero I/O to read.
 * Using it for BOTH the write (here) and the read (the mu-plugin) guarantees
 * the two sides always agree on the marker's path, independent of any
 * uploads relocation (`UPLOADS` constant / `upload_path` option) a site may
 * have configured.
 *
 * MULTI-ITEM BATCHES — `UpdateCommand::execute()` can apply several
 * plugin/theme items in one request. The marker file holds a small JSON
 * ARRAY of per-slug entries (keyed by exact type+slug), not a single
 * overwritten record, so a later item in the same batch succeeding never
 * erases an earlier item's watch — each slug's entry is independently
 * refreshed/replaced, and the mu-plugin's cheap per-request check still
 * costs exactly one `is_file()` stat regardless of how many entries the file
 * holds once it exists.
 *
 * EARLY DISARM (MEDIUM-1b, GitHub issue #210 security review) — TTL_SECONDS
 * alone left a genuinely GOOD update revert-eligible for the full 5-minute
 * window. `disarmHealthy()` clears an armed entry as soon as a normal,
 * un-fataled agent boot proves the exact slug it is watching is installed,
 * active, and on-disk at the armed `to_version` — see that method's own doc
 * for the full reasoning and the fail-closed guarantee on every uncertain
 * case.
 */

/**
 * Persists the update-watchdog marker consumed by the mu-plugin shutdown hook.
 */
final class UpdateWatchdogMarker
{
    /**
     * StoragePaths purpose slug — resolves (via legacyBase(), the
     * WP_CONTENT_DIR-based resolver, NOT the uploads-first dataBase()) to
     * `wp-content/wpmgr-update-watchdog/`. See class doc, STORAGE LOCATION.
     */
    private const PURPOSE = 'update-watchdog';

    /**
     * Deterministic marker filename. The mu-plugin's cheap per-request check
     * stats EXACTLY this path (mirrored verbatim in
     * `a-wpmgr-update-watchdog.php`'s own `wpmgr_watchdog_marker_path()`) —
     * keep both in sync if this ever changes.
     */
    public const MARKER_BASENAME = 'watchdog-marker.json';

    /**
     * Prefix for the one-shot "claim" sentinel files the mu-plugin creates
     * (via an atomic `fopen($file, 'x')`) the instant it commits to restoring
     * one specific snapshot, so a second, concurrently-fataling request can
     * never restore the same snapshot twice. Exposed here only so this
     * class's opportunistic GC (pruneStaleClaims()) recognizes its own
     * sentinels; the mu-plugin is the only writer.
     */
    private const CLAIM_PREFIX = 'watchdog-claim-';

    /**
     * A claim sentinel older than this is considered abandoned bookkeeping
     * (its owning restore attempt — success or failure — finished long ago)
     * and is swept opportunistically on the next arm() call. Generously
     * longer than TTL_SECONDS so it can never race a genuinely in-progress
     * watch window; purely a disk-hygiene backstop, not a safety control.
     */
    private const CLAIM_STALE_AFTER_SECONDS = 3600;

    /**
     * Watch-window TTL. MUST stay strictly less than
     * `SnapshotManager::MIN_KEEP_AGE_SECONDS` (600s, see that constant's own
     * class-level doc) so a marker can never legitimately outlive the
     * retention floor protecting the exact snapshot payload it points at —
     * if the marker ever DID outlive it, the mu-plugin's independent
     * re-derivation of the payload path (which requires the payload
     * directory to still exist on disk) would simply fail closed instead,
     * but keeping the TTL strictly shorter means that never needs to be
     * relied on. 300s (5 minutes) comfortably overlaps the control plane's
     * ~1-minute post-update health-probe window (including its retry — see
     * UpdateCommand's own class doc) while remaining far short of the 600s
     * floor.
     */
    public const TTL_SECONDS = 300;

    /**
     * Arm (or refresh) the watch-marker entry for one successfully applied
     * plugin/theme item. Best-effort and silent on any failure: this is
     * defense-in-depth for a recovery path that does not exist today (GitHub
     * issue #210); a failure to persist it must never affect the apply that
     * already succeeded, nor the response returned to the control plane.
     *
     * Refuses to arm (silently, not an error) when:
     *   - any argument is empty, or $type is not exactly 'plugin'/'theme';
     *   - $liveDir resolves to the running agent's OWN plugin directory —
     *     mirrors `FilesRestorer::PRESERVE_FROM_LIVE`'s protection of
     *     `plugins/wpmgr-agent`: a directory-replacement recovery mechanism
     *     must never be armed against the very code that could someday run
     *     it.
     *
     * @param string $type       'plugin'|'theme'.
     * @param string $slug       Sanitized slug (already validated by
     *                            UpdateCommand::sanitizeSlug() upstream).
     * @param string $snapshotId Snapshot identifier captured for this apply.
     * @param string $liveDir    ABSOLUTE live target directory, already
     *                            resolved + validated by
     *                            SnapshotManager::resolvedRestorePaths().
     * @param string $payloadDir ABSOLUTE snapshot payload directory (same).
     * @param string $toVersion  The version this apply moved the item to.
     *                            Recorded so MEDIUM-1b's disarmHealthy() can
     *                            confirm the on-disk version still matches
     *                            before clearing this entry early (see that
     *                            method's own doc). An empty string is
     *                            stored as-is when the caller could not
     *                            determine it — disarmHealthy() then simply
     *                            never disarms that entry early, falling
     *                            back to the existing TTL.
     * @return void
     */
    public static function arm(string $type, string $slug, string $snapshotId, string $liveDir, string $payloadDir, string $toVersion = ''): void
    {
        if ($type !== 'plugin' && $type !== 'theme') {
            return;
        }
        if ($slug === '' || $snapshotId === '' || $liveDir === '' || $payloadDir === '') {
            return;
        }

        $liveDir    = rtrim($liveDir, '/\\');
        $payloadDir = rtrim($payloadDir, '/\\');

        if (defined('WPMGR_AGENT_DIR')) {
            $selfDir = rtrim((string) constant('WPMGR_AGENT_DIR'), '/\\');
            if ($selfDir !== '' && $liveDir === $selfDir) {
                return;
            }
        }

        try {
            $dir = StoragePaths::ensureHardenedPath(self::stateDir());
            if ($dir === '') {
                return;
            }

            $file = $dir . '/' . self::MARKER_BASENAME;
            $now  = time();

            $kept = [];
            foreach (self::readMarkers($file) as $entry) {
                if (!is_array($entry)) {
                    continue;
                }
                $entryType = isset($entry['type']) && is_string($entry['type']) ? $entry['type'] : '';
                $entrySlug = isset($entry['slug']) && is_string($entry['slug']) ? $entry['slug'] : '';
                $expiresAt = isset($entry['expires_at']) && is_numeric($entry['expires_at']) ? (int) $entry['expires_at'] : 0;

                if ($entryType === $type && $entrySlug === $slug) {
                    // Superseded by the fresh entry below — "a new apply
                    // overwrites/refreshes" (this exact slug's own watch).
                    continue;
                }
                if ($expiresAt <= $now) {
                    // Opportunistic prune of an already-expired sibling entry
                    // so the file never grows across a long-lived site's
                    // update history.
                    continue;
                }
                $kept[] = $entry;
            }

            $kept[] = [
                'type'        => $type,
                'slug'        => $slug,
                'snapshot_id' => $snapshotId,
                'live_dir'    => $liveDir,
                'payload_dir' => $payloadDir,
                'to_version'  => $toVersion,
                'applied_at'  => $now,
                'expires_at'  => $now + self::TTL_SECONDS,
            ];

            self::writeMarkers($file, $kept);
            self::pruneStaleClaims($dir, $now);
        } catch (\Throwable $e) {
            DebugLog::write(
                'WPMgr Agent: update-watchdog arm() failed for ' . $type . ':' . $slug . ': ' . $e->getMessage()
            );
        }
    }

    /**
     * MEDIUM-1b (GitHub issue #210 security review) — clear the marker entry
     * for exactly one type/slug pair. Low-level primitive: mirrors
     * `UpdateInFlight::clear()`'s per-slug removal shape. Used both by
     * disarmHealthy() below and available standalone for any future caller
     * that needs to remove a single slug's watch without touching sibling
     * entries. Safe to call unconditionally, including when no marker file
     * exists or no entry matches — both are silent no-ops.
     *
     * @param string $type 'plugin'|'theme'.
     * @param string $slug Sanitized slug.
     * @return void
     */
    public static function clearSlug(string $type, string $slug): void
    {
        if ($type === '' || $slug === '') {
            return;
        }

        try {
            $file = self::markerFile();
            if ($file === '' || !is_file($file)) {
                return;
            }

            $markers = self::readMarkers($file);
            if ($markers === []) {
                return;
            }

            $kept    = [];
            $changed = false;
            foreach ($markers as $entry) {
                if (!is_array($entry)) {
                    continue;
                }
                $entryType = isset($entry['type']) && is_string($entry['type']) ? $entry['type'] : '';
                $entrySlug = isset($entry['slug']) && is_string($entry['slug']) ? $entry['slug'] : '';

                if ($entryType === $type && $entrySlug === $slug) {
                    $changed = true;
                    continue;
                }
                $kept[] = $entry;
            }

            if (!$changed) {
                return;
            }

            if ($kept === []) {
                // Delete rather than write an empty {"markers":[]} file —
                // keeps the mu-plugin's cheap per-request is_file() check
                // returning false again immediately, rather than finding an
                // empty-but-present file and registering a shutdown handler
                // that would just no-op every time.
                wp_delete_file($file);

                return;
            }

            self::writeMarkers($file, $kept);
        } catch (\Throwable $e) {
            DebugLog::write(
                'WPMgr Agent: update-watchdog clearSlug() failed for ' . $type . ':' . $slug . ': ' . $e->getMessage()
            );
        }
    }

    /**
     * MEDIUM-1b (GitHub issue #210 security review) — the "principled
     * disarm": clear the watchdog marker entry for any armed slug that has
     * just been proven healthy on THIS exact request. Intended to be called
     * from the agent's own `plugins_loaded` boot hook (see
     * `WPMgr\Agent\Plugin`) — reaching that hook at all is itself proof the
     * ENTIRE plugin-loading loop (every active plugin, including the one an
     * armed entry is watching) completed this request without triggering
     * the every-request bootstrap fatal GitHub issue #210 exists to catch.
     * This shrinks the over-fire window from the full TTL_SECONDS down to
     * exactly that scenario: a fataling boot never reaches this hook, so the
     * marker correctly survives for the watchdog in that case.
     *
     * Gated on the marker file existing (a single is_file() stat, mirroring
     * the mu-plugin's own cheap per-request check) so the common healthy
     * hot path — no marker at all — costs nothing beyond that one stat.
     *
     * "Healthy" for one entry means ALL of:
     *   - the recorded to_version is non-empty (a marker armed before this
     *     fix, or one whose caller could not determine the target version,
     *     is left alone — nothing to positively confirm against);
     *   - the plugin/theme is currently installed AND its on-disk Version
     *     header exactly equals the recorded to_version;
     *   - the plugin/theme is currently ACTIVE (is_plugin_active()/
     *     is_plugin_active_for_network() for a plugin; the current
     *     stylesheet/template for a theme) — an installed-but-inactive item
     *     proves nothing about whether ITS code loads cleanly, since
     *     inactive plugin code is never included during the bootstrap loop
     *     this feature guards.
     * Every one of these fails CLOSED: any WP detection API being
     * unavailable, or any single condition not positively confirmed, leaves
     * the entry armed — the existing TTL_SECONDS window remains the
     * backstop exactly as it was before this fix. This method never widens
     * the window a marker survives in, only shrinks it.
     *
     * Deliberately does NOT reuse `UpdateRunner::isInstalled()` — that
     * method fails OPEN (returns true when detection is unavailable) by
     * design for its own (very different) purpose; a fail-open default here
     * would be exactly backwards for a check whose only job is deciding
     * whether it is safe to give up a safety net early.
     *
     * @return void
     */
    public static function disarmHealthy(): void
    {
        try {
            $file = self::markerFile();
            if ($file === '' || !is_file($file)) {
                return;
            }

            $markers = self::readMarkers($file);
            if ($markers === []) {
                return;
            }

            foreach ($markers as $entry) {
                if (!is_array($entry)) {
                    continue;
                }
                $type = isset($entry['type']) && is_string($entry['type']) ? $entry['type'] : '';
                $slug = isset($entry['slug']) && is_string($entry['slug']) ? $entry['slug'] : '';
                if ($type === '' || $slug === '') {
                    continue;
                }
                if (self::isEntryHealthy($entry)) {
                    self::clearSlug($type, $slug);
                }
            }
        } catch (\Throwable $e) {
            DebugLog::write('WPMgr Agent: update-watchdog disarmHealthy() failed: ' . $e->getMessage());
        }
    }

    /**
     * @param array<string,mixed> $entry Decoded marker entry.
     * @return bool
     */
    private static function isEntryHealthy(array $entry): bool
    {
        $type      = isset($entry['type']) && is_string($entry['type']) ? $entry['type'] : '';
        $slug      = isset($entry['slug']) && is_string($entry['slug']) ? $entry['slug'] : '';
        $toVersion = isset($entry['to_version']) && is_string($entry['to_version']) ? $entry['to_version'] : '';

        if ($slug === '' || $toVersion === '') {
            return false;
        }

        if ($type === 'plugin') {
            return self::isPluginHealthyAt($slug, $toVersion);
        }
        if ($type === 'theme') {
            return self::isThemeHealthyAt($slug, $toVersion);
        }

        return false;
    }

    /**
     * @param string $slug      Plugin slug ("folder/main-file.php").
     * @param string $toVersion Recorded target version.
     * @return bool
     */
    private static function isPluginHealthyAt(string $slug, string $toVersion): bool
    {
        // Mirrors UpdateRunner::loadPluginApi()'s own guard exactly.
        if (!function_exists('get_plugins') && defined('ABSPATH') && file_exists(ABSPATH . 'wp-admin/includes/plugin.php')) {
            require_once ABSPATH . 'wp-admin/includes/plugin.php';
        }
        if (!function_exists('get_plugins') || !function_exists('is_plugin_active')) {
            return false;
        }

        $all = get_plugins();
        if (!is_array($all) || !isset($all[$slug]) || !is_array($all[$slug]) || !isset($all[$slug]['Version'])) {
            return false;
        }
        if ((string) $all[$slug]['Version'] !== $toVersion) {
            return false;
        }

        if (\is_plugin_active($slug)) {
            return true;
        }

        return function_exists('is_plugin_active_for_network') && \is_plugin_active_for_network($slug);
    }

    /**
     * @param string $slug      Theme stylesheet slug.
     * @param string $toVersion Recorded target version.
     * @return bool
     */
    private static function isThemeHealthyAt(string $slug, string $toVersion): bool
    {
        if (!function_exists('wp_get_themes')) {
            return false;
        }
        $themes = wp_get_themes();
        if (!is_array($themes) || !isset($themes[$slug])) {
            return false;
        }
        $theme = $themes[$slug];
        if (!is_object($theme) || !method_exists($theme, 'get')) {
            return false;
        }
        if ((string) $theme->get('Version') !== $toVersion) {
            return false;
        }

        if (function_exists('get_stylesheet') && get_stylesheet() === $slug) {
            return true;
        }

        return function_exists('get_template') && get_template() === $slug;
    }

    /**
     * The WP_CONTENT_DIR-based watchdog state directory, unhardened (no
     * mkdir/guard-file side effects) — safe to call from the cheap
     * disarmHealthy()/clearSlug() read paths. arm() is the only caller that
     * needs the hardened (created + guarded) variant.
     *
     * @return string
     */
    private static function stateDir(): string
    {
        return StoragePaths::legacyBase(self::PURPOSE);
    }

    /**
     * Absolute marker file path, or '' when the state dir cannot be
     * resolved. Does not check existence.
     *
     * @return string
     */
    private static function markerFile(): string
    {
        $dir = self::stateDir();

        return $dir === '' ? '' : rtrim($dir, '/\\') . '/' . self::MARKER_BASENAME;
    }

    /**
     * Read the current marker file's entries. Returns an empty list when the
     * file is absent, unreadable, or not shaped as expected — never throws.
     *
     * @param string $file Absolute marker file path.
     * @return list<array<string,mixed>>
     */
    private static function readMarkers(string $file): array
    {
        if (!is_file($file)) {
            return [];
        }
        $raw = @file_get_contents($file);
        if (!is_string($raw) || $raw === '') {
            return [];
        }
        $decoded = json_decode($raw, true);
        if (!is_array($decoded) || !isset($decoded['markers']) || !is_array($decoded['markers'])) {
            return [];
        }

        return array_values($decoded['markers']);
    }

    /**
     * Atomically rewrite the marker file: write to a randomly-named temp file
     * in the same directory, then `rename()` over the real path. rename() is
     * atomic on the same filesystem, so a shutdown-handler reader (or a
     * concurrent arm() call) never observes a half-written JSON file.
     *
     * @param string                     $file    Absolute marker file path.
     * @param list<array<string,mixed>>  $markers Entries to persist.
     * @return void
     */
    private static function writeMarkers(string $file, array $markers): void
    {
        $payload = wp_json_encode(['markers' => array_values($markers)]);
        if (!is_string($payload)) {
            return;
        }

        $tmp = $file . '.' . bin2hex(random_bytes(8)) . '.tmp';
        // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_file_put_contents -- headless agent; WP_Filesystem never initialized; direct write of a small, server-derived JSON marker to a temp path before an atomic rename
        $written = @file_put_contents($tmp, $payload, LOCK_EX);
        if ($written === false) {
            return;
        }
        @chmod($tmp, 0600); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_chmod -- explicit security perms (0600); WP_Filesystem would coerce to wider FS_CHMOD_FILE

        if (!@rename($tmp, $file)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- atomic same-filesystem swap so a concurrent reader never sees a half-written marker; WP_Filesystem::move() is copy+delete (non-atomic)
            wp_delete_file($tmp);
        }
    }

    /**
     * Sweep claim sentinel files older than CLAIM_STALE_AFTER_SECONDS. Purely
     * disk hygiene (each sentinel is a 0-byte file) — never affects
     * correctness, since the mu-plugin's one-shot semantics only ever depend
     * on a claim file's EXISTENCE at the moment it attempts `fopen($f,'x')`,
     * never on its age.
     *
     * @param string $dir Watchdog state directory.
     * @param int    $now Current time.
     * @return void
     */
    private static function pruneStaleClaims(string $dir, int $now): void
    {
        $items = @scandir($dir);
        if (!is_array($items)) {
            return;
        }
        foreach ($items as $item) {
            if (strpos($item, self::CLAIM_PREFIX) !== 0) {
                continue;
            }
            $path = $dir . '/' . $item;
            if (!is_file($path)) {
                continue;
            }
            $mtime = @filemtime($path);
            if ($mtime !== false && ($now - $mtime) < self::CLAIM_STALE_AFTER_SECONDS) {
                continue;
            }
            wp_delete_file($path);
        }
    }
}
